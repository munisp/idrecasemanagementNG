package main

// Mojaloop settlement option (future rails boundary).
//
// Mojaloop itself is NOT deployed (docs/PAYMENTS.md). This file implements
// the platform-side of the boundary if a state ever mandates Mojaloop-based
// settlement: payments with provider='mojaloop' go through an SDK
// scheme-adapter (MOJALOOP_ADAPTER_URL) using Mojaloop's transfer lifecycle —
// prepare -> fulfil | reject — mapped onto TigerBeetle pending -> post | void,
// the same 2-phase escrow semantics as the rest of the ledger.
//
//   prepare: TB PENDING transfer clearing->escrow (funds reserved)
//   fulfil:  TB post of the pending transfer; invoice + payment PAID
//   reject:  TB void of the pending transfer; payment FAILED
//
// Disabled unless MOJALOOP_ADAPTER_URL is set; nothing in the Stripe or
// manual paths changes when it is off.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	tb_types "github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

func (s *server) mojaloopEnabled() bool { return s.cfg.MojaloopAdapter != "" }

// mojaloopPost calls the scheme adapter; the adapter owns DFSP auth + quoting.
func (s *server) mojaloopPost(r *http.Request, path string, body map[string]any) (map[string]any, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.cfg.MojaloopAdapter+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.MojaloopSecret != "" {
		mac := hmac.New(sha256.New, []byte(s.cfg.MojaloopSecret))
		mac.Write(raw)
		req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := stripeHTTP.Do(req) // shared tuned client; no Mojaloop SDK dep
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]any{}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("scheme adapter %s: %s", resp.Status, string(data))
	}
	_ = json.Unmarshal(data, &out)
	return out, nil
}

// checkoutMojaloop prepares a settlement transfer for an invoice through the
// scheme adapter and reserves funds as a TB pending transfer (prepare phase).
func (s *server) checkoutMojaloop(w http.ResponseWriter, r *http.Request, tenant, caseID, invID, party string, amount int64) {
	transferID := hash16("mojaloop", invID, party)
	prep, err := s.mojaloopPost(r, "/transfers", map[string]any{
		"transferId": hex128(transferID),
		"payeeFsp":   "idre-platform", "payerFsp": party,
		"amount":     fmt.Sprintf("%d", amount), "currency": "USD",
		"note":       fmt.Sprintf("invoice %s", invID),
	})
	if err != nil {
		http.Error(w, `{"error":"scheme adapter unreachable"}`, http.StatusBadGateway)
		return
	}
	if s.tb != nil {
		res, err := s.tb.CreateTransfers([]tb_types.Transfer{{
			ID:             transferID,
			DebitAccountID: acctID(tenant, acctStripeClearing, ""),
			CreditAccountID: acctID(tenant, acctEscrowTrustHeld, party),
			Amount:         tb_types.ToUint128(uint64(amount)),
			Ledger:         tenantLedgerID(tenant),
			Code:           ledgerCodeIDRE,
			Flags:          tb_types.TransferFlags{Pending: true}.ToUint16(),
		}})
		if err != nil {
			http.Error(w, `{"error":"ledger unavailable"}`, http.StatusBadGateway)
			return
		}
		for _, rr := range res {
			if rr.Result != tb_types.TransferOK && rr.Result != tb_types.TransferExists {
				http.Error(w, `{"error":"ledger rejected prepare"}`, http.StatusUnprocessableEntity)
				return
			}
		}
	}
	_, err = s.db.Exec(r.Context(),
		`INSERT INTO public.payments (tenant, case_id, invoice_id, provider, session_id, amount_cents, status)
		 VALUES ($1,$2,$3,'mojaloop',$4,$5,'PENDING') ON CONFLICT (session_id) DO NOTHING`,
		tenant, caseID, invID, hex128(transferID), amount)
	if err != nil {
		http.Error(w, `{"error":"payment persist"}`, http.StatusInternalServerError)
		return
	}
	s.finEvent(r, tenant, caseID, invID, "PAYMENT_INITIATED", "NONE", amount, party,
		hex128(transferID), "mojaloop-scheme-adapter")
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": "mojaloop", "transfer_id": hex128(transferID),
		"adapter_response": prep,
	})
}

// mojaloopWebhook receives fulfil/reject notifications from the scheme
// adapter. HMAC-verified; fulfil posts the pending TB transfer and settles
// the invoice, reject voids it. Idempotent per transferId.
func (s *server) mojaloopWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"read"}`, http.StatusBadRequest)
		return
	}
	if s.cfg.MojaloopSecret == "" {
		http.Error(w, `{"error":"mojaloop disabled"}`, http.StatusUnauthorized)
		return
	}
	mac := hmac.New(sha256.New, []byte(s.cfg.MojaloopSecret))
	mac.Write(body)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-Signature"))) {
		http.Error(w, `{"error":"bad signature"}`, http.StatusUnauthorized)
		return
	}
	var evt struct {
		TransferID string `json:"transferId"`
		State      string `json:"state"` // COMMITTED (fulfil) | ABORTED (reject)
	}
	if json.Unmarshal(body, &evt); evt.TransferID == "" {
		http.Error(w, `{"error":"invalid event"}`, http.StatusBadRequest)
		return
	}
	var tenant, caseID, invID string
	var amount int64
	err = s.db.QueryRow(r.Context(),
		`UPDATE public.payments SET status=$2, raw=$3, updated_at=now()
		  WHERE session_id=$1 AND provider='mojaloop' AND status='PENDING'
		  RETURNING tenant, case_id, invoice_id, amount_cents`,
		evt.TransferID,
		map[bool]string{true: "PAID", false: "FAILED"}[evt.State == "COMMITTED"],
		body).Scan(&tenant, &caseID, &invID, &amount)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"}) // unknown/dup transfer
		return
	}
	idBytes, _ := hex.DecodeString(evt.TransferID)
	var idArr [16]byte
	copy(idArr[:], idBytes)
	transferID := tb_types.BytesToUint128(idArr)
	if s.tb != nil {
		flags := tb_types.TransferFlags{PostPendingTransfer: evt.State == "COMMITTED",
			VoidPendingTransfer: evt.State != "COMMITTED"}.ToUint16()
		res, err := s.tb.CreateTransfers([]tb_types.Transfer{{
			ID:              hash16("mojaloop-finalize", evt.TransferID, evt.State),
			PendingID:       transferID,
			DebitAccountID:  acctID(tenant, acctStripeClearing, ""),
			CreditAccountID: acctID(tenant, acctEscrowTrustHeld, ""),
			Amount:          tb_types.ToUint128(uint64(amount)),
			Ledger:          tenantLedgerID(tenant),
			Code:            ledgerCodeIDRE,
			Flags:           flags,
		}})
		if err == nil {
			for _, rr := range res {
				if rr.Result != tb_types.TransferOK && rr.Result != tb_types.TransferExists {
					slog.Error("mojaloop finalize rejected", "transfer", evt.TransferID, "result", rr.Result.String())
				}
			}
		} else {
			slog.Error("mojaloop finalize failed", "transfer", evt.TransferID, "err", err)
		}
	}
	if evt.State == "COMMITTED" {
		s.db.Exec(r.Context(),
			`UPDATE public.invoices SET status='PAID', paid_at=now(), remittance_ref=$2 WHERE id=$1`,
			invID, "mojaloop:"+evt.TransferID)
		s.finEvent(r, tenant, caseID, invID, "PAYMENT_PAID", "IN", amount, "", evt.TransferID, "mojaloop")
		s.logActivity(r.Context(), tenant, caseID, "PAYMENT", "Mojaloop transfer fulfilled ("+evt.TransferID+")")
	} else {
		s.finEvent(r, tenant, caseID, invID, "PAYMENT_FAILED", "NONE", amount, "", evt.TransferID, "mojaloop")
		s.logActivity(r.Context(), tenant, caseID, "PAYMENT", "Mojaloop transfer aborted ("+evt.TransferID+")")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// mojaloopCheckoutRoute dispatches /invoices/{invId}/checkout when the
// requested provider is mojaloop (see createCheckout in payments.go).
var _ = chi.URLParam // keep chi import explicit for route wiring
