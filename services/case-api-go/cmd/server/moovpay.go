package main

// moovpay.go — payment rails built on the moov-io open-source stack (NG)
// (Apache-2.0, offline, no vendor dependency):
//
//   - fed:      ABA routing-number validation + FedACH directory lookup when
//               the directory file is mounted (FED_ACH_DIR_FILE).
//   - ach:      fee-collection WEB debit batches against vault-sealed,
//               authorization-on-file payment accounts; NACHA return-file
//               ingestion that traces R-codes back to payment_file_items.
//   - wire:     Fedwire customer-transfer files for high-value payables.
//   - imagecashletter: X9.37 files assembled from cleared physical checks
//               (MICR fields + vault-unsealed images re-encoded to TIFF).
//   - metro2:   Metro 2 furnishing file for seriously delinquent fee accounts.
//
// Bank account numbers are NEVER stored in plaintext: vault /docs/seal
// encrypts under the tenant data key with AAD bound to the account's object
// key; the DB row holds only the sealed blob and last4. Decryption happens
// only inside file generation, in-memory, per item.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moov-io/ach"
	"github.com/moov-io/fed"
	imagecashletter "github.com/moov-io/imagecashletter"
	"github.com/minio/minio-go/v7"
	"github.com/moov-io/metro2/pkg/file"
	metro2lib "github.com/moov-io/metro2/pkg/lib"
	metro2utils "github.com/moov-io/metro2/pkg/utils"
	"github.com/moov-io/wire"
	"golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// ---------- fed: routing validation + directory ----------

var fedDir struct {
	once sync.Once
	dict *fed.ACHDictionary
}

func fedACHDict() *fed.ACHDictionary {
	fedDir.once.Do(func() {
		path := os.Getenv("FED_ACH_DIR_FILE")
		if path == "" {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		d := fed.NewACHDictionary()
		if err := d.Read(f); err == nil {
			fedDir.dict = d
		}
	})
	return fedDir.dict
}

// routingLookup: checksum validation always; bank name when the FedACH
// directory is mounted. Dictionary absence degrades to checksum-only.
func routingLookup(routing string) (valid bool, bankName string) {
	if !validRouting(routing) {
		return false, ""
	}
	if d := fedACHDict(); d != nil {
		if p := d.RoutingNumberSearchSingle(routing); p != nil {
			bankName = p.CustomerName
		}
	}
	return true, bankName
}

func (s *server) lookupRoutingNumber(w http.ResponseWriter, r *http.Request) {
	rt := r.PathValue("routing")
	ok, name := routingLookup(rt)
	if !ok {
		http.Error(w, `{"error":"invalid ABA routing number (checksum)"}`, http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routing_number": rt, "valid": true, "bank_name": name})
}

// ---------- payment accounts (vault-sealed) ----------

var paymentAccountRoles = []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

func (s *server) createPaymentAccount(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, paymentAccountRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		OwnerType          string `json:"owner_type"` // PARTY|ARBITRATOR|TPA|VENDOR
		OwnerRef           string `json:"owner_ref"`
		Label              string `json:"label"`
		RoutingNumber      string `json:"routing_number"`
		AccountNumber      string `json:"account_number"`
		AccountType        string `json:"account_type"` // CHECKING|SAVINGS
		ACHDebitAuthorized bool   `json:"ach_debit_authorized"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	in.OwnerType = strings.ToUpper(strings.TrimSpace(in.OwnerType))
	switch in.OwnerType {
	case "PARTY", "ARBITRATOR", "TPA", "VENDOR":
	default:
		http.Error(w, `{"error":"owner_type must be PARTY, ARBITRATOR, TPA or VENDOR"}`, http.StatusBadRequest)
		return
	}
	acct := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, in.AccountNumber)
	if len(acct) < 4 || len(acct) > 17 {
		http.Error(w, `{"error":"account_number must be 4-17 digits"}`, http.StatusBadRequest)
		return
	}
	ok, bankName := routingLookup(strings.TrimSpace(in.RoutingNumber))
	if !ok {
		http.Error(w, `{"error":"invalid ABA routing number (checksum)"}`, http.StatusUnprocessableEntity)
		return
	}
	at := strings.ToUpper(strings.TrimSpace(in.AccountType))
	if at == "" {
		at = "CHECKING"
	}
	if at != "CHECKING" && at != "SAVINGS" {
		http.Error(w, `{"error":"account_type must be CHECKING or SAVINGS"}`, http.StatusBadRequest)
		return
	}
	id := newUUID()
	key := "payment-account:" + tenant + ":" + id
	sealed, err := s.vaultSealDoc(r, tenant, key, []byte(acct))
	if err != nil {
		http.Error(w, `{"error":"vault seal failed"}`, http.StatusBadGateway)
		return
	}
	_, err = s.db.Exec(r.Context(), `
		INSERT INTO public.payment_accounts
		  (id, tenant, owner_type, owner_ref, label, routing_number, bank_name,
		   account_sealed, account_last4, account_type, ach_debit_authorized, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		id, tenant, in.OwnerType, strings.TrimSpace(in.OwnerRef), strings.TrimSpace(in.Label),
		in.RoutingNumber, bankName, sealed, acct[len(acct)-4:], at, in.ACHDebitAuthorized, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"persist failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "routing_number": in.RoutingNumber, "bank_name": bankName,
		"account_last4": acct[len(acct)-4:], "account_type": at,
	})
}

func (s *server) listPaymentAccounts(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	// Never SELECT account_sealed here: list views see masked data only.
	rows, _ := s.queryRows(r, `
		SELECT id, owner_type, owner_ref, label, routing_number, bank_name,
		       account_last4, account_type, ach_debit_authorized,
		       ach_authorization_name, ach_authorization_at, created_at
		FROM public.payment_accounts WHERE tenant=$1 ORDER BY created_at DESC LIMIT 500`, tenant)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": rows})
}

func (s *server) deletePaymentAccount(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, paymentAccountRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("accountId")
	res, err := s.db.Exec(r.Context(),
		`DELETE FROM public.payment_accounts WHERE tenant=$1 AND id=$2`, tenant, id)
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "DELETED"})
}

type paymentAccount struct {
	ID, Routing, BankName, AcctType, OwnerRef string
	Number                                    string // decrypted, in-memory only
	DebitAuthorized                           bool
	HasAuthorization                          bool // proof-of-authorization artifact sealed in vault
}

// loadPaymentAccount decrypts a stored account via the vault. Callers use the
// plaintext only for the immediate file build and never log or persist it.
func (s *server) loadPaymentAccount(r *http.Request, tenant, id string) (*paymentAccount, error) {
	var a paymentAccount
	var sealed []byte
	err := s.db.QueryRow(r.Context(), `
		SELECT id, routing_number, bank_name, account_type, owner_ref,
		       account_sealed, ach_debit_authorized, ach_authorization_sealed IS NOT NULL
		FROM public.payment_accounts WHERE tenant=$1 AND id=$2`, tenant, id).
		Scan(&a.ID, &a.Routing, &a.BankName, &a.AcctType, &a.OwnerRef, &sealed, &a.DebitAuthorized, &a.HasAuthorization)
	if err != nil {
		return nil, err
	}
	pt, err := s.vaultOpenDoc(r, tenant, "payment-account:"+tenant+":"+id, sealed)
	if err != nil {
		return nil, fmt.Errorf("vault open failed")
	}
	a.Number = string(pt)
	return &a, nil
}

// ---------- ach: fee-collection debit batches ----------

// createACHDebitBatch: FINANCE collects open fee invoices by ACH debit against
// payment accounts that carry an explicit NACHA authorization flag. Item
// selection is explicit ({invoice_id, payment_account_id} pairs) — money moves
// only for pairs a human with FINANCE authority named.
func (s *server) createACHDebitBatch(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, paymentAccountRoles...) {
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
		EffectiveDate string `json:"effective_date"`
		Items         []struct {
			InvoiceID        string `json:"invoice_id"`
			PaymentAccountID string `json:"payment_account_id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Items) == 0 || len(in.Items) > 500 {
		http.Error(w, `{"error":"items: 1-500 {invoice_id, payment_account_id} pairs required"}`, http.StatusBadRequest)
		return
	}
	eff := time.Now()
	if in.EffectiveDate != "" {
		if t, err := time.Parse("2006-01-02", in.EffectiveDate); err == nil {
			eff = t
		}
	}

	fh := ach.NewFileHeader()
	fh.ImmediateDestination = cfg.ImmediateDestination
	fh.ImmediateOrigin = cfg.ImmediateOrigin
	fh.FileCreationDate = eff.Format("060102")
	fh.FileCreationTime = eff.Format("1504")
	fh.FileIDModifier = "A"
	fh.ImmediateDestinationName = "RECEIVING BANK"
	fh.ImmediateOriginName = cfg.CompanyName
	fh.ReferenceCode = "IDREFEE"
	f := ach.NewFile()
	f.Header = fh

	bh := ach.NewBatchHeader()
	bh.ServiceClassCode = ach.DebitsOnly
	bh.CompanyName = cfg.CompanyName
	bh.CompanyIdentification = cfg.ImmediateOrigin
	bh.StandardEntryClassCode = ach.WEB
	desc := cfg.EntryDescription
	if desc == "" {
		desc = "IDRE FEE"
	}
	bh.CompanyEntryDescription = desc
	bh.EffectiveEntryDate = eff.Format("060102")
	bh.ODFIIdentification = cfg.OriginatingDFI
	batch := ach.NewBatchWEB(bh)

	type itemRef struct{ invoiceID, acctID, trace string }
	var refs []itemRef
	var total int64
	for i, it := range in.Items {
		var amount int64
		var status, party, invoiceNo string
		err := s.db.QueryRow(r.Context(), `
			SELECT amount_cents, status, party, invoice_no
			FROM public.invoices WHERE tenant=$1 AND id=$2`, tenant, it.InvoiceID).
			Scan(&amount, &status, &party, &invoiceNo)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"invoice %s not found"}`, it.InvoiceID), http.StatusNotFound)
			return
		}
		if status != "OPEN" {
			http.Error(w, fmt.Sprintf(`{"error":"invoice %s is %s, not OPEN"}`, invoiceNo, status), http.StatusConflict)
			return
		}
		acct, err := s.loadPaymentAccount(r, tenant, it.PaymentAccountID)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"payment account %s unavailable"}`, it.PaymentAccountID), http.StatusNotFound)
			return
		}
		if !acct.DebitAuthorized {
			http.Error(w, fmt.Sprintf(`{"error":"payment account %s has no ACH debit authorization on file"}`, it.PaymentAccountID), http.StatusConflict)
			return
		}
		// NACHA WEB authorization-retention rule: the flag alone is not
		// enough — the signed authorization artifact must be sealed on file.
		if !acct.HasAuthorization {
			http.Error(w, fmt.Sprintf(`{"error":"payment account %s has no proof-of-authorization artifact — POST /payments/accounts/{id}/authorization first (NACHA WEB retention rule)"}`, it.PaymentAccountID), http.StatusConflict)
			return
		}
		// OFAC gate: strict-liability — a match blocks origination outright.
		if blocked, _, code, msg := s.screenPaymentParty(r, tenant, "PARTY", it.InvoiceID, party); blocked {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), code)
			return
		}
		tcode := ach.CheckingDebit
		if acct.AcctType == "SAVINGS" {
			tcode = ach.SavingsDebit
		}
		entry := ach.NewEntryDetail()
		entry.TransactionCode = tcode
		entry.RDFIIdentification = acct.Routing[:8]
		entry.CheckDigit = acct.Routing[8:9]
		entry.DFIAccountNumber = acct.Number
		entry.Amount = int(amount)
		entry.IdentificationNumber = invoiceNo
		entry.IndividualName = party
		entry.DiscretionaryData = "FEE"
		trace := cfg.OriginatingDFI + numeric(strconv.Itoa(i+1), 7)
		entry.TraceNumber = trace
		batch.AddEntry(entry)
		refs = append(refs, itemRef{it.InvoiceID, it.PaymentAccountID, trace})
		total += amount
	}
	if err := batch.Create(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "batch build: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	f.AddBatch(batch)
	if err := f.Create(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "file build: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	if err := f.Validate(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "NACHA validation: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	var buf bytes.Buffer
	if err := ach.NewWriter(&buf).Write(f); err != nil {
		http.Error(w, `{"error":"file render failed"}`, http.StatusInternalServerError)
		return
	}
	ref := "DEBIT-" + eff.Format("060102") + "-" + numeric(strconv.Itoa(len(refs)), 4)
	var fileID string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.payment_files (tenant, kind, reference, item_count, total_cents, file_body, created_by)
		VALUES ($1,'ACH_DEBIT',$2,$3,$4,$5,$6) RETURNING id`,
		tenant, ref, len(refs), total, buf.String(), p.Subject).Scan(&fileID)
	if err != nil {
		http.Error(w, `{"error":"persist failed (reference collision? pick another effective date)"}`, http.StatusConflict)
		return
	}
	for _, rf := range refs {
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.payment_file_items (tenant, file_id, invoice_id, amount_cents, trace_number, status)
			SELECT $1,$2,$3, amount_cents, $4, 'SENT' FROM public.invoices WHERE id=$3 AND tenant=$1`,
			tenant, fileID, rf.invoiceID, rf.trace)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file_id": fileID, "reference": ref, "entry_count": len(refs),
		"total_cents": total, "filename": ref + ".ach", "file": buf.String(),
	})
}

// ingestACHReturns: FINANCE uploads the bank's NACHA return file. Each entry's
// Addenda99 carries the original trace number; matching items flip to RETURNED
// with the R-code and reason, and the invoice stays open for follow-up.
func (s *server) ingestACHReturns(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, paymentAccountRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil || len(body) < 94 {
		http.Error(w, `{"error":"raw NACHA file body required"}`, http.StatusBadRequest)
		return
	}
	f, err := ach.NewReader(bytes.NewReader(body)).Read()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "NACHA parse: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	type ret struct{ Trace, Code, Reason string }
	var returns []ret
	for _, b := range f.Batches {
		for _, e := range b.GetEntries() {
			if a := e.Addenda99; a != nil {
				returns = append(returns, ret{e.TraceNumber, a.ReturnCode, achReturnReason(a.ReturnCode)})
			}
		}
	}
	if len(returns) == 0 {
		http.Error(w, `{"error":"file parsed but contains no return entries (Addenda99)"}`, http.StatusUnprocessableEntity)
		return
	}
	applied, unmatched := 0, 0
	for _, rt := range returns {
		res, err := s.db.Exec(r.Context(), `
			UPDATE public.payment_file_items
			SET status='RETURNED', return_code=$3, return_reason=$4, updated_at=now()
			WHERE tenant=$1 AND trace_number=$2 AND status <> 'RETURNED'`,
			tenant, rt.Trace, rt.Code, rt.Reason)
		if err == nil && res.RowsAffected() > 0 {
			applied++
		} else {
			unmatched++
		}
	}
	ref := "RETURN-" + time.Now().Format("060102-150405")
	var fileID string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.payment_files (tenant, kind, reference, item_count, status, file_body, detail, created_by)
		VALUES ($1,'ACH_RETURN',$2,$3,'GENERATED',$4,$5,$6) RETURNING id`,
		tenant, ref, len(returns), string(body),
		json.RawMessage(fmt.Sprintf(`{"applied":%d,"unmatched":%d}`, applied, unmatched)), p.Subject).Scan(&fileID)
	if applied > 0 {
		s.notify(r, tenant, "*", "ACH_RETURN",
			fmt.Sprintf("%d ACH return(s) matched to payment items; %d unmatched.", applied, unmatched), "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file_id": fileID, "returns": len(returns), "applied": applied, "unmatched": unmatched})
}

// listPaymentFiles / paymentFileBody: staff visibility over generated and
// ingested payment files (bodies gated to FINANCE/admin).
func (s *server) listPaymentFiles(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	kind := strings.ToUpper(r.URL.Query().Get("kind"))
	q := `
		SELECT f.id, f.kind, f.reference, f.item_count, f.total_cents, f.status, f.created_at, f.detail
		FROM public.payment_files f WHERE f.tenant=$1`
	args := []any{tenant}
	if kind != "" {
		q += ` AND f.kind=$2`
		args = append(args, kind)
	}
	q += ` ORDER BY f.created_at DESC LIMIT 200`
	rows, _ := s.queryRows(r, q, args...)
	writeJSON(w, http.StatusOK, map[string]any{"files": rows})
}

func (s *server) paymentFileItems(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, _ := s.queryRows(r, `
		SELECT i.id, i.invoice_id, i.payable_id, i.check_id, i.amount_cents,
		       i.trace_number, i.status, i.return_code, i.return_reason
		FROM public.payment_file_items i
		JOIN public.payment_files f ON f.id = i.file_id
		WHERE f.tenant=$1 AND f.id=$2 ORDER BY i.id LIMIT 1000`, tenant, r.PathValue("fileId"))
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (s *server) paymentFileBody(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, paymentAccountRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var kind, ref, body string
	if err := s.db.QueryRow(r.Context(), `
		SELECT kind, reference, file_body FROM public.payment_files WHERE tenant=$1 AND id=$2`,
		tenant, r.PathValue("fileId")).Scan(&kind, &ref, &body); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	ext := map[string]string{"ACH_DEBIT": ".ach", "ACH_RETURN": ".ach", "WIRE": ".fed", "ICL": ".x937", "METRO2": ".dat"}[kind]
	writeJSON(w, http.StatusOK, map[string]any{"filename": ref + ext, "file": body})
}

// achReturnReason: NACHA return-code descriptions for the codes a fee
// collection program realistically sees; unknown codes surface as the code.
func achReturnReason(code string) string {
	reasons := map[string]string{
		"R01": "Insufficient funds", "R02": "Account closed",
		"R03": "No account/unable to locate", "R04": "Invalid account number",
		"R05": "Unauthorized debit (consumer)", "R06": "Returned per ODFI request",
		"R07": "Authorization revoked", "R08": "Payment stopped",
		"R09": "Uncollected funds", "R10": "Customer advises unauthorized",
		"R11": "Check truncation entry return", "R12": "Account sold to another DFI",
		"R13": "Invalid ACH routing number", "R16": "Account frozen",
		"R20": "Non-transaction account", "R23": "Credit entry refused by receiver",
		"R29": "Corporate customer advises not authorized",
	}
	if r, ok := reasons[code]; ok {
		return r
	}
	return "Return code " + code
}

// ---------- wire: Fedwire customer transfers ----------

// createWireFile: high-value payables settle by Fedwire instead of ACH. One
// payable per wire file (Fedwire is per-message); the beneficiary's account
// comes from a vault-sealed payment account.
func (s *server) createWireFile(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, paymentAccountRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	cfg := s.bankConfig(r, tenant).Nacha
	if cfg.OriginatingDFI == "" || cfg.CompanyName == "" {
		http.Error(w, `{"error":"bank.nacha not configured (sender identity required for wires)"}`, http.StatusConflict)
		return
	}
	var in struct {
		PayableID        string `json:"payable_id"`
		PaymentAccountID string `json:"payment_account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.PayableID == "" || in.PaymentAccountID == "" {
		http.Error(w, `{"error":"payable_id and payment_account_id required"}`, http.StatusBadRequest)
		return
	}
	var amount int64
	var status, payee string
	err := s.db.QueryRow(r.Context(), `
		SELECT amount_cents, status, payee FROM public.payables WHERE tenant=$1 AND id=$2`,
		tenant, in.PayableID).Scan(&amount, &status, &payee)
	if err != nil {
		http.Error(w, `{"error":"payable not found"}`, http.StatusNotFound)
		return
	}
	if status != "OPEN" {
		http.Error(w, fmt.Sprintf(`{"error":"payable is %s, not OPEN"}`, status), http.StatusConflict)
		return
	}
	// OFAC strict-liability gate: a wire is an irreversible same-day settlement,
	// so a matched payee must stop the file before any instruction is built.
	if blocked, _, code, msg := s.screenPaymentParty(r, tenant, "PAYABLE", in.PayableID, payee); blocked {
		http.Error(w, msg, code)
		return
	}
	acct, err := s.loadPaymentAccount(r, tenant, in.PaymentAccountID)
	if err != nil {
		http.Error(w, `{"error":"payment account unavailable"}`, http.StatusNotFound)
		return
	}

	now := time.Now()
	fwm := wire.FEDWireMessage{}
	fwm.SenderSupplied = &wire.SenderSupplied{
		FormatVersion: "30", UserRequestCorrelation: numeric(in.PayableID, 8),
		TestProductionCode: "P", MessageDuplicationCode: " ",
	}
	fwm.TypeSubType = &wire.TypeSubType{TypeCode: "10", SubTypeCode: "00"}
	fwm.InputMessageAccountabilityData = &wire.InputMessageAccountabilityData{
		InputCycleDate: now.Format("20060102"), InputSource: "IDRE",
		InputSequenceNumber: now.Format("150405"),
	}
	fwm.Amount = &wire.Amount{Amount: numeric(strconv.FormatInt(amount, 10), 12)}
	fwm.SenderDepositoryInstitution = &wire.SenderDepositoryInstitution{
		SenderABANumber: cfg.OriginatingDFI + "0", SenderShortName: cfg.CompanyName,
	}
	fwm.ReceiverDepositoryInstitution = &wire.ReceiverDepositoryInstitution{
		ReceiverABANumber: acct.Routing, ReceiverShortName: acct.BankName,
	}
	fwm.BusinessFunctionCode = &wire.BusinessFunctionCode{BusinessFunctionCode: "CTR"}
	fwm.Beneficiary = &wire.Beneficiary{Personal: wire.Personal{
		IdentificationCode: "D", Identifier: acct.Number, Name: payee,
	}}
	fwm.Originator = &wire.Originator{Personal: wire.Personal{
		IdentificationCode: "D", Identifier: cfg.ImmediateOrigin, Name: cfg.CompanyName,
	}}

	f := wire.NewFile()
	f.FEDWireMessage = fwm
	if err := f.Create(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "wire build: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	if err := f.Validate(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "wire validation: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	var buf bytes.Buffer
	if err := wire.NewWriter(&buf).Write(f); err != nil {
		http.Error(w, `{"error":"wire render failed"}`, http.StatusInternalServerError)
		return
	}
	ref := "WIRE-" + now.Format("060102-150405")
	var fileID string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.payment_files (tenant, kind, reference, item_count, total_cents, file_body, created_by)
		VALUES ($1,'WIRE',$2,1,$3,$4,$5) RETURNING id`, tenant, ref, amount, buf.String(), p.Subject).Scan(&fileID)
	if err != nil {
		http.Error(w, `{"error":"persist failed"}`, http.StatusConflict)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.payment_file_items (tenant, file_id, payable_id, amount_cents, trace_number, status)
		VALUES ($1,$2,$3,$4,$5,'SENT')`, tenant, fileID, in.PayableID, amount, ref)
	writeJSON(w, http.StatusOK, map[string]any{
		"file_id": fileID, "reference": ref, "amount_cents": amount,
		"filename": ref + ".fed", "file": buf.String()})
}

// ---------- imagecashletter: X9.37 from cleared checks ----------

// toTIFF re-encodes a check scan (jpeg/png/webp/gif) to TIFF for X9.37 image
// view records. X9.37 mandates TIFF 6.0 image data.
func toTIFF(img []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(img))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tiff.Encode(&buf, src, &tiff.Options{Compression: tiff.Uncompressed}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// createImageCashLetter: assemble an X9.37 file from CLEARED checks not yet
// batched into a cash letter. MICR data comes from doc-intel's extraction
// (routing/account/check number on the checks row); images are vault-unsealed
// in-memory, converted to TIFF, and never written anywhere unsealed.
func (s *server) createImageCashLetter(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, paymentAccountRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	cfg := s.bankConfig(r, tenant).Nacha
	if cfg.ImmediateDestination == "" || cfg.OriginatingDFI == "" {
		http.Error(w, `{"error":"bank.nacha not configured (destination/ECE routing required for X9.37)"}`, http.StatusConflict)
		return
	}
	rows, err := s.queryRows(r, `
		SELECT c.id, c.object_key, c.routing_number, c.account_number, c.check_number,
		       c.amount_cents, c.payer_name
		FROM public.checks c
		WHERE c.tenant=$1 AND c.status='CLEARED' AND c.routing_number IS NOT NULL
		  AND c.account_number IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM public.payment_file_items i WHERE i.check_id = c.id)
		ORDER BY c.cleared_at LIMIT 200`, tenant)
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"no cleared checks awaiting cash-letter deposit"}`, http.StatusNotFound)
		return
	}
	now := time.Now()
	f := imagecashletter.NewFile()
	fh := imagecashletter.NewFileHeader()
	fh.StandardLevel = "35"
	fh.TestFileIndicator = "P"
	fh.ImmediateDestination = cfg.ImmediateDestination
	fh.ImmediateOrigin = cfg.OriginatingDFI + "0"
	fh.FileCreationDate = now
	fh.FileCreationTime = now
	fh.ResendIndicator = "N"
	fh.ImmediateDestinationName = "RECEIVING BANK"
	fh.ImmediateOriginName = cfg.CompanyName
	f.SetHeader(fh)

	clh := imagecashletter.NewCashLetterHeader()
	clh.CollectionTypeIndicator = "01"
	clh.DestinationRoutingNumber = cfg.ImmediateDestination
	clh.ECEInstitutionRoutingNumber = cfg.OriginatingDFI + "0"
	clh.CashLetterBusinessDate = now
	clh.CashLetterCreationDate = now
	clh.CashLetterCreationTime = now
	clh.RecordTypeIndicator = "I"
	clh.DocumentationTypeIndicator = "G"
	clh.CashLetterID = "IDRE" + now.Format("01021504")
	cl := imagecashletter.NewCashLetter(clh)

	bh := imagecashletter.NewBundleHeader()
	bh.CollectionTypeIndicator = "01"
	bh.DestinationRoutingNumber = cfg.ImmediateDestination
	bh.ECEInstitutionRoutingNumber = cfg.OriginatingDFI + "0"
	bh.BundleBusinessDate = now
	bh.BundleCreationDate = now
	bh.BundleID = "1"
	bh.BundleSequenceNumber = "1"
	bh.CycleNumber = "01"
	bundle := imagecashletter.NewBundle(bh)

	type ref struct{ id string; amount int64 }
	var refs []ref
	var total int64
	for i, row := range rows {
		checkID := fmt.Sprint(row["id"])
		obj, err := s.docs.mc.GetObject(r.Context(), docBucket, fmt.Sprint(row["object_key"]), minio.GetObjectOptions{})
		if err != nil {
			continue
		}
		sealed, err := io.ReadAll(obj)
		obj.Close()
		if err != nil {
			continue
		}
		img, err := s.vaultOpenDoc(r, tenant, fmt.Sprint(row["object_key"]), sealed)
		if err != nil {
			continue
		}
		tiffData, err := toTIFF(img)
		if err != nil {
			continue // unreadable image: check stays eligible for the next run
		}
		rt := numeric(fmt.Sprint(row["routing_number"]), 9)
		amt := toInt64(row["amount_cents"])

		cd := imagecashletter.NewCheckDetail()
		cd.AuxiliaryOnUs = numeric(fmt.Sprint(row["check_number"]), 15)
		cd.ExternalProcessingCode = ""
		cd.PayorBankRoutingNumber = rt[:8]
		cd.PayorBankCheckDigit = rt[8:9]
		cd.OnUs = fmt.Sprintf("%s/%s", fmt.Sprint(row["account_number"]), numeric(fmt.Sprint(row["check_number"]), 10))
		cd.ItemAmount = int(amt)
		cd.EceInstitutionItemSequenceNumber = numeric(strconv.Itoa(i+1), 15)
		cd.DocumentationTypeIndicator = "G"
		cd.ReturnAcceptanceIndicator = "D"
		cd.MICRValidIndicator = 1
		cd.BOFDIndicator = "Y"
		cd.AddendumCount = 0
		cd.CorrectionIndicator = 0
		cd.ArchiveTypeIndicator = "B"

		ivd := imagecashletter.NewImageViewDetail()
		ivd.ImageIndicator = 1
		ivd.ImageCreatorRoutingNumber = cfg.OriginatingDFI + "0"
		ivd.ImageCreatorDate = now
		ivd.ImageViewFormatIndicator = "00" // TIFF 6.0
		ivd.ImageViewCompressionAlgorithm = "00"
		ivd.ImageViewDataSize = strconv.Itoa(len(tiffData))
		ivd.ViewSideIndicator = 0 // front
		ivd.ViewDescriptor = "00" // full view
		ivd.DigitalSignatureIndicator = 0
		cd.AddImageViewDetail(ivd)

		ivdata := imagecashletter.NewImageViewData()
		ivdata.EceInstitutionRoutingNumber = cfg.OriginatingDFI + "0"
		ivdata.BundleBusinessDate = now
		ivdata.CycleNumber = "01"
		ivdata.EceInstitutionItemSequenceNumber = cd.EceInstitutionItemSequenceNumber
		ivdata.LengthImageData = strconv.Itoa(len(tiffData))
		ivdata.ImageData = tiffData
		cd.AddImageViewData(ivdata)

		bundle.AddCheckDetail(cd)
		refs = append(refs, ref{checkID, amt})
		total += amt
	}
	if len(refs) == 0 {
		http.Error(w, `{"error":"no usable check images (unseal/TIFF conversion failed for all)"}`, http.StatusUnprocessableEntity)
		return
	}
	cl.AddBundle(bundle)
	f.AddCashLetter(cl)
	if err := f.Create(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "cash letter build: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	if err := f.Validate(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "X9.37 validation: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	var buf bytes.Buffer
	if err := imagecashletter.NewWriter(&buf).Write(f); err != nil {
		http.Error(w, `{"error":"cash letter render failed"}`, http.StatusInternalServerError)
		return
	}
	iclRef := "ICL-" + now.Format("060102-150405")
	var fileID string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.payment_files (tenant, kind, reference, item_count, total_cents, file_body, created_by)
		VALUES ($1,'ICL',$2,$3,$4,$5,$6) RETURNING id`,
		tenant, iclRef, len(refs), total, buf.String(), p.Subject).Scan(&fileID)
	if err != nil {
		http.Error(w, `{"error":"persist failed"}`, http.StatusConflict)
		return
	}
	for _, rf := range refs {
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.payment_file_items (tenant, file_id, check_id, amount_cents, trace_number, status)
			VALUES ($1,$2,$3,$4,$5,'SENT')`, tenant, fileID, rf.id, rf.amount, iclRef)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file_id": fileID, "reference": iclRef, "check_count": len(refs),
		"total_cents": total, "filename": iclRef + ".x937", "file": buf.String()})
}

// ---------- metro2: delinquent-fee furnishing ----------

// createMetro2File: seriously delinquent fee accounts (OPEN invoices past the
// program's delinquency threshold) rendered as a Metro 2 character-format
// furnishing file. Furnishing is a regulated act (FCRA): generation is
// FINANCE/admin-gated and audit-logged; submission to bureaus happens
// off-platform by the entity's compliance function.
func (s *server) createMetro2File(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, paymentAccountRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		ThresholdDays      int    `json:"threshold_days"`       // default 60
		IdentificationNumber string `json:"identification_number"` // furnisher code, >=5 chars
		ReporterName       string `json:"reporter_name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.ThresholdDays <= 0 {
		in.ThresholdDays = 60
	}
	if len(strings.TrimSpace(in.IdentificationNumber)) < 5 {
		http.Error(w, `{"error":"identification_number (furnisher code, >=5 chars) required"}`, http.StatusBadRequest)
		return
	}
	rows, err := s.queryRows(r, `
		SELECT id, invoice_no, party, amount_cents, due_date
		FROM public.invoices
		WHERE tenant=$1 AND status='OPEN' AND due_date < now()::date - $2
		ORDER BY due_date LIMIT 5000`, tenant, in.ThresholdDays)
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"no invoices past the delinquency threshold"}`, http.StatusNotFound)
		return
	}
	f, err := file.NewFile("character")
	if err != nil {
		http.Error(w, `{"error":"metro2 init failed"}`, http.StatusInternalServerError)
		return
	}
	hdr := metro2lib.NewHeaderRecord()
	if h, ok := hdr.(*metro2lib.HeaderRecord); ok {
		h.RecordDescriptorWord = 426
		h.RecordIdentifier = "HEADER"
		h.CycleIdentifier = time.Now().Format("01")
		h.ActivityDate = metro2utils.Time(time.Now())
		h.DateCreated = metro2utils.Time(time.Now())
	}
	if err := f.SetRecord(hdr); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "header: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	count := 0
	var total int64
	for _, row := range rows {
		base := metro2lib.NewBaseSegment()
		b, ok := base.(*metro2lib.BaseSegment)
		if !ok {
			continue
		}
		amt := toInt64(row["amount_cents"])
		due, _ := time.Parse("2006-01-02", fmt.Sprint(row["due_date"])[:10])
		b.RecordDescriptorWord = 426
		b.ProcessingIndicator = 1
		b.TimeStamp = metro2utils.Time(time.Now())
		b.IdentificationNumber = strings.TrimSpace(in.IdentificationNumber)
		b.ConsumerAccountNumber = fmt.Sprint(row["invoice_no"])
		b.PortfolioType = "O" // open account
		b.AccountType = "48"  // debt collection / other collection
		b.TermsDuration = "LOC"
		b.AccountStatus = "71" // seriously past due / collection
		b.CurrentBalance = int(amt)
		b.AmountPastDue = int(amt)
		b.DateOpened = metro2utils.Time(due)
		b.DateAccountInformation = metro2utils.Time(time.Now())
		// Business debtor: party name goes in the surname field per Metro 2
		// convention for non-natural-person furnishers.
		name := strings.ToUpper(fmt.Sprint(row["party"]))
		if len(name) > 25 {
			name = name[:25]
		}
		b.Surname = name
		b.ECOACode = "X" // business account
		if err := f.AddDataRecord(base); err == nil {
			count++
			total += amt
		}
	}
	if count == 0 {
		http.Error(w, `{"error":"no furnishable records"}`, http.StatusUnprocessableEntity)
		return
	}
	if err := f.Validate(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "Metro 2 validation: "+err.Error()), http.StatusUnprocessableEntity)
		return
	}
	ref := "M2-" + time.Now().Format("060102-150405")
	var fileID string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.payment_files (tenant, kind, reference, item_count, total_cents, file_body, created_by)
		VALUES ($1,'METRO2',$2,$3,$4,$5,$6) RETURNING id`,
		tenant, ref, count, total, f.String(false), p.Subject).Scan(&fileID)
	if err != nil {
		http.Error(w, `{"error":"persist failed"}`, http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file_id": fileID, "reference": ref, "records": count,
		"total_cents": total, "filename": ref + ".dat", "file": f.String(false)})
}


// attachPaymentAuthorization: FINANCE seals the signed ACH authorization
// (NACHA WEB retention) into the vault against an account. Raw body is the
// artifact itself (PDF/scan/text), filename via ?name=. Uploading proof IS
// the authorization: the debit-authorized flag sets here, not by checkbox.
func (s *server) attachPaymentAuthorization(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, paymentAccountRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("accountId")
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil || len(body) < 16 {
		http.Error(w, `{"error":"artifact body required (raw file, max 8 MiB)"}`, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if len(name) > 120 {
		name = name[:120]
	}
	if name == "" {
		name = "authorization.bin"
	}
	sealed, err := s.vaultSealDoc(r, tenant, "payment-account-auth:"+tenant+":"+id, body)
	if err != nil {
		http.Error(w, `{"error":"vault seal failed"}`, http.StatusBadGateway)
		return
	}
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.payment_accounts
		SET ach_authorization_sealed=$3, ach_authorization_name=$4,
		    ach_authorization_at=now(), ach_debit_authorized=true
		WHERE tenant=$1 AND id=$2`, tenant, id, sealed, name)
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// NG: no audit_log — the payment_accounts row (ach_authorization_at) is the record.
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": id, "authorization_name": name, "ach_debit_authorized": true})
}
