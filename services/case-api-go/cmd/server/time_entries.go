package main

// Time entries — per-role effort tracking on each dispute. Eligibility
// decides WHETHER work can be billed; this records the work itself: who
// (subject + role), when (entry_date), how long (minutes), on which case.
// Reports roll it up weekly (per dispute) and monthly (whole team, per
// dispute) for billing and staffing review.
//
// Invariants: entries are fully editable by their owner (a timesheet is
// wrong until it's right), but every mutation first snapshots the prior
// version into time_entry_revisions (append-only) — the record itself is
// the audit trail, as everywhere in NG. The role is chosen per entry and
// must be one the principal actually holds.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
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
	// Role is the role being PERFORMED for this entry, chosen per entry (a
	// multi-role user picks per entry, e.g. coding one hour and QA the
	// next). It must be a role the principal actually holds.
	Role string `json:"role"`
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
	// Program time rules (config.time): FL requires decimal quarter hours —
	// 0.25/0.5/0.75/1.0…, never odd minutes (contract field criteria).
	if cfg := s.loadProgram(r, tenant); cfg != nil && cfg.Time.QuarterHours && minutes%15 != 0 {
		http.Error(w, `{"error":"time must be recorded in 0.25-hour (15-minute) increments — e.g. 0.25, 0.5, 0.75, 1.0"}`, http.StatusUnprocessableEntity)
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
	if want := strings.ToUpper(strings.TrimSpace(in.Role)); want != "" {
		if !hasRole(p, want) {
			http.Error(w, fmt.Sprintf(`{"error":"role %q is not one you hold — the entry role is chosen per entry but must be a role assigned to you"}`, strings.TrimSpace(in.Role)), http.StatusUnprocessableEntity)
			return
		}
		role = want
	}
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
	q := r.URL.Query()
	from, to, label, perr := timeReportPeriod(q.Get("week"), q.Get("month"), q.Get("start"), q.Get("end"))
	if perr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, perr.Error()), http.StatusBadRequest)
		return
	}
	resp := s.buildTimeReport(r, tenant, from, to, label, hasAnyRole(p, timeRateViewRoles...))
	if resp == nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// timeReportAcc is one aggregation row — per dispute or per person. Amount
// is attached only for rate-viewing roles; nil otherwise — money never
// leaks to plain viewers.
type timeReportAcc struct {
	CaseID     string `json:"case_id"`
	CaseNumber string `json:"case_number"`
	Minutes    int64  `json:"minutes"`
	Billable   int64  `json:"billable_minutes"`
	People     int    `json:"people"`
	Amount     *int64 `json:"amount_cents,omitempty"`
	Unrated    *int64 `json:"unrated_billable_minutes,omitempty"`
}

// buildTimeReport aggregates everyone's timesheet hours over [from, to) —
// per dispute and per person — with role-rated dollar amounts when the
// caller may see money. Shared verbatim by the on-screen report and the
// email-out path; returns nil on a database error.
func (s *server) buildTimeReport(r *http.Request, tenant, from, to, label string, showMoney bool) map[string]any {
	rows, err := s.queryRows(r, `
		SELECT case_id, subject, role, sum(minutes) AS minutes,
		       sum(minutes) FILTER (WHERE billable) AS billable_minutes
		FROM public.time_entries
		WHERE tenant=$1 AND entry_date >= $2::date AND entry_date < $3::date
		GROUP BY case_id, subject, role`, tenant, from, to)
	if err != nil {
		return nil
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
	rates := map[string]int64{}
	if showMoney {
		rates = s.timeRateMap(r, tenant)
	}
	byCase := map[string]*timeReportAcc{}
	byPerson := map[string]*timeReportAcc{}
	seen := map[string]map[string]bool{}
	addMoney := func(a *timeReportAcc, role string, bill int64) {
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
			byCase[cid] = &timeReportAcc{CaseID: cid, CaseNumber: numbers[cid]}
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
			byPerson[key] = &timeReportAcc{CaseID: "", CaseNumber: subj}
		}
		byPerson[key].Minutes += mins
		byPerson[key].Billable += bill
		addMoney(byPerson[key], role, bill)
	}
	caseList, personList := []*timeReportAcc{}, []*timeReportAcc{}
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
	return resp
}

// timeReportPeriod resolves week/month/range presets to a [from, to) date
// window plus a display label. Shared by the on-screen report and the
// email-out path so both always cover exactly the same days.
func timeReportPeriod(week, month, start, end string) (from, to, label string, err error) {
	if start != "" || end != "" {
		t0, err0 := time.Parse("2006-01-02", start)
		t1, err1 := time.Parse("2006-01-02", end)
		if err0 != nil || err1 != nil {
			return "", "", "", fmt.Errorf("start and end must both be YYYY-MM-DD")
		}
		if !t1.After(t0) {
			return "", "", "", fmt.Errorf("end must be after start")
		}
		if t1.Sub(t0) > 366*24*time.Hour {
			return "", "", "", fmt.Errorf("range too large (max 366 days)")
		}
		return start, end, start + " .. " + end, nil
	}
	if month != "" {
		t0, e := time.Parse("2006-01", month)
		if e != nil {
			return "", "", "", fmt.Errorf("month must be YYYY-MM")
		}
		return t0.Format("2006-01-02"), t0.AddDate(0, 1, 0).Format("2006-01-02"), "month " + month, nil
	}
	t0, e := time.Parse("2006-01-02", week)
	if week == "" {
		t0 = time.Now()
	} else if e != nil {
		return "", "", "", fmt.Errorf("week must be YYYY-MM-DD (any day in the week)")
	}
	monday := t0.AddDate(0, 0, -(int(t0.Weekday())+6)%7) // Monday of t0's week
	return monday.Format("2006-01-02"), monday.AddDate(0, 0, 7).Format("2006-01-02"), "week of " + monday.Format("2006-01-02"), nil
}

// timeReportSend: POST /time/report/send — CASE_MANAGER/FINANCE/admins
// aggregate the WHOLE team's timesheet hours for a period and email the
// report out (plain summary body + CSV attachment: per person, then per
// dispute, billable split and dollar amounts). NG has no audit_log by
// design: the send is itself recorded as a notification to the sender, so
// the platform keeps a durable record of what went out, to whom, when.
func (s *server) timeReportSend(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, timeRateViewRoles...) {
		http.Error(w, `{"error":"forbidden: requires CASE_MANAGER, FINANCE, or admin"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Week    string   `json:"week"`
		Month   string   `json:"month"`
		Start   string   `json:"start"`
		End     string   `json:"end"`
		Emails  []string `json:"emails"`
		Message string   `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	emails := make([]string, 0, len(in.Emails))
	for _, e := range in.Emails {
		e = strings.TrimSpace(e)
		if e != "" && strings.Contains(e, "@") {
			emails = append(emails, e)
		}
	}
	if len(emails) == 0 {
		http.Error(w, `{"error":"at least one valid recipient email required"}`, http.StatusBadRequest)
		return
	}
	if len(emails) > 20 {
		http.Error(w, `{"error":"at most 20 recipients"}`, http.StatusBadRequest)
		return
	}
	from, to, label, perr := timeReportPeriod(in.Week, in.Month, in.Start, in.End)
	if perr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, perr.Error()), http.StatusBadRequest)
		return
	}
	rpt := s.buildTimeReport(r, tenant, from, to, label, true)

	hrs := func(m int64) string { return fmt.Sprintf("%.2f", float64(m)/60) }
	usd := func(c int64) string { return fmt.Sprintf("$%d.%02d", c/100, c%100) }

	var body strings.Builder
	fmt.Fprintf(&body, "Team timesheet report — %s\nTenant: %s\nGenerated by: %s at %s\n\n",
		label, tenant, p.Subject, time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&body, "Total: %s hours (%s billable)", hrs(toInt64(rpt["total_minutes"])), hrs(toInt64(rpt["billable_minutes"])))
	if amt, ok := rpt["total_amount_cents"]; ok {
		fmt.Fprintf(&body, " — %s billable amount", usd(toInt64(amt)))
	}
	if unrated, ok := rpt["unrated_billable_minutes"]; ok && toInt64(unrated) > 0 {
		fmt.Fprintf(&body, " (%s unrated billable hours)", hrs(toInt64(unrated)))
	}
	body.WriteString("\n\nPer person:\n")
	for _, a := range rpt["by_person"].([]*timeReportAcc) {
		fmt.Fprintf(&body, "  %s: %s h (%s billable)\n", a.CaseNumber, hrs(a.Minutes), hrs(a.Billable))
	}
	body.WriteString("\nPer dispute:\n")
	for _, a := range rpt["by_case"].([]*timeReportAcc) {
		fmt.Fprintf(&body, "  %s: %s h (%s billable, %d people)\n", orDash(a.CaseNumber), hrs(a.Minutes), hrs(a.Billable), a.People)
	}
	if msg := strings.TrimSpace(in.Message); msg != "" {
		fmt.Fprintf(&body, "\nMessage from sender:\n%s\n", truncate(msg, 2000))
	}
	body.WriteString("\nFull detail in the attached CSV.\n")

	var csv strings.Builder
	csv.WriteString("section,who_or_case,role,hours,billable_hours,amount\n")
	for _, a := range rpt["by_person"].([]*timeReportAcc) {
		fmt.Fprintf(&csv, "person,%s,%s,%s,%s,%s\n", csvCell(a.CaseNumber), "", hrs(a.Minutes), hrs(a.Billable), moneyCell(a.Amount))
	}
	for _, a := range rpt["by_case"].([]*timeReportAcc) {
		fmt.Fprintf(&csv, "case,%s,%s,%s,%s,%s\n", csvCell(a.CaseNumber), "", hrs(a.Minutes), hrs(a.Billable), moneyCell(a.Amount))
	}

	subject := fmt.Sprintf("Team timesheet report — %s (%s)", label, strings.ToUpper(tenant))
	fname := fmt.Sprintf("timesheet-%s-%s.csv", strings.ReplaceAll(label, " ", "_"), tenant)
	if err := s.sendMailWithAttachment(emails, nil, subject, body.String(), fname, []byte(csv.String())); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "email failed: "+err.Error()), http.StatusBadGateway)
		return
	}
	s.notify(r, tenant, p.Subject, "TIME_REPORT_SENT",
		fmt.Sprintf("Team timesheet report (%s) emailed to %s", label, strings.Join(emails, ", ")), "#/time")
	writeJSON(w, http.StatusOK, map[string]any{"sent": len(emails), "period": label})
}

func csvCell(v string) string {
	if strings.ContainsAny(v, ",\"\n") {
		return "\"" + strings.ReplaceAll(v, "\"", "\"\"") + "\""
	}
	return v
}

func moneyCell(c *int64) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%d.%02d", *c/100, *c%100)
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
		"FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", "BULK_SUBMITTER", "TPA":
		return true
	}
	return false
}

// ---- Personal timesheet (daily entry screen) -------------------------------
//
// Time data is fully editable by its owner (a timesheet is wrong until it's
// right). NG has no audit_log by design — the record is the audit trail — so
// every mutation first copies the prior version into time_entry_revisions,
// an append-only sibling table. Editability never costs the trail.

// myTimeEntries: GET /time/mine?from&to — the current user's entries across
// all disputes with case numbers, newest first, plus per-day totals.
func (s *server) myTimeEntries(w http.ResponseWriter, r *http.Request) {
	princ := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if to == "" {
		to = time.Now().Format("2006-01-02")
	}
	if from == "" {
		if t, err := time.Parse("2006-01-02", to); err == nil {
			from = t.AddDate(0, 0, -13).Format("2006-01-02")
		} else {
			http.Error(w, `{"error":"to must be YYYY-MM-DD"}`, http.StatusBadRequest)
			return
		}
	}
	rows, err := s.queryRows(r, `
		SELECT t.id, t.case_id, c.case_number, t.role, t.entry_date::text,
		       t.minutes, t.note, t.billable, t.created_at
		FROM public.time_entries t
		JOIN tenant_`+sanitizeTenant(tenant)+`.cases c ON c.id = t.case_id
		WHERE t.tenant=$1 AND t.subject=$2 AND t.entry_date >= $3::date AND t.entry_date <= $4::date
		ORDER BY t.entry_date DESC, t.id DESC LIMIT 1000`, tenant, princ.Subject, from, to)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	dayTotals, _ := s.queryRows(r, `
		SELECT entry_date::text, sum(minutes) AS minutes, count(*) AS entries
		FROM public.time_entries
		WHERE tenant=$1 AND subject=$2 AND entry_date >= $3::date AND entry_date <= $4::date
		GROUP BY entry_date ORDER BY entry_date DESC`, tenant, princ.Subject, from, to)
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": rows, "day_totals": dayTotals, "from": from, "to": to,
	})
}

// myTimeSummary: GET /time/summary — today / this ISO week / this month.
func (s *server) myTimeSummary(w http.ResponseWriter, r *http.Request) {
	princ := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	var daily, weekly, monthly int64
	q := `SELECT coalesce(sum(minutes),0) FROM public.time_entries WHERE tenant=$1 AND subject=$2 AND %s`
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(q, `entry_date = current_date`), tenant, princ.Subject).Scan(&daily)
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(q, `date_trunc('week', entry_date) = date_trunc('week', current_date)`), tenant, princ.Subject).Scan(&weekly)
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(q, `date_trunc('month', entry_date) = date_trunc('month', current_date)`), tenant, princ.Subject).Scan(&monthly)
	writeJSON(w, http.StatusOK, map[string]any{
		"today_minutes": daily, "week_minutes": weekly, "month_minutes": monthly,
	})
}

// loadTimeEntryForEdit: owner may edit; FEDERAL_ADMIN/PLATFORM_ADMIN may edit
// anyone's (billing-review corrections). NG's role set is deliberately lean.
func (s *server) loadTimeEntryForEdit(r *http.Request, tenant string, id int64, p principal) (map[string]any, error) {
	rows, err := s.queryRows(r, `
		SELECT id, subject, case_id, role, entry_date::text, minutes, note, billable
		FROM public.time_entries WHERE tenant=$1 AND id=$2`, tenant, id)
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("not found")
	}
	e := rows[0]
	if fmt.Sprint(e["subject"]) != p.Subject && !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		return nil, fmt.Errorf("forbidden: only the owner or an admin may edit a time entry")
	}
	return e, nil
}

// snapshotTimeRevision preserves the pre-mutation row (record-as-audit).
func (s *server) snapshotTimeRevision(r *http.Request, tenant string, e map[string]any, action string, p principal) {
	bj, _ := json.Marshal(e)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.time_entry_revisions (tenant, entry_id, action, snapshot, changed_by)
		VALUES ($1,$2,$3,$4,$5)`,
		tenant, e["id"], action, bj, p.Subject)
}

// updateTimeEntry: PUT /time/{entryId} — full edit with creation-time
// validation (quarter-hour enforcement included).
func (s *server) updateTimeEntry(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	id, _ := strconv.ParseInt(chi.URLParam(r, "entryId"), 10, 64)
	before, err := s.loadTimeEntryForEdit(r, tenant, id, p)
	if err != nil {
		if err.Error() == "not found" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusForbidden)
		}
		return
	}
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
	if cfg := s.loadProgram(r, tenant); cfg != nil && cfg.Time.QuarterHours && minutes%15 != 0 {
		http.Error(w, `{"error":"time must be recorded in 0.25-hour (15-minute) increments"}`, http.StatusUnprocessableEntity)
		return
	}
	role := fmt.Sprint(before["role"])
	if want := strings.ToUpper(strings.TrimSpace(in.Role)); want != "" {
		if !hasRole(p, want) {
			http.Error(w, fmt.Sprintf(`{"error":"role %q is not one you hold"}`, in.Role), http.StatusUnprocessableEntity)
			return
		}
		role = want
	}
	billable := fmt.Sprint(before["billable"]) == "true"
	if in.Billable != nil {
		billable = *in.Billable
	}
	s.snapshotTimeRevision(r, tenant, before, "UPDATE", p)
	if _, err := s.db.Exec(r.Context(), `
		UPDATE public.time_entries SET entry_date=$1, minutes=$2, role=$3, note=$4, billable=$5
		WHERE tenant=$6 AND id=$7`,
		date, minutes, role, truncate(strings.TrimSpace(in.Note), 1000), billable, tenant, id); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "entry_date": date, "minutes": minutes, "role": role, "billable": billable})
}

// deleteTimeEntry: DELETE /time/{entryId} — removes the row after snapshotting
// it into time_entry_revisions; deletion is never erasure from the record.
func (s *server) deleteTimeEntry(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	id, _ := strconv.ParseInt(chi.URLParam(r, "entryId"), 10, 64)
	before, err := s.loadTimeEntryForEdit(r, tenant, id, p)
	if err != nil {
		if err.Error() == "not found" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusForbidden)
		}
		return
	}
	s.snapshotTimeRevision(r, tenant, before, "DELETE", p)
	if _, err := s.db.Exec(r.Context(), `DELETE FROM public.time_entries WHERE tenant=$1 AND id=$2`, tenant, id); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
