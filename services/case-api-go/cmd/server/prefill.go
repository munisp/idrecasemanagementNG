package main

// prefill.go — Phase 2 of the productivity program: cut the case-initiation
// form down to review-and-confirm instead of blank-form typing.
//
// GET /cases/prefill?doc_id=... returns suggested values for every field on
// the "New dispute" form, each tagged with value/confidence/source/evidence
// so the portal can show WHY a value was suggested (advisory automation:
// a human still submits). With no doc_id, only history-based defaults (the
// tenant's most recent case) are returned. Also returns the tenant's recent
// analyzed documents so the picker in the form doesn't need a second call.
//
// Sources, in priority order per field:
//   1. doc   — the selected document's doc_analysis.result.extracted map,
//              entity-resolved against public.accounts (NPI first, then
//              name match) so the suggestion is a real registry id.
//   2. hist  — the tenant's most recent case (provider/payer/plan/line
//              repeat constantly in batch intake).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type prefillValue struct {
	Value      string `json:"value"`
	Confidence string `json:"confidence"` // high | medium | low
	Source     string `json:"source"`     // doc | hist
	Evidence   string `json:"evidence"`   // human-readable "why"
}

type prefillDoc struct {
	DocID      string `json:"doc_id"`
	CaseID     string `json:"case_id"`
	DocType    string `json:"doc_type"`
	AnalyzedAt string `json:"analyzed_at"`
}

func (s *server) casePrefill(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// Same roles as initiateCase — prefill is the first half of filing.
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "PARTY", "BULK_SUBMITTER", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}

	fields := map[string]prefillValue{}

	// --- source 1: the selected document's extracted fields ---------------
	if docID := r.URL.Query().Get("doc_id"); docID != "" {
		var extracted, conf []byte
		err := s.db.QueryRow(r.Context(), `
			SELECT result->'extracted', COALESCE(result->'field_confidence','{}'::jsonb)
			FROM public.doc_analysis
			WHERE doc_id=$1 AND tenant=$2 AND status LIKE 'ANALYZED%'`, docID, tenant).Scan(&extracted, &conf)
		if err != nil {
			http.Error(w, `{"error":"document not found or not analyzed"}`, http.StatusNotFound)
			return
		}
		var ex map[string]any
		_ = json.Unmarshal(extracted, &ex)
		var fc map[string]string
		_ = json.Unmarshal(conf, &fc)
		confOf := func(k string) string {
			if c := fc[k]; c != "" {
				return c
			}
			return "medium"
		}
		get := func(keys ...string) string {
			for _, k := range keys {
				if v, ok := ex[k]; ok {
					switch t := v.(type) {
					case string:
						if strings.TrimSpace(t) != "" {
							return strings.TrimSpace(t)
						}
					case float64:
						return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", t), "0"), ".")
					}
				}
			}
			return ""
		}
		ev := "document analysis"
		if v := get("claim_number", "case_number"); v != "" {
			fields["case_number"] = prefillValue{v, confOf("claim_number"), "doc", ev}
		}
		if v := get("qpa_usd", "qpa"); v != "" {
			fields["qpa"] = prefillValue{v, confOf("qpa_usd"), "doc", ev}
		}
		if v := get("service_line"); v != "" {
			fields["service_line"] = prefillValue{v, confOf("service_line"), "doc", ev}
		}
		if v := get("line_of_business", "plan_type"); v != "" {
			up := strings.ToUpper(strings.ReplaceAll(v, " ", "_"))
			if up == "SELF_FUNDED" || up == "FULLY_INSURED" {
				fields["plan_type"] = prefillValue{up, confOf("line_of_business"), "doc", ev}
			}
		}
		// Entity-resolve provider/payer names against the CRM registry so the
		// form carries a real id, not free text.
		if name := get("provider_name"); name != "" {
			if id, ok := s.resolveAccount(r, tenant, "PROVIDER", get("provider_npi"), name); ok {
				fields["provider_id"] = prefillValue{id, "high", "doc", "provider name on document matched registry"}
			}
		}
		if name := get("payer_name", "health_plan"); name != "" {
			if id, ok := s.resolveAccount(r, tenant, "PAYER", get("payer_npi"), name); ok {
				fields["payer_id"] = prefillValue{id, "high", "doc", "payer name on document matched registry"}
			}
		}
	}

	// --- source 2: history defaults (don't overwrite doc-derived values) --
	{
		var prov, payer, plan, line string
		row := s.db.QueryRow(r.Context(), fmt.Sprintf(`
			SELECT COALESCE(provider_id,''), COALESCE(payer_id,''), COALESCE(plan_type,''), COALESCE(service_line,'')
			FROM tenant_%s.cases ORDER BY opened_at DESC NULLS LAST LIMIT 1`, sanitizeTenant(tenant)))
		if err := row.Scan(&prov, &payer, &plan, &line); err == nil {
			hist := "most recent case in this tenant"
			if prov != "" {
				if _, ok := fields["provider_id"]; !ok {
					fields["provider_id"] = prefillValue{prov, "medium", "hist", hist}
				}
			}
			if payer != "" {
				if _, ok := fields["payer_id"]; !ok {
					fields["payer_id"] = prefillValue{payer, "medium", "hist", hist}
				}
			}
			if plan != "" {
				if _, ok := fields["plan_type"]; !ok {
					fields["plan_type"] = prefillValue{plan, "medium", "hist", hist}
				}
			}
			if line != "" {
				if _, ok := fields["service_line"]; !ok {
					fields["service_line"] = prefillValue{line, "low", "hist", hist}
				}
			}
		}
	}

	// --- recent analyzed docs for the picker -------------------------------
	docs := []prefillDoc{}
	rows, err := s.db.Query(r.Context(), `
		SELECT doc_id::text, COALESCE(case_id::text,''), COALESCE(doc_type,''),
		       COALESCE(to_char(analyzed_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'),'')
		FROM public.doc_analysis
		WHERE tenant=$1 AND status LIKE 'ANALYZED%'
		ORDER BY analyzed_at DESC NULLS LAST LIMIT 15`, tenant)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var d prefillDoc
			if rows.Scan(&d.DocID, &d.CaseID, &d.DocType, &d.AnalyzedAt) == nil {
				docs = append(docs, d)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"fields": fields, "recent_docs": docs})
}

// resolveAccount maps an extracted party name (or NPI) onto public.accounts.
// NPI match wins over name match; returns the account id.
func (s *server) resolveAccount(r *http.Request, tenant, typ, npi, name string) (string, bool) {
	var id string
	err := s.db.QueryRow(r.Context(), `
		SELECT id::text FROM public.accounts
		WHERE tenant=$1 AND type=$2
		  AND (($3<>'' AND npi=$3) OR legal_name ILIKE '%'||$4||'%')
		ORDER BY ($3<>'' AND npi=$3) DESC, length(legal_name) LIMIT 1`,
		tenant, typ, npi, name).Scan(&id)
	return id, err == nil
}
