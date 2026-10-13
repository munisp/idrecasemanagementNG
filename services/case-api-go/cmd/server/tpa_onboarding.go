package main

// tpa_onboarding.go — full onboarding pipeline for TPA initiating parties
// (feature flag: tpa_onboarding).
//
// Per client (initiating party) the TPA completes three gates before filing:
//   1. Profile  — NPI (Luhn/80840), TIN, address.
//   2. Payment  — CARD (processor token ref) or ACH (ABA checksum + debit
//                 authorization capture), verified per client.
//   3. Package  — six required documents, either DISCRETE (six uploads) or
//                 MERGED (one PDF, split by doc-intel page classification).
//                 Every slot is validated by doc-intel; filing is refused
//                 until all six are ACCEPTED.
//
// Bulk paths: CSV roster import (row-level report) and ZIP package upload
// (top-level folder = client party_name; merged.pdf = MERGED package).
//
// Card numbers never touch this platform — only processor token refs and
// last4. onboarding_status is derived (recomputeClientStatus), never set
// directly, so no client can be "READY" by declaration.

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// discardWriter lets security gates that write an HTTP error (scanOrRefuse)
// run inside flows that return their own error string instead (ZIP entries,
// shared store paths) — the gate's verdict is read from its bool, its
// response body is discarded.
type discardWriter struct{ header http.Header }

func (d discardWriter) Header() http.Header {
	if d.header == nil {
		return http.Header{}
	}
	return d.header
}
func (d discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardWriter) WriteHeader(int)             {}

// tpaSlots is the default six-document requirement set. Programs can widen
// it later; the filing gate reads ACCEPTED slots per package, so config
// changes never invalidate in-flight packages.
var tpaSlots = []string{
	"EOB", "ITEMIZED_BILL", "PROOF_OF_COVERAGE",
	"OPEN_NEGOTIATION_NOTICE", "OFFER_SUBSTANTIATION", "IDRE_ATTESTATION",
}

func tpaSlotValid(slot string) bool {
	for _, s := range tpaSlots {
		if s == slot {
			return true
		}
	}
	return false
}

// tpaSlotFromFilename maps a bulk-upload filename onto a slot.
func tpaSlotFromFilename(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "eob"), strings.Contains(n, "remit"):
		return "EOB"
	case strings.Contains(n, "itemized"), strings.Contains(n, "ub04"), strings.Contains(n, "ub-04"):
		return "ITEMIZED_BILL"
	case strings.Contains(n, "coverage"), strings.Contains(n, "eligibility"):
		return "PROOF_OF_COVERAGE"
	case strings.Contains(n, "negotiation"):
		return "OPEN_NEGOTIATION_NOTICE"
	case strings.Contains(n, "substantiation"), strings.Contains(n, "offer"):
		return "OFFER_SUBSTANTIATION"
	case strings.Contains(n, "attestation"), strings.Contains(n, "attest"):
		return "IDRE_ATTESTATION"
	}
	return ""
}

// --- validators -----------------------------------------------------------

// npiValid: NPI is 10 digits, Luhn check with the 80840 health-care prefix.
func npiValid(npi string) bool {
	if len(npi) != 10 {
		return false
	}
	full := "80840" + npi
	sum := 0
	for i := 0; i < len(full); i++ {
		d := int(full[len(full)-1-i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if i%2 == 1 { // double every second digit from the right, skipping the check digit
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

func tinValid(tin string) bool {
	if len(tin) != 9 {
		return false
	}
	for _, c := range tin {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// routingValid: ABA routing transit number checksum (3-7-1 weights).
func routingValid(rt string) bool {
	if len(rt) != 9 {
		return false
	}
	w := []int{3, 7, 1}
	sum := 0
	for i, c := range rt {
		d := int(c - '0')
		if d < 0 || d > 9 {
			return false
		}
		sum += d * w[i%3]
	}
	return sum%10 == 0
}

// --- onboarding status (derived) ------------------------------------------

// recomputeClientStatus derives PROFILE_INCOMPLETE -> PAYMENT_PENDING ->
// DOCS_PENDING -> READY from the three gates. Never trusts caller input.
func (s *server) recomputeClientStatus(r *http.Request, tenant, clientID string) string {
	status := "PROFILE_INCOMPLETE"
	var npi, tin, addr *string
	if err := s.db.QueryRow(r.Context(), `
		SELECT npi, tin, address FROM public.tpa_clients WHERE tenant=$1 AND id=$2`,
		tenant, clientID).Scan(&npi, &tin, &addr); err != nil {
		return status
	}
	if npi == nil || *npi == "" || tin == nil || *tin == "" || addr == nil || *addr == "" {
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.tpa_clients SET onboarding_status=$3 WHERE tenant=$1 AND id=$2`,
			tenant, clientID, status)
		return status
	}
	status = "PAYMENT_PENDING"
	var payments int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.tpa_payment_methods
		WHERE tenant=$1 AND client_id=$2 AND verified AND status='ACTIVE'`,
		tenant, clientID).Scan(&payments)
	if payments == 0 {
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.tpa_clients SET onboarding_status=$3 WHERE tenant=$1 AND id=$2`,
			tenant, clientID, status)
		return status
	}
	status = "DOCS_PENDING"
	var ready int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.tpa_packages
		WHERE tenant=$1 AND client_id=$2 AND status='READY'`,
		tenant, clientID).Scan(&ready)
	if ready > 0 {
		status = "READY"
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.tpa_clients SET onboarding_status=$3 WHERE tenant=$1 AND id=$2`,
		tenant, clientID, status)
	return status
}

// --- profile ---------------------------------------------------------------

// tpaUpdateClientProfile: NPI/TIN/address for one initiating party.
func (s *server) tpaUpdateClientProfile(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	clientID := chi.URLParam(r, "clientId")
	var in struct {
		NPI     string `json:"npi"`
		TIN     string `json:"tin"`
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"npi, tin, address required"}`, http.StatusBadRequest)
		return
	}
	in.NPI = strings.TrimSpace(in.NPI)
	in.TIN = strings.TrimSpace(in.TIN)
	in.Address = strings.TrimSpace(in.Address)
	if !npiValid(in.NPI) {
		http.Error(w, `{"error":"NPI failed the 10-digit Luhn check (80840 prefix)"}`, http.StatusBadRequest)
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
	tag, err := s.db.Exec(r.Context(), `
		UPDATE public.tpa_clients SET npi=$3, tin=$4, address=$5
		WHERE tenant=$1 AND tpa_id=$2 AND id=$6`,
		tenant, org.ID, in.NPI, in.TIN, in.Address, clientID)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, `{"error":"client not found"}`, http.StatusNotFound)
		return
	}
	st := s.recomputeClientStatus(r, tenant, clientID)
	writeJSON(w, http.StatusOK, map[string]any{"client_id": clientID, "onboarding_status": st})
}

// --- bulk roster import -----------------------------------------------------

// tpaImportClientsCSV: multipart CSV, one initiating party per row.
// Header: party_name,party_type,contact_email,npi,tin,address
// Every row is validated independently; the report lists per-row outcomes —
// bad rows never block good ones (bulk = partial success, always reported).
func (s *server) tpaImportClientsCSV(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
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
		ClientID  string `json:"client_id,omitempty"`
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
		case ptype != "PROVIDER" && ptype != "HEALTH_PLAN":
			res.Error = "party_type must be PROVIDER or HEALTH_PLAN"
		case npi != "" && !npiValid(npi):
			res.Error = "NPI failed Luhn check"
		case tin != "" && !tinValid(tin):
			res.Error = "TIN must be 9 digits"
		default:
			var id string
			err := s.db.QueryRow(r.Context(), `
				INSERT INTO public.tpa_clients (tenant, tpa_id, party_name, party_type, contact_email, npi, tin, address)
				VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''))
				ON CONFLICT (tenant, tpa_id, party_name) DO UPDATE SET
				  contact_email=excluded.contact_email, npi=coalesce(excluded.npi, public.tpa_clients.npi),
				  tin=coalesce(excluded.tin, public.tpa_clients.tin), address=coalesce(excluded.address, public.tpa_clients.address)
				RETURNING id`, tenant, org.ID, name, ptype, email, npi, tin, addr).Scan(&id)
			if err != nil {
				res.Error = "insert failed"
			} else {
				res.OK, res.ClientID = true, id
				imported++
				s.recomputeClientStatus(r, tenant, id)
			}
		}
		report = append(report, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": imported, "total": len(report), "rows": report})
}

// --- payment methods ---------------------------------------------------------

// tpaAddPayment: attach a CARD or ACH rail to one client. Card: processor
// token ref + brand + last4 (PAN never stored). ACH: routing (checksum) +
// account last4 + explicit debit-authorization capture.
func (s *server) tpaAddPayment(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	clientID := chi.URLParam(r, "clientId")
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
		verified = true // processor tokenization already verified the instrument
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
		SELECT count(*) FROM public.tpa_clients WHERE tenant=$1 AND tpa_id=$2 AND id=$3`,
		tenant, org.ID, clientID).Scan(&owner)
	if owner == 0 {
		http.Error(w, `{"error":"client not found"}`, http.StatusNotFound)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.tpa_payment_methods
		  (tenant, tpa_id, client_id, method, label, brand, last4, routing, processor_ref, auth_received, verified)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),$10,$11)
		RETURNING id`,
		tenant, org.ID, clientID, in.Method, in.Label, strings.ToUpper(in.Brand),
		in.Last4, in.Routing, in.ProcessorRef, in.AuthReceived, verified).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		return
	}
	st := s.recomputeClientStatus(r, tenant, clientID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"payment_method_id": id, "verified": verified, "onboarding_status": st})
}

// tpaListPayments: masked rails for one client.
func (s *server) tpaListPayments(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT id, method, coalesce(label,''), coalesce(brand,''), last4,
		       coalesce(routing,''), auth_received, verified, status
		FROM public.tpa_payment_methods
		WHERE tenant=$1 AND tpa_id=$2 AND client_id=$3 ORDER BY created_at`,
		tenant, org.ID, chi.URLParam(r, "clientId"))
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

// tpaPackageOwned resolves package -> (clientID, mode, status), enforcing
// TPA ownership. Every package handler starts here.
func (s *server) tpaPackageOwned(r *http.Request, tenant, tpaID, pkgID string) (clientID, mode, status string, err error) {
	err = s.db.QueryRow(r.Context(), `
		SELECT client_id, mode, status FROM public.tpa_packages
		WHERE tenant=$1 AND tpa_id=$2 AND id=$3`, tenant, tpaID, pkgID).
		Scan(&clientID, &mode, &status)
	if err != nil {
		err = fmt.Errorf("package not found")
	}
	return
}

// tpaCreatePackage opens a DISCRETE or MERGED package for a client.
func (s *server) tpaCreatePackage(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	clientID := chi.URLParam(r, "clientId")
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
		SELECT count(*) FROM public.tpa_clients WHERE tenant=$1 AND tpa_id=$2 AND id=$3`,
		tenant, org.ID, clientID).Scan(&owner)
	if owner == 0 {
		http.Error(w, `{"error":"client not found"}`, http.StatusNotFound)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.tpa_packages (tenant, tpa_id, client_id, mode) VALUES ($1,$2,$3,$4) RETURNING id`,
		tenant, org.ID, clientID, mode).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"package_id": id, "mode": mode, "status": "ASSEMBLING", "slots": tpaSlots})
}

// storePackageDoc seals + stores one package document and publishes the
// validation event. Shared by discrete, merged, and ZIP bulk uploads.
func (s *server) storePackageDoc(r *http.Request, tenant, tpaID, pkgID, slot, filename string, raw []byte) (string, error) {
	if n, err := s.rds.incrExpire("idre:rl:tpadoc:"+tenant+":"+tpaID, 3600); err == nil && n > 400 {
		return "", fmt.Errorf("upload rate limit exceeded (400/hour)")
	}
	if err := contentPolicy(filename, raw); err != nil {
		return "", err
	}
	if sig, ok := s.scanOrRefuse(discardWriter{}, bytes.NewReader(raw), "file"); !ok {
		return "", fmt.Errorf("malware scan rejected: %s", sig)
	}
	docID := newUUID()
	objectKey := fmt.Sprintf("%s/tpa-packages/%s/%s.enc", tenant, pkgID, docID)
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
		INSERT INTO public.tpa_package_docs (tenant, package_id, slot, object_key, filename, status)
		VALUES ($1,$2,$3,$4,$5,'PENDING')
		ON CONFLICT (package_id, slot) DO UPDATE SET
		  object_key=excluded.object_key, filename=excluded.filename,
		  status='PENDING', reasons='[]'::jsonb, analyzed_at=NULL
		RETURNING id`, tenant, pkgID, slot, objectKey, filename).Scan(&rowID)
	if err != nil {
		return "", fmt.Errorf("db")
	}
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "tpa_doc.uploaded", "tenant": tenant, "package_id": pkgID,
		"package_doc_id": rowID, "slot": slot, "object_key": objectKey,
		"filename": filename, "merged": false, "at": time.Now().UTC(),
	})
	return rowID, nil
}

// tpaAttachDoc: one DISCRETE slot upload (multipart file + slot field).
func (s *server) tpaAttachDoc(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	_, mode, status, err := s.tpaPackageOwned(r, tenant, org.ID, pkgID)
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
	rowID, err := s.storePackageDoc(r, tenant, org.ID, pkgID, slot, hdr.Filename, raw)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"package_doc_id": rowID, "slot": slot, "status": "PENDING",
		"note": "doc-intel validates the document; poll the package for the verdict"})
}

// tpaUploadMerged: one PDF containing all six documents; doc-intel splits it.
func (s *server) tpaUploadMerged(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	_, mode, status, err := s.tpaPackageOwned(r, tenant, org.ID, pkgID)
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
	objectKey := fmt.Sprintf("%s/tpa-packages/%s/merged-%s.enc", tenant, pkgID, newUUID())
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
		UPDATE public.tpa_packages SET source_object_key=$3, status='VALIDATING', updated_at=now()
		WHERE tenant=$1 AND id=$2`, tenant, pkgID, objectKey)
	_, _ = s.db.Exec(r.Context(), `DELETE FROM public.tpa_package_docs WHERE tenant=$1 AND package_id=$2`, tenant, pkgID)
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "tpa_doc.uploaded", "tenant": tenant, "package_id": pkgID,
		"object_key": objectKey, "filename": hdr.Filename, "merged": true,
		"at": time.Now().UTC(),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"package_id": pkgID, "status": "VALIDATING",
		"note": "doc-intel is splitting the PDF into the six required slots; poll the package for per-slot verdicts"})
}

// tpaPackageStatus: per-slot verdict grid + package readiness.
func (s *server) tpaPackageStatus(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	clientID, mode, status, err := s.tpaPackageOwned(r, tenant, org.ID, pkgID)
	if err != nil {
		http.Error(w, `{"error":"package not found"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT id, slot, coalesce(filename,''), status, reasons::text,
		       coalesce(page_start::text,''), coalesce(page_end::text,''), coalesce(analyzed_at::text,'')
		FROM public.tpa_package_docs WHERE tenant=$1 AND package_id=$2 ORDER BY slot`, tenant, pkgID)
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
		"package_id": pkgID, "client_id": clientID, "mode": mode,
		"status": status, "slots": grid})
}

// tpaOnboardingBoard: the TPA's whole roster with the three gates per client.
func (s *server) tpaOnboardingBoard(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT c.id, c.party_name, c.party_type, c.onboarding_status, c.status,
		       coalesce(c.npi,''), coalesce(c.tin,''),
		       (SELECT count(*) FROM public.tpa_payment_methods m
		         WHERE m.tenant=c.tenant AND m.client_id=c.id AND m.verified AND m.status='ACTIVE'),
		       (SELECT count(*) FROM public.tpa_packages pk
		         WHERE pk.tenant=c.tenant AND pk.client_id=c.id AND pk.status='READY'),
		       (SELECT json_agg(json_build_object('id',pk.id,'mode',pk.mode,'status',pk.status))
		        FROM public.tpa_packages pk WHERE pk.tenant=c.tenant AND pk.client_id=c.id AND pk.status<>'FILED')
		FROM public.tpa_clients c WHERE c.tenant=$1 AND c.tpa_id=$2 ORDER BY c.party_name`,
		tenant, org.ID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	clients := []map[string]any{}
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
		clients = append(clients, map[string]any{
			"client_id": id, "party_name": name, "party_type": ptype,
			"onboarding_status": ost, "status": cst,
			"profile_complete": npi != "" && tin != "",
			"verified_payments": npay, "ready_packages": nready,
			"open_packages": packages,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tpa_id": org.ID, "clients": clients, "required_slots": tpaSlots})
}

// --- bulk ZIP packages ---------------------------------------------------------

// tpaUploadBulkZip: one ZIP, many clients. Layout:
//   <party_name>/eob.pdf, itemized_bill.pdf, ...     -> DISCRETE package
//   <party_name>/merged.pdf                          -> MERGED package
// Every client folder gets its own package and per-slot validation; the
// response is a per-client report so one bad folder never blocks the rest.
func (s *server) tpaUploadBulkZip(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
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
	// Group entries by top-level folder = client party_name.
	type entry struct {
		name string
		zf   *zip.File
	}
	byClient := map[string][]entry{}
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		parts := strings.SplitN(strings.Trim(zf.Name, "/"), "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		byClient[parts[0]] = append(byClient[parts[0]], entry{parts[1], zf})
	}
	type clientReport struct {
		Client     string   `json:"client"`
		PackageID  string   `json:"package_id,omitempty"`
		Mode       string   `json:"mode,omitempty"`
		Attached   []string `json:"attached,omitempty"`
		Errors     []string `json:"errors,omitempty"`
	}
	reports := []clientReport{}
	for clientName, entries := range byClient {
		rep := clientReport{Client: clientName}
		var clientID string
		err := s.db.QueryRow(r.Context(), `
			SELECT id FROM public.tpa_clients WHERE tenant=$1 AND tpa_id=$2 AND lower(party_name)=lower($3)`,
			tenant, org.ID, clientName).Scan(&clientID)
		if err != nil {
			rep.Errors = append(rep.Errors, "no client with this party_name on your roster — import the roster first")
			reports = append(reports, rep)
			continue
		}
		// merged.pdf anywhere in the folder -> MERGED package.
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
			INSERT INTO public.tpa_packages (tenant, tpa_id, client_id, mode) VALUES ($1,$2,$3,$4) RETURNING id`,
			tenant, org.ID, clientID, mode).Scan(&pkgID); err != nil {
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
					objectKey := fmt.Sprintf("%s/tpa-packages/%s/merged-%s.enc", tenant, pkgID, newUUID())
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
							UPDATE public.tpa_packages SET source_object_key=$3, status='VALIDATING', updated_at=now()
							WHERE tenant=$1 AND id=$2`, tenant, pkgID, objectKey)
						s.publish(r.Context(), tenant, "documents", map[string]any{
							"type": "tpa_doc.uploaded", "tenant": tenant, "package_id": pkgID,
							"object_key": objectKey, "filename": "merged.pdf", "merged": true,
							"at": time.Now().UTC(),
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
			if _, err := s.storePackageDoc(r, tenant, org.ID, pkgID, slot, e.name, data); err != nil {
				rep.Errors = append(rep.Errors, e.name+": "+err.Error())
				continue
			}
			rep.Attached = append(rep.Attached, slot)
		}
		reports = append(reports, rep)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"reports": reports})
}
