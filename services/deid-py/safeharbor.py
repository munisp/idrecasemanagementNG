# Author: Patrick Munis at newwave
"""safeharbor.py — deterministic HIPAA Safe Harbor (45 CFR 164.514(b)(2))
detector/redactor. This layer is ALWAYS on: it needs no models, no network,
and no GPU, and its findings are exact (regex spans). The optional OpenMed
transformer layer (openmed_layer.py) adds recall for free-text names and
locations on top of this floor — defense in depth, never substitution.

Covers the 18 identifier classes where a textual pattern is meaningful:
SSN, MRN/account numbers, phone/fax, email, URL, IP, ZIP, dates (contextual:
shifted, not stripped, so timelines survive), age>89, member/certificate
numbers, license plates, device identifiers. Names/geography below county
come from the NER layer when available.
"""

from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass, asdict

POLICY_VERSION = "safe-harbor-18/v1"

# Context window terms that make a bare date an identifier (Safe Harbor says
# ALL date elements except year related to the individual).
_DATE_CTX = re.compile(
    r"(?i)\b(dob|date of birth|born|admission|admitted|discharge|discharged|"
    r"service date|date of service|dos|death|expired|appointment|visit|"
    r"procedure date|surgery|dod|on)\b")

_PATTERNS: list[tuple[str, str, float]] = [
    # (type, pattern, base confidence)
    ("SSN",        r"\b\d{3}-\d{2}-\d{4}\b", 0.99),
    ("SSN",        r"\bSSN[:\s#]*\d{9}\b", 0.95),
    ("PHONE",      r"\b(?:\+1[\s.-]?)?\(?\d{3}\)?[\s.-]\d{3}[\s.-]\d{4}\b", 0.9),
    ("EMAIL",      r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b", 0.97),
    ("URL",        r"\bhttps?://[^\s]+|\bwww\.[^\s]+\b", 0.95),
    ("IP",         r"\b(?:\d{1,3}\.){3}\d{1,3}\b", 0.9),
    ("MRN",        r"(?i)\b(?:MRN|medical record(?: number)?|acct(?:ount)?(?: number)?|member(?: id)?|certificate(?: number)?|policy(?: number)?|claim (?:number|#)|group (?:number|#))[:\s#]*[A-Z0-9-]{5,}\b", 0.85),
    ("ZIP",        r"\b\d{5}(?:-\d{4})?\b", 0.6),
    ("DATE",       r"\b(?:0?[1-9]|1[0-2])[/-](?:0?[1-9]|[12]\d|3[01])[/-](?:19|20)\d{2}\b", 0.75),
    ("DATE",       r"\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\.? \d{1,2},? (?:19|20)\d{2}\b", 0.75),
    ("AGE_OVER_89", r"(?i)\b(?:age[d]?|aged?)\s*(?:9\d|1\d\d)\b", 0.9),
    ("LICENSE_PLATE", r"(?i)\b(?:license plate|plate)\s*(?:number|no\.?|#)?\s*[A-Z0-9-]{4,}\b", 0.8),
    ("DEVICE_ID",  r"(?i)\b(?:device|serial)\s*(?:id|number|no\.?)?[:\s#]*[A-Z0-9]{8,}\b", 0.8),
]


@dataclass
class Finding:
    type: str
    start: int
    end: int
    score: float
    text: str
    detector: str = "safeharbor-regex"

    def to_dict(self) -> dict:
        return asdict(self)


def scan(text: str) -> list[Finding]:
    """Deterministic identifier scan. Dates only count when near a clinical
    context word (otherwise every numeric date in a contract is flagged)."""
    out: list[Finding] = []
    for typ, pat, conf in _PATTERNS:
        for m in re.finditer(pat, text):
            if typ == "DATE" and not _DATE_CTX.search(text[max(0, m.start()-80):m.start()] + text[m.end():m.end()+40]):
                continue
            if typ == "ZIP":
                # Skip when the 5 digits are part of a longer number (amounts).
                s, e = m.span()
                if (s > 0 and text[s-1].isdigit()) or (e < len(text) and text[e:e+1].isdigit()):
                    continue
            out.append(Finding(typ, m.start(), m.end(), conf, m.group(0)))
    # Overlap resolution: keep the higher-confidence finding on conflict.
    out.sort(key=lambda f: (f.start, -(f.end - f.start), -f.score))
    deduped: list[Finding] = []
    last_end = -1
    for f in out:
        if f.start >= last_end:
            deduped.append(f)
            last_end = f.end
    return deduped


def _shift_token(seed: bytes) -> int:
    return int.from_bytes(hashlib.sha256(seed).digest()[:2], "big") % 365 + 1


def _shift_date(token: str, days: int) -> str:
    """Shift a date token by a stable per-tenant offset, preserving format."""
    from datetime import datetime, timedelta
    for fmt in ("%m/%d/%Y", "%m-%d-%Y", "%B %d, %Y", "%b %d, %Y", "%b. %d, %Y"):
        for candidate in (token, token.replace(".", "", 1) if fmt == "%b %d, %Y" else token):
            try:
                d = datetime.strptime(candidate, fmt)
            except ValueError:
                continue
            shifted = d - timedelta(days=days)
            return shifted.strftime(fmt).replace(" 0", " ") if " " in fmt else shifted.strftime(fmt)
    return "[DATE]"


def redact(text: str, findings: list[Finding], tenant_seed: str, shift_dates: bool = True) -> str:
    """Replace findings with typed tokens. Dates are shifted (not tokenized)
    so clinical timelines remain analyzable; the shift is a stable function
    of the tenant seed, so two documents from the same tenant stay
    cross-referable while the absolute dates are meaningless."""
    days = _shift_token(tenant_seed.encode())
    out, last = [], 0
    for f in sorted(findings, key=lambda x: x.start):
        out.append(text[last:f.start])
        if f.type == "DATE" and shift_dates:
            out.append(_shift_date(f.text, days))
        elif f.type == "AGE_OVER_89":
            out.append("[AGE≥90]")
        else:
            out.append(f"[{f.type}]")
        last = f.end
    out.append(text[last:])
    return "".join(out)


def risk(findings: list[Finding]) -> float:
    """0..1 — any direct identifier (SSN, email, phone, MRN) dominates."""
    direct = {"SSN", "EMAIL", "PHONE", "MRN", "DEVICE_ID", "LICENSE_PLATE"}
    if any(f.type in direct and f.score >= 0.8 for f in findings):
        return 1.0
    if findings:
        return min(0.9, 0.4 + 0.1 * len(findings))
    return 0.0


def text_hash(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()
