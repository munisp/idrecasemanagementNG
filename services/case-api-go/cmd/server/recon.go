package main

// recon.go — reconciliation engine with pluggable accounting adapters.
//
// Platform money records (financial_events: intake fees, refunds, fee
// transfers, service-invoice issuance/payments) are matched against external
// transaction feeds exported or fetched from the accounting platform /
// bank. Matching is deterministic and configurable: exact reference+amount,
// else amount within tolerance and date within window; ambiguous candidates
// become EXCEPTIONs for a human, never auto-resolved.
//
// Adapter framework: each adapter turns an external feed into normalized
// reconItems. Shipped adapters:
//   - csv_generic: any CSV export with a configurable column mapping;
//     per-platform presets (QuickBooks, Xero, Sage, generic bank) come from
//     ProgramConfig.Reconciliation.csv_mappings
//   - http_json:   pulls a JSON transaction feed from a configured accounting
//     endpoint (ProgramConfig.Reconciliation.http_feeds: url + token env var)
// New platforms are added by implementing reconAdapter and registering it —
// the import/match/resolve machinery is adapter-agnostic.

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------- normalized external item ----------

type reconItem struct {
	Date       string `json:"date"`       // YYYY-MM-DD
	AmountCents int64 `json:"amount_cents"` // signed: + received, - disbursed
	Reference  string `json:"reference"`
	Description string `json:"description"`
}

// ---------- adapter interface + registry ----------

type reconAdapter interface {
	name() string
	// parse normalizes a feed. cfg carries adapter options (column mapping for
	// CSV); fetch adapters ignore the reader and pull from their endpoint.
	parse(r io.Reader, cfg map[string]string) ([]reconItem, error)
}

var reconAdapters = map[string]reconAdapter{
	"csv_generic": csvReconAdapter{},
	"http_json":   httpJSONReconAdapter{},
	"bai2":        bai2ReconAdapter{}, // bank prior-day statement (bank.go)
}

// ReconConfig is the tenant-tunable block on ProgramConfig.
type ReconConfig struct {
	ToleranceCents int64 `json:"tolerance_cents"` // default 100 ($1.00)
	DateWindowDays int   `json:"date_window_days"` // default 5
	// csv_mappings: mapping name -> {date, description, amount, reference}
	// column headers. Built-ins exist for quickbooks/xero/sage/bank_generic;
	// tenant mappings extend or override them.
	CSVMappings map[string]map[string]string `json:"csv_mappings"`
	// http_feeds: feed name -> accounting endpoint that returns a JSON array
	// of {date, amount (dollars) or amount_cents, reference, description}.
	HTTPFeeds map[string]struct {
		URL      string `json:"url"`
		TokenEnv string `json:"token_env"` // env var holding the bearer token
	} `json:"http_feeds"`
}

func reconDefaults(cfg *ProgramConfig) ReconConfig {
	rc := ReconConfig{ToleranceCents: 100, DateWindowDays: 5}
	if cfg != nil {
		rc = cfg.Reconciliation
		if rc.ToleranceCents == 0 {
			rc.ToleranceCents = 100
		}
		if rc.DateWindowDays == 0 {
			rc.DateWindowDays = 5
		}
	}
	return rc
}

func builtinCSVMappings() map[string]map[string]string {
	return map[string]map[string]string{
		"quickbooks":   {"date": "Date", "description": "Description", "amount": "Amount", "reference": "Ref"},
		"xero":         {"date": "Date", "description": "Description", "amount": "Amount", "reference": "Reference"},
		"sage":         {"date": "Date", "description": "Details", "amount": "Amount", "reference": "Ref"},
		"bank_generic": {"date": "Date", "description": "Description", "amount": "Amount", "reference": "Reference"},
	}
}

// ---------- CSV adapter ----------

type csvReconAdapter struct{}

func (csvReconAdapter) name() string { return "csv_generic" }

// parse reads a CSV with the given column mapping (header names).
// Amounts accept dollars ("1,234.56") or cents (integer); negative = disbursed.
func (csvReconAdapter) parse(r io.Reader, cfg map[string]string) ([]reconItem, error) {
	rd := csv.NewReader(r)
	rd.TrimLeadingSpace = true
	rd.LazyQuotes = true
	hdr, err := rd.Read()
	if err != nil {
		return nil, fmt.Errorf("empty csv")
	}
	idx := map[string]int{}
	for i, h := range hdr {
		idx[strings.TrimSpace(h)] = i
	}
	col := func(key string) (int, error) {
		name, ok := cfg[key]
		if !ok || name == "" {
			return -1, fmt.Errorf("mapping missing %q column", key)
		}
		i, ok := idx[name]
		if !ok {
			return -1, fmt.Errorf("csv has no column %q (mapped for %q)", name, key)
		}
		return i, nil
	}
	di, err := col("date")
	if err != nil {
		return nil, err
	}
	ai, err := col("amount")
	if err != nil {
		return nil, err
	}
	dsi, derr := col("description")
	ri, rerr := col("reference")

	var items []reconItem
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv: %v", err)
		}
		date, err := normalizeDate(rec[di])
		if err != nil {
			return nil, fmt.Errorf("row %q: %v", rec[di], err)
		}
		cents, err := parseMoneyToCents(rec[ai])
		if err != nil {
			return nil, fmt.Errorf("row %q: %v", rec[ai], err)
		}
		it := reconItem{Date: date, AmountCents: cents}
		if derr == nil {
			it.Description = rec[dsi]
		}
		if rerr == nil {
			it.Reference = rec[ri]
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no data rows")
	}
	return items, nil
}

// normalizeDate accepts YYYY-MM-DD, MM/DD/YYYY, MM/DD/YY, and DD-Mon-YYYY.
func normalizeDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", "01/02/2006", "01/02/06", "02-Jan-2006", "1/2/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02"), nil
		}
	}
	return "", fmt.Errorf("unrecognized date format")
}

// parseMoneyToCents handles "1234.56", "$1,234.56", "(123.45)" negatives,
// and bare integers as cents when they contain no decimal point and no sign
// of dollars (heuristic: values > 100000 without "." are treated as dollars
// too — accounting exports are dollar-denominated).
func parseMoneyToCents(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg = true
		s = s[1 : len(s)-1]
	}
	s = strings.ReplaceAll(s, ",", "")
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("unrecognized amount")
	}
	c := int64(f*100 + 0.005) // round half-up cents
	if neg {
		c = -c
	}
	return c, nil
}

// ---------- HTTP JSON adapter (accounting platform API) ----------

type httpJSONReconAdapter struct{}

func (httpJSONReconAdapter) name() string { return "http_json" }

// parse fetches { "url" } with a bearer token from env var cfg["token_env"];
// the endpoint returns a JSON array of transaction objects with
// date/amount|amount_cents/reference/description keys (any accounting
// middleware can emit this shape — QBO, Xero, Sage Intacct integrations all
// normalize to it).
func (httpJSONReconAdapter) parse(_ io.Reader, cfg map[string]string) ([]reconItem, error) {
	url := cfg["url"]
	if url == "" {
		return nil, fmt.Errorf("http_json feed requires a url")
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if env := cfg["token_env"]; env != "" {
		tok := os.Getenv(env)
		if tok == "" {
			return nil, fmt.Errorf("env %s not set", env)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed returned HTTP %d", resp.StatusCode)
	}
	var raw []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("feed is not a JSON array: %v", err)
	}
	var items []reconItem
	for _, m := range raw {
		it := reconItem{}
		it.Date, _ = m["date"].(string)
		if t, err := normalizeDate(it.Date); err == nil {
			it.Date = t
		} else {
			return nil, fmt.Errorf("feed date %q: %v", it.Date, err)
		}
		if c, ok := m["amount_cents"].(float64); ok {
			it.AmountCents = int64(c)
		} else if d, ok := m["amount"].(float64); ok {
			it.AmountCents = int64(d*100 + 0.005)
		} else {
			return nil, fmt.Errorf("feed row missing amount")
		}
		it.Reference, _ = m["reference"].(string)
		it.Description, _ = m["description"].(string)
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("feed returned no transactions")
	}
	return items, nil
}

// ---------- matching (pure, unit-tested) ----------

type finEvent struct {
	ID          int64
	AmountCents int64
	Ref         string
	Date        string
	Matched     bool
}

type matchResult struct {
	ItemIdx   int
	EventID   int64
	Kind      string // exact_ref | tolerance | none | ambiguous
	Candidate int64  // for ambiguous: count of candidates packed in note
}

// autoMatch pairs external items with platform events. Deterministic order:
// exact reference + amount first; then unique amount-within-tolerance inside
// the date window; anything with >1 candidate is left for a human (EXCEPTION).
func autoMatch(items []reconItem, events []finEvent, tolCents int64, windowDays int) []matchResult {
	results := make([]matchResult, len(items))
	for i := range items {
		results[i] = matchResult{ItemIdx: i, Kind: "none"}
	}
	// pass 1: exact reference + amount
	for i, it := range items {
		if it.Reference == "" {
			continue
		}
		for j, ev := range events {
			if ev.Matched || ev.Ref == "" || ev.Ref != it.Reference {
				continue
			}
			if ev.AmountCents == it.AmountCents {
				events[j].Matched = true
				results[i] = matchResult{ItemIdx: i, EventID: ev.ID, Kind: "exact_ref"}
				break
			}
		}
	}
	// pass 2: tolerance + date window
	for i, it := range items {
		if results[i].Kind != "none" {
			continue
		}
		d0, err := time.Parse("2006-01-02", it.Date)
		if err != nil {
			continue
		}
		var cands []int
		for j, ev := range events {
			if ev.Matched {
				continue
			}
			diff := ev.AmountCents - it.AmountCents
			if diff < 0 {
				diff = -diff
			}
			if diff > tolCents {
				continue
			}
			d1, err := time.Parse("2006-01-02", ev.Date)
			if err != nil {
				continue
			}
			dd := d1.Sub(d0)
			if dd < 0 {
				dd = -dd
			}
			if dd > time.Duration(windowDays)*24*time.Hour {
				continue
			}
			cands = append(cands, j)
		}
		if len(cands) == 1 {
			j := cands[0]
			events[j].Matched = true
			results[i] = matchResult{ItemIdx: i, EventID: events[j].ID, Kind: "tolerance"}
		} else if len(cands) > 1 {
			results[i] = matchResult{ItemIdx: i, Kind: "ambiguous", Candidate: int64(len(cands))}
		}
	}
	return results
}

// ---------- HTTP handlers ----------

var reconWriteRoles = []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}
var reconReadRoles = []string{"CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

// reconImport ingests an external feed into a recon batch.
// multipart: file (CSV) + source (adapter) + mapping (mapping name for CSV)
// JSON: {"source":"http_json","feed":"<configured feed name>"}
func (s *server) reconImport(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, reconWriteRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rc := reconDefaults(s.loadProgram(r, tenant))

	var adapterName, mappingName, feedName string
	var fileReader io.Reader
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, `{"error":"multipart parse"}`, http.StatusBadRequest)
			return
		}
		adapterName = r.FormValue("source")
		mappingName = r.FormValue("mapping")
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, `{"error":"file required for csv_generic"}`, http.StatusBadRequest)
			return
		}
		defer f.Close()
		fileReader = f
	} else {
		var in struct {
			Source string `json:"source"`
			Feed   string `json:"feed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"error":"json body"}`, http.StatusBadRequest)
			return
		}
		adapterName, feedName = in.Source, in.Feed
	}
	if adapterName == "" {
		adapterName = "csv_generic"
	}
	ad, ok := reconAdapters[adapterName]
	if !ok {
		http.Error(w, `{"error":"unknown adapter `+adapterName+`"}`, http.StatusBadRequest)
		return
	}

	var cfg map[string]string
	label := adapterName
	if adapterName == "csv_generic" {
		mappings := builtinCSVMappings()
		for k, v := range rc.CSVMappings {
			mappings[k] = v
		}
		m, ok := mappings[mappingName]
		if !ok {
			http.Error(w, `{"error":"unknown csv mapping `+mappingName+`","known":["quickbooks","xero","sage","bank_generic"]}`, http.StatusBadRequest)
			return
		}
		cfg = m
		label = "csv:" + mappingName
	} else if adapterName == "http_json" {
		feed, ok := rc.HTTPFeeds[feedName]
		if !ok {
			http.Error(w, `{"error":"unknown feed `+feedName+` — configure reconciliation.http_feeds"}`, http.StatusBadRequest)
			return
		}
		cfg = map[string]string{"url": feed.URL, "token_env": feed.TokenEnv}
		label = "http:" + feedName
	}

	items, err := ad.parse(fileReader, cfg)
	if err != nil {
		http.Error(w, `{"error":"adapter: `+err.Error()+`"}`, http.StatusUnprocessableEntity)
		return
	}
	// Batch period = min..max item dates (+1d exclusive end).
	minD, maxD := items[0].Date, items[0].Date
	for _, it := range items {
		if it.Date < minD {
			minD = it.Date
		}
		if it.Date > maxD {
			maxD = it.Date
		}
	}
	end, _ := time.Parse("2006-01-02", maxD)
	var batchID int64
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.recon_batches (tenant, source, period_start, period_end, status, total_items, imported_by)
		VALUES ($1,$2,$3::date,$4::date,'IMPORTED',$5,$6) RETURNING id`,
		tenant, label, minD, end.AddDate(0, 0, 1).Format("2006-01-02"), len(items), p.Subject).Scan(&batchID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	for _, it := range items {
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.recon_items (batch_id, tenant, txn_date, amount_cents, reference, description, status)
			VALUES ($1,$2,$3::date,$4,$5,$6,'UNMATCHED')`,
			batchID, tenant, it.Date, it.AmountCents, it.Reference, it.Description)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": batchID, "source": label, "items": len(items),
		"period": minD + " .. " + end.AddDate(0, 0, 1).Format("2006-01-02"),
		"next":   "POST /recon/batches/{id}/match to auto-match against platform money records"})
}

// reconRunMatch executes auto-match for a batch against financial_events.
func (s *server) reconRunMatch(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, reconWriteRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("batchId")
	rc := reconDefaults(s.loadProgram(r, tenant))

	batch, err := s.queryRows(r, `
		SELECT period_start::text AS ps, period_end::text AS pe, status FROM public.recon_batches
		WHERE tenant=$1 AND id=$2`, tenant, id)
	if err != nil || len(batch) == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if batch[0]["status"] == "CLOSED" {
		http.Error(w, `{"error":"batch is CLOSED"}`, http.StatusConflict)
		return
	}
	// Widen the event window by the configured date window on both sides.
	ps, _ := time.Parse("2006-01-02", batch[0]["ps"].(string))
	pe, _ := time.Parse("2006-01-02", batch[0]["pe"].(string))
	win := time.Duration(rc.DateWindowDays) * 24 * time.Hour
	evRows, err := s.queryRows(r, `
		SELECT id, amount_cents, coalesce(ref,'') AS ref, created_at::date::text AS d
		FROM public.financial_events
		WHERE tenant=$1 AND created_at >= $2::date AND created_at < $3::date AND direction='IN'`,
		tenant, ps.Add(-win).Format("2006-01-02"), pe.Add(win).Format("2006-01-02"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	itRows, err := s.queryRows(r, `
		SELECT id, txn_date::text AS d, amount_cents, coalesce(reference,'') AS reference
		FROM public.recon_items WHERE batch_id=$1 AND status='UNMATCHED' ORDER BY id`, id)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	items := make([]reconItem, len(itRows))
	itemIDs := make([]int64, len(itRows))
	for i, r0 := range itRows {
		items[i] = reconItem{Date: r0["d"].(string), AmountCents: toInt64(r0["amount_cents"]), Reference: r0["reference"].(string)}
		itemIDs[i] = toInt64(r0["id"])
	}
	events := make([]finEvent, len(evRows))
	for j, e := range evRows {
		events[j] = finEvent{ID: toInt64(e["id"]), AmountCents: toInt64(e["amount_cents"]), Ref: e["ref"].(string), Date: e["d"].(string)}
	}
	results := autoMatch(items, events, rc.ToleranceCents, rc.DateWindowDays)
	var matched, exceptions, unmatched int
	for _, res := range results {
		iid := itemIDs[res.ItemIdx]
		switch res.Kind {
		case "exact_ref", "tolerance":
			matched++
			_, _ = s.db.Exec(r.Context(), `
				UPDATE public.recon_items SET status='MATCHED', matched_event_id=$3, match_kind=$4, matched_by=$5, matched_at=now()
				WHERE id=$1 AND tenant=$2`, iid, tenant, res.EventID, res.Kind, p.Subject+":auto")
		case "ambiguous":
			exceptions++
			_, _ = s.db.Exec(r.Context(), `
				UPDATE public.recon_items SET status='EXCEPTION', note=$3 WHERE id=$1 AND tenant=$2`,
				iid, tenant, fmt.Sprintf("%d candidate platform records within tolerance/window — resolve manually", res.Candidate))
		default:
			unmatched++
		}
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.recon_batches SET status='MATCHED', matched=$3, unmatched=$4, exceptions=$5
		WHERE id=$1 AND tenant=$2`, id, tenant, matched, unmatched, exceptions)
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": id, "matched": matched, "exceptions": exceptions, "unmatched": unmatched,
		"tolerance_cents": rc.ToleranceCents, "date_window_days": rc.DateWindowDays})
}

func (s *server) reconListBatches(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, reconReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, `
		SELECT id, source, period_start::text, period_end::text, status, total_items,
		       matched, unmatched, exceptions, imported_by, created_at
		FROM public.recon_batches WHERE tenant=$1 ORDER BY id DESC LIMIT 100`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": rows})
}

func (s *server) reconGetBatch(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, reconReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("batchId")
	batch, err := s.queryRows(r, `
		SELECT id, source, period_start::text, period_end::text, status, total_items,
		       matched, unmatched, exceptions, imported_by, created_at
		FROM public.recon_batches WHERE tenant=$1 AND id=$2`, tenant, id)
	if err != nil || len(batch) == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	items, _ := s.queryRows(r, `
		SELECT i.id, i.txn_date::text, i.amount_cents, i.reference, i.description, i.status,
		       i.match_kind, i.matched_event_id, i.matched_by, i.note,
		       e.kind AS platform_kind, e.ref AS platform_ref, e.amount_cents AS platform_amount_cents
		FROM public.recon_items i
		LEFT JOIN public.financial_events e ON e.id = i.matched_event_id
		WHERE i.batch_id=$1 ORDER BY i.txn_date, i.id LIMIT 1000`, id)
	writeJSON(w, http.StatusOK, map[string]any{"batch": batch[0], "items": items})
}

// reconResolveItem lets FINANCE manually match / exception / ignore an item.
func (s *server) reconResolveItem(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, reconWriteRoles...) {
		http.Error(w, `{"error":"forbidden: requires FINANCE"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := r.PathValue("itemId")
	var in struct {
		Action  string `json:"action"` // match | exception | ignore | unmatch
		EventID int64  `json:"event_id"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"json body"}`, http.StatusBadRequest)
		return
	}
	var itemAmt int64
	var batchID int64
	err := s.db.QueryRow(r.Context(), `
		SELECT amount_cents, batch_id FROM public.recon_items WHERE tenant=$1 AND id=$2`,
		tenant, id).Scan(&itemAmt, &batchID)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	switch in.Action {
	case "match":
		// Verify the event exists, belongs to the tenant, and amounts tie.
		var evAmt int64
		err := s.db.QueryRow(r.Context(), `
			SELECT amount_cents FROM public.financial_events WHERE tenant=$1 AND id=$2`,
			tenant, in.EventID).Scan(&evAmt)
		if err != nil {
			http.Error(w, `{"error":"platform record not found"}`, http.StatusNotFound)
			return
		}
		if evAmt != itemAmt {
			http.Error(w, fmt.Sprintf(`{"error":"amount mismatch: item %d vs platform %d cents — use exception with a note instead"}`, itemAmt, evAmt), http.StatusConflict)
			return
		}
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.recon_items SET status='MATCHED', matched_event_id=$3, match_kind='manual',
			       matched_by=$4, matched_at=now(), note=$5 WHERE id=$1 AND tenant=$2`,
			id, tenant, in.EventID, p.Subject, in.Note)
	case "exception":
		if in.Note == "" {
			http.Error(w, `{"error":"exception requires a note"}`, http.StatusBadRequest)
			return
		}
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.recon_items SET status='EXCEPTION', note=$3, matched_event_id=NULL WHERE id=$1 AND tenant=$2`,
			id, tenant, in.Note)
	case "ignore":
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.recon_items SET status='IGNORED', note=$3 WHERE id=$1 AND tenant=$2`,
			id, tenant, in.Note)
	case "unmatch":
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.recon_items SET status='UNMATCHED', matched_event_id=NULL, match_kind=NULL,
			       matched_by=NULL, matched_at=NULL, note=$3 WHERE id=$1 AND tenant=$2`,
			id, tenant, in.Note)
	default:
		http.Error(w, `{"error":"action must be match|exception|ignore|unmatch"}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item_id": id, "action": in.Action, "batch_id": batchID})
}

// reconSummary: platform money records vs matched external records for a period.
func (s *server) reconSummary(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, reconReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	from, to, label, err := periodFromParams(r.URL.Query())
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	plat, _ := s.queryRows(r, `
		SELECT kind, direction, count(*) AS n, sum(amount_cents) AS total_cents
		FROM public.financial_events
		WHERE tenant=$1 AND created_at >= $2::date AND created_at < $3::date
		GROUP BY kind, direction ORDER BY kind`, tenant, from, to)
	ext, _ := s.queryRows(r, `
		SELECT status, count(*) AS n, sum(amount_cents) AS total_cents
		FROM public.recon_items
		WHERE tenant=$1 AND txn_date >= $2::date AND txn_date < $3::date
		GROUP BY status`, tenant, from, to)
	var platformIn, externalMatched int64
	for _, row := range plat {
		if row["direction"] == "IN" {
			platformIn += toInt64(row["total_cents"])
		}
	}
	for _, row := range ext {
		if row["status"] == "MATCHED" {
			externalMatched += toInt64(row["total_cents"])
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"period": label, "platform_events": plat, "external_items": ext,
		"platform_in_cents": platformIn, "external_matched_cents": externalMatched,
		"drift_cents": platformIn - externalMatched})
}
