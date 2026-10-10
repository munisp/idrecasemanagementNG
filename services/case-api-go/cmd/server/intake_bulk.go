package main

// Bulk dispute intake — a third-party filer (revenue-cycle vendor, legal
// representative, plan delegate) submits many disputes on behalf of MANY
// parties in one authenticated call.
//
// Design contract (NG):
//   - Idempotent by batch_ref: the filer's own batch identifier. A retried
//     submission replays the recorded results instead of double-filing.
//   - Per-item isolation: every row is processed independently; one bad row
//     never blocks the rest, and every row reports its own outcome.
//   - Manifest-driven, same as single intake: filing-party codes and sector
//     intake fields come from the tenant's Program Manifest; undeclared keys
//     are dropped, required fields enforced — bulk is a multiplier on the
//     existing path, never a parallel one.
//   - The audit trail is the record itself (NG has no audit_log): each
//     created intake row carries its own timestamps, and public.intake_batches
//     persists the batch with full per-row results — replay-safe and
//     reconcilable without a parallel log.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

const bulkIntakeMaxItems = 500

// bulkIntakeItem mirrors the NG createIntake payload plus the filer's own
// row-level reference (their spreadsheet row / claim-system key), echoed
// back so results reconcile against the source file.
type bulkIntakeItem struct {
	ExternalRef     string         `json:"external_ref"`
	Email           string         `json:"email"`
	ContactName     string         `json:"contact_name"`
	Org             string         `json:"org"`
	Notes           string         `json:"notes"`
	FilingPartyType string         `json:"filing_party_type"`
	Fields          map[string]any `json:"fields"` // manifest intake_fields
}

type bulkIntakeResult struct {
	ExternalRef string `json:"external_ref,omitempty"`
	Status      string `json:"status"` // CREATED | ERROR
	IntakeID    string `json:"intake_id,omitempty"`
	Error       string `json:"error,omitempty"`
}

// validateBulkIntakeItem is the pure per-row gate — same rules as NG
// createIntake: email shape, filing-party codes from the manifest, declared
// intake fields with required enforcement. Exercised by unit tests without
// a database.
func validateBulkIntakeItem(it bulkIntakeItem, codeA, codeB string, m *ProgramManifest) string {
	if strings.TrimSpace(it.Email) == "" || !strings.Contains(it.Email, "@") {
		return "email required"
	}
	fpt := strings.ToUpper(strings.TrimSpace(it.FilingPartyType))
	if fpt != "" && fpt != codeA && fpt != codeB {
		return fmt.Sprintf("filing_party_type must be %s or %s", codeA, codeB)
	}
	if m != nil {
		for _, f := range m.IntakeFields {
			v, present := it.Fields[f.Name]
			if f.Required && (!present || fmt.Sprint(v) == "") {
				return fmt.Sprintf("intake field %q required", f.Name)
			}
		}
	}
	return ""
}

// processOneBulkIntake runs ONE row through the exact NG createIntake
// semantics: declared-fields-only details, manifest party codes, intake_requests
// insert. Never called without validateBulkIntakeItem passing first.
func (s *server) processOneBulkIntake(r *http.Request, tenant string, it bulkIntakeItem, codeA, codeB string, m *ProgramManifest) bulkIntakeResult {
	res := bulkIntakeResult{ExternalRef: it.ExternalRef}
	if msg := validateBulkIntakeItem(it, codeA, codeB, m); msg != "" {
		res.Status, res.Error = "ERROR", msg
		return res
	}
	fpt := strings.ToUpper(strings.TrimSpace(it.FilingPartyType))
	if fpt == "" {
		fpt = codeA
	}
	details := map[string]any{}
	if m != nil {
		for _, f := range m.IntakeFields {
			if v, present := it.Fields[f.Name]; present {
				details[f.Name] = v
			}
		}
	}
	detailsJSON, _ := json.Marshal(details)
	var id string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.intake_requests (tenant, email, contact_name, org, notes, outreach_at, filing_party_type, details)
		VALUES ($1,$2,$3,$4,$5, now(), $6, $7) RETURNING id`,
		tenant, it.Email, it.ContactName, it.Org, it.Notes, fpt, detailsJSON).Scan(&id)
	if id == "" {
		res.Status, res.Error = "ERROR", "intake insert failed"
		return res
	}
	res.Status, res.IntakeID = "CREATED", id
	return res
}

// bulkIntake handles POST /intake/bulk — see the file header for the
// contract. Response is 200 for any well-formed request; per-row outcomes
// live in results[]. A replayed batch_ref returns the recorded results with
// idempotent_replay: true and files nothing twice.
func (s *server) bulkIntake(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	// BULK_SUBMITTER and TPA are the third-party-filer roles: they open intake
	// and read their own batches — nothing else on the platform. TPA is a
	// first-class role filing on behalf of the initiating party; BULK_SUBMITTER
	// remains for plain batch filers.
	if !hasAnyRole(p, "BULK_SUBMITTER", "TPA", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires TPA, BULK_SUBMITTER, or staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		BatchRef string           `json:"batch_ref"`
		Items    []bulkIntakeItem `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	in.BatchRef = strings.TrimSpace(in.BatchRef)
	if in.BatchRef == "" || len(in.BatchRef) > 128 {
		http.Error(w, `{"error":"batch_ref required (<=128 chars) — your idempotency key for safe retries"}`, http.StatusBadRequest)
		return
	}
	if len(in.Items) == 0 {
		http.Error(w, `{"error":"items must contain at least one dispute"}`, http.StatusBadRequest)
		return
	}
	if len(in.Items) > bulkIntakeMaxItems {
		http.Error(w, fmt.Sprintf(`{"error":"at most %d items per batch — split larger files into multiple batch_refs"}`, bulkIntakeMaxItems), http.StatusBadRequest)
		return
	}

	// Party codes + field spec from the tenant manifest (defaults preserve the
	// healthcare legacy codes).
	codeA, codeB := "PROVIDER", "HEALTH_PLAN"
	var manifest *ProgramManifest
	if m, err := s.manifestFor(r.Context(), tenant); err == nil && m != nil {
		manifest = m
		if m.Terminology.PartyACode != "" {
			codeA = m.Terminology.PartyACode
		}
		if m.Terminology.PartyBCode != "" {
			codeB = m.Terminology.PartyBCode
		}
	}

	// Idempotency check first: this batch_ref already completed -> replay.
	var existing struct {
		ID      int64
		Results []byte
	}
	if err := s.db.QueryRow(r.Context(), `
		SELECT id, results FROM public.intake_batches
		WHERE tenant=$1 AND submitter=$2 AND batch_ref=$3`, tenant, p.Subject, in.BatchRef).Scan(&existing.ID, &existing.Results); err == nil {
		var results []bulkIntakeResult
		_ = json.Unmarshal(existing.Results, &results)
		writeJSON(w, http.StatusOK, map[string]any{
			"batch_id": existing.ID, "idempotent_replay": true, "results": results,
		})
		return
	}

	results := make([]bulkIntakeResult, 0, len(in.Items))
	created, errored := 0, 0
	for i, it := range in.Items {
		if strings.TrimSpace(it.ExternalRef) == "" {
			it.ExternalRef = fmt.Sprintf("row-%d", i+1)
		}
		res := s.processOneBulkIntake(r, tenant, it, codeA, codeB, manifest)
		if res.Status == "CREATED" {
			created++
		} else {
			errored++
		}
		results = append(results, res)
	}
	resultsJSON, _ := json.Marshal(results)

	var batchID int64
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.intake_batches (tenant, submitter, batch_ref, item_count, created_count, error_count, results)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant, submitter, batch_ref) DO NOTHING
		RETURNING id`, tenant, p.Subject, in.BatchRef, len(in.Items), created, errored, resultsJSON).Scan(&batchID)
	if err != nil || batchID == 0 {
		// Lost the race against a concurrent identical submission: replay.
		if scanErr := s.db.QueryRow(r.Context(), `
			SELECT id, results FROM public.intake_batches
			WHERE tenant=$1 AND submitter=$2 AND batch_ref=$3`, tenant, p.Subject, in.BatchRef).Scan(&existing.ID, &existing.Results); scanErr == nil {
			_ = json.Unmarshal(existing.Results, &results)
			writeJSON(w, http.StatusOK, map[string]any{
				"batch_id": existing.ID, "idempotent_replay": true, "results": results,
			})
			return
		}
		http.Error(w, `{"error":"batch record failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": batchID, "batch_ref": in.BatchRef,
		"items": len(in.Items), "created": created, "errors": errored,
		"results": results,
	})
}

// getIntakeBatch returns a previously submitted batch with per-row results —
// the filer's receipt and the support desk's reconciliation view. Submitters
// see only their own batches; staff see any batch in the tenant.
func (s *server) getIntakeBatch(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "BULK_SUBMITTER", "TPA", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "batchId")
	var (
		submitter, batchRef         string
		itemCount, createdC, errorC int
		resultsJSON                 []byte
		createdAt                   string
	)
	q := `SELECT submitter, batch_ref, item_count, created_count, error_count, results, created_at::text
	      FROM public.intake_batches WHERE tenant=$1 AND id=$2`
	args := []any{tenant, id}
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		q += ` AND submitter=$3`
		args = append(args, p.Subject)
	}
	if err := s.db.QueryRow(r.Context(), q, args...).Scan(
		&submitter, &batchRef, &itemCount, &createdC, &errorC, &resultsJSON, &createdAt); err != nil {
		http.Error(w, `{"error":"batch not found"}`, http.StatusNotFound)
		return
	}
	var results []bulkIntakeResult
	_ = json.Unmarshal(resultsJSON, &results)
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": id, "batch_ref": batchRef, "submitter": submitter,
		"items": itemCount, "created": createdC, "errors": errorC,
		"submitted_at": createdAt, "results": results,
	})
}

// listIntakeBatches: GET /intake/bulk — the filer's status board. Third-party
// filers (TPA, BULK_SUBMITTER) see only their own batches; staff may pass
// ?submitter= to inspect a specific filer, or see all batches in the tenant.
func (s *server) listIntakeBatches(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "BULK_SUBMITTER", "TPA", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	staff := hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole)
	q := `SELECT id, submitter, batch_ref, item_count, created_count, error_count, created_at::text
	      FROM public.intake_batches WHERE tenant=$1`
	args := []any{tenant}
	if !staff {
		q += ` AND submitter=$2`
		args = append(args, p.Subject)
	} else if sub := strings.TrimSpace(r.URL.Query().Get("submitter")); sub != "" {
		q += ` AND submitter=$2`
		args = append(args, sub)
	}
	q += ` ORDER BY id DESC LIMIT 200`
	rows, err := s.queryRows(r, q, args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": rows})
}
