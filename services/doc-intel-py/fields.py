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


# Context-tagged money: "Total ... $1,234.56", "Amount Billed: $900.00",
# EOB columns: "Allowed Amount $412.00", "Plan Paid $329.60".
# Canonical label groups let cross_check reason about billing arithmetic
# (billed >= allowed >= paid; paid + patient_resp ~= allowed) regardless of
# which synonym a payer's EOB template uses.
_MONEY_LABELS: dict[str, tuple[str, ...]] = {
    "billed": ("total billed", "amount billed", "billed amount", "billed charges",
               "total charges", "charge amount", "total charge"),
    "allowed": ("allowed amount", "allowed charges", "eligible amount",
                "negotiated amount", "contracted rate"),
    "paid": ("plan paid", "amount paid", "paid amount", "insurance paid",
             "insurer paid", "payer paid", "total paid", "payment amount",
             "plan payment", "benefits paid"),
    "patient_resp": ("patient responsibility", "patient balance", "patient owes",
                     "member responsibility", "member liability", "your share",
                     "balance due", "amount you owe", "patient amount"),
    "deductible": ("deductible",),
    "copay": ("copay", "co-pay", "copayment"),
    "coinsurance": ("coinsurance",),
    "adjustment": ("adjustment amount", "contractual adjustment", "plan discount",
                   "provider adjustment", "write-off", "write off", "discount amount"),
    "total": ("total", "total amount", "total due", "amount due"),
    "qpa": ("qpa", "qualifying payment amount"),
}
_LABEL_TO_CANONICAL: dict[str, str] = {
    syn: canon for canon, syns in _MONEY_LABELS.items() for syn in syns
}
_MONEY_LABEL_ALT = "|".join(re.escape(s) for s in sorted(
    _LABEL_TO_CANONICAL, key=len, reverse=True))

_TAGGED_MONEY_RE = re.compile(
    rf"(?i)\b({_MONEY_LABEL_ALT})\b"
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
    r"statement\s+date|date\s+issued|filing\s+date|discharge\s+date|admission\s+date|"
    r"determination\s+date|process(?:ed)?\s+date|paid\s+date|check\s+date|"
    r"decision\s+date)\b"
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
# Member / subscriber identifier on EOBs and claim forms.
_MEMBER_ID_RE = re.compile(
    r"(?i)\b(?:member|subscriber|patient|insured)\s*(?:id|number|no\.?|#)\b\s*[:#]?\s*([A-Z0-9][-A-Z0-9]{5,20})\b")
# CARC/RARC adjustment + remark codes on EOBs (CO-45, PR-1, OA-23, N127 ...).
_REMARK_CODE_RE = re.compile(r"\b((?:CO|PR|OA|PI|CR)-?\d{1,3}|N\d{1,3})\b")
# ICD-10 diagnosis codes: letter + 2 digits + optional decimal suffix. Require
# diagnostic context so CPT-adjacent tokens and ZIP+4s don't false-positive.
_DX_CONTEXT_RE = re.compile(
    r"(?i)\b(?:diagnosis|dx|icd(?:-?10)?)\b[^\n]{0,40}?\b([A-TV-Z]\d{2}(?:\.\d{1,4})?)\b")
# Provider taxonomy code (207Q00000X shape).
_TAXONOMY_RE = re.compile(r"\b(\d{3}[A-Z]\d{5}X)\b")


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


# --- OCR repair helpers ----------------------------------------------------
# Degraded faxes break text in characteristic ways; every repair below is
# gated by a strict field-format validation so it can only ever RECOVER a
# value the format rules already bless — never invent one.

#: Lines that are ONLY a label (faxed forms often render value-then-label in
#: reading order, and OCR emits them as adjacent lines):
#:   "Marisol Ortega\nMember Name:"  ->  "Member Name: Marisol Ortega"
_STANDALONE_LABELS = (
    "payer name", "member name", "patient name", "provider name",
    "billing provider", "rendering provider", "member id", "subscriber id",
    "patient id", "provider npi", "npi", "taxonomy", "claim number",
    "service date", "date of service", "determination date", "process date",
    "diagnosis code", "diagnosis", "place of service", "network status",
    "cpt/hcpcs codes", "cpt codes", "billed amount", "allowed amount",
    "plan paid", "patient responsibility", "units",
)
_STANDALONE_LABEL_RE = re.compile(
    r"(?im)^[ \t]*(" + "|".join(re.escape(s) for s in _STANDALONE_LABELS) + r")[ \t]*[:#]?[ \t]*$")


def _join_label_lines(text: str) -> str:
    """Fold value-then-label line pairs into canonical 'label: value' lines."""
    lines = text.splitlines()
    out: list[str] = []
    for ln in lines:
        m = _STANDALONE_LABEL_RE.match(ln)
        if m and out and out[-1].strip():
            out[-1] = f"{m.group(1)}: {out[-1].strip()}"
        else:
            out.append(ln)
    return "\n".join(out)


def _to_cents_ocr(s: str) -> int:
    """Cents for OCR-degraded money: when several grouping separators appear,
    the LAST one is the decimal point ("$1.850.00" -> 185000)."""
    body = re.sub(r"[^\d,\.\-]", "", s)
    seps = [i for i, ch in enumerate(body) if ch in ",."]
    if len(seps) > 1 and body[seps[-1]] == "." and (len(body) - seps[-1] - 1) == 2:
        body = body[:seps[-1]].replace(",", "").replace(".", "") + body[seps[-1]:]
    return _to_cents(body)


# Date with one stray digit/char before the year: "03/1272026" -> 2026-03-12.
# Only accepted when the repaired date is a REAL calendar date.
_DATE_STRAY_RE = re.compile(r"\b(\d{1,2})/(\d{2})\d(\d{4})\b")


def _norm_date_token_repaired(tok: str) -> str | None:
    iso = _norm_date_token(tok)
    if iso:
        return iso
    m = _DATE_STRAY_RE.search(tok)
    if m:
        return _parse_date(int(m.group(3)), int(m.group(1)), int(m.group(2)))
    return None


# OCR digit->letter confusions, restricted to positions where the field format
# REQUIRES a letter (ICD-10 leading alpha, taxonomy 4th char).
_OCR_DIGIT_TO_LETTER = {"0": "O", "1": "I", "2": "Z", "5": "S", "6": "G", "7": "T", "8": "B"}

# Diagnosis context where the code's leading letter was OCR'd as a digit
# ("Diagnosis Code: 110" -> I10).
_DX_CONTEXT_DIGIT_RE = re.compile(
    r"(?i)\b(?:diagnosis|dx|icd(?:-?10)?)\b[\s\S]{0,40}?\b([0125678]\d{2}(?:\.\d{1,4})?)\b")

# Taxonomy whose 4th (always-alpha) char was OCR'd as a digit: "207000000X".
_TAXONOMY_DIGIT_RE = re.compile(r"\b(\d{3})(\d)(\d{5}X)\b")

# --- public entry point ----------------------------------------------------

def extract_fields(text: str, extra_labels: dict[str, list[str]] | None = None) -> dict:
    """Pull typed, validated fields out of document text. Deterministic.

    extra_labels: canonical money label -> additional synonym phrases learned
    from human corrections (see learning.py). Synonyms are regex-escaped and
    OR'd into the tagged-money pattern, so a payer template the platform has
    never seen becomes extractable after a few reviewed corrections — without
    a code change."""
    out: dict = {"amounts": [], "dates": [], "cpt_codes": [], "identifiers": {}}
    if not text:
        return out
    # Faxed forms OCR as value-then-label line pairs; canonicalize first.
    text = _join_label_lines(text)

    label_map = dict(_LABEL_TO_CANONICAL)
    if extra_labels:
        for canon, syns in extra_labels.items():
            for syn in syns:
                label_map[syn.lower()] = canon
    label_alt = "|".join(re.escape(s) for s in sorted(label_map, key=len, reverse=True))
    # Separator-tolerant: degraded faxes mix up "," and "." ("$1.850.00").
    tagged_money = re.compile(
        rf"(?i)\b({label_alt})\b[^\d$]{{0,40}}\$\s*"
        r"(-?\d{1,3}(?:[,.]\d{3})*(?:[.,]\d{2})?|-?\d+[.,]\d{2})")

    for m in tagged_money.finditer(text):
        raw_label = re.sub(r"\s+", " ", m.group(1)).lower()
        out["amounts"].append({"label": raw_label,
                               "canonical": label_map.get(raw_label, raw_label),
                               "cents": _to_cents_ocr(m.group(2))})

    for m in _TAGGED_DATE_RE.finditer(text):
        iso = _norm_date_token_repaired(m.group(2).strip())
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
    member = _MEMBER_ID_RE.search(text)
    if member:
        out["identifiers"]["member_id"] = member.group(1)
    taxonomy = _TAXONOMY_RE.search(text)
    if taxonomy:
        out["identifiers"]["taxonomy"] = taxonomy.group(1)

    cpt = sorted({c for c in _CPT_RE.findall(text)})
    if cpt:
        out["cpt_codes"] = cpt[:50]
    hcpcs = sorted({c for c in _HCPCS_RE.findall(text)})
    if hcpcs:
        out["hcpcs_codes"] = hcpcs[:50]
    # Taxonomy 4th char is always alpha; recover when OCR made it a digit.
    if "taxonomy" not in out["identifiers"]:
        for m in _TAXONOMY_DIGIT_RE.finditer(text):
            letter = _OCR_DIGIT_TO_LETTER.get(m.group(2))
            if letter:
                out["identifiers"]["taxonomy_repaired"] = m.group(1) + letter + m.group(3)
                break

    remark = sorted({c.upper().replace("-", "") if c[:2].upper() in
                     {"CO", "PR", "OA", "PI", "CR"} else c.upper()
                     for c in _REMARK_CODE_RE.findall(text)})
    # Normalize to canonical hyphenated form: CO45 -> CO-45.
    remark = sorted({(c[:2] + "-" + c[2:]) if re.fullmatch(r"(?:CO|PR|OA|PI|CR)\d{1,3}", c) else c
                     for c in remark})
    if remark:
        out["adjustment_codes"] = remark[:25]
    dx = {m.group(1).upper() for m in _DX_CONTEXT_RE.finditer(text)}
    # OCR repair: leading letter of an ICD-10 read as a digit ("110" -> I10);
    # accepted only when the repair yields a strict ICD-10 shape.
    for m in _DX_CONTEXT_DIGIT_RE.finditer(text):
        tok = m.group(1)
        repaired = _OCR_DIGIT_TO_LETTER.get(tok[0], tok[0]) + tok[1:]
        if re.fullmatch(r"[A-Z]\d{2}(?:\.\d{1,4})?", repaired):
            dx.add(repaired)
    if dx:
        out["diagnosis_codes"] = sorted(dx)[:25]

    return out


def extraction_score(text: str) -> float:
    """Quality score for competing OCR reads of the SAME page. Field-bearing
    text beats noise: tagged amounts, parseable tagged dates, checksum-valid
    identifiers, and real code patterns are things a correct read produces
    and confetti noise does not. Word count only breaks ties."""
    if not text.strip():
        return 0.0
    ex = extract_fields(text)
    score = 3.0 * len(ex.get("amounts", []))
    score += 2.0 * len(ex.get("dates", []))
    ids = ex.get("identifiers", {})
    score += 3.0 * len(ids)
    if ids.get("npi", {}).get("luhn_valid"):
        score += 4.0  # a Luhn-valid NPI is near-impossible in noise
    score += 1.5 * len(ex.get("cpt_codes", []))
    score += 1.5 * len(ex.get("adjustment_codes", []))
    words = [w for w in text.split() if sum(c.isalpha() for c in w) >= 3]
    score += min(len(words), 200) * 0.05
    return score


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

    # EOB billing arithmetic over canonical labels. billed >= allowed >= paid
    # and paid + patient_responsibility ~= allowed are invariants of the
    # remittance model; violations are OCR misreads or genuine payer errors,
    # either way review-worthy. Tolerance $1 for rounding across lines.
    def canon_values(canon: str) -> list[int]:
        return [a["cents"] for a in fields.get("amounts", [])
                if a.get("canonical") == canon]

    billed = canon_values("billed")
    allowed = canon_values("allowed")
    paid = canon_values("paid")
    resp = canon_values("patient_resp")
    if billed and allowed and max(allowed) > max(billed) + 100:
        issues.append(f"Allowed amount ({max(allowed)}c) exceeds billed amount ({max(billed)}c) — impossible remittance arithmetic; verify extraction")
    if allowed and paid and max(paid) > max(allowed) + 100:
        issues.append(f"Plan paid ({max(paid)}c) exceeds allowed amount ({max(allowed)}c) — impossible remittance arithmetic; verify extraction")
    if allowed and paid and resp:
        gap = abs(max(paid) + max(resp) - max(allowed))
        if gap > 100:
            issues.append(f"Plan paid + patient responsibility ({max(paid) + max(resp)}c) != allowed amount ({max(allowed)}c) — EOB does not balance; verify before use")
    return issues
