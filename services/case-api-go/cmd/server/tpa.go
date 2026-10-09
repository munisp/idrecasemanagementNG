package main

// tpa.go — third-party administrators (TPAs).
//
// A TPA is an organization that files and tracks disputes ON BEHALF OF one
// or more initiating parties (manifest PartyA/PartyB — providers, health
// plans, policyholders… — a client list the TPA manages itself). Onboarding
// is self-serve: the TPA registers publicly and receives a one-time claim
// code; its first portal user claims the org with that code (no staff
// approval gate — states retain SUSPEND as the after-the-fact control).
//
// NG differences from the legacy service:
//   - Intake is staff-driven: tpaIntake opens an intake_request through the
//     exact NG createIntake semantics (manifest party codes, declared intake
//     fields only) with the TPA as the contact — payment/document outreach
//     goes to the TPA, which is the "pay/act on behalf" requirement.
//   - case_origin is keyed by intake_id and gains case_id when staff convert
//     the intake (hook in advanceIntake), so attribution survives conversion.
//   - No audit_log: tpas/tpa_clients/case_origin rows carry their own
//     timestamps — the record is the audit trail.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type tpaOrg struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContactName  string `json:"contact_name"`
	ContactEmail string `json:"contact_email"`
	Status       string `json:"status"`
}

// tpaForPrincipal resolves the caller's TPA org (linked via tpa_users).
func (s *server) tpaForPrincipal(r *http.Request, tenant string) (*tpaOrg, error) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var o tpaOrg
	err := s.db.QueryRow(r.Context(), `
		SELECT t.id, t.name, t.contact_name, t.contact_email, t.status
		FROM public.tpa_users u JOIN public.tpas t ON t.id = u.tpa_id
		WHERE u.tenant=$1 AND u.user_sub=$2`, tenant, p.Subject).
		Scan(&o.ID, &o.Name, &o.ContactName, &o.ContactEmail, &o.Status)
	if err != nil {
		return nil, fmt.Errorf("no TPA organization linked to your account — claim one with your claim code")
	}
	if o.Status != "ACTIVE" {
		return nil, fmt.Errorf("TPA organization is %s", o.Status)
	}
	return &o, nil
}

// registerTPA: PUBLIC self-serve onboarding. Returns the org and a one-time
// claim code (shown once — the first portal user links the org with it).
func (s *server) registerTPA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tenant       string `json:"tenant"`
		Name         string `json:"name"`
		ContactName  string `json:"contact_name"`
		ContactEmail string `json:"contact_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		in.Tenant == "" || in.Name == "" || in.ContactEmail == "" || !strings.Contains(in.ContactEmail, "@") {
		http.Error(w, `{"error":"tenant, name, and a valid contact_email are required"}`, http.StatusBadRequest)
		return
	}
	code := "TPA-" + strings.ToUpper(hex.EncodeToString(func() []byte { b := make([]byte, 3); _, _ = rand.Read(b); return b }()))
	var id string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.tpas (tenant, name, contact_name, contact_email, claim_code, status)
		VALUES (lower($1), $2, $3, $4, $5, 'ACTIVE') RETURNING id`,
		in.Tenant, in.Name, in.ContactName, in.ContactEmail, code).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"registration failed — an organization with this email may already exist for this program"}`, http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tpa_id": id, "status": "ACTIVE", "claim_code": code,
		"next": "Sign in to the portal with a TPA account and enter this claim code (TPA → Claim) to link your organization. Then add your initiating parties and file on their behalf.",
	})
}

// claimTPA links the authenticated TPA user to their org via the claim code.
func (s *server) claimTPA(w http.ResponseWriter, r *http.Request) {
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
		SELECT id FROM public.tpas WHERE tenant=$1 AND claim_code=$2 AND status='ACTIVE'`,
		tenant, strings.ToUpper(strings.TrimSpace(in.Code))).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"invalid claim code"}`, http.StatusNotFound)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.tpa_users (tenant, user_sub, tpa_id) VALUES ($1,$2,$3)
		ON CONFLICT (tenant, user_sub) DO UPDATE SET tpa_id=$3`, tenant, p.Subject, id)
	// Burn the code: it has done its one job.
	_, _ = s.db.Exec(r.Context(), `UPDATE public.tpas SET claim_code=NULL, updated_at=now() WHERE id=$1`, id)
	writeJSON(w, http.StatusOK, map[string]any{"tpa_id": id, "status": "linked"})
}

// tpaMe: the caller's org.
func (s *server) tpaMe(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tpa": org})
}

// tpaAddClient registers an initiating party the TPA acts for. Party-type
// codes come from the tenant manifest (PartyACode/PartyBCode), not
// hard-coded healthcare labels.
func (s *server) tpaAddClient(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		PartyName    string `json:"party_name"`
		PartyType    string `json:"party_type"` // manifest PartyACode | PartyBCode
		ContactEmail string `json:"contact_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.PartyName == "" {
		http.Error(w, `{"error":"party_name required"}`, http.StatusBadRequest)
		return
	}
	codeA, codeB := s.partyCodes(r, tenant)
	pt := strings.ToUpper(strings.TrimSpace(in.PartyType))
	if pt == "" {
		pt = codeA
	}
	if pt != codeA && pt != codeB {
		http.Error(w, fmt.Sprintf(`{"error":"party_type must be %s or %s"}`, codeA, codeB), http.StatusBadRequest)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.tpa_clients (tenant, tpa_id, party_name, party_type, contact_email)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`, tenant, org.ID, in.PartyName, pt, in.ContactEmail).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"client already exists for this TPA"}`, http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"client_id": id, "party_type": pt, "status": "ACTIVE"})
}

func (s *server) tpaListClients(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, _ := s.queryRows(r, `
		SELECT c.id, c.party_name, c.party_type, c.contact_email, c.status, c.created_at,
		       (SELECT count(*) FROM public.case_origin o WHERE o.client_id=c.id) AS intake_count
		FROM public.tpa_clients c WHERE c.tenant=$1 AND c.tpa_id=$2 ORDER BY c.party_name`, tenant, org.ID)
	writeJSON(w, http.StatusOK, map[string]any{"clients": rows})
}

// tpaSetClientStatus suspends/reactivates a client relationship.
func (s *server) tpaSetClientStatus(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		Status string `json:"status"` // ACTIVE | SUSPENDED
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Status != "ACTIVE" && in.Status != "SUSPENDED" {
		http.Error(w, `{"error":"status must be ACTIVE|SUSPENDED"}`, http.StatusBadRequest)
		return
	}
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.tpa_clients SET status=$3 WHERE tenant=$1 AND tpa_id=$2 AND id=$4`,
		tenant, org.ID, in.Status, r.PathValue("clientId"))
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": in.Status})
}

// tpaIntake files a dispute on behalf of an initiating party through the
// standard NG intake pipeline (staff advance it like any other intake). The
// TPA is the operational contact — outreach, payment instructions, and
// document requests go to them; case_origin ties the filing to both the TPA
// and the client it belongs to.
func (s *server) tpaIntake(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "BULK_SUBMITTER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		ClientID string         `json:"client_id"`
		Notes    string         `json:"notes"`
		Fields   map[string]any `json:"fields"` // manifest intake_fields
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ClientID == "" {
		http.Error(w, `{"error":"client_id required"}`, http.StatusBadRequest)
		return
	}
	var partyName, partyType, clientStatus string
	err = s.db.QueryRow(r.Context(), `
		SELECT party_name, party_type, status FROM public.tpa_clients
		WHERE tenant=$1 AND tpa_id=$2 AND id=$3`, tenant, org.ID, in.ClientID).
		Scan(&partyName, &partyType, &clientStatus)
	if err != nil {
		http.Error(w, `{"error":"client not found"}`, http.StatusNotFound)
		return
	}
	if clientStatus != "ACTIVE" {
		http.Error(w, `{"error":"client relationship is SUSPENDED"}`, http.StatusConflict)
		return
	}
	// Same gate as createIntake: declared intake fields only, required
	// enforced, from the tenant's manifest.
	var manifest *ProgramManifest
	if m, merr := s.manifestFor(r.Context(), tenant); merr == nil {
		manifest = m
	}
	details := map[string]any{"initiating_party": partyName, "filed_by_tpa": org.Name}
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
		tenant, org.ContactEmail, org.ContactName, org.Name+" on behalf of "+partyName, in.Notes, partyType, detailsJSON).Scan(&intakeID)
	if intakeID == "" || err != nil {
		http.Error(w, `{"error":"intake insert failed"}`, http.StatusInternalServerError)
		return
	}
	// Origin record: the dispute belongs to the initiating party; the TPA
	// filed it. case_id attaches when staff convert the intake.
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.case_origin
		  (tenant, intake_id, tpa_id, client_id, initiating_party_name, initiating_party_type, filed_by_email)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		tenant, intakeID, org.ID, in.ClientID, partyName, partyType, org.ContactEmail)
	writeJSON(w, http.StatusCreated, map[string]any{
		"intake_id": intakeID, "status": "INSTRUCTED", "filing_party_type": partyType,
		"initiating_party": partyName, "filed_by": org.Name,
		"note": "intake opened with the TPA as contact — payment and document requests go to you on behalf of " + partyName,
	})
}

// tpaDashboard: every initiating party with its filings, statuses, and money.
func (s *server) tpaDashboard(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	orgID := ""
	if hasAnyRole(p, "TPA") && !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		org, err := s.tpaForPrincipal(r, tenant)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
			return
		}
		orgID = org.ID
	} else if q := r.URL.Query().Get("tpa_id"); q != "" {
		orgID = q // staff inspecting a specific TPA
	}
	if orgID == "" {
		http.Error(w, `{"error":"tpa_id required for staff view"}`, http.StatusBadRequest)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT o.intake_id, o.case_id, o.initiating_party_name, o.initiating_party_type, o.filed_by_email,
		       o.created_at::date::text AS filed,
		       ir.status AS intake_status, ir.details,
		       c.case_number, c.status AS case_status, c.disputed_amount_cents,
		       cl.party_name AS client_name, cl.id AS client_id,
		       (SELECT coalesce(sum(py.amount_cents),0) FROM public.payments py
		         WHERE py.tenant=o.tenant AND py.case_id=o.case_id AND py.status='PAID') AS paid_cents
		FROM public.case_origin o
		JOIN public.tpa_clients cl ON cl.id = o.client_id
		JOIN public.intake_requests ir ON ir.id = o.intake_id
		LEFT JOIN tenant_%s.cases c ON c.id = o.case_id
		WHERE o.tenant=$1 AND o.tpa_id=$2
		ORDER BY o.created_at DESC LIMIT 500`, sanitizeTenant(tenant)), tenant, orgID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// Roll up per initiating party.
	type rollup struct {
		ClientID  string `json:"client_id"`
		Party     string `json:"initiating_party"`
		PartyType string `json:"party_type"`
		Filings   int    `json:"filings"`
		Converted int    `json:"converted"`
		Paid      int64  `json:"paid_cents"`
	}
	byClient := map[string]*rollup{}
	order := []string{}
	for _, row := range rows {
		cid := fmt.Sprint(row["client_id"])
		rl, ok := byClient[cid]
		if !ok {
			rl = &rollup{ClientID: cid, Party: fmt.Sprint(row["initiating_party_name"]),
				PartyType: fmt.Sprint(row["initiating_party_type"])}
			byClient[cid] = rl
			order = append(order, cid)
		}
		rl.Filings++
		if row["case_id"] != nil && fmt.Sprint(row["case_id"]) != "" {
			rl.Converted++
		}
		rl.Paid += toInt64(row["paid_cents"])
	}
	rollups := []rollup{}
	for _, cid := range order {
		rollups = append(rollups, *byClient[cid])
	}
	writeJSON(w, http.StatusOK, map[string]any{"filings": rows, "by_client": rollups})
}

// adminListTPAs: staff view of all TPA orgs in the tenant.
func (s *server) adminListTPAs(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, _ := s.queryRows(r, `
		SELECT t.id, t.name, t.contact_name, t.contact_email, t.status, t.created_at,
		       (SELECT count(*) FROM public.tpa_clients c WHERE c.tpa_id=t.id) AS clients,
		       (SELECT count(*) FROM public.case_origin o WHERE o.tpa_id=t.id) AS filings
		FROM public.tpas t WHERE t.tenant=$1 ORDER BY t.created_at DESC`, tenant)
	writeJSON(w, http.StatusOK, map[string]any{"tpas": rows})
}

// adminSetTPAStatus: the state's after-the-fact control (suspend bad actors).
func (s *server) adminSetTPAStatus(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Status string `json:"status"` // ACTIVE | SUSPENDED
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Status != "ACTIVE" && in.Status != "SUSPENDED" {
		http.Error(w, `{"error":"status must be ACTIVE|SUSPENDED"}`, http.StatusBadRequest)
		return
	}
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.tpas SET status=$3, updated_at=now() WHERE tenant=$1 AND id=$2`,
		tenant, r.PathValue("tpaId"), in.Status)
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": in.Status})
}

// partyCodes resolves the tenant's filing-party codes from its manifest
// (healthcare legacy default PROVIDER/HEALTH_PLAN).
func (s *server) partyCodes(r *http.Request, tenant string) (string, string) {
	codeA, codeB := "PROVIDER", "HEALTH_PLAN"
	if m, err := s.manifestFor(r.Context(), tenant); err == nil && m != nil {
		if m.Terminology.PartyACode != "" {
			codeA = m.Terminology.PartyACode
		}
		if m.Terminology.PartyBCode != "" {
			codeB = m.Terminology.PartyBCode
		}
	}
	return codeA, codeB
}
