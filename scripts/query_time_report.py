#!/usr/bin/env python3
"""Query the platform's team time report for any period and emit CSV.

Weekly and monthly are presets; any custom range works too. The output
columns match the Cost Model workbook's "Platform Labor Cost" sheet, so the
export can be pasted straight in (labor cost recomputes from live rates).

Usage:
  python3 scripts/query_time_report.py --base https://case-api.example.gov \
      --token $STAFF_JWT --tenant fl [--week 2026-10-05 | --month 2026-10 |
      --start 2026-10-01 --end 2026-11-01] [--out time_report.csv]

Requires a staff JWT for a CASE_MANAGER, PM (where the program defines it),
FINANCE, or admin principal. Dollar amounts are included only when the
principal may view billable rates (PM/FINANCE/admin); otherwise the amount
columns are blank and hours remain authoritative.
"""
import argparse
import csv
import json
import sys
import urllib.request


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", required=True, help="case-api base URL")
    ap.add_argument("--token", required=True, help="staff bearer JWT")
    ap.add_argument("--tenant", required=True, help="tenant id (state code)")
    per = ap.add_mutually_exclusive_group()
    per.add_argument("--week", help="any date in the week, YYYY-MM-DD (default: current week)")
    per.add_argument("--month", help="YYYY-MM")
    ap.add_argument("--start", help="custom range start, YYYY-MM-DD (requires --end)")
    ap.add_argument("--end", help="custom range end, exclusive, YYYY-MM-DD")
    ap.add_argument("--out", help="write CSV here instead of stdout")
    a = ap.parse_args()

    if bool(a.start) != bool(a.end):
        ap.error("--start and --end must be given together")

    if a.start:
        q = f"start={a.start}&end={a.end}"
    elif a.month:
        q = f"month={a.month}"
    elif a.week:
        q = f"week={a.week}"
    else:
        q = ""

    req = urllib.request.Request(
        f"{a.base}/t/{a.tenant}/reports/time" + (f"?{q}" if q else ""),
        headers={"Authorization": f"Bearer {a.token}"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = json.load(resp)
    except urllib.error.HTTPError as e:
        sys.exit(f"query failed: HTTP {e.code} {e.read().decode()[:200]}")

    rows = [["period", "case_number", "case_id", "subject", "role",
             "minutes", "billable_minutes", "amount_cents", "unrated_billable_minutes"]]
    for r0 in data.get("by_person", []):
        rows.append([
            data.get("period", q), r0.get("case_number", ""), r0.get("case_id", ""),
            r0.get("subject", ""), r0.get("role", ""), r0.get("minutes", 0),
            r0.get("billable_minutes", 0),
            r0.get("amount_cents", ""), r0.get("unrated_billable_minutes", ""),
        ])

    out = open(a.out, "w", newline="") if a.out else sys.stdout
    csv.writer(out).writerows(rows)
    if a.out:
        print(f"wrote {len(rows) - 1} rows to {a.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
