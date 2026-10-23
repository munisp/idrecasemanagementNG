package main

// doc_review.go — the tenant-level review inbox for analysis findings.
// doc-intel sets ANALYZED_WITH_FINDINGS when a document surfaces something a
// human should check; until now nothing cleared it, and the reviewer had to
// open each case to find them. GET /documents/review-queue lists everything
// outstanding, ordered by the case's nearest statutory date (same urgency
// rule as the QA queue); POST .../acknowledge records who reviewed and
// clears the flag.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

var docReviewRoles = []string{"CASE_MANAGER", "ARBITRATOR", "DOCTOR", "NURSE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

type docReviewItem struct {
	DocID       string     `json:"doc_id"`
	CaseID      string     `json:"case_id"`
	CaseNumber  string     `json:"case_number"`
	DocType     string     `json:"doc_type"`
	AnalyzedAt  *time.Time `json:"analyzed_at"`
	Findings    int        `json:"findings"`
	NextStatute *time.Time `json:"next_statutory_date,omitempty"`
}

// docReviewQueue: GET /documents/review-queue.
func (s *server) docReviewQueue(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, docReviewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT a.doc_id::text, a.case_id::text, c.case_number, coalesce(a.doc_type,''),
		       a.analyzed_at,
		       CASE WHEN jsonb_typeof(a.result->'findings')='array'
		            THEN jsonb_array_length(a.result->'findings') ELSE 0 END,
		       LEAST(c.offer_window_ends_at, c.open_negotiation_end::timestamptz)
		FROM public.doc_analysis a
		JOIN tenant_%s.cases c ON c.id = a.case_id
		WHERE a.tenant=$1 AND a.status='ANALYZED_WITH_FINDINGS'
		ORDER BY LEAST(c.offer_window_ends_at, c.open_negotiation_end::timestamptz) ASC NULLS LAST,
		         a.analyzed_at DESC
		LIMIT 200`, sanitizeTenant(tenant)), tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []docReviewItem{}
	for rows.Next() {
		var it docReviewItem
		if rows.Scan(&it.DocID, &it.CaseID, &it.CaseNumber, &it.DocType, &it.AnalyzedAt, &it.Findings, &it.NextStatute) == nil {
			out = append(out, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": out, "count": len(out)})
}

// acknowledgeDocFindings: POST /cases/{caseId}/documents/{docId}/acknowledge
// — the reviewer checked the findings; record who and clear the flag.
func (s *server) acknowledgeDocFindings(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, docReviewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	caseID := chi.URLParam(r, "caseId")
	docID := chi.URLParam(r, "docId")
	tag, err := s.db.Exec(r.Context(), `
		UPDATE public.doc_analysis
		SET status='ANALYZED', reviewed_by=$4, reviewed_at=now()
		WHERE tenant=$1 AND doc_id=$2 AND case_id=$3 AND status='ANALYZED_WITH_FINDINGS'`,
		tenant, docID, caseID, p.Subject)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found or already reviewed"}`, http.StatusConflict)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "DOC_FINDINGS_REVIEWED",
		fmt.Sprintf("Analysis findings on document %s reviewed by %s", docID, p.Subject))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ANALYZED"})
}
