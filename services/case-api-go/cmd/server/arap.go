package main

// arap.go — accounts receivable / accounts payable subledger with aging.
//
// AR is DERIVED, never duplicated: open receivables are computed live from
// the two real invoice stores — case-fee invoices (public.invoices net of
// card/check payments) and service-fee invoices (billing_invoices net of
// recorded payments). AP is EXPLICIT: obligations live in public.payables,
// created by workflow hooks (determination award recorded) or by FINANCE,
// settled with method + remittance reference. Both sides post TigerBeetle
// legs (billing.go / here), so the Temporal reconcile_ledger activity can
// prove Postgres == ledger (service_ar_check, ap_check) daily.
//
// Aging: current / 1-30 / 31-60 / 61-90 / 90+ days past the item's basis date
// (invoice due date for AR, payable due date for AP; creation date when no
// due date exists).

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	tb_types "github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

// agingBucket classifies days outstanding (negative = not yet due).
func agingBucket(days int) string {
	switch {
	case days <= 0:
		return "current"
	case days <= 30:
		return "1-30"
	case days <= 60:
		return "31-60"
	case days <= 90:
		return "61-90"
	default:
		return "90+"
	}
}

var arapReadRoles = []string{"CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}
var arapWriteRoles = []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

func asOfDate(r *http.Request) (time.Time, error) {
	if v := r.URL.Query().Get("as_of"); v != "" {
		return time.Parse("2006-01-02", v)
	}
	return time.Now(), nil
}

type arapRow struct {
	Source     string `json:"source"` // case_fee_invoice | service_invoice | payable
	ID         string `json:"id"`
	Number     string `json:"number,omitempty"`
	CaseID     string `json:"case_id,omitempty"`
	Party      string `json:"party"` // counterparty: who owes (AR) / who is owed (AP)
	Kind       string `json:"kind,omitempty"`
	Amount     int64  `json:"amount_cents"`
	Paid       int64  `json:"paid_cents"`
	Due        int64  `json:"due_cents"`
	BasisDate  string `json:"basis_date"` // due date or creation date
	Days       int    `json:"days_outstanding"`
	Bucket     string `json:"bucket"`
}

func withAging(rows []arapRow, asOf time.Time) []arapRow {
	for i := range rows {
		if t, err := time.Parse("2006-01-02", rows[i].BasisDate); err == nil {
			rows[i].Days = int(asOf.Sub(t).Hours() / 24)
			rows[i].Bucket = agingBucket(rows[i].Days)
		}
	}
	return rows
}

func bucketTotals(rows []arapRow) map[string]int64 {
	t := map[string]int64{"current": 0, "1-30": 0, "31-60": 0, "61-90": 0, "90+": 0}
	for _, r := range rows {
		t[r.Bucket] += r.Due
	}
	return t
}

// listReceivables: every open receivable, derived live from invoice stores.
func (s *server) listReceivables(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, arapReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	asOf, err := asOfDate(r)
	if err != nil {
		http.Error(w, `{"error":"as_of must be YYYY-MM-DD"}`, http.StatusBadRequest)
		return
	}
	var rows []arapRow

	// Case-fee invoices (intake INITIAL_FEE, FULL_REVIEW, ...) net of payments.
	ci, err := s.queryRows(r, `
		SELECT i.id, i.invoice_no, i.case_id, i.party, i.kind, i.amount_cents,
		       coalesce((SELECT sum(p.amount_cents) FROM public.payments p
		                 WHERE p.invoice_id=i.id AND p.status='PAID'),0) AS paid,
		       coalesce(i.due_date::text, i.created_at::date::text) AS basis
		FROM public.invoices i
		WHERE i.tenant=$1 AND i.status='OPEN'`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	for _, q := range ci {
		due := toInt64(q["amount_cents"]) - toInt64(q["paid"])
		if due <= 0 {
			continue
		}
		rows = append(rows, arapRow{Source: "case_fee_invoice", ID: fmt.Sprint(q["id"]),
			Number: str(q["invoice_no"]), CaseID: str(q["case_id"]), Party: str(q["party"]),
			Kind: str(q["kind"]), Amount: toInt64(q["amount_cents"]), Paid: toInt64(q["paid"]),
			Due: due, BasisDate: str(q["basis"])})
	}

	// Service-fee invoices (time-ledger billing) net of recorded payments.
	si, err := s.queryRows(r, `
		SELECT b.id, b.invoice_no, b.case_id, b.bill_to_name, b.total_cents,
		       coalesce((SELECT sum(p.amount_cents) FROM public.billing_invoice_payments p
		                 WHERE p.invoice_id=b.id),0) AS paid,
		       coalesce(b.due_date::text, b.created_at::date::text) AS basis
		FROM public.billing_invoices b
		WHERE b.tenant=$1 AND b.status='ISSUED'`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	for _, q := range si {
		due := toInt64(q["total_cents"]) - toInt64(q["paid"])
		if due <= 0 {
			continue
		}
		rows = append(rows, arapRow{Source: "service_invoice", ID: str(q["id"]),
			Number: str(q["invoice_no"]), CaseID: str(q["case_id"]), Party: str(q["bill_to_name"]),
			Kind: "SERVICE_FEE", Amount: toInt64(q["total_cents"]), Paid: toInt64(q["paid"]),
			Due: due, BasisDate: str(q["basis"])})
	}

	rows = withAging(rows, asOf)
	var total int64
	for _, r0 := range rows {
		total += r0.Due
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of": asOf.Format("2006-01-02"), "receivables": rows,
		"total_due_cents": total, "aging": bucketTotals(rows)})
}

// listPayables: obligations, OPEN by default.
func (s *server) listPayables(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, arapReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	asOf, err := asOfDate(r)
	if err != nil {
		http.Error(w, `{"error":"as_of must be YYYY-MM-DD"}`, http.StatusBadRequest)
		return
	}
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "OPEN"
	}
	rows0, err := s.queryRows(r, `
		SELECT id, case_id, payee, source, amount_cents, due_date::text, created_at::date::text AS created,
		       settled_at::text, settle_method, settle_ref, note, created_by
		FROM public.payables WHERE tenant=$1 AND status=$2 ORDER BY created_at DESC LIMIT 500`, tenant, status)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var rows []arapRow
	for _, q := range rows0 {
		basis := str(q["due_date"])
		if basis == "" {
			basis = str(q["created"])
		}
		rows = append(rows, arapRow{Source: "payable", ID: str(q["id"]),
			CaseID: str(q["case_id"]), Party: str(q["payee"]), Kind: str(q["source"]),
			Amount: toInt64(q["amount_cents"]), Due: toInt64(q["amount_cents"]), BasisDate: basis})
	}
	rows = withAging(rows, asOf)
	var total int64
	for _, r0 := range rows {
		total += r0.Due
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of": asOf.Format("2006-01-02"), "status": status, "payables": rows,
		"total_open_cents": total, "aging": bucketTotals(rows)})
}

// postAPLedger: record -> debit program expense (7004) / credit AP (7003);
// settle -> debit AP (7003) / credit operating cash (7002).
func (s *server) postAPLedger(tenant, key string, debitCode, creditCode uint32, amount int64) {
	if s.tb == nil || amount <= 0 {
		return
	}
	ih := sha256.Sum256([]byte("ap:" + key))
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
		slog.Error("ledger leg failed for payable", "key", key, "err", err)
		return
	}
	for _, rr := range res {
		if rr.Result != tb_types.TransferOK && rr.Result != tb_types.TransferExists {
			slog.Error("ledger rejected payable transfer", "key", key, "result", rr.Result.String())
		}
	}
}

var payableSources = map[string]bool{"award": true, "refund": true, "vendor": true, "tax": true, "other": true}

// createPayable records an obligation (FINANCE). Award payables are normally
// created by the setCaseDetails hook (syncAwardPayable); this endpoint covers
// vendor/refund/tax/other obligations and manual award adjustments.
func (s *server) createPayable(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, arapWriteRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		CaseID      string `json:"case_id"`
		Payee       string `json:"payee"`
		Source      string `json:"source"`
		AmountCents int64  `json:"amount_cents"`
		DueDate     string `json:"due_date"`
		Note        string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Payee == "" || in.AmountCents <= 0 {
		http.Error(w, `{"error":"payee and amount_cents>0 required"}`, http.StatusBadRequest)
		return
	}
	if !payableSources[in.Source] {
		http.Error(w, `{"error":"source must be award|refund|vendor|tax|other"}`, http.StatusBadRequest)
		return
	}
	if in.Source == "" {
		in.Source = "other"
	}
	if in.DueDate != "" {
		if _, err := time.Parse("2006-01-02", in.DueDate); err != nil {
			http.Error(w, `{"error":"due_date must be YYYY-MM-DD"}`, http.StatusBadRequest)
			return
		}
	}
	var id string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.payables (tenant, case_id, payee, source, amount_cents, due_date, note, created_by)
		VALUES ($1, NULLIF($2,''), $3, $4, $5, NULLIF($6,'')::date, $7, $8) RETURNING id`,
		tenant, in.CaseID, in.Payee, in.Source, in.AmountCents, in.DueDate, in.Note, p.Subject).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"db — an OPEN award payable may already exist for this case"}`, http.StatusConflict)
		return
	}
	s.postAPLedger(tenant, "record:"+id, acctProgramExpense, acctAccountsPayable, in.AmountCents)
		writeJSON(w, http.StatusOK, map[string]any{"payable_id": id, "status": "OPEN"})
}

// settlePayable marks an obligation paid: method + remittance ref (which the
// recon engine then matches against the bank/accounting feed).
func (s *server) settlePayable(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, arapWriteRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("payableId")
	action := r.PathValue("action") // settle | void
	var in struct {
		Method string `json:"method"` // ach|wire|check
		Ref    string `json:"ref"`
		Date   string `json:"settled_at"`
		Note   string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if action != "settle" && action != "void" {
		http.Error(w, `{"error":"action must be settle|void"}`, http.StatusBadRequest)
		return
	}
	var status, caseID, payee string
	var amount int64
	err := s.db.QueryRow(r.Context(), `
		SELECT status, coalesce(case_id,''), payee, amount_cents FROM public.payables
		WHERE tenant=$1 AND id=$2`, tenant, id).Scan(&status, &caseID, &payee, &amount)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if status != "OPEN" {
		http.Error(w, `{"error":"payable is `+status+`"}`, http.StatusConflict)
		return
	}
	if action == "void" {
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.payables SET status='VOID', note=coalesce($3, note), updated_at=now()
			WHERE id=$1 AND tenant=$2`, id, tenant, nullStr(in.Note))
		// Reverse the expense/AP leg.
		s.postAPLedger(tenant, "void:"+id, acctAccountsPayable, acctProgramExpense, amount)
				writeJSON(w, http.StatusOK, map[string]any{"payable_id": id, "status": "VOID"})
		return
	}
	if in.Method == "" {
		http.Error(w, `{"error":"method required (ach|wire|check)"}`, http.StatusBadRequest)
		return
	}
	settled := time.Now().Format("2006-01-02")
	if in.Date != "" {
		if _, err := time.Parse("2006-01-02", in.Date); err != nil {
			http.Error(w, `{"error":"settled_at must be YYYY-MM-DD"}`, http.StatusBadRequest)
			return
		}
		settled = in.Date
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.payables SET status='SETTLED', settled_at=$3::date, settle_method=$4, settle_ref=$5,
		       updated_at=now() WHERE id=$1 AND tenant=$2 AND status='OPEN'`,
		id, tenant, settled, in.Method, in.Ref)
	// Money out: debit AP, credit operating cash; and into the unified event
	// stream so recon can match the disbursement against the bank feed.
	s.postAPLedger(tenant, "settle:"+id, acctAccountsPayable, acctOperatingCash, amount)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.financial_events (tenant, case_id, kind, direction, amount_cents, party, ref, actor)
		VALUES ($1, NULLIF($2,''), 'PAYOUT', 'OUT', $3, $4, $5, $6)`,
		tenant, caseID, -amount, payee, in.Ref, p.Subject)
		writeJSON(w, http.StatusOK, map[string]any{"payable_id": id, "status": "SETTLED", "amount_cents": amount})
}

// syncAwardPayable keeps an OPEN award payable in step with the case's
// final_amount_awarded_cents detail. Called from setCaseDetails whenever the
// award field is written. Idempotent: one OPEN award payable per case
// (payables_award_uniq partial index); amount updates in place; award cleared
// or zeroed voids the payable.
func (s *server) syncAwardPayable(r *http.Request, tenant, caseID string, details map[string]any, actor string) {
	raw, present := details["final_amount_awarded_cents"]
	if !present {
		return
	}
	amount := toInt64(raw)
	if amount <= 0 {
		// Award cleared: void any open award payable.
		var id string
		var prev int64
		if err := s.db.QueryRow(r.Context(), `
			UPDATE public.payables SET status='VOID', updated_at=now()
			WHERE tenant=$1 AND case_id=$2 AND source='award' AND status='OPEN'
			RETURNING id, amount_cents`, tenant, caseID).Scan(&id, &prev); err == nil {
			s.postAPLedger(tenant, "void:"+id, acctAccountsPayable, acctProgramExpense, prev)
					}
		return
	}
	payee, _ := details["provider_org"].(string)
	if payee == "" {
		payee = "Provider (case award)"
	}
	var id string
	var prev int64
	err := s.db.QueryRow(r.Context(), `
		SELECT id, amount_cents FROM public.payables
		WHERE tenant=$1 AND case_id=$2 AND source='award' AND status='OPEN'`, tenant, caseID).Scan(&id, &prev)
	if err == nil {
		if prev == amount {
			return // already in step
		}
		// Replace: reverse old leg, record new.
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.payables SET amount_cents=$3, payee=$4, updated_at=now()
			WHERE id=$1 AND tenant=$2`, id, tenant, amount, payee)
		s.postAPLedger(tenant, "void:"+id, acctAccountsPayable, acctProgramExpense, prev)
		s.postAPLedger(tenant, "record:"+id+":"+fmt.Sprint(amount), acctProgramExpense, acctAccountsPayable, amount)
				return
	}
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.payables (tenant, case_id, payee, source, amount_cents, note, created_by)
		VALUES ($1,$2,$3,'award',$4,'determination award',$5) RETURNING id`,
		tenant, caseID, payee, amount, actor).Scan(&id); err != nil {
		slog.Error("award payable insert failed", "case", caseID, "err", err)
		return
	}
	s.postAPLedger(tenant, "record:"+id, acctProgramExpense, acctAccountsPayable, amount)
	}

// arapSummary: net working-capital position — open AR vs open AP with aging.
func (s *server) arapSummary(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, arapReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	asOf, err := asOfDate(r)
	if err != nil {
		http.Error(w, `{"error":"as_of must be YYYY-MM-DD"}`, http.StatusBadRequest)
		return
	}
	var arCase, arSvc, ap int64
	_ = s.db.QueryRow(r.Context(), `
		SELECT coalesce(sum(i.amount_cents),0) - coalesce((
			SELECT sum(p.amount_cents) FROM public.payments p
			JOIN public.invoices i2 ON i2.id=p.invoice_id
			WHERE i2.tenant=$1 AND i2.status='OPEN' AND p.status='PAID'),0)
		FROM public.invoices i WHERE i.tenant=$1 AND i.status='OPEN'`, tenant).Scan(&arCase)
	_ = s.db.QueryRow(r.Context(), `
		SELECT coalesce(sum(b.total_cents),0) - coalesce((
			SELECT sum(p.amount_cents) FROM public.billing_invoice_payments p
			JOIN public.billing_invoices b2 ON b2.id=p.invoice_id
			WHERE b2.tenant=$1 AND b2.status='ISSUED'),0)
		FROM public.billing_invoices b WHERE b.tenant=$1 AND b.status='ISSUED'`, tenant).Scan(&arSvc)
	_ = s.db.QueryRow(r.Context(), `
		SELECT coalesce(sum(amount_cents),0) FROM public.payables WHERE tenant=$1 AND status='OPEN'`, tenant).Scan(&ap)
	ar := arCase + arSvc
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of": asOf.Format("2006-01-02"),
		"ar": map[string]any{"case_fee_cents": arCase, "service_fee_cents": arSvc, "total_cents": ar},
		"ap": map[string]any{"open_cents": ap},
		"net_position_cents": ar - ap})
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// caseFinancials: every money record tied to one dispute — case-fee invoices
// and their payments, service-invoice lines and their payment allocations,
// payables, and the raw financial event stream. This is the per-case view of
// the AR/AP subledger: what the payer/provider was billed, what was
// collected, what the platform still owes (awards), and what is outstanding.
func (s *server) caseFinancials(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "DOCTOR", "NURSE", "ARBITRATOR", "ATTORNEY", "FINANCE", "STATE_AUDITOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := r.PathValue("caseId")

	feeInvoices, _ := s.queryRows(r, `
		SELECT i.id, i.invoice_no, i.party, i.kind, i.amount_cents, i.status, i.due_date::text,
		       i.paid_at::text, i.remittance_ref, i.created_at,
		       coalesce((SELECT sum(p.amount_cents) FROM public.payments p
		                 WHERE p.invoice_id=i.id AND p.status='PAID'),0) AS paid_cents
		FROM public.invoices i WHERE i.tenant=$1 AND i.case_id=$2 ORDER BY i.created_at DESC`, tenant, caseID)
	feePayments, _ := s.queryRows(r, `
		SELECT p.id, p.invoice_id, p.provider, p.amount_cents, p.status, p.payer_email, p.payer_name,
		       p.stripe_fee_cents, p.created_at
		FROM public.payments p WHERE p.tenant=$1 AND p.case_id=$2 ORDER BY p.created_at DESC`, tenant, caseID)
	svcLines, _ := s.queryRows(r, `
		SELECT l.id, l.invoice_id, b.invoice_no, b.status AS invoice_status, b.period_start::text, b.period_end::text,
		       l.role, l.minutes, l.rate_cents_per_hour, l.amount_cents, l.description
		FROM public.billing_invoice_lines l
		JOIN public.billing_invoices b ON b.id = l.invoice_id
		WHERE l.tenant=$1 AND l.case_id=$2 AND b.status <> 'VOID' ORDER BY b.created_at DESC, l.id`, tenant, caseID)
	svcPayments, _ := s.queryRows(r, `
		SELECT a.id, a.payment_id, a.invoice_id, b.invoice_no, a.amount_cents,
		       p.method, p.ref, p.received_at::text, p.recorded_by
		FROM public.billing_payment_allocations a
		JOIN public.billing_invoice_payments p ON p.id = a.payment_id
		JOIN public.billing_invoices b ON b.id = a.invoice_id
		WHERE a.tenant=$1 AND a.case_id=$2 ORDER BY a.id DESC`, tenant, caseID)
	payables, _ := s.queryRows(r, `
		SELECT id, payee, source, amount_cents, status, due_date::text, settled_at::text,
		       settle_method, settle_ref, note, created_by, created_at
		FROM public.payables WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC`, tenant, caseID)
	events, _ := s.queryRows(r, `
		SELECT id, kind, direction, amount_cents, party, ref, actor, created_at
		FROM public.financial_events WHERE tenant=$1 AND case_id=$2 ORDER BY id DESC`, tenant, caseID)

	// Rollups.
	var arDue, collected, apOpen, settledOut, svcBilled, svcCollected int64
	for _, q := range feeInvoices {
		if str(q["status"]) == "OPEN" {
			arDue += toInt64(q["amount_cents"]) - toInt64(q["paid_cents"])
		}
	}
	for _, q := range feePayments {
		if str(q["status"]) == "PAID" {
			collected += toInt64(q["amount_cents"])
		}
	}
	for _, q := range svcLines {
		if str(q["invoice_status"]) != "VOID" {
			svcBilled += toInt64(q["amount_cents"])
		}
	}
	for _, q := range svcPayments {
		svcCollected += toInt64(q["amount_cents"])
	}
	for _, q := range payables {
		switch str(q["status"]) {
		case "OPEN":
			apOpen += toInt64(q["amount_cents"])
		case "SETTLED":
			settledOut += toInt64(q["amount_cents"])
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"case_id": caseID,
		"fee_invoices": feeInvoices, "fee_payments": feePayments,
		"service_lines": svcLines, "service_payments": svcPayments,
		"payables": payables, "events": events,
		"totals": map[string]int64{
			"ar_due_cents": arDue, "fee_collected_cents": collected,
			"service_billed_cents": svcBilled, "service_collected_cents": svcCollected,
			"ap_open_cents": apOpen, "settled_out_cents": settledOut,
			"collected_total_cents": collected + svcCollected,
		}})
}
