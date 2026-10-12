"""Deterministic structured-field extraction over OCR/parsed text.

Inspired by cheque-processing pipelines (ChequeWise-style): don't leave the
document as a text blob — pull typed, validated fields out of it. Everything
here is pure regex + checksum validation: no vendor SDK, no LLM call, fully
offline. These fields feed eligibility evidence and the validate() stage's
cross-checks (e.g. billed total vs line-item sum), so a bad extraction is
caught deterministically instead of being trusted blindly.
"""

import re
from datetime import datetime

# --- money -----------------------------------------------------------------

_MONEY_RE = re.compile(r"\$\s*(-?\d{1,3}(?:,\d{3})*(?:\.\d{2})?|-?\d+\.\d{2})")


def _to_cents(s: str) -> int:
    return int(round(float(s.replace(",", "")) * 100))


# Context-tagged money: "Total ... $1,234.56", "Amount Billed: $900.00".
_TAGGED_MONEY_RE = re.compile(
    r"(?i)\b(total(?:\s+(?:billed|charges?|amount|due))?|amount\s+(?:billed|due|paid)"
    r"|balance\s+due|patient\s+(?:responsibility|balance)|allowed\s+amount|qpa)\b"
    r"[^\d$]{0,40}\$\s*(-?\d{1,3}(?:,\d{3})*(?:\.\d{2})?)"
)

_LINE_ITEM_RE = re.compile(
    r"(?m)^\s*.{0,60}?\$\s*(-?\d{1,3}(?:,\d{3})*\.\d{2})\s*$"
)

# --- dates -----------------------------------------------------------------

_DATE_PATTERNS = [
    (re.compile(r"\b(\d{4})-(\d{2})-(\d{2})\b"), "ymd"),
    (re.compile(r"\b(\d{1,2})/(\d{1,2})/(\d{4})\b"), "mdy"),
    (re.compile(r"\b(\d{1,2})-(\d{1,2})-(\d{4})\b"), "mdy"),
    (re.compile(
        r"\b(Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|"
        r"Jul(?:y)?|Aug(?:ust)?|Sep(?:t(?:ember)?)?|Oct(?:ober)?|Nov(?:ember)?|"
        r"Dec(?:ember)?)\s+(\d{1,2}),?\s+(\d{4})\b", re.I), "named"),
]
_MONTHS = {"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
           "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}

_TAGGED_DATE_RE = re.compile(
    r"(?i)\b(date\s+of\s+service|service\s+date|dos|date\s+of\s+birth|dob|"
    r"statement\s+date|date\s+issued|filing\s+date|discharge\s+date|admission\s+date)\b"
    r"\s*[:#]?\s*([^\n,;]{4,30})"
)


def _parse_date(y: int, m: int, d: int) -> str | None:
    try:
        return datetime(y, m, d).date().isoformat()
    except ValueError:
        return None


def _norm_date_token(tok: str) -> str | None:
    for rx, kind in _DATE_PATTERNS:
        m = rx.search(tok)
        if not m:
            continue
        if kind == "ymd":
            return _parse_date(int(m.group(1)), int(m.group(2)), int(m.group(3)))
        if kind == "mdy":
            return _parse_date(int(m.group(3)), int(m.group(1)), int(m.group(2)))
        mon = _MONTHS.get(m.group(1).lower()[:3])
        if mon:
            return _parse_date(int(m.group(3)), mon, int(m.group(2)))
    return None


# --- identifiers -----------------------------------------------------------

_NPI_RE = re.compile(r"(?i)\bNPI\b\s*[:#]?\s*(\d{10})\b")
_TIN_RE = re.compile(r"(?i)\b(?:TIN|EIN|FEIN|tax\s*id(?:entification)?\s*(?:number|no\.?)?)\b\s*[:#]?\s*(\d{2}-?\d{7})\b")
_CLAIM_RE = re.compile(r"(?i)\b(?:claim|invoice|account|bill|statement)\s*(?:number|no\.?|#|:)?\s*[:#]?\s*([A-Z0-9][-A-Z0-9]{5,24})\b")
_CPT_RE = re.compile(r"(?<![\d-])(\d{5})(?![\d])")
_HCPCS_RE = re.compile(r"\b([A-VY]\d{4})\b")


def _luhn_ok(digits: str) -> bool:
    total = 0
    for i, ch in enumerate(reversed(digits)):
        n = int(ch)
        if i % 2 == 1:
            n *= 2
            if n > 9:
                n -= 9
        total += n
    return total % 10 == 0


def valid_npi(digits: str) -> bool:
    """NPI = 10 digits, Luhn-valid with the 80840 prefix per CMS spec."""
    return bool(re.fullmatch(r"\d{10}", digits)) and _luhn_ok("80840" + digits)


# --- public entry point ----------------------------------------------------

def extract_fields(text: str) -> dict:
    """Pull typed, validated fields out of document text. Deterministic."""
    out: dict = {"amounts": [], "dates": [], "cpt_codes": [], "identifiers": {}}
    if not text:
        return out

    for m in _TAGGED_MONEY_RE.finditer(text):
        out["amounts"].append({"label": re.sub(r"\s+", " ", m.group(1)).lower(),
                               "cents": _to_cents(m.group(2))})

    for m in _TAGGED_DATE_RE.finditer(text):
        iso = _norm_date_token(m.group(2).strip())
        if iso:
            out["dates"].append({"label": re.sub(r"\s+", " ", m.group(1)).lower(), "iso": iso})

    npi = _NPI_RE.search(text)
    if npi:
        out["identifiers"]["npi"] = {"value": npi.group(1), "luhn_valid": valid_npi(npi.group(1))}
    tin = _TIN_RE.search(text)
    if tin:
        out["identifiers"]["tin"] = re.sub(r"-", "", tin.group(1))
    claim = _CLAIM_RE.search(text)
    if claim:
        out["identifiers"]["claim_number"] = claim.group(1)

    cpt = sorted({c for c in _CPT_RE.findall(text)})
    if cpt:
        out["cpt_codes"] = cpt[:50]
    hcpcs = sorted({c for c in _HCPCS_RE.findall(text)})
    if hcpcs:
        out["hcpcs_codes"] = hcpcs[:50]

    return out


def cross_check(fields: dict) -> list[str]:
    """Consistency checks over extracted fields; returns human-readable issues."""
    issues: list[str] = []
    npi = fields.get("identifiers", {}).get("npi")
    if npi and not npi["luhn_valid"]:
        issues.append(f"NPI {npi['value']} fails Luhn checksum — OCR misread likely; verify against document")
    totals = [a["cents"] for a in fields.get("amounts", [])
              if a["label"].startswith("total") or a["label"] == "amount billed"]
    if len(totals) >= 2 and totals[0] != totals[-1]:
        issues.append(f"Conflicting total amounts on document ({totals[0]} vs {totals[-1]} cents) — verify before use")
    return issues
