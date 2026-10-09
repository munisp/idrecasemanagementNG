package main

// Time entries — per-role effort tracking on each dispute. Eligibility
// decides WHETHER work can be billed; this records the work itself: who
// (subject + role), when (entry_date), how long (minutes), on which case.
// Reports roll it up weekly (per dispute) and monthly (whole team, per
// dispute) for billing and staffing review.
//
// Invariants: entries are append-only through the API (a wrong entry is
// corrected by a compensating entry with a note, never silently edited —
// the record itself is the audit trail, as everywhere in NG);
// the role recorded is the principal's actual role at entry time, not a
// self-declared one.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	chi "github.com/go-chi/chi/v5"
)

// toInt64 coerces a queryRows aggregate value (int64 for sum(int), but
// float64/string from some drivers) into int64 minutes.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		var x int64
		fmt.Sscan(n, &x)
		return x
	}
	return 0
}

// timeEntryRole picks the role recorded on the entry: the most specific
// case-staff role the principal actually holds (admin roles last — an admin
// who is also a nurse records as NURSE).
func timeEntryRole(p principal) string {
	priority := []string{"DOCTOR", "NURSE", "ARBITRATOR", "ATTORNEY", "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}
	for _, want := range priority {
		if hasRole(p, want) {
			return want
		}
	}
	if len(p.Roles) > 0 {
		return p.Roles[0]
	}
	return "STAFF"
}

type timeEntryIn struct {
	Minutes   int     `json:"minutes"`    // exact minutes; wins when >0
	Hours     float64 `json:"hours"`      // convenience: 1.5h = 90m
	EntryDate string  `json:"entry_date"` // YYYY-MM-DD; default today
	Note      string  `json:"note"`
	Billable  *bool   `json:"billable"` // default true
}

func (in *timeEntryIn) normalize() (minutes int, date string, err error) {
	minutes = in.Minutes
	if minutes <= 0 && in.Hours > 0 {
		minutes = int(in.Hours*60 + 0.5)
	}
	if minutes <= 0 || minutes > 1440 {
		return 0, "", fmt.Errorf("minutes must be 1..1440 (or hours 0.02..24)")
	}
	date = strings.TrimSpace(in.EntryDate)
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if _, perr := time.Parse("2006-01-02", date); perr != nil {
		return 0, "", fmt.Errorf("entry_date must be YYYY-MM-DD")
	}
	return minutes, date, nil
}

// addTimeEntry: POST /cases/{caseId}/time — any case-staff role logs work.
func (s *server) addTimeEntry(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "ARBITRATOR", "DOCTOR", "NURSE", "ATTORNEY", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires a case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in timeEntryIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	minutes, date, err := in.normalize()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	// the case must exist in this tenant — an entry against a phantom case
	// would corrupt the billing rollup
	var caseNumber string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	billable := in.Billable == nil || *in.Billable
	role := timeEntryRole(p)
	var id int64
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.time_entries (tenant, case_id, subject, role, entry_date, minutes, note, billable)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		tenant, caseID, p.Subject, role, date, minutes, truncate(strings.TrimSpace(in.Note), 1000), billable).Scan(&id); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "case_id": caseID, "case_number": caseNumber,
		"subject": p.Subject, "role": role, "entry_date": date, "minutes": minutes, "billable": billable,
	})
}

// listTimeEntries: GET /cases/{caseId}/time — per-case ledger + total.
func (s *server) listTimeEntries(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "ARBITRATOR", "DOCTOR", "NURSE", "ATTORNEY", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	rows, err := s.queryRows(r, `
		SELECT id, subject, role, entry_date::text, minutes, note, billable, created_at
		FROM public.time_entries WHERE tenant=$1 AND case_id=$2
		ORDER BY entry_date DESC, id DESC LIMIT 500`, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var total int64
	_ = s.db.QueryRow(r.Context(),
		`SELECT coalesce(sum(minutes),0) FROM public.time_entries WHERE tenant=$1 AND case_id=$2`,
		tenant, caseID).Scan(&total)
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "total_minutes": total})
}

// timeReport: GET /reports/time?week=YYYY-MM-DD | ?month=YYYY-MM
//   - week: 7 days starting the Monday of the given date — per dispute,
//     with the people who worked it.
//   - month: calendar month — whole team, per dispute, plus per-person
//     totals. This is the billing-review table.
func (s *server) timeReport(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires CASE_MANAGER or FINANCE"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var from, to, label string
	if m := r.URL.Query().Get("month"); m != "" {
		t0, err := time.Parse("2006-01", m)
		if err != nil {
			http.Error(w, `{"error":"month must be YYYY-MM"}`, http.StatusBadRequest)
			return
		}
		from = t0.Format("2006-01-02")
		to = t0.AddDate(0, 1, 0).Format("2006-01-02")
		label = "month " + m
	} else {
		wk := r.URL.Query().Get("week")
		t0, err := time.Parse("2006-01-02", wk)
		if wk == "" {
			t0 = time.Now()
		} else if err != nil {
			http.Error(w, `{"error":"week must be YYYY-MM-DD (any day in the week)"}`, http.StatusBadRequest)
			return
		}
		monday := t0.AddDate(0, 0, -(int(t0.Weekday())+6)%7) // Monday of t0's week
		from = monday.Format("2006-01-02")
		to = monday.AddDate(0, 0, 7).Format("2006-01-02")
		label = "week of " + from
	}
	rows, err := s.queryRows(r, `
		SELECT case_id, subject, role, sum(minutes) AS minutes,
		       sum(minutes) FILTER (WHERE billable) AS billable_minutes
		FROM public.time_entries
		WHERE tenant=$1 AND entry_date >= $2::date AND entry_date < $3::date
		GROUP BY case_id, subject, role`, tenant, from, to)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// Resolve case numbers in one pass.
	numbers := map[string]string{}
	for _, row := range rows {
		cid, _ := row["case_id"].(string)
		if cid == "" || numbers[cid] != "" {
			continue
		}
		var num string
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
			`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), cid).Scan(&num)
		numbers[cid] = num
	}
	type acc struct {
		CaseID     string `json:"case_id"`
		CaseNumber string `json:"case_number"`
		Minutes    int64  `json:"minutes"`
		Billable   int64  `json:"billable_minutes"`
		People     int    `json:"people"`
	}
	byCase := map[string]*acc{}
	byPerson := map[string]*acc{}
	seen := map[string]map[string]bool{}
	for _, row := range rows {
		cid, _ := row["case_id"].(string)
		subj, _ := row["subject"].(string)
		mins := toInt64(row["minutes"])
		bill := toInt64(row["billable_minutes"])
		if byCase[cid] == nil {
			byCase[cid] = &acc{CaseID: cid, CaseNumber: numbers[cid]}
			seen[cid] = map[string]bool{}
		}
		byCase[cid].Minutes += mins
		byCase[cid].Billable += bill
		if !seen[cid][subj] {
			seen[cid][subj] = true
			byCase[cid].People++
		}
		key := subj + "|" + fmt.Sprint(row["role"])
		if byPerson[key] == nil {
			byPerson[key] = &acc{CaseID: "", CaseNumber: subj}
		}
		byPerson[key].Minutes += mins
		byPerson[key].Billable += bill
	}
	caseList, personList := []*acc{}, []*acc{}
	for _, a := range byCase {
		caseList = append(caseList, a)
	}
	for _, a := range byPerson {
		personList = append(personList, a)
	}
	sort.Slice(caseList, func(i, j int) bool { return caseList[i].Minutes > caseList[j].Minutes })
	sort.Slice(personList, func(i, j int) bool { return personList[i].Minutes > personList[j].Minutes })
	var total, totalBillable int64
	for _, a := range caseList {
		total += a.Minutes
		totalBillable += a.Billable
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"period": label, "from": from, "to": to,
		"by_case": caseList, "by_person": personList,
		"total_minutes": total, "billable_minutes": totalBillable,
	})
}
