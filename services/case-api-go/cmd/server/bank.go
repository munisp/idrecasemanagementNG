// Author: Patrick Munis at newwave
package main

// bank.go — bank connectivity adapters (NG).
//
// Two rails, all tenant-configured via config.bank (migration-guarded like
// fees — the guard trigger covers this subtree):
//
//  1. OUTBOUND NACHA origination: FINANCE generates a PPD credit file from
//     open payables that carry bank destinations (routing/account) — award,
//     refund, and vendor payouts go out as a bank-ready ACH file with
//     per-batch file references; payables link to their batch for tracing.
//  2. BAI2 bank-statement adapter (registered in the recon engine): a daily
//     prior-day statement file reconciles automatically against
//     financial_events via the existing autoMatch machinery.
//
// NG note: no lockbox webhook rail — NG has no check-image intake (checks.go
// is a legacy-service surface). No third-party ACH vendor dependency:
// outbound emits bank-standard NACHA, inbound speaks bank-standard BAI2.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/moov-io/ach"
)

// BankConfig lives at config.bank in public.program_rules.
type BankConfig struct {
	LockboxSecretEnv string      `json:"lockbox_secret_env"` // env var with the webhook HMAC key
	Nacha            NachaConfig `json:"nacha"`
}

// NachaConfig: origination identity for outbound ACH files.
type NachaConfig struct {
	ImmediateDestination string `json:"immediate_destination"` // receiving bank routing (9 digits; space-padded in file)
	ImmediateOrigin      string `json:"immediate_origin"`      // company tax ID (9 digits)
	CompanyName          string `json:"company_name"`
	EntryDescription     string `json:"entry_description"` // e.g. "IDRE PMT" (<=10 chars)
	OriginatingDFI       string `json:"originating_dfi"`   // 8-digit routing prefix of our ODFI
}

func (s *server) bankConfig(r *http.Request, tenant string) BankConfig {
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		return cfg.Bank
	}
	return BankConfig{}
}

// ---------- shared HMAC ----------

// hmacSHA256Hex computes the hex HMAC-SHA256 of body under secret.
func hmacSHA256Hex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyHMACSHA256 constant-time compares a hex HMAC-SHA256 of body.
func verifyHMACSHA256(secret string, body []byte, sigHeader string) bool {
	want := hmacSHA256Hex(secret, body)
	got := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(sigHeader)), "sha256=")
	return hmac.Equal([]byte(want), []byte(got))
}

// ---------- rail 2: outbound NACHA ----------

type nachaDestination struct {
	RoutingNumber string `json:"routing_number"`
	AccountNumber string `json:"account_number"`
	AccountType   string `json:"account_type"` // checking | savings
	Name          string `json:"name"`
}

// validRouting verifies an ABA routing number (9 digits + checksum).
func validRouting(rt string) bool {
	if len(rt) != 9 {
		return false
	}
	d := make([]int, 9)
	for i, c := range rt {
		if c < '0' || c > '9' {
			return false
		}
		d[i] = int(c - '0')
	}
	sum := 3*(d[0]+d[3]+d[6]) + 7*(d[1]+d[4]+d[7]) + (d[2] + d[5] + d[8])
	return sum%10 == 0
}

// NACHA field helpers: every record is exactly 94 characters.
func alpha(s string, n int) string { // left-justified, blank-filled
	s = strings.ToUpper(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ' ' || r == '-' || r == '.' {
			return r
		}
		return -1
	}, s))
	if len(s) > n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}
func numeric(s string, n int) string { // right-justified, zero-filled
	s = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return strings.Repeat("0", n-len(s)) + s
}

// generateNACHA builds a balanced PPD credit-only file (service class 220)
// on the moov-io/ach engine: batch/file construction, control totals, blocking
// and NACHA rule validation all come from the library; the hand-rolled record
// writer it replaced had no validation layer. Pure: unit-testable without a DB.
func generateNACHA(cfg NachaConfig, entries []nachaDestination, amounts []int64, ids []string, eff time.Time, fileIDMod byte) (string, error) {
	if len(entries) == 0 || len(entries) != len(amounts) || len(entries) != len(ids) {
		return "", fmt.Errorf("entries/amounts/ids length mismatch")
	}
	if len(cfg.OriginatingDFI) != 8 {
		return "", fmt.Errorf("originating_dfi must be 8 digits")
	}
	desc := cfg.EntryDescription
	if desc == "" {
		desc = "IDRE PMT"
	}
	fh := ach.NewFileHeader()
	fh.ImmediateDestination = cfg.ImmediateDestination
	fh.ImmediateOrigin = cfg.ImmediateOrigin
	fh.FileCreationDate = eff.Format("060102")
	fh.FileCreationTime = eff.Format("1504")
	fh.FileIDModifier = string(fileIDMod)
	fh.ImmediateDestinationName = "RECEIVING BANK"
	fh.ImmediateOriginName = cfg.CompanyName
	fh.ReferenceCode = "IDREPMT"
	f := ach.NewFile()
	f.Header = fh

	bh := ach.NewBatchHeader()
	bh.ServiceClassCode = ach.CreditsOnly
	bh.CompanyName = cfg.CompanyName
	bh.CompanyIdentification = cfg.ImmediateOrigin
	bh.StandardEntryClassCode = ach.PPD
	bh.CompanyEntryDescription = desc
	bh.CompanyDescriptiveDate = eff.Format("060102")
	bh.EffectiveEntryDate = eff.Format("060102")
	bh.ODFIIdentification = cfg.OriginatingDFI
	batch := ach.NewBatchPPD(bh)

	for i, e := range entries {
		rt := numeric(e.RoutingNumber, 9)
		if !validRouting(rt) {
			return "", fmt.Errorf("entry %d: invalid routing number", i)
		}
		tcode := ach.CheckingCredit
		if strings.EqualFold(e.AccountType, "savings") {
			tcode = ach.SavingsCredit
		}
		entry := ach.NewEntryDetail()
		entry.TransactionCode = tcode
		entry.RDFIIdentification = rt[:8]
		entry.CheckDigit = rt[8:9]
		entry.DFIAccountNumber = e.AccountNumber
		entry.Amount = int(amounts[i])
		entry.IdentificationNumber = ids[i]
		entry.IndividualName = e.Name
		entry.TraceNumber = cfg.OriginatingDFI + numeric(strconv.Itoa(i+1), 7)
		batch.AddEntry(entry)
	}
	if err := batch.Create(); err != nil {
		return "", err
	}
	f.AddBatch(batch)
	if err := f.Create(); err != nil {
		return "", err
	}
	if err := f.Validate(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := ach.NewWriter(&buf).Write(f); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// createNachaPayout: FINANCE generates an ACH file from open payables that
// carry bank destinations; payables link to the batch for tracing. Returns
// JSON with the file body so the portal can save it locally.
func (s *server) createNachaPayout(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	cfg := s.bankConfig(r, tenant).Nacha
	if cfg.OriginatingDFI == "" || cfg.ImmediateDestination == "" || cfg.ImmediateOrigin == "" {
		http.Error(w, `{"error":"bank.nacha not configured for this program (migration-guarded config.bank)"}`, http.StatusConflict)
		return
	}
	var in struct {
		EffectiveDate string `json:"effective_date"` // YYYY-MM-DD; default today
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	eff := time.Now()
	if in.EffectiveDate != "" {
		if t, err := time.Parse("2006-01-02", in.EffectiveDate); err == nil {
			eff = t
		}
	}
	rows, err := s.queryRows(r, `
		SELECT id, party, amount_cents, destination FROM public.payables
		WHERE tenant=$1 AND status='OPEN' AND destination IS NOT NULL AND payout_batch_id IS NULL
		ORDER BY created_at LIMIT 500`, tenant)
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"no open payables with bank destinations awaiting payout"}`, http.StatusNotFound)
		return
	}
	entries := []nachaDestination{}
	amounts := []int64{}
	ids := []string{}
	payableIDs := []string{}
	blockedParties := []map[string]any{}
	for i, row := range rows {
		var dst nachaDestination
		raw, _ := json.Marshal(row["destination"])
		if json.Unmarshal(raw, &dst) != nil || dst.RoutingNumber == "" || dst.AccountNumber == "" {
			continue // skipped: unusable destination — stays open for correction
		}
		if dst.Name == "" {
			dst.Name = fmt.Sprint(row["party"])
		}
		// OFAC gate: strict-liability. A matched party is EXCLUDED from the
		// batch (its payable stays OPEN) and reported — blocking the whole
		// batch would hold up legitimate payments to everyone else.
		partyName := fmt.Sprint(row["party"])
		if blocked, top, code, msg := s.screenPaymentParty(r, tenant, "PARTY", fmt.Sprint(row["id"]), partyName); blocked {
			if code == http.StatusServiceUnavailable {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), code)
				return
			}
			blockedParties = append(blockedParties, map[string]any{
				"payable_id": fmt.Sprint(row["id"]), "party": partyName, "top_score": top})
			continue
		}
		entries = append(entries, dst)
		amounts = append(amounts, toInt64(row["amount_cents"]))
		ids = append(ids, fmt.Sprintf("PAY%04d", i+1))
		payableIDs = append(payableIDs, fmt.Sprint(row["id"]))
	}
	if len(entries) == 0 {
		if len(blockedParties) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": "every payable was blocked by OFAC screening", "blocked": blockedParties})
			return
		}
		http.Error(w, `{"error":"payables found but destinations are incomplete"}`, http.StatusUnprocessableEntity)
		return
	}
	fileRef := eff.Format("0602") + numeric(strconv.Itoa(len(entries)), 4)
	fileBody, err := generateNACHA(cfg, entries, amounts, ids, eff, 'A')
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnprocessableEntity)
		return
	}
	var total int64
	for _, a := range amounts {
		total += a
	}
	var batchID string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.bank_payout_batches (tenant, file_reference, entry_count, total_cents, status, created_by, file_body)
		VALUES ($1,$2,$3,$4,'GENERATED',$5,$6) RETURNING id`,
		tenant, fileRef, len(entries), total, p.Subject, fileBody).Scan(&batchID)
	if err != nil {
		http.Error(w, `{"error":"batch persist failed (file_reference collision? regenerate with a different effective date)"}`, http.StatusConflict)
		return
	}
	for _, pid := range payableIDs {
		_, _ = s.db.Exec(r.Context(),
			`UPDATE public.payables SET payout_batch_id=$3 WHERE tenant=$1 AND id=$2`, tenant, pid, batchID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": batchID, "file_reference": fileRef, "entry_count": len(entries),
		"total_cents": total, "filename": "IDRE-PAYOUT-" + fileRef + ".ach", "file": fileBody,
		"blocked": blockedParties,
	})
}

func (s *server) listPayoutBatches(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FINANCE", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, _ := s.queryRows(r, `
		SELECT id, file_reference, entry_count, total_cents, status, created_at
		FROM public.bank_payout_batches WHERE tenant=$1 ORDER BY created_at DESC LIMIT 100`, tenant)
	writeJSON(w, http.StatusOK, map[string]any{"batches": rows})
}

func (s *server) payoutBatchFile(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var ref, body string
	if err := s.db.QueryRow(r.Context(), `
		SELECT file_reference, file_body FROM public.bank_payout_batches WHERE tenant=$1 AND id=$2`,
		tenant, r.PathValue("batchId")).Scan(&ref, &body); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"filename": "IDRE-PAYOUT-" + ref + ".ach", "file": body})
}
