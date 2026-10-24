// Author: Patrick Munis at newwave
package main

// moovpay_test.go — pure-function coverage for the moov-io payment rails:
// NACHA round-trip (generate then re-parse with the library reader), return
// ingestion parsing, TIFF conversion, the screening scorer, and routing
// validation. No database, no vault, no network.

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/moov-io/ach"
)

func TestGenerateNACHAMoovRoundTrip(t *testing.T) {
	cfg := NachaConfig{
		ImmediateDestination: "021000021", ImmediateOrigin: "123456789",
		CompanyName: "State IDRE", EntryDescription: "IDRE PMT", OriginatingDFI: "02100002",
	}
	entries := []nachaDestination{
		{RoutingNumber: "021000021", AccountNumber: "12345678", AccountType: "checking", Name: "Provider One"},
		{RoutingNumber: "021000021", AccountNumber: "87654321", AccountType: "savings", Name: "Plan Two"},
	}
	body, err := generateNACHA(cfg, entries, []int64{150000, 4250}, []string{"PAY0001", "PAY0002"},
		time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC), 'A')
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Re-parse with the library reader: the file must be readable and valid.
	f, err := ach.NewReader(strings.NewReader(body)).Read()
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("re-parsed file invalid: %v", err)
	}
	if len(f.Batches) != 1 || len(f.Batches[0].GetEntries()) != 2 {
		t.Fatalf("expected 1 batch x 2 entries")
	}
	e0 := f.Batches[0].GetEntries()[0]
	if e0.Amount != 150000 || e0.DFIAccountNumber != "12345678" {
		t.Fatalf("entry 0 mismatch: amount=%d acct=%s", e0.Amount, e0.DFIAccountNumber)
	}
	if e0.TransactionCode != ach.CheckingCredit || f.Batches[0].GetEntries()[1].TransactionCode != ach.SavingsCredit {
		t.Fatal("transaction codes wrong")
	}
	if f.Control.TotalCreditEntryDollarAmountInFile != 154250 {
		t.Fatalf("file control credit total: %d", f.Control.TotalCreditEntryDollarAmountInFile)
	}
}

func TestACHReturnParsing(t *testing.T) {
	// Build a PPD debit file, then synthesize its return by hand-rolling an
	// Addenda99 entry through the library (same path ingestACHReturns uses).
	fh := ach.NewFileHeader()
	fh.ImmediateDestination = "021000021"
	fh.ImmediateOrigin = "123456789"
	fh.FileCreationDate = "261012"
	fh.FileIDModifier = "A"
	fh.ImmediateDestinationName = "RECEIVING BANK"
	fh.ImmediateOriginName = "State IDRE"
	fh.ReferenceCode = "RETURNS"
	f := ach.NewFile()
	f.Header = fh
	bh := ach.NewBatchHeader()
	bh.ServiceClassCode = ach.DebitsOnly
	bh.CompanyName = "State IDRE"
	bh.CompanyIdentification = "123456789"
	bh.StandardEntryClassCode = ach.PPD
	bh.CompanyEntryDescription = "RETURN"
	bh.EffectiveEntryDate = "261013"
	bh.ODFIIdentification = "02100002"
	batch := ach.NewBatchPPD(bh)
	e := ach.NewEntryDetail()
	e.TransactionCode = ach.CheckingDebit
	e.RDFIIdentification = "02100002"
	e.CheckDigit = "1"
	e.DFIAccountNumber = "12345678"
	e.Amount = 100
	e.IdentificationNumber = "INV1"
	e.IndividualName = "Plan One"
	e.TraceNumber = "021000020000001"
	e.AddendaRecordIndicator = 1
	e.Category = ach.CategoryReturn
	e.Addenda99 = &ach.Addenda99{TypeCode: "99", ReturnCode: "R01", OriginalTrace: "021000020000001"}
	batch.AddEntry(e)
	if err := batch.Create(); err != nil {
		t.Fatalf("batch: %v", err)
	}
	f.AddBatch(batch)
	if err := f.Create(); err != nil {
		t.Fatalf("file: %v", err)
	}
	var buf bytes.Buffer
	if err := ach.NewWriter(&buf).Write(f); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := ach.NewReader(bytes.NewReader(buf.Bytes())).Read()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := back.Batches[0].GetEntries()[0]
	if got.Addenda99 == nil || got.Addenda99.ReturnCode != "R01" {
		t.Fatal("return addenda lost in round-trip")
	}
	if achReturnReason("R01") != "Insufficient funds" {
		t.Fatal("R01 reason mapping wrong")
	}
	if !strings.HasPrefix(achReturnReason("R99"), "Return code") {
		t.Fatal("unknown codes must surface as the code")
	}
}

func TestRoutingLookupChecksum(t *testing.T) {
	if ok, _ := routingLookup("021000021"); !ok {
		t.Fatal("valid ABA rejected")
	}
	if ok, _ := routingLookup("021000022"); ok {
		t.Fatal("bad checksum accepted")
	}
	if ok, _ := routingLookup("12345"); ok {
		t.Fatal("short routing accepted")
	}
}

func TestToTIFF(t *testing.T) {
	img := testImagePNG(t)
	out, err := toTIFF(img)
	if err != nil {
		t.Fatalf("tiff: %v", err)
	}
	if len(out) < 8 || out[0] != 'I' || out[1] != 'I' {
		t.Fatal("not a TIFF stream")
	}
}

func TestNameSimilarity(t *testing.T) {
	if s := nameSimilarity("ACME HEALTH PLAN", "ACME HEALTH PLAN"); s < 0.99 {
		t.Fatalf("identical names: %f", s)
	}
	if s := nameSimilarity("ACME HEALTH PLAN", "ACME HEALTH PLANS INC"); s < 0.7 {
		t.Fatalf("suffix variant too low: %f", s)
	}
	if s := nameSimilarity("ACME HEALTH PLAN", "BRIGHT PATH SURGERY"); s > 0.4 {
		t.Fatalf("disjoint names too high: %f", s)
	}
	if s := nameSimilarity("MOHAMMAD AHMAD", "MOHAMMAD AHMED"); s < 0.8 {
		t.Fatalf("transliteration variant too low: %f", s)
	}
}

func TestNormName(t *testing.T) {
	got := normName("Acme-Health, Plan!")
	if len(got) != 3 || got[0] != "ACME" || got[2] != "PLAN" {
		t.Fatalf("normName: %v", got)
	}
	if len(normName("a b c")) != 0 {
		t.Fatal("single-char tokens must drop")
	}
}

// testImagePNG: minimal 8x8 PNG generated in-test (no fixture files).
func testImagePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}
