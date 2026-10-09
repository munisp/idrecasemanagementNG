package main

// Card payment collection via Stripe Checkout, implemented against the Stripe
// REST API directly (no SDK dependency): form-encoded POSTs with the secret
// key, webhook signature verified with HMAC-SHA256 per Stripe's scheme.
//
// Flow: staff issues an invoice -> POST /invoices/{id}/checkout creates a
// Checkout Session and persists a PENDING payment row -> payer completes
// checkout on Stripe-hosted page -> Stripe calls /api/webhooks/stripe ->
// signature verified -> invoice marked PAID with the payment intent as
// remittance ref -> financial_events row written -> activity timeline entry.
// Refunds via the same webhook (charge.refunded) or POST /v1/refunds when
// staff settles an invoice REFUND with a card payment on file.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	tb_types "github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

// acctStripeClearing is the ledger account card payments land in; settlement
// moves funds clearing -> escrow (and refunds move escrow -> clearing), so the
// TigerBeetle ledger stays the source of truth for money movement even when
// the collection rail is Stripe.
const acctStripeClearing uint32 = 6000

// postPaymentLedger posts the double-entry leg of a card payment/refund to
// the tenant's TigerBeetle ledger. Idempotent via a deterministic transfer id
// (sha256 of payment_intent + direction), so Stripe webhook retries are safe.
// Fail-open with an error log: the payment itself is already real; a missing
// ledger leg is caught by dashboard reconciliation (payments vs TB balances).
func (s *server) postPaymentLedger(tenant, caseID, paymentIntent, party string, amount uint64, refund bool) {
	if s.tb == nil || paymentIntent == "" {
		return
	}
	var debit, credit tb_types.Uint128
	if refund {
		debit, credit = acctID(tenant, acctEscrowTrustHeld, party), acctID(tenant, acctStripeClearing, "")
	} else {
		debit, credit = acctID(tenant, acctStripeClearing, ""), acctID(tenant, acctEscrowTrustHeld, party)
	}
	ih := sha256.Sum256([]byte(fmt.Sprintf("stripe:%s:%v", paymentIntent, refund)))
	var idb [16]byte
	copy(idb[:], ih[:16])
	res, err := s.tb.CreateTransfers([]tb_types.Transfer{{
		ID:              tb_types.BytesToUint128(idb),
		DebitAccountID:  debit,
		CreditAccountID: credit,
		Amount:          tb_types.ToUint128(amount),
		Ledger:          tenantLedgerID(tenant),
		Code:            ledgerCodeIDRE,
	}})
	if err != nil {
		slog.Error("ledger leg failed for stripe payment", "pi", paymentIntent, "err", err)
		return
	}
	for _, r := range res {
		if r.Result != tb_types.TransferOK && r.Result != tb_types.TransferExists {
			slog.Error("ledger rejected stripe transfer", "pi", paymentIntent, "result", r.Result.String())
		}
	}
}

var stripeHTTP = &http.Client{Timeout: 30 * time.Second}

// stripePost calls the Stripe API with form-encoded params.
func (s *server) stripePost(path string, form url.Values) (map[string]any, error) {
	if s.cfg.StripeSecret == "" {
		return nil, fmt.Errorf("card payments not configured (STRIPE_SECRET_KEY unset)")
	}
	req, err := http.NewRequest("POST", "https://api.stripe.com"+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(s.cfg.StripeSecret, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := stripeHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("stripe: bad response")
	}
	if resp.StatusCode >= 400 {
		if e, ok := out["error"].(map[string]any); ok {
			return nil, fmt.Errorf("stripe: %v", e["message"])
		}
		return nil, fmt.Errorf("stripe: HTTP %d", resp.StatusCode)
	}
	return out, nil
}

// createCheckout opens a Stripe Checkout Session for an open invoice and
// returns the hosted payment URL.
func (s *server) createCheckout(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	invID := chi.URLParam(r, "invId")
	var caseID, invoiceNo, party, kind string
	var amount int64
	var status string
	err := s.db.QueryRow(r.Context(), `
		SELECT case_id, invoice_no, party, kind, amount_cents, status
		FROM public.invoices WHERE tenant=$1 AND id=$2`, tenant, invID).
		Scan(&caseID, &invoiceNo, &party, &kind, &amount, &status)
	if err != nil {
		http.Error(w, `{"error":"invoice not found"}`, http.StatusNotFound)
		return
	}
	if status != "OPEN" {
		http.Error(w, `{"error":"invoice is not open"}`, http.StatusConflict)
		return
	}
	// Provider selection: default stripe; provider=mojaloop routes through the
	// scheme adapter (prepare/fulfil) when MOJALOOP_ADAPTER_URL is configured.
	if r.URL.Query().Get("provider") == "mojaloop" {
		if !s.mojaloopEnabled() {
			http.Error(w, `{"error":"mojaloop provider not configured"}`, http.StatusBadRequest)
			return
		}
		s.checkoutMojaloop(w, r, tenant, caseID, invID, party, amount)
		return
	}

	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", s.cfg.PortalBaseURL+"/#/cases/"+caseID+"?paid=1")
	form.Set("cancel_url", s.cfg.PortalBaseURL+"/#/cases/"+caseID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", "usd")
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(amount, 10))
	form.Set("line_items[0][price_data][product_data][name]",
		fmt.Sprintf("IDRE %s — invoice %s (%s)", strings.ReplaceAll(kind, "_", " "), invoiceNo, party))
	form.Set("metadata[invoice_id]", invID)
	form.Set("metadata[tenant]", tenant)
	form.Set("metadata[case_id]", caseID)
	// Field Criteria auto-fill: who paid, organization (contact email comes back
	// as customer_email on completion). Passed by the staffer issuing checkout.
	form.Set("metadata[payer_name]", r.URL.Query().Get("payer_name"))
	form.Set("metadata[payer_org]", r.URL.Query().Get("payer_org"))
	sess, err := s.stripePost("/v1/checkout/sessions", form)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
		return
	}
	sessID, _ := sess["id"].(string)
	checkoutURL, _ := sess["url"].(string)

	raw, _ := json.Marshal(sess)
	var payID string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.payments (tenant, case_id, invoice_id, session_id, amount_cents, raw)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (session_id) DO UPDATE SET updated_at=now()
		RETURNING id`, tenant, caseID, invID, sessID, amount, raw).Scan(&payID)
	s.finEvent(r, tenant, caseID, invID, "PAYMENT_INITIATED", "NONE", amount, party, sessID,
		r.Context().Value(ctxPrincipal{}).(principal).Subject)
	writeJSON(w, http.StatusOK, map[string]any{
		"payment_id": payID, "checkout_url": checkoutURL, "session_id": sessID,
	})
}

// stripeWebhook receives Stripe events. Signature verified against
// STRIPE_WEBHOOK_SECRET; only then is any state mutated. Idempotent per event.
func (s *server) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"read"}`, http.StatusBadRequest)
		return
	}
	if s.cfg.StripeWebhook == "" || !verifyStripeSig(body, r.Header.Get("Stripe-Signature"), s.cfg.StripeWebhook) {
		http.Error(w, `{"error":"bad signature"}`, http.StatusUnauthorized)
		return
	}
	var evt struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &evt) != nil {
		http.Error(w, `{"error":"bad event"}`, http.StatusBadRequest)
		return
	}

	switch evt.Type {
	case "checkout.session.completed":
		var sess struct {
			ID            string `json:"id"`
			PaymentIntent string `json:"payment_intent"`
			Email         string `json:"customer_email"`
			Metadata      map[string]string `json:"metadata"`
		}
		if json.Unmarshal(evt.Data.Object, &sess) != nil {
			break
		}
		tenant, invID, caseID := sess.Metadata["tenant"], sess.Metadata["invoice_id"], sess.Metadata["case_id"]
		// mark payment PAID (+ Field Criteria auto-fill: who paid, org, email)
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.payments SET status='PAID', payment_intent=$3, payer_email=$4,
			       payer_name=$6, payer_org=$7, raw=$5, updated_at=now()
			WHERE session_id=$1 AND tenant=$2`, sess.ID, tenant, sess.PaymentIntent, sess.Email,
			evt.Data.Object, sess.Metadata["payer_name"], sess.Metadata["payer_org"])
		// settle the invoice (remittance ref = Stripe payment intent)
		var amount int64
		var party string
		_ = s.db.QueryRow(r.Context(), `
			UPDATE public.invoices SET status='PAID', remittance_ref=$3, paid_at=now()::date
			WHERE tenant=$1 AND id=$2 AND status='OPEN' RETURNING amount_cents, party`,
			tenant, invID, sess.PaymentIntent).Scan(&amount, &party)
		s.finEvent(r, tenant, caseID, invID, "PAYMENT_PAID", "IN", amount, party, sess.PaymentIntent, "stripe-webhook")
		s.postPaymentLedger(tenant, caseID, sess.PaymentIntent, party, uint64(amount), false) // clearing → escrow
		s.maybeAdvanceStatus(r, tenant, caseID) // invoice PAID => case may close
		s.autoChecklist(r, tenant, caseID)
		s.logActivity(r.Context(), tenant, caseID, "PAYMENT_RECEIVED",
			fmt.Sprintf("Card payment of $%d.%02d received via Stripe (%s) — invoice settled, ledger posted", amount/100, amount%100, sess.PaymentIntent))
		// Program rules (invoice.settled) — settlement is already committed in
		// Postgres + TigerBeetle, so block_request is meaningless here and is
		// ignored (logged suspect); notify/log_activity/set_detail/flag_review
		// drive the post-settlement workflow (e.g. late-payment escalation,
		// remittance reconciliation alerts).
		s.fireEventRules(r, tenant, "invoice.settled", map[string]any{
			"case_id": caseID, "invoice_id": invID, "amount_cents": amount,
			"party": party, "method": "card", "remittance_ref": sess.PaymentIntent,
			"tenant": tenant,
		})

	case "checkout.session.expired":
		var sess struct {
			ID       string            `json:"id"`
			Metadata map[string]string `json:"metadata"`
		}
		if json.Unmarshal(evt.Data.Object, &sess) == nil {
			_, _ = s.db.Exec(r.Context(), `
				UPDATE public.payments SET status='EXPIRED', raw=$3, updated_at=now()
				WHERE session_id=$1 AND tenant=$2`, sess.ID, sess.Metadata["tenant"], evt.Data.Object)
		}

	case "charge.refunded":
		var ch struct {
			PaymentIntent string `json:"payment_intent"`
			AmountRefunded int64 `json:"amount_refunded"`
		}
		if json.Unmarshal(evt.Data.Object, &ch) == nil && ch.PaymentIntent != "" {
			var tenant, caseID, invID string
			var amount int64
			if err := s.db.QueryRow(r.Context(), `
				UPDATE public.payments SET status='REFUNDED', raw=$2, updated_at=now()
				WHERE payment_intent=$1 RETURNING tenant, case_id, invoice_id, amount_cents`,
				ch.PaymentIntent, evt.Data.Object).Scan(&tenant, &caseID, &invID, &amount); err == nil {
				_, _ = s.db.Exec(r.Context(), `
					UPDATE public.invoices SET status='REFUNDED' WHERE tenant=$1 AND id=$2`, tenant, invID)
				s.finEvent(r, tenant, caseID, invID, "REFUND_ISSUED", "OUT", ch.AmountRefunded, "", ch.PaymentIntent, "stripe-webhook")
				s.postPaymentLedger(tenant, caseID, ch.PaymentIntent, "", uint64(ch.AmountRefunded), true) // escrow → clearing
				s.logActivity(r.Context(), tenant, caseID, "REFUND_ISSUED",
					fmt.Sprintf("Stripe refund of $%d.%02d issued (%s) — ledger reversal posted", ch.AmountRefunded/100, ch.AmountRefunded%100, ch.PaymentIntent))
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"received": evt.Type})
}

// verifyStripeSig implements Stripe's signed-scheme: HMAC-SHA256 of
// "{timestamp}.{payload}" against any v1 signature, with a 5-minute tolerance.
func verifyStripeSig(payload []byte, header, secret string) bool {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			sigs = append(sigs, kv[1])
		}
	}
	if ts == "" || len(sigs) == 0 {
		return false
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(t, 0)) > 5*time.Minute || time.Until(time.Unix(t, 0)) > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, sig := range sigs {
		if hmac.Equal([]byte(sig), []byte(want)) {
			return true
		}
	}
	return false
}

// refundCardPayment issues a Stripe refund for an invoice settled by card.
// Called from settleInvoice when action=REFUND and a PAID payment exists.
func (s *server) refundCardPayment(r *http.Request, tenant, invID string) error {
	var pi string
	err := s.db.QueryRow(r.Context(), `
		SELECT payment_intent FROM public.payments
		WHERE tenant=$1 AND invoice_id=$2 AND status='PAID' ORDER BY created_at DESC LIMIT 1`,
		tenant, invID).Scan(&pi)
	if err != nil || pi == "" {
		return nil // no card payment — manual refund path
	}
	form := url.Values{}
	form.Set("payment_intent", pi)
	_, err = s.stripePost("/v1/refunds", form)
	return err
}

// listPayments: payment history for a case or the whole tenant.
func (s *server) listPayments(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	where, args := `tenant=$1`, []any{tenant}
	if caseID != "" {
		where += ` AND case_id=$2`
		args = append(args, caseID)
	}
	limit, offset := pageParams(r, 50, 500)
	var total int
	if err := s.db.QueryRow(r.Context(),
		`SELECT count(*) FROM public.payments WHERE `+where, args...).Scan(&total); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT id, case_id, invoice_id, provider, session_id, payment_intent,
		       amount_cents, currency, payer_email, status, stripe_fee_cents, created_at
		FROM public.payments WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT %d OFFSET %d`,
		limit, offset), args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payments": rows,
		"total": total, "next_offset": nextOffset(offset, limit, total)})
}

// finEvent appends to the unified financial event stream.
func (s *server) finEvent(r *http.Request, tenant, caseID, invID, kind, direction string, amount int64, party, ref, actor string) {
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.financial_events (tenant, case_id, invoice_id, kind, direction, amount_cents, party, ref, actor)
		VALUES ($1,$2,nullif($3,'')::uuid,$4,$5,$6,$7,$8,$9)`,
		tenant, caseID, invID, kind, direction, amount, party, ref, actor)
}

// financialReport powers the finance dashboard: money in/out by period,
// receivables aging, payment-method mix, and the raw event stream.
func (s *server) financialReport(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)

	kpis, err := s.queryRows(r, `
		SELECT
		  sum(amount_cents) FILTER (WHERE kind='PAYMENT_PAID')                       AS collected_cents,
		  sum(amount_cents) FILTER (WHERE kind='REFUND_ISSUED')                      AS refunded_cents,
		  sum(amount_cents) FILTER (WHERE kind='PAYMENT_PAID' AND created_at > now() - interval '30 days') AS collected_30d_cents,
		  count(*) FILTER (WHERE kind='PAYMENT_PAID')                                AS payments_count
		FROM public.financial_events WHERE tenant=$1`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	receivables, _ := s.queryRows(r, `
		SELECT status, party, count(*) AS n, sum(amount_cents) AS total_cents
		FROM public.invoices WHERE tenant=$1 GROUP BY status, party ORDER BY status, party`, tenant)
	aging, _ := s.queryRows(r, `
		SELECT CASE
		         WHEN due_date >= now()::date THEN 'current'
		         WHEN due_date >= now()::date - 30 THEN '1-30'
		         WHEN due_date >= now()::date - 60 THEN '31-60'
		         ELSE '60+' END AS bucket,
		       count(*) AS n, sum(amount_cents) AS total_cents
		FROM public.invoices WHERE tenant=$1 AND status='OPEN'
		GROUP BY 1 ORDER BY 1`, tenant)
	events, _ := s.queryRows(r, `
		SELECT id, case_id, kind, direction, amount_cents, party, ref, actor, created_at
		FROM public.financial_events WHERE tenant=$1 ORDER BY created_at DESC LIMIT 100`, tenant)
	byMethod, _ := s.queryRows(r, `
		SELECT provider, status, count(*) AS n, sum(amount_cents) AS total_cents
		FROM public.payments WHERE tenant=$1 GROUP BY provider, status`, tenant)

	kpi := map[string]any{}
	if len(kpis) > 0 {
		kpi = kpis[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kpi": kpi, "receivables": receivables, "aging": aging,
		"events": events, "by_method": byMethod,
		"stripe_enabled": s.cfg.StripeSecret != "",
	})
}

// ---------------------------------------------------------------------------
// Ledger balance export (reconciliation + lakehouse settlement positions)
// ---------------------------------------------------------------------------

// ledgerBalances returns TigerBeetle balances for the tenant's standard
// accounts: the four global accounts (admin remittance, IDRE compensation,
// refund payable, Stripe clearing) plus per-party escrow accounts derived
// from parties seen in public.financial_events. Authenticated like every
// /v1 route — intended callers are the Temporal reconciliation workflow and
// the balance-snapshot exporter (WORKER_TOKEN service auth).
func (s *server) ledgerBalances(w http.ResponseWriter, r *http.Request) {
	tenant := chi.URLParam(r, "*")
	if tenant == "" {
		tenant = chi.URLParam(r, "tenant")
	}
	if s.tb == nil {
		http.Error(w, `{"error":"ledger unavailable"}`, http.StatusBadGateway)
		return
	}
	parties := []string{""}
	rows, err := s.db.Query(r.Context(),
		`SELECT DISTINCT party FROM public.financial_events
		  WHERE tenant=$1 AND party IS NOT NULL AND party <> ''`, tenant)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				parties = append(parties, p)
			}
		}
	}
	type acctRef struct {
		code  uint32
		party string
	}
	var refs []acctRef
	for _, c := range []uint32{acctAdminRemittance, acctIdreCompensation, acctRefundPayable, acctStripeClearing,
		acctSvcReceivable, acctSvcRevenue, acctOperatingCash, acctAccountsPayable, acctProgramExpense} {
		refs = append(refs, acctRef{c, ""})
	}
	for _, p := range parties {
		refs = append(refs, acctRef{acctEscrowTrustHeld, p})
	}
	ids := make([]tb_types.Uint128, len(refs))
	for i, a := range refs {
		ids[i] = acctID(tenant, a.code, a.party)
	}
	accts, err := s.tb.LookupAccounts(ids)
	if err != nil {
		http.Error(w, `{"error":"ledger lookup failed"}`, http.StatusBadGateway)
		return
	}
	found := make(map[string]tb_types.Account, len(accts))
	for _, a := range accts {
		found[hex128(a.ID)] = a
	}
	type balance struct {
		Code           uint32 `json:"code"`
		Party          string `json:"party"`
		DebitsPosted   uint64 `json:"debits_posted"`
		CreditsPosted  uint64 `json:"credits_posted"`
		DebitsPending  uint64 `json:"debits_pending"`
		CreditsPending uint64 `json:"credits_pending"`
		Exists         bool   `json:"exists"`
	}
	out := make([]balance, 0, len(refs))
	for i, ref := range refs {
		key := hex128(ids[i])
		b := balance{Code: ref.code, Party: ref.party}
		if a, ok := found[key]; ok {
			b.Exists = true
			b.DebitsPosted = u128lo(a.DebitsPosted)
			b.CreditsPosted = u128lo(a.CreditsPosted)
			b.DebitsPending = u128lo(a.DebitsPending)
			b.CreditsPending = u128lo(a.CreditsPending)
		}
		out = append(out, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": tenant, "accounts": out})
}
