package main

// billing.go — configurable service-fee invoicing engine.
//
// Invoices are generated from the append-only time ledger (time_entries x
// time_rates) over any parameterized period — week, month, or custom
// start/end range, the same convention as /reports/time. Rates are snapshotted
// onto each line at generation time, so later rate changes never mutate an
// issued invoice. Lifecycle: DRAFT -> APPROVED -> ISSUED -> PAID (or VOID),
// every transition written to billing_invoice_events and mirrored into
// financial_events so the reconciliation engine (recon.go) sees one unified
// money stream.
//
// Per-tenant configurability comes from ProgramConfig.Billing: invoice prefix,
// due days, tax %, currency, consolidation mode, unrated-time policy, minimum
// invoice amount, default bill-to, and memo template.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	tb_types "github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

// BillingConfig is the tenant-tunable block on ProgramConfig.
type BillingConfig struct {
	InvoicePrefix   string  `json:"invoice_prefix"`    // default "SVC"
	DueDays         int     `json:"due_days"`          // default 30
	TaxPct          float64 `json:"tax_pct"`           // default 0
	Currency        string  `json:"currency"`          // default "usd"
	Consolidate     *bool   `json:"consolidate"`       // default true: one invoice per period; false: one per case
	BlockOnUnrated  *bool   `json:"block_on_unrated"`  // default true: refuse to generate while billable time lacks a rate
	MinInvoiceCents int64   `json:"min_invoice_cents"` // per-case mode drops smaller invoices (reported back)
	BillToName      string  `json:"bill_to_name"`
	BillToEmail     string  `json:"bill_to_email"`
	Memo            string  `json:"memo"` // template; {period} is substituted
}

// billingDefaults overlays the tenant block on safe defaults.
func billingDefaults(cfg *ProgramConfig) BillingConfig {
	b := BillingConfig{InvoicePrefix: "SVC", DueDays: 30, Currency: "usd",
		Memo: "Professional review services — {period}"}
	if cfg != nil {
		b = cfg.Billing
		if b.InvoicePrefix == "" {
			b.InvoicePrefix = "SVC"
		}
		if b.DueDays <= 0 {
			b.DueDays = 30
		}
		if b.Currency == "" {
			b.Currency = "usd"
		}
		if b.Memo == "" {
			b.Memo = "Professional review services — {period}"
		}
	}
	return b
}

// periodFromParams parses week= / month= / start=+end= query parameters —
// identical semantics to the time report, so an invoice period always matches
// the report a reviewer just ran. end is exclusive.
func periodFromParams(q url.Values) (from, to, label string, err error) {
	if sd, ed := q.Get("start"), q.Get("end"); sd != "" || ed != "" {
		t0, e0 := time.Parse("2006-01-02", sd)
		t1, e1 := time.Parse("2006-01-02", ed)
		if e0 != nil || e1 != nil {
			return "", "", "", fmt.Errorf("start and end must both be YYYY-MM-DD")
		}
		if !t1.After(t0) {
			return "", "", "", fmt.Errorf("end must be after start")
		}
		if t1.Sub(t0) > 366*24*time.Hour {
			return "", "", "", fmt.Errorf("range too large (max 366 days)")
		}
		return sd, ed, sd + " .. " + ed, nil
	}
	if m := q.Get("month"); m != "" {
		t0, e := time.Parse("2006-01", m)
		if e != nil {
			return "", "", "", fmt.Errorf("month must be YYYY-MM")
		}
		return t0.Format("2006-01-02"), t0.AddDate(0, 1, 0).Format("2006-01-02"), "month " + m, nil
	}
	wk := q.Get("week")
	t0 := time.Now()
	if wk != "" {
		var e error
		t0, e = time.Parse("2006-01-02", wk)
		if e != nil {
			return "", "", "", fmt.Errorf("week must be YYYY-MM-DD (any day in the week)")
		}
	}
	monday := t0.AddDate(0, 0, -(int(t0.Weekday())+6)%7)
	return monday.Format("2006-01-02"), monday.AddDate(0, 0, 7).Format("2006-01-02"), "week of " + monday.Format("2006-01-02"), nil
}

// lineAmount converts minutes at a cents-per-hour rate to cents, half-up.
func lineAmount(minutes, rateCentsPerHour int64) int64 {
	return (minutes*rateCentsPerHour + 60/2) / 60
}

// allocatePayment splits a payment across the invoice's cases proportionally
// to each case's line amounts (largest-remainder so the split sums exactly).
// Every dollar received is thereby tied back to a dispute.
func allocatePayment(total int64, weights []int64) []int64 {
	out := make([]int64, len(weights))
	var wsum int64
	for _, w := range weights {
		wsum += w
	}
	if wsum <= 0 {
		return out
	}
	var assigned int64
	type rem struct {
		i int
		r int64
	}
	rems := make([]rem, len(weights))
	for i, w := range weights {
		exact := total * w
		out[i] = exact / wsum
		assigned += out[i]
		rems[i] = rem{i, exact % wsum}
	}
	// distribute the leftover cents to the largest remainders
	for d := total - assigned; d > 0; d-- {
		best := 0
		for i := 1; i < len(rems); i++ {
			if rems[i].r > rems[best].r {
				best = i
			}
		}
		out[rems[best].i]++
		rems[best].r = 0
	}
	return out
}

var billingViewRoles = []string{"CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

// postSvcLedger posts the double-entry leg of a service-invoice event to the
// tenant's TigerBeetle ledger:
//   issue   -> debit AR (7000)           credit revenue (7001)
//   void    -> debit revenue (7001)      credit AR (7000)
//   payment -> debit operating cash (7002) credit AR (7000)
// Idempotent via deterministic transfer ids; fail-open with an error log —
// the Temporal reconcile_ledger service_ar_check catches any gap.
func (s *server) postSvcLedger(tenant, key string, debitCode, creditCode uint32, amount int64) {
	if s.tb == nil || amount <= 0 {
		return
	}
	ih := sha256.Sum256([]byte("svc:" + key))
	var idb [16]byte
	copy(idb[:], ih[:16])
	res, err := s.tb.CreateTransfers([]tb_types.Transfer{{
		ID:              tb_types.BytesToUint128(idb),
		DebitAccountID:  acctID(tenant, debitCode, ""),
		CreditAccountID: acctID(tenant, creditCode, ""),
		Amount:          tb_types.ToUint128(uint64(amount)),
		Ledger:          tenantLedgerID(tenant),
		Code:            ledgerCodeIDRE,
	}})
	if err != nil {
		slog.Error("ledger leg failed for service invoice", "key", key, "err", err)
		return
	}
	for _, rr := range res {
		if rr.Result != tb_types.TransferOK && rr.Result != tb_types.TransferExists {
			slog.Error("ledger rejected service-invoice transfer", "key", key, "result", rr.Result.String())
		}
	}
}

// generateInvoice builds DRAFT invoice(s) from the time ledger for a period.
func (s *server) generateInvoice(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, billingViewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Week, Month, Start, End string `json:"-"`
		CaseID                  string `json:"case_id"`
		Consolidate             *bool  `json:"consolidate"`
		BillToName              string `json:"bill_to_name"`
		BillToEmail             string `json:"bill_to_email"`
		Memo                    string `json:"memo"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	q := url.Values{}
	for k, v := range map[string]string{"week": r.URL.Query().Get("week"), "month": r.URL.Query().Get("month"),
		"start": r.URL.Query().Get("start"), "end": r.URL.Query().Get("end")} {
		if v != "" {
			q.Set(k, v)
		}
	}
	from, to, label, err := periodFromParams(q)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	cfg := billingDefaults(s.loadProgram(r, tenant))
	consolidate := true
	if cfg.Consolidate != nil {
		consolidate = *cfg.Consolidate
	}
	if in.Consolidate != nil {
		consolidate = *in.Consolidate
	}
	if in.CaseID != "" {
		consolidate = false // a case-scoped invoice is per-case by definition
	}
	blockUnrated := true
	if cfg.BlockOnUnrated != nil {
		blockUnrated = *cfg.BlockOnUnrated
	}

	sql := `
		SELECT case_id, role, sum(minutes) FILTER (WHERE billable) AS billable_minutes
		FROM public.time_entries
		WHERE tenant=$1 AND entry_date >= $2::date AND entry_date < $3::date`
	args := []any{tenant, from, to}
	if in.CaseID != "" {
		sql += ` AND case_id=$4`
		args = append(args, in.CaseID)
	}
	sql += ` GROUP BY case_id, role HAVING sum(minutes) FILTER (WHERE billable) > 0 ORDER BY case_id, role`
	rows, err := s.queryRows(r, sql, args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	rates := s.timeRateMap(r, tenant)

	type line struct {
		CaseID, Role string
		Minutes      int64
		Rate         int64
	}
	byCase := map[string][]line{}
	var unrated []map[string]any
	for _, row := range rows {
		cid, _ := row["case_id"].(string)
		role, _ := row["role"].(string)
		mins := toInt64(row["billable_minutes"])
		rate, ok := rates[role]
		if !ok {
			unrated = append(unrated, map[string]any{"case_id": cid, "role": role, "billable_minutes": mins})
			continue
		}
		byCase[cid] = append(byCase[cid], line{cid, role, mins, rate})
	}
	if len(unrated) > 0 && blockUnrated {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "billable time without a configured role rate — set rates first (or set billing.block_on_unrated=false to exclude it)",
			"unrated": unrated})
		return
	}
	if len(byCase) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"invoices": []any{}, "period": label, "note": "no billable time in period"})
		return
	}

	// Resolve case numbers once for line descriptions.
	caseNums := map[string]string{}
	numRows, _ := s.queryRows(r, `SELECT id, case_number FROM public.cases WHERE tenant=$1`, tenant)
	for _, n := range numRows {
		cid, _ := n["id"].(string)
		caseNums[cid], _ = n["case_number"].(string)
	}

	billTo := func(caseID string) (string, string) {
		name, email := in.BillToName, in.BillToEmail
		if name == "" {
			name = cfg.BillToName
		}
		if email == "" {
			email = cfg.BillToEmail
		}
		if name == "" {
			name = "Program " + tenant
		}
		return name, email
	}

	memo := strings.ReplaceAll(cfg.Memo, "{period}", label)
	if in.Memo != "" {
		memo = in.Memo
	}

	// Group: consolidated = one invoice keyed "" ; per-case = one per case_id.
	type group struct {
		Scope string
		Lines []line
	}
	var groups []group
	if consolidate {
		var all []line
		for _, ls := range byCase {
			all = append(all, ls...)
		}
		groups = append(groups, group{"", all})
	} else {
		for cid, ls := range byCase {
			groups = append(groups, group{cid, ls})
		}
	}

	created := []map[string]any{}
	skipped := []map[string]any{}
	for _, g := range groups {
		var sub int64
		for _, l := range g.Lines {
			sub += lineAmount(l.Minutes, l.Rate)
		}
		if !consolidate && cfg.MinInvoiceCents > 0 && sub < cfg.MinInvoiceCents {
			skipped = append(skipped, map[string]any{"case_id": g.Scope, "subtotal_cents": sub,
				"reason": "below billing.min_invoice_cents"})
			continue
		}
		tax := int64(float64(sub)*cfg.TaxPct/100 + 0.5)
		total := sub + tax
		btName, btEmail := billTo(g.Scope)

		var invoiceID string
		seq, err := s.nextInvoiceSeq(r, tenant)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		invNo := fmt.Sprintf("%s-%s-%d-%04d", cfg.InvoicePrefix, strings.ToUpper(tenant), time.Now().Year(), seq)
		// Idempotency guard: the partial unique index billing_invoices_period_uniq
		// rejects a second active (non-VOID) invoice for the same period+scope;
		// we surface the existing invoice instead of double-billing.
		err = s.db.QueryRow(r.Context(), `
			INSERT INTO public.billing_invoices
			  (tenant, invoice_no, period_start, period_end, case_id, bill_to_name, bill_to_email,
			   status, subtotal_cents, tax_cents, total_cents, currency, memo, created_by)
			VALUES ($1,$2,$3::date,$4::date,NULLIF($5,''),$6,$7,'DRAFT',$8,$9,$10,$11,$12,$13)
			RETURNING id`,
			tenant, invNo, from, to, g.Scope, btName, btEmail, sub, tax, total, cfg.Currency, memo, p.Subject).Scan(&invoiceID)
		if err != nil {
			var existingID, existingNo string
			_ = s.db.QueryRow(r.Context(), `
				SELECT id, invoice_no FROM public.billing_invoices
				WHERE tenant=$1 AND period_start=$2::date AND period_end=$3::date
				  AND coalesce(case_id,'')=$4 AND status <> 'VOID'`,
				tenant, from, to, g.Scope).Scan(&existingID, &existingNo)
			skipped = append(skipped, map[string]any{"case_id": g.Scope, "reason": "active invoice already exists for this period",
				"existing_invoice_id": existingID, "existing_invoice_no": existingNo})
			continue
		}
		for _, l := range g.Lines {
			desc := fmt.Sprintf("%s — %s %.2f h @ $%.2f/h",
				caseNums[l.CaseID], l.Role, float64(l.Minutes)/60, float64(l.Rate)/100)
			if g.Scope == "" {
				desc = fmt.Sprintf("Case %s · %s", caseNums[l.CaseID], desc)
			}
			_, _ = s.db.Exec(r.Context(), `
				INSERT INTO public.billing_invoice_lines
				  (invoice_id, tenant, case_id, case_number, role, minutes, rate_cents_per_hour, amount_cents, description)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				invoiceID, tenant, l.CaseID, caseNums[l.CaseID], l.Role, l.Minutes, l.Rate, lineAmount(l.Minutes, l.Rate), desc)
		}
		s.billingEvent(r, tenant, invoiceID, "GENERATED", p.Subject, map[string]any{"period": label, "total_cents": total})
				created = append(created, map[string]any{"invoice_id": invoiceID, "invoice_no": invNo,
			"case_id": g.Scope, "subtotal_cents": sub, "tax_cents": tax, "total_cents": total, "lines": len(g.Lines)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"period": label, "invoices": created, "skipped": skipped, "excluded_unrated": unrated,
		"status": "DRAFT — approve then issue to bill"})
}

// nextInvoiceSeq allocates a per-tenant, per-year invoice sequence atomically.
func (s *server) nextInvoiceSeq(r *http.Request, tenant string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.billing_sequences (tenant, year, next) VALUES ($1, $2, 1)
		ON CONFLICT (tenant, year) DO UPDATE SET next = public.billing_sequences.next + 1
		RETURNING next - 1`, tenant, time.Now().Year()).Scan(&seq)
	return seq, err
}

// billingEvent appends to the invoice's own event trail.
func (s *server) billingEvent(r *http.Request, tenant, invoiceID, event, actor string, detail map[string]any) {
	d, _ := json.Marshal(detail)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.billing_invoice_events (invoice_id, tenant, event, actor, detail)
		VALUES ($1,$2,$3,$4,$5::jsonb)`, invoiceID, tenant, event, actor, string(d))
}

func (s *server) listBillingInvoices(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, billingViewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	sql := `
		SELECT i.id, i.invoice_no, i.period_start::text, i.period_end::text, i.case_id, i.bill_to_name,
		       i.status, i.subtotal_cents, i.tax_cents, i.total_cents, i.currency, i.due_date::text,
		       i.created_at, count(l.id) AS line_count,
		       coalesce((SELECT sum(amount_cents) FROM public.billing_invoice_payments bp WHERE bp.invoice_id=i.id),0) AS paid_cents
		FROM public.billing_invoices i
		LEFT JOIN public.billing_invoice_lines l ON l.invoice_id = i.id
		WHERE i.tenant=$1`
	args := []any{tenant}
	n := 1
	if st := r.URL.Query().Get("status"); st != "" {
		n++
		sql += fmt.Sprintf(` AND i.status=$%d`, n)
		args = append(args, strings.ToUpper(st))
	}
	if from := r.URL.Query().Get("start"); from != "" {
		n++
		sql += fmt.Sprintf(` AND i.period_end > $%d::date`, n)
		args = append(args, from)
	}
	if to := r.URL.Query().Get("end"); to != "" {
		n++
		sql += fmt.Sprintf(` AND i.period_start < $%d::date`, n)
		args = append(args, to)
	}
	sql += ` GROUP BY i.id ORDER BY i.created_at DESC LIMIT 200`
	rows, err := s.queryRows(r, sql, args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoices": rows})
}

func (s *server) getBillingInvoice(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, billingViewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("invoiceId")
	inv, err := s.queryRows(r, `
		SELECT id, invoice_no, period_start::text, period_end::text, case_id, bill_to_name, bill_to_email,
		       status, subtotal_cents, tax_cents, total_cents, currency, due_date::text, memo, created_by, created_at
		FROM public.billing_invoices WHERE tenant=$1 AND id=$2`, tenant, id)
	if err != nil || len(inv) == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	lines, _ := s.queryRows(r, `
		SELECT case_number, role, minutes, rate_cents_per_hour, amount_cents, description
		FROM public.billing_invoice_lines WHERE invoice_id=$1 ORDER BY case_number, role`, id)
	payments, _ := s.queryRows(r, `
		SELECT amount_cents, method, ref, received_at, recorded_by FROM public.billing_invoice_payments
		WHERE invoice_id=$1 ORDER BY received_at DESC`, id)
	events, _ := s.queryRows(r, `
		SELECT event, actor, detail, created_at FROM public.billing_invoice_events
		WHERE invoice_id=$1 ORDER BY id DESC`, id)
	writeJSON(w, http.StatusOK, map[string]any{
		"invoice": inv[0], "lines": lines, "payments": payments, "events": events})
}

// transitionInvoice handles approve / issue / void with strict state checks.
func (s *server) transitionInvoice(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("invoiceId")
	action := r.PathValue("action")

	type rule struct {
		from  []string
		to    string
		roles []string
	}
	rules := map[string]rule{
		"approve": {[]string{"DRAFT"}, "APPROVED", []string{"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}},
		"issue":   {[]string{"APPROVED"}, "ISSUED", []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}},
		"void":    {[]string{"DRAFT", "APPROVED", "ISSUED"}, "VOID", []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}},
	}
	rl, ok := rules[action]
	if !ok {
		http.Error(w, `{"error":"unknown action"}`, http.StatusBadRequest)
		return
	}
	if !hasAnyRole(p, rl.roles...) {
		http.Error(w, `{"error":"forbidden: requires `+strings.Join(rl.roles, " or ")+`"}`, http.StatusForbidden)
		return
	}
	var status string
	var total int64
	var invNo string
	err := s.db.QueryRow(r.Context(), `
		SELECT status, total_cents, invoice_no FROM public.billing_invoices
		WHERE tenant=$1 AND id=$2`, tenant, id).Scan(&status, &total, &invNo)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	allowed := false
	for _, f := range rl.from {
		if status == f {
			allowed = true
		}
	}
	if !allowed {
		http.Error(w, `{"error":"cannot `+action+` an invoice in status `+status+`"}`, http.StatusConflict)
		return
	}
	var due interface{}
	if action == "issue" {
		cfg := billingDefaults(s.loadProgram(r, tenant))
		due = time.Now().AddDate(0, 0, cfg.DueDays).Format("2006-01-02")
	}
	_, err = s.db.Exec(r.Context(), `
		UPDATE public.billing_invoices SET status=$3,
		       due_date = coalesce($4::date, due_date), updated_at=now()
		WHERE tenant=$1 AND id=$2 AND status=$5`,
		tenant, id, rl.to, due, status)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	event := strings.ToUpper(action) + "D"
	if action == "issue" {
		event = "ISSUED"
		// Mirror into the unified money stream for reconciliation.
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.financial_events (tenant, case_id, kind, direction, amount_cents, party, ref, actor)
			SELECT $1, case_id, 'SVC_INVOICE_ISSUED', 'IN', $2, 'IDRE', $3, $4
			FROM public.billing_invoices WHERE id=$5`, tenant, total, invNo, p.Subject, id)
	}
	if action == "issue" {
		s.postSvcLedger(tenant, "issue:"+id, acctSvcReceivable, acctSvcRevenue, total)
	}
	if action == "void" {
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.financial_events (tenant, case_id, kind, direction, amount_cents, party, ref, actor)
			SELECT $1, case_id, 'SVC_INVOICE_VOIDED', 'NONE', $2, 'IDRE', $3, $4
			FROM public.billing_invoices WHERE id=$5`, tenant, total, invNo, p.Subject, id)
		s.postSvcLedger(tenant, "void:"+id, acctSvcRevenue, acctSvcReceivable, total)
	}
	s.billingEvent(r, tenant, id, event, p.Subject, map[string]any{"from": status, "to": rl.to})
		writeJSON(w, http.StatusOK, map[string]any{"invoice_id": id, "status": rl.to, "due_date": due})
}

// recordInvoicePayment posts a payment against an issued invoice; PAID when covered.
func (s *server) recordInvoicePayment(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires FINANCE"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("invoiceId")
	var in struct {
		AmountCents int64  `json:"amount_cents"`
		Method      string `json:"method"` // ach|wire|check|card
		Ref         string `json:"ref"`    // remittance / bank reference — recon keys on this
		ReceivedAt  string `json:"received_at"`
		// Allocations tie every received dollar to disputes. Optional: when
		// absent, the payment is split proportionally across the invoice's
		// case lines. When present, amounts must sum to amount_cents.
		Allocations []struct {
			CaseID      string `json:"case_id"`
			AmountCents int64  `json:"amount_cents"`
		} `json:"allocations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.AmountCents <= 0 || in.Method == "" {
		http.Error(w, `{"error":"amount_cents>0 and method required"}`, http.StatusBadRequest)
		return
	}
	received := time.Now().Format("2006-01-02")
	if in.ReceivedAt != "" {
		if _, err := time.Parse("2006-01-02", in.ReceivedAt); err != nil {
			http.Error(w, `{"error":"received_at must be YYYY-MM-DD"}`, http.StatusBadRequest)
			return
		}
		received = in.ReceivedAt
	}
	var status string
	var total int64
	var invNo string
	err := s.db.QueryRow(r.Context(), `
		SELECT status, total_cents, invoice_no FROM public.billing_invoices
		WHERE tenant=$1 AND id=$2`, tenant, id).Scan(&status, &total, &invNo)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if status != "ISSUED" && status != "PAID" {
		http.Error(w, `{"error":"payments only against ISSUED invoices"}`, http.StatusConflict)
		return
	}
	var paymentID int64
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.billing_invoice_payments (invoice_id, tenant, amount_cents, method, ref, received_at, recorded_by)
		VALUES ($1,$2,$3,$4,$5,$6::date,$7) RETURNING id`,
		id, tenant, in.AmountCents, in.Method, in.Ref, received, p.Subject).Scan(&paymentID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}

	// Tie the payment to disputes: explicit allocations, else proportional
	// across the invoice's case lines (largest-remainder, sums exactly).
	type caseWeight struct {
		CaseID string
		Weight int64
	}
	weights := []caseWeight{}
	lines, _ := s.queryRows(r, `
		SELECT case_id, sum(amount_cents) AS amt FROM public.billing_invoice_lines
		WHERE invoice_id=$1 GROUP BY case_id ORDER BY case_id`, id)
	for _, l := range lines {
		weights = append(weights, caseWeight{str(l["case_id"]), toInt64(l["amt"])})
	}
	type alloc struct {
		CaseID string
		Amount int64
	}
	var allocs []alloc
	if len(in.Allocations) > 0 {
		var sum int64
		for _, a := range in.Allocations {
			sum += a.AmountCents
		}
		if sum != in.AmountCents {
			http.Error(w, `{"error":"allocations must sum to amount_cents"}`, http.StatusBadRequest)
			return
		}
		valid := map[string]bool{}
		for _, w0 := range weights {
			valid[w0.CaseID] = true
		}
		for _, a := range in.Allocations {
			if !valid[a.CaseID] {
				http.Error(w, `{"error":"case `+a.CaseID+` has no line on this invoice"}`, http.StatusBadRequest)
				return
			}
			allocs = append(allocs, alloc{a.CaseID, a.AmountCents})
		}
	} else {
		ws := make([]int64, len(weights))
		for i, w0 := range weights {
			ws[i] = w0.Weight
		}
		for i, amt := range allocatePayment(in.AmountCents, ws) {
			allocs = append(allocs, alloc{weights[i].CaseID, amt})
		}
	}
	for _, a := range allocs {
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.billing_payment_allocations (payment_id, invoice_id, tenant, case_id, amount_cents)
			VALUES ($1,$2,$3,$4,$5)`, paymentID, id, tenant, a.CaseID, a.Amount)
	}
	var paid int64
	_ = s.db.QueryRow(r.Context(), `
		SELECT coalesce(sum(amount_cents),0) FROM public.billing_invoice_payments WHERE invoice_id=$1`, id).Scan(&paid)
	newStatus := status
	if paid >= total && status != "PAID" {
		newStatus = "PAID"
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.billing_invoices SET status='PAID', updated_at=now() WHERE id=$1`, id)
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.financial_events (tenant, case_id, kind, direction, amount_cents, party, ref, actor)
		SELECT $1, case_id, 'SVC_INVOICE_PAYMENT', 'IN', $2, 'IDRE', $3, $4
		FROM public.billing_invoices WHERE id=$5`, tenant, in.AmountCents, in.Ref, p.Subject, id)
	s.postSvcLedger(tenant, fmt.Sprintf("pay:%s:%s:%d", id, in.Ref, in.AmountCents), acctOperatingCash, acctSvcReceivable, in.AmountCents)
	s.billingEvent(r, tenant, id, "PAYMENT_RECORDED", p.Subject,
		map[string]any{"amount_cents": in.AmountCents, "method": in.Method, "ref": in.Ref, "covered_cents": paid})
		writeJSON(w, http.StatusOK, map[string]any{"invoice_id": id, "status": newStatus, "paid_cents": paid, "total_cents": total})
}

// exportInvoiceCSV renders lines as a CSV accounting systems can import.
func (s *server) exportInvoiceCSV(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, billingViewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("invoiceId")
	inv, err := s.queryRows(r, `
		SELECT invoice_no, status, total_cents FROM public.billing_invoices WHERE tenant=$1 AND id=$2`, tenant, id)
	if err != nil || len(inv) == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	lines, _ := s.queryRows(r, `
		SELECT case_number, role, minutes, rate_cents_per_hour, amount_cents, description
		FROM public.billing_invoice_lines WHERE invoice_id=$1 ORDER BY case_number, role`, id)
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s.csv"`, inv[0]["invoice_no"]))
	fmt.Fprintln(w, "invoice_no,case_number,role,minutes,hours,rate_cents_per_hour,amount_cents,description")
	for _, l := range lines {
		mins := toInt64(l["minutes"])
		fmt.Fprintf(w, "%s,%s,%s,%d,%.2f,%d,%d,%q\n",
			inv[0]["invoice_no"], l["case_number"], l["role"], mins, float64(mins)/60,
			toInt64(l["rate_cents_per_hour"]), toInt64(l["amount_cents"]), l["description"])
	}
}
