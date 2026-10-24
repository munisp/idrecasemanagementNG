// Author: Patrick Munis at newwave
package main

// healthplan_onboarding.go — payer-side onboarding rail (feature flag:
// healthplan_onboarding), the mirror of tpa_onboarding.go.
//
// A health plan registers as a first-class organization (NAIC / state
// license / line-of-business credentialing), claims itself via one-time
// code, rosters the parties it submits for (self-funded employer groups,
// delegated networks/IPAs, facilities), stands up a verified CARD/ACH rail
// PER PARTY, and assembles a validated six-document package per party —
// discrete uploads or one merged PDF split by doc-intel. Filing is gated on
// a READY package + verified payment.
//
// Reuses the TPA rail's validators and slot vocabulary; payer-specific
// differences live in credentialing (NAIC/license/LOB) and doc-intel's
// QPA-substantiation expectation for plan-uploaded OFFER_SUBSTANTIATION.

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// hpPartyTypes: who a health plan may submit for.
var hpPartyTypes = map[string]bool{
	"EMPLOYER_GROUP":    true, // self-funded / ASO group the plan administers
	"DELEGATED_NETWORK": true, // IPA / capitated network the plan pays for
	"FACILITY":          true, // ancillary facility under the master contract
}

var hpLOBs = map[string]bool{
	"FULLY_INSURED": true, "ASO": true, "MEDICARE_ADVANTAGE": true, "MEDICAID_MANAGED": true,
}

type healthPlanOrg struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	NAIC         string `json:"naic"`
	LOB          string `json:"line_of_business"`
	ContactName  string `json:"contact_name"`
	ContactEmail string `json:"contact_email"`
	Status       string `json:"status"`
}

// healthPlanForPrincipal resolves the caller's plan org (linked via
// health_plan_users).
func (s *server) healthPlanForPrincipal(r *http.Request, tenant string) (*healthPlanOrg, error) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var o healthPlanOrg
	err := s.db.QueryRow(r.Context(), `
		SELECT h.id, h.name, coalesce(h.naic,''), coalesce(h.line_of_business,''),
		       h.contact_name, h.contact_email, h.status
		FROM public.health_plan_users u JOIN public.health_plans h ON h.id = u.plan_id
		WHERE u.tenant=$1 AND u.user_sub=$2`, tenant, p.Subject).
		Scan(&o.ID, &o.Name, &o.NAIC, &o.LOB, &o.ContactName, &o.ContactEmail, &o.Status)
	if err != nil {
		return nil, fmt.Errorf("no health plan organization linked to your account — claim one with your claim code")
	}
	if o.Status != "ACTIVE" {
		return nil, fmt.Errorf("health plan organization is %s", o.Status)
	}
	return &o, nil
}

// registerHealthPlan: PUBLIC self-serve onboarding; returns a one-time claim code.
func (s *server) registerHealthPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tenant         string `json:"tenant"`
		Name           string `json:"name"`
		NAIC           string `json:"naic"`
		FEIN           string `json:"fein"`
		StateLicense   string `json:"state_license"`
		LineOfBusiness string `json:"line_of_business"`
		QPAContact     string `json:"qpa_contact_email"`
		ContactName    string `json:"contact_name"`
		ContactEmail   string `json:"contact_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		in.Tenant == "" || in.Name == "" || in.ContactEmail == "" || !strings.Contains(in.ContactEmail, "@") {
		http.Error(w, `{"error":"tenant, name, and a valid contact_email are required"}`, http.StatusBadRequest)
		return
	}
	in.NAIC = strings.TrimSpace(in.NAIC)
	if in.NAIC != "" && !isDigits(in.NAIC, 5) {
		http.Error(w, `{"error":"NAIC must be 5 digits"}`, http.StatusBadRequest)
		return
	}
	if in.FEIN != "" && !tinValid(strings.TrimSpace(in.FEIN)) {
		http.Error(w, `{"error":"FEIN must be 9 digits"}`, http.StatusBadRequest)
		return
	}
	lob := strings.ToUpper(strings.TrimSpace(in.LineOfBusiness))
	if lob != "" && !hpLOBs[lob] {
		http.Error(w, `{"error":"line_of_business must be FULLY_INSURED|ASO|MEDICARE_ADVANTAGE|MEDICAID_MANAGED"}`, http.StatusBadRequest)
		return
	}
	code := "HP-" + strings.ToUpper(hex6())
	var id string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.health_plans
		  (tenant, name, naic, fein, state_license, line_of_business, qpa_contact_email, contact_name, contact_email, claim_code, status)
		VALUES (lower($1), $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), $8, $9, $10, 'ACTIVE')
		RETURNING id`,
		in.Tenant, in.Name, in.NAIC, strings.TrimSpace(in.FEIN), in.StateLicense, lob,
		in.QPAContact, in.ContactName, in.ContactEmail, code).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"registration failed — an organization with this email may already exist for this program"}`, http.StatusConflict)
		return
	}
	if s.featureEnabled(r, strings.ToLower(in.Tenant), "entity360") {
		s.ensureEntityQuiet(r.Context(), strings.ToLower(in.Tenant), "health_plans", id, "HEALTH_PLAN", in.Name, "", strings.TrimSpace(in.FEIN), in.NAIC, strings.TrimSpace(in.FEIN))
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"plan_id": id, "status": "ACTIVE", "claim_code": code,
		"next": "Sign in to the portal with a health plan account and enter this claim code to link your organization. Then roster your parties, set up payment per party, and assemble validated document packages.",
	})
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// hex6: 3 random bytes as uppercase hex — one-time claim codes.
func hex6() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// claimHealthPlan links the authenticated user to their plan org via the code.
func (s *server) claimHealthPlan(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Code == "" {
		http.Error(w, `{"error":"claim code required"}`, http.StatusBadRequest)
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(), `
		SELECT id FROM public.health_plans WHERE tenant=$1 AND claim_code=$2 AND status='ACTIVE'`,
		tenant, strings.ToUpper(strings.TrimSpace(in.Code))).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"invalid claim code"}`, http.StatusNotFound)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.health_plan_users (tenant, user_sub, plan_id) VALUES ($1,$2,$3)
		ON CONFLICT (tenant, user_sub) DO UPDATE SET plan_id=$3`, tenant, p.Subject, id)
	_, _ = s.db.Exec(r.Context(), `UPDATE public.health_plans SET claim_code=NULL, updated_at=now() WHERE id=$1`, id)
	writeJSON(w, http.StatusOK, map[string]any{"plan_id": id, "status": "linked"})
}

// hpMe: the caller's plan org.
func (s *server) hpMe(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"health_plan": org})
}

// --- parties ---------------------------------------------------------------

// hpAddParty rosters one party the plan submits for.
func (s *server) hpAddParty(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		PartyName    string `json:"party_name"`
		PartyType    string `json:"party_type"`
		ContactEmail string `json:"contact_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.PartyName) == "" {
		http.Error(w, `{"error":"party_name required"}`, http.StatusBadRequest)
		return
	}
	ptype := strings.ToUpper(strings.TrimSpace(in.PartyType))
	if !hpPartyTypes[ptype] {
		http.Error(w, `{"error":"party_type must be EMPLOYER_GROUP|DELEGATED_NETWORK|FACILITY"}`, http.StatusBadRequest)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.hp_parties (tenant, plan_id, party_name, party_type, contact_email)
		VALUES ($1,$2,$3,$4,NULLIF($5,'')) RETURNING id`,
		tenant, org.ID, strings.TrimSpace(in.PartyName), ptype, strings.TrimSpace(in.ContactEmail)).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"party already on your roster"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		return
	}
	if s.featureEnabled(r, tenant, "entity360") {
		s.ensureEntityQuiet(r.Context(), tenant, "hp_parties", id, ptype, strings.TrimSpace(in.PartyName), "", "", "", "")
	}
	writeJSON(w, http.StatusCreated, map[string]any{"party_id": id, "onboarding_status": "PROFILE_INCOMPLETE"})
}

// hpUpdatePartyProfile: NPI (required for networks/facilities), TIN, address.
func (s *server) hpUpdatePartyProfile(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	partyID := chi.URLParam(r, "partyId")
	var in struct {
		NPI     string `json:"npi"`
		TIN     string `json:"tin"`
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"npi, tin, address required"}`, http.StatusBadRequest)
		return
	}
	in.NPI, in.TIN, in.Address = strings.TrimSpace(in.NPI), strings.TrimSpace(in.TIN), strings.TrimSpace(in.Address)
	var ptype string
	if err := s.db.QueryRow(r.Context(), `
		SELECT party_type FROM public.hp_parties WHERE tenant=$1 AND plan_id=$2 AND id=$3`,
		tenant, org.ID, partyID).Scan(&ptype); err != nil {
		http.Error(w, `{"error":"party not found"}`, http.StatusNotFound)
		return
	}
	// Employer groups have no NPI; networks and facilities must have one.
	if ptype != "EMPLOYER_GROUP" && !npiValid(in.NPI) {
		http.Error(w, `{"error":"NPI failed the 10-digit Luhn check (required for DELEGATED_NETWORK/FACILITY)"}`, http.StatusBadRequest)
		return
	}
	if in.NPI != "" && !npiValid(in.NPI) {
		http.Error(w, `{"error":"NPI failed the 10-digit Luhn check"}`, http.StatusBadRequest)
		return
	}
	if !tinValid(in.TIN) {
		http.Error(w, `{"error":"TIN must be exactly 9 digits"}`, http.StatusBadRequest)
		return
	}
	if in.Address == "" {
		http.Error(w, `{"error":"address required"}`, http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		UPDATE public.hp_parties SET npi=NULLIF($4,''), tin=$5, address=$6
		WHERE tenant=$1 AND plan_id=$2 AND id=$3`,
		tenant, org.ID, partyID, in.NPI, in.TIN, in.Address); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	st := s.recomputeHpPartyStatus(r, tenant, partyID)
	writeJSON(w, http.StatusOK, map[string]any{"party_id": partyID, "onboarding_status": st})
}

// recomputeHpPartyStatus derives PROFILE_INCOMPLETE -> PAYMENT_PENDING ->
// DOCS_PENDING -> READY. Employer groups skip the NPI requirement.
func (s *server) recomputeHpPartyStatus(r *http.Request, tenant, partyID string) string {
	status := "PROFILE_INCOMPLETE"
	var npi, tin, addr *string
	var ptype string
	if err := s.db.QueryRow(r.Context(), `
		SELECT npi, tin, address, party_type FROM public.hp_parties WHERE tenant=$1 AND id=$2`,
		tenant, partyID).Scan(&npi, &tin, &addr, &ptype); err != nil {
		return status
	}
	profileOK := tin != nil && *tin != "" && addr != nil && *addr != "" &&
		(ptype == "EMPLOYER_GROUP" || (npi != nil && *npi != ""))
	if !profileOK {
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.hp_parties SET onboarding_status=$3 WHERE tenant=$1 AND id=$2`,
			tenant, partyID, status)
		return status
	}
	status = "PAYMENT_PENDING"
	var payments int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.hp_payment_methods
		WHERE tenant=$1 AND party_id=$2 AND verified AND status='ACTIVE'`,
		tenant, partyID).Scan(&payments)
	if payments == 0 {
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.hp_parties SET onboarding_status=$3 WHERE tenant=$1 AND id=$2`,
			tenant, partyID, status)
		return status
	}
	status = "DOCS_PENDING"
	var ready int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.hp_packages
		WHERE tenant=$1 AND party_id=$2 AND status='READY'`,
		tenant, partyID).Scan(&ready)
	if ready > 0 {
		status = "READY"
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.hp_parties SET onboarding_status=$3 WHERE tenant=$1 AND id=$2`,
		tenant, partyID, status)
	return status
}

// hpImportPartiesCSV: bulk roster. Header:
// party_name,party_type,contact_email,npi,tin,address
func (s *server) hpImportPartiesCSV(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, `{"error":"csv too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	rows, err := csv.NewReader(io.LimitReader(f, 8<<20)).ReadAll()
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"unparseable CSV"}`, http.StatusBadRequest)
		return
	}
	start := 0
	if len(rows[0]) > 0 && strings.EqualFold(strings.TrimSpace(rows[0][0]), "party_name") {
		start = 1
	}
	type rowResult struct {
		Row       int    `json:"row"`
		PartyName string `json:"party_name"`
		OK        bool   `json:"ok"`
		PartyID   string `json:"party_id,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	report := make([]rowResult, 0, len(rows)-start)
	imported := 0
	for i := start; i < len(rows); i++ {
		rec := rows[i]
		res := rowResult{Row: i + 1}
		if len(rec) < 1 || strings.TrimSpace(rec[0]) == "" {
			res.Error = "empty row"
			report = append(report, res)
			continue
		}
		cell := func(j int) string {
			if j < len(rec) {
				return strings.TrimSpace(rec[j])
			}
			return ""
		}
		name, ptype, email, npi, tin, addr := cell(0), strings.ToUpper(cell(1)), cell(2), cell(3), cell(4), cell(5)
		res.PartyName = name
		switch {
		case !hpPartyTypes[ptype]:
			res.Error = "party_type must be EMPLOYER_GROUP|DELEGATED_NETWORK|FACILITY"
		case npi != "" && !npiValid(npi):
			res.Error = "NPI failed Luhn check"
		case ptype != "EMPLOYER_GROUP" && npi == "":
			res.Error = "NPI required for DELEGATED_NETWORK/FACILITY"
		case tin != "" && !tinValid(tin):
			res.Error = "TIN must be 9 digits"
		default:
			var id string
			err := s.db.QueryRow(r.Context(), `
				INSERT INTO public.hp_parties (tenant, plan_id, party_name, party_type, contact_email, npi, tin, address)
				VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''))
				ON CONFLICT (tenant, plan_id, party_name) DO UPDATE SET
				  contact_email=excluded.contact_email, npi=coalesce(excluded.npi, public.hp_parties.npi),
				  tin=coalesce(excluded.tin, public.hp_parties.tin), address=coalesce(excluded.address, public.hp_parties.address)
				RETURNING id`, tenant, org.ID, name, ptype, email, npi, tin, addr).Scan(&id)
			if err != nil {
				res.Error = "insert failed"
			} else {
				res.OK, res.PartyID = true, id
				imported++
				s.recomputeHpPartyStatus(r, tenant, id)
				if s.featureEnabled(r, tenant, "entity360") {
					s.ensureEntityQuiet(r.Context(), tenant, "hp_parties", id, ptype, name, npi, tin, "", "")
				}
			}
		}
		report = append(report, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": imported, "total": len(report), "rows": report})
}

// --- payment rails -----------------------------------------------------------

// hpAddPayment: CARD (processor token) or ACH (ABA checksum + auth capture)
// per party.
func (s *server) hpAddPayment(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	partyID := chi.URLParam(r, "partyId")
	var in struct {
		Method       string `json:"method"`
		Label        string `json:"label"`
		Brand        string `json:"brand"`
		Last4        string `json:"last4"`
		Routing      string `json:"routing"`
		ProcessorRef string `json:"processor_ref"`
		AuthReceived bool   `json:"auth_received"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"method required"}`, http.StatusBadRequest)
		return
	}
	in.Method = strings.ToUpper(strings.TrimSpace(in.Method))
	if len(in.Last4) != 4 {
		http.Error(w, `{"error":"last4 (4 digits) required"}`, http.StatusBadRequest)
		return
	}
	verified := false
	switch in.Method {
	case "CARD":
		if in.ProcessorRef == "" {
			http.Error(w, `{"error":"processor_ref (card token) required — raw card numbers are never accepted"}`, http.StatusBadRequest)
			return
		}
		verified = true
	case "ACH":
		if !routingValid(in.Routing) {
			http.Error(w, `{"error":"routing number failed the ABA checksum"}`, http.StatusBadRequest)
			return
		}
		if !in.AuthReceived {
			http.Error(w, `{"error":"ACH debit authorization must be captured (auth_received=true)"}`, http.StatusBadRequest)
			return
		}
		verified = true
	default:
		http.Error(w, `{"error":"method must be CARD or ACH"}`, http.StatusBadRequest)
		return
	}
	var owner int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.hp_parties WHERE tenant=$1 AND plan_id=$2 AND id=$3`,
		tenant, org.ID, partyID).Scan(&owner)
	if owner == 0 {
		http.Error(w, `{"error":"party not found"}`, http.StatusNotFound)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.hp_payment_methods
		  (tenant, plan_id, party_id, method, label, brand, last4, routing, processor_ref, auth_received, verified)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),$10,$11)
		RETURNING id`,
		tenant, org.ID, partyID, in.Method, in.Label, strings.ToUpper(in.Brand),
		in.Last4, in.Routing, in.ProcessorRef, in.AuthReceived, verified).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		return
	}
	st := s.recomputeHpPartyStatus(r, tenant, partyID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"payment_method_id": id, "verified": verified, "onboarding_status": st})
}

// hpListPayments: masked rails for one party.
func (s *server) hpListPayments(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT id, method, coalesce(label,''), coalesce(brand,''), last4,
		       coalesce(routing,''), auth_received, verified, status
		FROM public.hp_payment_methods
		WHERE tenant=$1 AND plan_id=$2 AND party_id=$3 ORDER BY created_at`,
		tenant, org.ID, chi.URLParam(r, "partyId"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, m, label, brand, last4, routing, status string
		var auth, verified bool
		if rows.Scan(&id, &m, &label, &brand, &last4, &routing, &auth, &verified, &status) == nil {
			out = append(out, map[string]any{
				"id": id, "method": m, "label": label, "brand": brand, "last4": last4,
				"routing": routing, "auth_received": auth, "verified": verified, "status": status})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"payment_methods": out})
}

// --- document packages -------------------------------------------------------

func (s *server) hpPackageOwned(r *http.Request, tenant, planID, pkgID string) (partyID, mode, status string, err error) {
	err = s.db.QueryRow(r.Context(), `
		SELECT party_id, mode, status FROM public.hp_packages
		WHERE tenant=$1 AND plan_id=$2 AND id=$3`, tenant, planID, pkgID).
		Scan(&partyID, &mode, &status)
	if err != nil {
		err = fmt.Errorf("package not found")
	}
	return
}

// hpCreatePackage opens a DISCRETE or MERGED package for a party.
func (s *server) hpCreatePackage(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	partyID := chi.URLParam(r, "partyId")
	var in struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	mode := strings.ToUpper(in.Mode)
	if mode != "MERGED" {
		mode = "DISCRETE"
	}
	var owner int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.hp_parties WHERE tenant=$1 AND plan_id=$2 AND id=$3`,
		tenant, org.ID, partyID).Scan(&owner)
	if owner == 0 {
		http.Error(w, `{"error":"party not found"}`, http.StatusNotFound)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.hp_packages (tenant, plan_id, party_id, mode) VALUES ($1,$2,$3,$4) RETURNING id`,
		tenant, org.ID, partyID, mode).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"package_id": id, "mode": mode, "status": "ASSEMBLING", "slots": tpaSlots})
}

// hpStorePackageDoc seals + stores one document and publishes the validation
// event with actor=healthplan (doc-intel's QPA floor applies to payer-side
// OFFER_SUBSTANTIATION).
func (s *server) hpStorePackageDoc(r *http.Request, tenant, planID, pkgID, slot, filename string, raw []byte) (string, error) {
	if n, err := s.rds.incrExpire("idre:rl:hpdoc:"+tenant+":"+planID, 3600); err == nil && n > 400 {
		return "", fmt.Errorf("upload rate limit exceeded (400/hour)")
	}
	if err := contentPolicy(filename, raw); err != nil {
		return "", err
	}
	if sig, ok := s.scanOrRefuse(discardWriter{}, bytes.NewReader(raw), "file"); !ok {
		return "", fmt.Errorf("malware scan rejected: %s", sig)
	}
	docID := newUUID()
	objectKey := fmt.Sprintf("%s/hp-packages/%s/%s.enc", tenant, pkgID, docID)
	ct, err := s.vaultSealDoc(r, tenant, objectKey, raw)
	if err != nil {
		return "", fmt.Errorf("seal failed")
	}
	if _, err = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
		bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
			UserMetadata: map[string]string{"tenant": tenant, "package": pkgID},
		}); err != nil {
		return "", fmt.Errorf("store failed")
	}
	var rowID string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.hp_package_docs (tenant, package_id, slot, object_key, filename, status)
		VALUES ($1,$2,$3,$4,$5,'PENDING')
		ON CONFLICT (package_id, slot) DO UPDATE SET
		  object_key=excluded.object_key, filename=excluded.filename,
		  status='PENDING', reasons='[]'::jsonb, analyzed_at=NULL
		RETURNING id`, tenant, pkgID, slot, objectKey, filename).Scan(&rowID)
	if err != nil {
		return "", fmt.Errorf("db")
	}
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "tpa_doc.uploaded", "tenant": tenant, "actor": "healthplan",
		"package_id": pkgID, "package_doc_id": rowID, "slot": slot,
		"object_key": objectKey, "filename": filename, "merged": false,
		"at": time.Now().UTC(),
	})
	return rowID, nil
}

// hpAttachDoc: one DISCRETE slot upload.
func (s *server) hpAttachDoc(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	_, mode, status, err := s.hpPackageOwned(r, tenant, org.ID, pkgID)
	if err != nil {
		http.Error(w, `{"error":"package not found"}`, http.StatusNotFound)
		return
	}
	if mode != "DISCRETE" || (status != "ASSEMBLING" && status != "REJECTED") {
		http.Error(w, `{"error":"package does not accept discrete slot uploads in its current state"}`, http.StatusConflict)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDocBytes)
	if err := r.ParseMultipartForm(maxDocBytes); err != nil {
		http.Error(w, `{"error":"file too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	slot := strings.ToUpper(strings.TrimSpace(r.FormValue("slot")))
	if !tpaSlotValid(slot) {
		http.Error(w, `{"error":"slot must be one of: `+strings.Join(tpaSlots, ", ")+`"}`, http.StatusBadRequest)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	rowID, err := s.hpStorePackageDoc(r, tenant, org.ID, pkgID, slot, hdr.Filename, raw)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"package_doc_id": rowID, "slot": slot, "status": "PENDING",
		"note": "doc-intel validates the document; poll the package for the verdict"})
}

// hpUploadMerged: one PDF containing all six documents; doc-intel splits it.
func (s *server) hpUploadMerged(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	_, mode, status, err := s.hpPackageOwned(r, tenant, org.ID, pkgID)
	if err != nil {
		http.Error(w, `{"error":"package not found"}`, http.StatusNotFound)
		return
	}
	if mode != "MERGED" || (status != "ASSEMBLING" && status != "REJECTED") {
		http.Error(w, `{"error":"package does not accept a merged upload in its current state"}`, http.StatusConflict)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDocBytes)
	if err := r.ParseMultipartForm(maxDocBytes); err != nil {
		http.Error(w, `{"error":"file too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	if !bytes.HasPrefix(raw, []byte("%PDF")) {
		http.Error(w, `{"error":"merged package must be a single PDF"}`, http.StatusUnsupportedMediaType)
		return
	}
	if err := contentPolicy(hdr.Filename, raw); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
		return
	}
	if sig, ok := s.scanOrRefuse(discardWriter{}, bytes.NewReader(raw), "file"); !ok {
		http.Error(w, fmt.Sprintf(`{"error":"malware scan rejected: %s"}`, sig), http.StatusUnsupportedMediaType)
		return
	}
	objectKey := fmt.Sprintf("%s/hp-packages/%s/merged-%s.enc", tenant, pkgID, newUUID())
	ct, err := s.vaultSealDoc(r, tenant, objectKey, raw)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	if _, err = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
		bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
			UserMetadata: map[string]string{"tenant": tenant, "package": pkgID},
		}); err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.hp_packages SET source_object_key=$3, status='VALIDATING', updated_at=now()
		WHERE tenant=$1 AND id=$2`, tenant, pkgID, objectKey)
	_, _ = s.db.Exec(r.Context(), `DELETE FROM public.hp_package_docs WHERE tenant=$1 AND package_id=$2`, tenant, pkgID)
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "tpa_doc.uploaded", "tenant": tenant, "actor": "healthplan",
		"package_id": pkgID, "object_key": objectKey, "filename": hdr.Filename,
		"merged": true, "at": time.Now().UTC(),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"package_id": pkgID, "status": "VALIDATING",
		"note": "doc-intel is splitting the PDF into the six required slots; poll the package for per-slot verdicts"})
}

// hpPackageStatus: per-slot verdict grid + package readiness.
func (s *server) hpPackageStatus(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	partyID, mode, status, err := s.hpPackageOwned(r, tenant, org.ID, pkgID)
	if err != nil {
		http.Error(w, `{"error":"package not found"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT id, slot, coalesce(filename,''), status, reasons::text,
		       coalesce(page_start::text,''), coalesce(page_end::text,''), coalesce(analyzed_at::text,'')
		FROM public.hp_package_docs WHERE tenant=$1 AND package_id=$2 ORDER BY slot`, tenant, pkgID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	bySlot := map[string]map[string]any{}
	for rows.Next() {
		var id, slot, fn, st, reasons, ps, pe, at string
		if rows.Scan(&id, &slot, &fn, &st, &reasons, &ps, &pe, &at) == nil {
			bySlot[slot] = map[string]any{
				"id": id, "slot": slot, "filename": fn, "status": st,
				"reasons": json.RawMessage(reasons), "page_start": ps, "page_end": pe, "analyzed_at": at}
		}
	}
	grid := make([]map[string]any, 0, len(tpaSlots))
	for _, slot := range tpaSlots {
		if d, ok := bySlot[slot]; ok {
			grid = append(grid, d)
		} else {
			grid = append(grid, map[string]any{"slot": slot, "status": "MISSING"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"package_id": pkgID, "party_id": partyID, "mode": mode,
		"status": status, "slots": grid})
}

// hpOnboardingBoard: the plan's whole party roster with the three gates.
func (s *server) hpOnboardingBoard(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT c.id, c.party_name, c.party_type, c.onboarding_status, c.status,
		       coalesce(c.npi,''), coalesce(c.tin,''),
		       (SELECT count(*) FROM public.hp_payment_methods m
		         WHERE m.tenant=c.tenant AND m.party_id=c.id AND m.verified AND m.status='ACTIVE'),
		       (SELECT count(*) FROM public.hp_packages pk
		         WHERE pk.tenant=c.tenant AND pk.party_id=c.id AND pk.status='READY'),
		       (SELECT json_agg(json_build_object('id',pk.id,'mode',pk.mode,'status',pk.status))
		        FROM public.hp_packages pk WHERE pk.tenant=c.tenant AND pk.party_id=c.id AND pk.status<>'FILED')
		FROM public.hp_parties c WHERE c.tenant=$1 AND c.plan_id=$2 ORDER BY c.party_name`,
		tenant, org.ID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	parties := []map[string]any{}
	for rows.Next() {
		var id, name, ptype, ost, cst, npi, tin string
		var npay, nready int
		var pkgs *string
		if rows.Scan(&id, &name, &ptype, &ost, &cst, &npi, &tin, &npay, &nready, &pkgs) != nil {
			continue
		}
		var packages json.RawMessage
		if pkgs != nil {
			packages = json.RawMessage(*pkgs)
		} else {
			packages = json.RawMessage("[]")
		}
		parties = append(parties, map[string]any{
			"party_id": id, "party_name": name, "party_type": ptype,
			"onboarding_status": ost, "status": cst,
			"profile_complete": tin != "" && (ptype == "EMPLOYER_GROUP" || npi != ""),
			"verified_payments": npay, "ready_packages": nready,
			"open_packages": packages,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan_id": org.ID, "parties": parties, "required_slots": tpaSlots})
}

// --- bulk ZIP packages ---------------------------------------------------------

// hpUploadBulkZip: one ZIP, many parties. Top-level folder = party_name;
// inside, six slot-named files or one merged.pdf. Per-party report.
func (s *server) hpUploadBulkZip(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<20)
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		http.Error(w, `{"error":"zip too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 256<<20))
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		http.Error(w, `{"error":"not a valid ZIP archive"}`, http.StatusBadRequest)
		return
	}
	type entry struct {
		name string
		zf   *zip.File
	}
	byParty := map[string][]entry{}
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		parts := strings.SplitN(strings.Trim(zf.Name, "/"), "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		byParty[parts[0]] = append(byParty[parts[0]], entry{parts[1], zf})
	}
	type partyReport struct {
		Party     string   `json:"party"`
		PackageID string   `json:"package_id,omitempty"`
		Mode      string   `json:"mode,omitempty"`
		Attached  []string `json:"attached,omitempty"`
		Errors    []string `json:"errors,omitempty"`
	}
	reports := []partyReport{}
	for partyName, entries := range byParty {
		rep := partyReport{Party: partyName}
		var partyID string
		err := s.db.QueryRow(r.Context(), `
			SELECT id FROM public.hp_parties WHERE tenant=$1 AND plan_id=$2 AND lower(party_name)=lower($3)`,
			tenant, org.ID, partyName).Scan(&partyID)
		if err != nil {
			rep.Errors = append(rep.Errors, "no party with this name on your roster — import the roster first")
			reports = append(reports, rep)
			continue
		}
		var merged *entry
		discrete := []entry{}
		for _, e := range entries {
			if strings.EqualFold(e.name, "merged.pdf") {
				m := e
				merged = &m
			} else {
				discrete = append(discrete, e)
			}
		}
		mode := "DISCRETE"
		if merged != nil {
			mode = "MERGED"
		}
		var pkgID string
		if err := s.db.QueryRow(r.Context(), `
			INSERT INTO public.hp_packages (tenant, plan_id, party_id, mode) VALUES ($1,$2,$3,$4) RETURNING id`,
			tenant, org.ID, partyID, mode).Scan(&pkgID); err != nil {
			rep.Errors = append(rep.Errors, "package create failed")
			reports = append(reports, rep)
			continue
		}
		rep.PackageID, rep.Mode = pkgID, mode
		if merged != nil {
			rc, err := merged.zf.Open()
			if err != nil {
				rep.Errors = append(rep.Errors, "merged.pdf unreadable")
			} else {
				data, _ := io.ReadAll(io.LimitReader(rc, maxDocBytes))
				rc.Close()
				if !bytes.HasPrefix(data, []byte("%PDF")) {
					rep.Errors = append(rep.Errors, "merged.pdf is not a PDF")
				} else if err := contentPolicy("merged.pdf", data); err != nil {
					rep.Errors = append(rep.Errors, err.Error())
				} else {
					objectKey := fmt.Sprintf("%s/hp-packages/%s/merged-%s.enc", tenant, pkgID, newUUID())
					ct, serr := s.vaultSealDoc(r, tenant, objectKey, data)
					if serr == nil {
						_, serr = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
							bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
								UserMetadata: map[string]string{"tenant": tenant, "package": pkgID}})
					}
					if serr != nil {
						rep.Errors = append(rep.Errors, "merged.pdf store failed")
					} else {
						_, _ = s.db.Exec(r.Context(), `
							UPDATE public.hp_packages SET source_object_key=$3, status='VALIDATING', updated_at=now()
							WHERE tenant=$1 AND id=$2`, tenant, pkgID, objectKey)
						s.publish(r.Context(), tenant, "documents", map[string]any{
							"type": "tpa_doc.uploaded", "tenant": tenant, "actor": "healthplan",
							"package_id": pkgID, "object_key": objectKey, "filename": "merged.pdf",
							"merged": true, "at": time.Now().UTC(),
						})
						rep.Attached = append(rep.Attached, "merged.pdf")
					}
				}
			}
		}
		for _, e := range discrete {
			slot := tpaSlotFromFilename(e.name)
			if slot == "" {
				rep.Errors = append(rep.Errors, e.name+": filename does not map to a required slot")
				continue
			}
			rc, err := e.zf.Open()
			if err != nil {
				rep.Errors = append(rep.Errors, e.name+": unreadable")
				continue
			}
			data, _ := io.ReadAll(io.LimitReader(rc, maxDocBytes))
			rc.Close()
			if _, err := s.hpStorePackageDoc(r, tenant, org.ID, pkgID, slot, e.name, data); err != nil {
				rep.Errors = append(rep.Errors, e.name+": "+err.Error())
				continue
			}
			rep.Attached = append(rep.Attached, slot)
		}
		reports = append(reports, rep)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"reports": reports})
}

// --- filing -------------------------------------------------------------------

// hpIntake: open an intake on behalf of a rostered party. Gated on a READY
// package + verified payment once the party has started onboarding. Uses the
// standard NG intake pipeline — staff advance it like any other intake.
func (s *server) hpIntake(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "HEALTH_PLAN", "CASE_MANAGER", "BULK_SUBMITTER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		PartyID   string         `json:"party_id"`
		PackageID string         `json:"package_id"`
		Notes     string         `json:"notes"`
		Fields    map[string]any `json:"fields"` // manifest intake_fields
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.PartyID == "" {
		http.Error(w, `{"error":"party_id required"}`, http.StatusBadRequest)
		return
	}
	var partyName, partyType, partyStatus string
	err = s.db.QueryRow(r.Context(), `
		SELECT party_name, party_type, status FROM public.hp_parties
		WHERE tenant=$1 AND plan_id=$2 AND id=$3`, tenant, org.ID, in.PartyID).
		Scan(&partyName, &partyType, &partyStatus)
	if err != nil {
		http.Error(w, `{"error":"party not found"}`, http.StatusNotFound)
		return
	}
	if partyStatus != "ACTIVE" {
		http.Error(w, `{"error":"party relationship is SUSPENDED"}`, http.StatusConflict)
		return
	}
	// Onboarding gate: READY package + verified payment for onboarded parties.
	var onboarded int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.hp_parties
		WHERE tenant=$1 AND id=$2 AND (tin IS NOT NULL OR
		  EXISTS (SELECT 1 FROM public.hp_packages p WHERE p.tenant=$1 AND p.party_id=$2))`,
		tenant, in.PartyID).Scan(&onboarded)
	if onboarded > 0 {
		if in.PackageID == "" {
			http.Error(w, `{"error":"this party is onboarded — filings must reference a READY package (package_id)"}`, http.StatusConflict)
			return
		}
		var pkgStatus string
		err = s.db.QueryRow(r.Context(), `
			SELECT status FROM public.hp_packages
			WHERE tenant=$1 AND plan_id=$2 AND party_id=$3 AND id=$4`,
			tenant, org.ID, in.PartyID, in.PackageID).Scan(&pkgStatus)
		if err != nil || pkgStatus != "READY" {
			http.Error(w, `{"error":"package not found or not READY (all six documents must be ACCEPTED)"}`, http.StatusConflict)
			return
		}
		var verified int
		_ = s.db.QueryRow(r.Context(), `
			SELECT count(*) FROM public.hp_payment_methods
			WHERE tenant=$1 AND party_id=$2 AND verified AND status='ACTIVE'`,
			tenant, in.PartyID).Scan(&verified)
		if verified == 0 {
			http.Error(w, `{"error":"no verified payment method for this party"}`, http.StatusConflict)
			return
		}
	}
	// Same gate as createIntake: declared intake fields only, required
	// enforced, from the tenant's manifest.
	var manifest *ProgramManifest
	if m, merr := s.manifestFor(r.Context(), tenant); merr == nil {
		manifest = m
	}
	details := map[string]any{"submitting_party": partyName, "filed_by_health_plan": org.Name}
	if manifest != nil {
		for _, f := range manifest.IntakeFields {
			v, present := in.Fields[f.Name]
			if f.Required && (!present || fmt.Sprint(v) == "") {
				http.Error(w, fmt.Sprintf(`{"error":"intake field %q required"}`, f.Name), http.StatusBadRequest)
				return
			}
			if present {
				details[f.Name] = v
			}
		}
	}
	detailsJSON, _ := json.Marshal(details)
	var intakeID string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.intake_requests (tenant, email, contact_name, org, notes, outreach_at, filing_party_type, details)
		VALUES ($1,$2,$3,$4,$5, now(), $6, $7) RETURNING id`,
		tenant, org.ContactEmail, org.ContactName, org.Name+" on behalf of "+partyName, in.Notes, "HEALTH_PLAN", detailsJSON).Scan(&intakeID)
	if intakeID == "" || err != nil {
		http.Error(w, `{"error":"intake insert failed"}`, http.StatusInternalServerError)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.case_origin
		  (tenant, intake_id, health_plan_id, hp_party_id, initiating_party_name, initiating_party_type, filed_by_email)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		tenant, intakeID, org.ID, in.PartyID, partyName, partyType, org.ContactEmail)
	if in.PackageID != "" {
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.hp_packages SET status='FILED', updated_at=now()
			WHERE tenant=$1 AND id=$2`, tenant, in.PackageID)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"intake_id": intakeID, "status": "INSTRUCTED",
		"party": partyName, "filed_by": org.Name,
		"note": "intake opened with the health plan as contact — payment and document requests go to you on behalf of " + partyName,
	})
}

// --- oversight ------------------------------------------------------------------

func (s *server) adminListHealthPlans(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), `
		SELECT h.id, h.name, coalesce(h.naic,''), coalesce(h.line_of_business,''),
		       h.contact_email, h.status,
		       (SELECT count(*) FROM public.hp_parties c WHERE c.tenant=h.tenant AND c.plan_id=h.id)
		FROM public.health_plans h WHERE h.tenant=$1 ORDER BY h.name`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, naic, lob, email, status string
		var parties int
		if rows.Scan(&id, &name, &naic, &lob, &email, &status, &parties) == nil {
			out = append(out, map[string]any{
				"id": id, "name": name, "naic": naic, "line_of_business": lob,
				"contact_email": email, "status": status, "parties": parties})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"health_plans": out})
}

func (s *server) adminSetHealthPlanStatus(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(strings.ToUpper(in.Status) != "ACTIVE" && strings.ToUpper(in.Status) != "SUSPENDED") {
		http.Error(w, `{"error":"status must be ACTIVE or SUSPENDED"}`, http.StatusBadRequest)
		return
	}
	tag, err := s.db.Exec(r.Context(), `
		UPDATE public.health_plans SET status=$3, updated_at=now() WHERE tenant=$1 AND id=$2`,
		tenant, chi.URLParam(r, "planId"), strings.ToUpper(in.Status))
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, `{"error":"health plan not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
