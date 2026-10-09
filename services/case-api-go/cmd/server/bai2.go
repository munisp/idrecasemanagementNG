package main

// bai2.go — BAI2 bank-statement adapter for the recon engine (bank.go rail 3).
//
// A bank's prior-day statement drops on an SFTP/share; staff (or a cron
// fetch) uploads it through the existing POST /recon/import with
// adapter=bai2, and the file flows through the standard autoMatch machinery
// against financial_events — no new matching logic, just one more parser in
// the registry.
//
// Parsing scope (the records that carry money):
//   01 file header · 02 group header (as-of date YYMMDD) · 03 account header
//   16 transaction detail (type code, amount, …, bank ref, …, text)
//   49/88/98/99 trailers — ignored (totals already verified bank-side)
// Continuation lines (physical record splits, code 88 or raw text runs) are
// re-joined onto the previous logical record before parsing.
//
// Amount convention: BAI2 amounts are integers with implied decimals per
// currency (2 for USD) — we treat them as cents directly. Sign from the
// type code: 100–399 credit (received, +), 400–699 debit (disbursed, −).

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type bai2ReconAdapter struct{}

func (bai2ReconAdapter) name() string { return "bai2" }

func (bai2ReconAdapter) parse(r io.Reader, _ map[string]string) ([]reconItem, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	logical := []string{}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// A new logical record starts with a 2-digit record code + comma;
		// anything else continues the previous record (BAI2 continuation).
		if len(line) >= 3 && line[0] >= '0' && line[0] <= '9' && line[1] >= '0' && line[1] <= '9' && line[2] == ',' {
			logical = append(logical, line)
		} else if len(logical) > 0 {
			logical[len(logical)-1] += line
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	items := []reconItem{}
	asOf := ""
	for _, rec := range logical {
		code := rec[:2]
		f := strings.Split(rec[3:], ",")
		switch code {
		case "02": // group header: 02,receiver,originator,status,asOfDate,asOfTime,...
			if len(f) >= 4 && len(f[3]) == 6 {
				asOf = "20" + f[3][:2] + "-" + f[3][2:4] + "-" + f[3][4:6]
			}
		case "16": // transaction detail
			if len(f) < 2 {
				continue
			}
			tc, err := strconv.Atoi(strings.TrimSpace(f[0]))
			if err != nil {
				continue
			}
			amt, err := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
			if err != nil {
				continue
			}
			sign := int64(1)
			if tc >= 400 && tc < 700 {
				sign = -1
			} else if tc < 100 || tc >= 700 {
				continue // non-monetary summary type codes
			}
			ref, desc := "", ""
			if len(f) >= 4 {
				ref = strings.TrimSpace(f[3]) // bank reference number
			}
			if len(f) >= 6 {
				desc = strings.TrimSpace(f[len(f)-1]) // free text tail
			}
			items = append(items, reconItem{
				Date:        asOf,
				AmountCents: sign * amt,
				Reference:   ref,
				Description: desc,
			})
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no BAI2 transaction (16) records found — is this a BAI2 file?")
	}
	return items, nil
}
