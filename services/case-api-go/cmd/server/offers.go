package main

// offers.go — double-blind offer protocol (45 CFR 149.520(b)).
//
// Flow:  party submits offer -> vault /seal (plaintext never touches Postgres)
//        -> offers row holds only (nonce, ct, wrapped_dek) + metadata
//        -> OFFER_SUBMITTED signal advances the Temporal workflow
//        -> workflow ends the window (BOTH_SUBMITTED | WINDOW_EXPIRED) and sets
//           case status OFFERS_REVEALED with the reason
//        -> arbitrator calls POST /offers/reveal -> case-api calls vault
//           /reveal per offer -> the vault INDEPENDENTLY re-verifies lawfulness
//           via GET /internal/reveal-check/{tenant}/{caseId} before decrypting
//        -> amounts land in revealed_* columns; determination selects one offer
//
// Tampering with DB flags alone cannot reveal an offer: the vault is the
// second verifier, and AEAD AAD binding prevents cross-case/ct swaps.

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
)

// offerParties are the two sides of a baseball-style dispute.
var offerParties = []string{"INITIATING", "RESPONDING"}

func validOfferParty(p string) bool {
	return p == "INITIATING" || p == "RESPONDING"
}

// ---- vault offer helpers ---------------------------------------------------

type vaultSealResp struct {
	NonceB64      string `json:"nonce_b64"`
	CiphertextB64 string `json:"ciphertext_b64"`
	WrappedDekB64 string `json:"wrapped_dek_b64"`
}

// vaultSealOffer seals the offer payload; returns the envelope columns.
func (s *server) vaultSealOffer(r *http.Request, tenant, caseID, offerID, partyID string, plaintext []byte) (nonce, ct, wrapped []byte, err error) {
	body, _ := json.Marshal(map[string]string{
		"tenant": tenant, "case_id": caseID, "offer_id": offerID,
		"party_id": partyID, "plaintext_b64": base64.StdEncoding.EncodeToString(plaintext),
	})
	resp, err := s.vaultPost(r, "/seal", body)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, fmt.Errorf("vault seal: %s", resp.Status)
	}
	var out vaultSealResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, nil, nil, err
	}
	nonce, err1 := base64.StdEncoding.DecodeString(out.NonceB64)
	ct, err2 := base64.StdEncoding.DecodeString(out.CiphertextB64)
	wrapped, err3 := base64.StdEncoding.DecodeString(out.WrappedDekB64)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, nil, nil, fmt.Errorf("vault seal: bad envelope encoding")
	}
	return nonce, ct, wrapped, nil
}

// vaultRevealOffer asks the vault to lawfully decrypt an offer. The vault
// re-checks the workflow state itself; a non-lawful reveal comes back 409.
func (s *server) vaultRevealOffer(r *http.Request, tenant, caseID, offerID, partyID string, nonce, ct, wrapped []byte) ([]byte, string, error) {
	body, _ := json.Marshal(map[string]string{
		"tenant": tenant, "case_id": caseID, "offer_id": offerID, "party_id": partyID,
		"caller_role":     "ARBITRATOR",
		"nonce_b64":       base64.StdEncoding.EncodeToString(nonce),
		"ciphertext_b64":  base64.StdEncoding.EncodeToString(ct),
		"wrapped_dek_b64": base64.StdEncoding.EncodeToString(wrapped),
	})
	resp, err := s.vaultPost(r, "/reveal", body)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("vault reveal: %s", resp.Status)
	}
	var out struct {
		PlaintextB64 string `json:"plaintext_b64"`
		RevealReason string `json:"reveal_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	pt, err := base64.StdEncoding.DecodeString(out.PlaintextB64)
	return pt, out.RevealReason, err
}

// ---- reveal-check oracle (called by the vault) -----------------------------
// Registered OUTSIDE the authenticated router: gated solely by the vault's
// internal token. Reveals nothing sensitive — a boolean plus the reason.

func (s *server) revealCheck(w http.ResponseWriter, r *http.Request) {
	tok := os.Getenv("VAULT_INTERNAL_TOKEN")
	given := r.Header.Get("X-Internal-Auth")
	if tok == "" || subtle.ConstantTimeCompare([]byte(given), []byte(tok)) != 1 {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	tenant := chi.URLParam(r, "tenant")
	caseID := chi.URLParam(r, "caseId")
	var status, reason string
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status, coalesce(status_reason,'') FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&status, &reason)
	if err != nil || status != "OFFERS_REVEALED" {
		// 409 mirrors the vault's "window still sealed" response contract.
		http.Error(w, `{"error":"reveal not lawful"}`, http.StatusConflict)
		return
	}
	if reason == "" {
		reason = "BOTH_SUBMITTED"
	}
	w.Header().Set("X-Reveal-Reason", reason)
	w.WriteHeader(http.StatusOK)
}

// ---- offer submission ------------------------------------------------------

// submitOffer: POST /v1/tenants/{tenant}/cases/{caseId}/offers
// PARTY callers must hold a party_accounts mapping for this case; staff may
// submit on a party's behalf (paper/fax offers happen in practice).
func (s *server) submitOffer(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if !hasAnyRole(p, "PARTY", "TPA", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	var in struct {
		PartyID      string   `json:"party_id"` // INITIATING | RESPONDING
		AmountCents  int64    `json:"amount_cents"`
		Justification string  `json:"justification"`
		Documents    []string `json:"documents"` // doc IDs already uploaded to the case docket
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil ||
		!validOfferParty(in.PartyID) || in.AmountCents <= 0 {
		http.Error(w, `{"error":"party_id (INITIATING|RESPONDING) and positive amount_cents required"}`, http.StatusBadRequest)
		return
	}
	// Party scoping: an authenticated PARTY may only act on mapped cases.
	if hasAnyRole(p, "PARTY") && !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		if !s.partyOwnsCase(r, tenant, caseID, p.Subject, in.PartyID) {
			http.Error(w, `{"error":"no party assignment for this case"}`, http.StatusForbidden)
			return
		}
	}
	// Statutory window: offers only while the workflow has the window open.
	var status, caseNumber string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status, case_number FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&status, &caseNumber); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if status != "OFFER_WINDOW_OPEN" {
		http.Error(w, fmt.Sprintf(`{"error":"offer window is not open (case is %s)"}`, status), http.StatusConflict)
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"amount_cents": in.AmountCents, "justification": in.Justification,
		"documents": in.Documents, "submitted_by": p.Subject,
		"submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	// The offer ID is generated before sealing so it is part of the AEAD AAD.
	offerID := newUUID()
	nonce, ct, wrapped, err := s.vaultSealOffer(r, tenant, caseID, offerID, in.PartyID, payload)
	if err != nil {
		http.Error(w, `{"error":"vault seal failed"}`, http.StatusBadGateway)
		return
	}
	_, err = s.db.Exec(r.Context(), `
		INSERT INTO public.offers (id, tenant, case_id, party_id, sealed_nonce, sealed_ct, wrapped_dek, submitted_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		offerID, tenant, caseID, in.PartyID, nonce, ct, wrapped, p.Subject)
	if err != nil {
		// UNIQUE(tenant, case_id, party_id): one offer per party, no amendments.
		http.Error(w, `{"error":"an offer from this party is already sealed for this case"}`, http.StatusConflict)
		return
	}
	s.signalWorkflow(r, tenant, caseID, caseNumber, "OFFER_SUBMITTED", map[string]any{"party_id": in.PartyID})
	s.logActivity(r.Context(), tenant, caseID, "OFFER_SEALED",
		fmt.Sprintf("Offer sealed for party %s (amount not recorded in the clear)", in.PartyID))
	writeJSON(w, http.StatusCreated, map[string]any{
		"offer_id": offerID, "party_id": in.PartyID, "status": "SEALED",
		"detail": "offer is cryptographically sealed; neither party nor staff can view it until lawful reveal"})
}

// listOffers: GET /cases/{caseId}/offers — metadata for all; amounts only to
// arbitrator/staff once the case is OFFERS_REVEALED or later.
func (s *server) listOffers(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if !hasAnyRole(p, "PARTY", "TPA", "ARBITRATOR", "CASE_MANAGER", "FINANCE", "STATE_AUDITOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	if hasAnyRole(p, "PARTY") && !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "ARBITRATOR", serviceRole) {
		if !s.partyOwnsCase(r, tenant, caseID, p.Subject, "") {
			http.Error(w, `{"error":"no party assignment for this case"}`, http.StatusForbidden)
			return
		}
	}
	var status string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&status)
	revealed := status == "OFFERS_REVEALED" || status == "SELECTION_FINALIZED" ||
		status == "DETERMINED" || status == "CLOSED_PAID"
	staff := hasAnyRole(p, "ARBITRATOR", "CASE_MANAGER", "FINANCE", "STATE_AUDITOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole)

	rows, _ := s.queryRows(r, `
		SELECT id, party_id, submitted_by, submitted_at, revealed_at,
		       CASE WHEN $2 THEN revealed_amount_cents END AS amount_cents,
		       CASE WHEN $2 THEN revealed_payload END AS payload,
		       reveal_reason
		FROM public.offers WHERE tenant=$1 AND case_id=$3 ORDER BY submitted_at`,
		tenant, revealed && staff, caseID)
	writeJSON(w, http.StatusOK, map[string]any{
		"offers": rows, "case_status": status,
		"double_blind": !revealed, // true = amounts remain sealed even to staff
	})
}

// revealOffers: POST /cases/{caseId}/offers/reveal — ARBITRATOR (and admins).
// Lawfulness is decided by the workflow (status OFFERS_REVEALED) and
// independently re-verified by the vault through the reveal-check oracle.
func (s *server) revealOffers(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires ARBITRATOR"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var status string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&status); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if status != "OFFERS_REVEALED" {
		http.Error(w, fmt.Sprintf(`{"error":"reveal not lawful: case is %s (the workflow must close the offer window first)"}`, status), http.StatusConflict)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT id, party_id, sealed_nonce, sealed_ct, wrapped_dek
		FROM public.offers WHERE tenant=$1 AND case_id=$2 AND revealed_at IS NULL`,
		tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	type sealedOffer struct {
		id, party string
		nonce, ct, wrapped []byte
	}
	var pending []sealedOffer
	for rows.Next() {
		var o sealedOffer
		if rows.Scan(&o.id, &o.party, &o.nonce, &o.ct, &o.wrapped) == nil {
			pending = append(pending, o)
		}
	}
	rows.Close()
	revealed := 0
	for _, o := range pending {
		pt, reason, err := s.vaultRevealOffer(r, tenant, caseID, o.id, o.party, o.nonce, o.ct, o.wrapped)
		if err != nil {
			// Vault refused (its own oracle disagreed) — fail the whole reveal.
			http.Error(w, fmt.Sprintf(`{"error":%q}`, "vault refused reveal: "+err.Error()), http.StatusConflict)
			return
		}
		var payload struct {
			AmountCents int64 `json:"amount_cents"`
		}
		_ = json.Unmarshal(pt, &payload)
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.offers
			SET revealed_at=now(), revealed_amount_cents=$3, revealed_payload=$4, reveal_reason=$5
			WHERE tenant=$1 AND id=$2`,
			tenant, o.id, payload.AmountCents, json.RawMessage(pt), reason)
		revealed++
	}
	s.logActivity(r.Context(), tenant, caseID, "OFFERS_REVEALED",
		fmt.Sprintf("%d offer(s) lawfully revealed by %s", revealed, p.Subject))
	writeJSON(w, http.StatusOK, map[string]any{"case_id": caseID, "revealed": revealed})
}

// issueDetermination: POST /cases/{caseId}/determination — baseball-style:
// the arbitrator selects exactly one sealed offer; no splitting the difference.
func (s *server) issueDetermination(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires ARBITRATOR"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		SelectedOfferID string `json:"selected_offer_id"`
		Rationale       string `json:"rationale"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil ||
		in.SelectedOfferID == "" || in.Rationale == "" {
		http.Error(w, `{"error":"selected_offer_id and rationale required"}`, http.StatusBadRequest)
		return
	}
	var caseNumber, status string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number, status FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&caseNumber, &status); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if status != "OFFERS_REVEALED" && status != "SELECTION_FINALIZED" {
		http.Error(w, fmt.Sprintf(`{"error":"determination requires revealed offers (case is %s)"}`, status), http.StatusConflict)
		return
	}
	var amount int64
	err := s.db.QueryRow(r.Context(), `
		SELECT revealed_amount_cents FROM public.offers
		WHERE tenant=$1 AND case_id=$2 AND id=$3 AND revealed_at IS NOT NULL`,
		tenant, caseID, in.SelectedOfferID).Scan(&amount)
	if err != nil {
		http.Error(w, `{"error":"selected offer not found or not revealed for this case"}`, http.StatusUnprocessableEntity)
		return
	}
	_, err = s.db.Exec(r.Context(), `
		INSERT INTO public.determinations (tenant, case_id, selected_offer_id, amount_cents, rationale, issued_by)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		tenant, caseID, in.SelectedOfferID, amount, in.Rationale, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"a determination already exists for this case"}`, http.StatusConflict)
		return
	}
	s.signalWorkflow(r, tenant, caseID, caseNumber, "DETERMINATION_ISSUED", map[string]any{
		"amount_cents": amount, "selected_offer_id": in.SelectedOfferID})
	s.logActivity(r.Context(), tenant, caseID, "DETERMINATION_ISSUED",
		fmt.Sprintf("Determination issued: $%d.%02d — %s", amount/100, amount%100, truncate(in.Rationale, 200)))
	s.notify(r, tenant, "*", "DETERMINATION",
		fmt.Sprintf("Determination issued on case %s: $%d.%02d.", caseNumber, amount/100, amount%100),
		"#/cases/"+caseID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"case_id": caseID, "amount_cents": amount, "status": "DETERMINED",
		"payment_due": "30 calendar days (45 CFR 149.520(b)(4))"})
}

// partyOwnsCase: is this Keycloak subject mapped to this case (and optionally
// this side of it)? The party_accounts row is the ONLY source of party access.
func (s *server) partyOwnsCase(r *http.Request, tenant, caseID, subject, partyID string) bool {
	q := `SELECT EXISTS(SELECT 1 FROM public.party_accounts WHERE tenant=$1 AND case_id=$2 AND kc_subject=$3`
	args := []any{tenant, caseID, subject}
	if partyID != "" {
		q += ` AND party_id=$4`
		args = append(args, partyID)
	}
	q += `)`
	var ok bool
	_ = s.db.QueryRow(r.Context(), q, args...).Scan(&ok)
	return ok
}

// signalWorkflow replicates signalCase's wfID lookup for internal callers
// (offer submission / determination) — the HTTP handler stays the public path.
func (s *server) signalWorkflow(r *http.Request, tenant, caseID, caseNumber, signal string, data map[string]any) {
	var wfID string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT workflow_id FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&wfID); err != nil || wfID == "" {
		return
	}
	_ = s.tc.SignalWorkflow(r.Context(), wfID, "", signal, data)
}
