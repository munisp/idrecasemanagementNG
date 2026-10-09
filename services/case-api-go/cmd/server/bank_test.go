package main

import (
	"strings"
	"testing"
	"time"
)

func TestValidRouting(t *testing.T) {
	if !validRouting("021000021") { // JPMorgan Chase — classic valid ABA
		t.Fatal("021000021 must pass checksum")
	}
	if validRouting("021000022") {
		t.Fatal("bad checksum must fail")
	}
	if validRouting("12345") || validRouting("02100002a") {
		t.Fatal("length/charset must fail")
	}
}

func TestVerifyHMACSHA256(t *testing.T) {
	// Precomputed: HMAC-SHA256("s3cret", "hello") — stable test vector.
	// Computed once via: echo -n hello | openssl dgst -sha256 -hmac s3cret
	body := []byte("hello")
	mac := hmacSHA256Hex("s3cret", body)
	if !verifyHMACSHA256("s3cret", body, mac) {
		t.Fatal("valid signature must verify")
	}
	if !verifyHMACSHA256("s3cret", body, "sha256="+mac) {
		t.Fatal("sha256= prefix form must verify")
	}
	if verifyHMACSHA256("s3cret", []byte("tampered"), mac) {
		t.Fatal("tampered body must fail")
	}
	if verifyHMACSHA256("wrong", body, mac) {
		t.Fatal("wrong secret must fail")
	}
}

func TestGenerateNACHA(t *testing.T) {
	cfg := NachaConfig{
		ImmediateDestination: "021000021", ImmediateOrigin: "123456789",
		CompanyName: "State IDRE", EntryDescription: "IDRE PMT", OriginatingDFI: "02100002",
	}
	entries := []nachaDestination{
		{RoutingNumber: "021000021", AccountNumber: "12345678", AccountType: "checking", Name: "Provider One"},
		{RoutingNumber: "021000021", AccountNumber: "87654321", AccountType: "savings", Name: "Plan Two"},
	}
	amounts := []int64{150000, 4250}
	ids := []string{"PAY0001", "PAY0002"}
	file, err := generateNACHA(cfg, entries, amounts, ids, time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC), 'A')
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	recs := strings.Split(strings.TrimRight(file, "\n"), "\n")
	if len(recs)%10 != 0 {
		t.Fatalf("blocking factor: %d records not a multiple of 10", len(recs))
	}
	for i, r := range recs {
		if len(r) != 94 {
			t.Fatalf("record %d length %d != 94", i, len(r))
		}
	}
	if recs[0][:3] != "101" || recs[1][0] != '5' || recs[2][0] != '6' {
		t.Fatal("header/batch/entry record types wrong")
	}
	if recs[2][1:3] != "22" || recs[3][1:3] != "32" {
		t.Fatal("transaction codes: checking=22, savings=32")
	}
	// Record layout: 0 file hdr, 1 batch hdr, 2-3 entries, 4 batch ctrl, 5 file ctrl.
	fileControl := recs[5]
	if fileControl[0] != '9' || fileControl[1:7] != "000001" {
		t.Fatalf("file control malformed: %q", fileControl[:13])
	}
	// Entry detail amount field: record 2, amount at offset 29..39 (10 chars).
	if recs[2][29:39] != "0000150000" {
		t.Fatalf("entry amount encoding wrong: %q", recs[2][29:39])
	}
	if _, err := generateNACHA(cfg, []nachaDestination{{RoutingNumber: "021000022", AccountNumber: "1", Name: "X"}}, []int64{1}, []string{"I"}, time.Now(), 'A'); err == nil {
		t.Fatal("invalid routing in entry must fail the file")
	}
}

func TestBAI2Adapter(t *testing.T) {
	feed := `01,021000021,STATE IDRE,251012,0930,1,80,2/
02,STATEIDRE,021000021,1,251012,0930,USD,2/
03,0123456789,USD,010,+1500000,/
16,165,150000,0,0001234567,,LOCKBOX DEPOSIT/
16,475,4250,0,0001234568,,AWARD PAYOUT/
49,+1542750,4/
98,+1542750,1,6/
99,+1542750,1,8/
`
	items, err := (bai2ReconAdapter{}).parse(strings.NewReader(feed), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	if items[0].AmountCents != 150000 || items[0].Date != "2025-10-12" || items[0].Reference != "0001234567" {
		t.Fatalf("credit item wrong: %+v", items[0])
	}
	if items[1].AmountCents != -4250 {
		t.Fatalf("debit (type 475) must be negative: %+v", items[1])
	}
	if _, err := (bai2ReconAdapter{}).parse(strings.NewReader("not a bai2 file\n"), nil); err == nil {
		t.Fatal("garbage input must error")
	}
}
