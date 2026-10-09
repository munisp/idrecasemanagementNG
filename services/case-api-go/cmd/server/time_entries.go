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
	if sd, ed := r.URL.Query().Get("start"), r.URL.Query().Get("end"); sd != "" || ed != "" {
		// Parameterized range: ?start=YYYY-MM-DD&end=YYYY-MM-DD (end exclusive).
		// Weekly/monthly are presets over this same query path.
		t0, err0 := time.Parse("2006-01-02", sd)
		t1, err1 := time.Parse("2006-01-02", ed)
		if err0 != nil || err1 != nil {
			http.Error(w, `{"error":"start and end must both be YYYY-MM-DD"}`, http.StatusBadRequest)
			return
		}
		if !t1.After(t0) {
			http.Error(w, `{"error":"end must be after start"}`, http.StatusBadRequest)
			return
		}
		if t1.Sub(t0) > 366*24*time.Hour {
			http.Error(w, `{"error":"range too large (max 366 days)"}`, http.StatusBadRequest)
			return
		}
		from, to = sd, ed
		label = sd + " .. " + ed
	} else if m := r.URL.Query().Get("month"); m != "" {
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
		Amount  *int64 `json:"amount_cents,omitempty"`
		Unrated *int64 `json:"unrated_billable_minutes,omitempty"`
	}
	showMoney := hasAnyRole(p, timeRateViewRoles...)
	rates := map[string]int64{}
	if showMoney {
		rates = s.timeRateMap(r, tenant)
	}
	byCase := map[string]*acc{}
	byPerson := map[string]*acc{}
	seen := map[string]map[string]bool{}
	addMoney := func(a *acc, role string, bill int64) {
		if !showMoney || bill == 0 {
			return
		}
		if a.Amount == nil {
			var zero int64
			a.Amount = &zero
			var z2 int64
			a.Unrated = &z2
		}
		if rate, ok := rates[role]; ok {
			*a.Amount += bill * rate / 60
		} else {
			*a.Unrated += bill
		}
	}
	for _, row := range rows {
		cid, _ := row["case_id"].(string)
		subj, _ := row["subject"].(string)
		role, _ := row["role"].(string)
		mins := toInt64(row["minutes"])
		bill := toInt64(row["billable_minutes"])
		if byCase[cid] == nil {
			byCase[cid] = &acc{CaseID: cid, CaseNumber: numbers[cid]}
			seen[cid] = map[string]bool{}
		}
		byCase[cid].Minutes += mins
		byCase[cid].Billable += bill
		addMoney(byCase[cid], role, bill)
		if !seen[cid][subj] {
			seen[cid][subj] = true
			byCase[cid].People++
		}
		key := subj + "|" + role
		if byPerson[key] == nil {
			byPerson[key] = &acc{CaseID: "", CaseNumber: subj}
		}
		byPerson[key].Minutes += mins
		byPerson[key].Billable += bill
		addMoney(byPerson[key], role, bill)
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
	var total, totalBillable, totalAmount, totalUnrated int64
	for _, a := range caseList {
		total += a.Minutes
		totalBillable += a.Billable
		if a.Amount != nil {
			totalAmount += *a.Amount
		}
		if a.Unrated != nil {
			totalUnrated += *a.Unrated
		}
	}
	resp := map[string]any{
		"period": label, "from": from, "to": to,
		"by_case": caseList, "by_person": personList,
		"total_minutes": total, "billable_minutes": totalBillable,
	}
	if showMoney {
		resp["total_amount_cents"] = totalAmount
		resp["unrated_billable_minutes"] = totalUnrated
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- Billable rates (per role, per tenant) ---------------------------------
// Rates are money-confidential: only PM/FINANCE/FEDERAL_ADMIN/PLATFORM_ADMIN
// ever see amounts, and only PM/FEDERAL_ADMIN/PLATFORM_ADMIN may set them.
// timeReport attaches amounts only when the caller holds one of those roles;
// a role with no configured rate contributes hours but no dollars — never a
// guessed rate.

// timeRateManageRoles: who may SET rates. timeRateViewRoles: who may SEE
// dollar amounts (superset: FINANCE reads for billing reconciliation).
var timeRateManageRoles = []string{"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}
var timeRateViewRoles = []string{"CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

// getTimeRates: GET /reports/time/rates
func (s *server) getTimeRates(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, timeRateViewRoles...) {
		http.Error(w, `{"error":"forbidden: rates are visible to CASE_MANAGER/FINANCE/admin only"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, `
		SELECT role, rate_cents_per_hour, updated_by, updated_at
		FROM public.time_rates WHERE tenant=$1 ORDER BY role`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rates": rows, "can_manage": hasAnyRole(p, timeRateManageRoles...),
	})
}

// putTimeRate: PUT /reports/time/rates {"role":"DOCTOR","rate_cents_per_hour":17500}
func (s *server) putTimeRate(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, timeRateManageRoles...) {
		http.Error(w, `{"error":"forbidden: only CASE_MANAGER/FEDERAL_ADMIN/PLATFORM_ADMIN may set rates"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Role             string `json:"role"`
		RateCentsPerHour int64  `json:"rate_cents_per_hour"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	role := strings.ToUpper(strings.TrimSpace(in.Role))
	if !validTimeRole(role) {
		http.Error(w, `{"error":"unknown role"}`, http.StatusBadRequest)
		return
	}
	if in.RateCentsPerHour < 0 || in.RateCentsPerHour > 100_000_00 { // $100k/h sanity ceiling
		http.Error(w, `{"error":"rate out of range"}`, http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.time_rates (tenant, role, rate_cents_per_hour, updated_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant, role) DO UPDATE SET rate_cents_per_hour=EXCLUDED.rate_cents_per_hour,
			updated_by=EXCLUDED.updated_by, updated_at=now()`,
		tenant, role, in.RateCentsPerHour, p.Subject); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": role, "rate_cents_per_hour": in.RateCentsPerHour})
}

// timeRateMap loads the tenant's role → rate table for report enrichment.
func (s *server) timeRateMap(r *http.Request, tenant string) map[string]int64 {
	m := map[string]int64{}
	rows, err := s.queryRows(r, `SELECT role, rate_cents_per_hour FROM public.time_rates WHERE tenant=$1`, tenant)
	if err != nil {
		return m
	}
	for _, row := range rows {
		role, _ := row["role"].(string)
		m[role] = toInt64(row["rate_cents_per_hour"])
	}
	return m
}

// validTimeRole — NG's assignable staff vocabulary (no global role table;
// roles are validated per-tenant at assignment time).
func validTimeRole(role string) bool {
	switch role {
	case "DOCTOR", "NURSE", "CASE_MANAGER", "ARBITRATOR", "ATTORNEY", "FINANCE",
		"FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", "BULK_SUBMITTER":
		return true
	}
	return false
}
