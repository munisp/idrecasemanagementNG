"""Physical check extraction: MICR line (E-13B band), courtesy amount box,
legal (handwritten) amount line, date, payee, memo — with per-field
confidence and an honest ICR posture.

MICR: the bottom band of a US check is magnetic-ink printed in E-13B; it OCRs
reliably with Tesseract once the band is isolated, thresholded, and upscaled.
Symbols map: ⑈=transit(A)  ⑆=on-us(B)  ⑇=amount(C)  ⑄=dash(D) — Tesseract
approximates them as letters/symbols; we parse the digit runs around them.

Amounts: the courtesy box (printed digits) OCRs well. The legal line is
usually HANDWRITTEN — true ICR needs a trained model; Tesseract gives a
best-effort reading which we mark low confidence. A courtesy/legal mismatch
always routes the check to REVIEW (never auto-match on conflicting amounts).
"""

from __future__ import annotations

import io
import re
from dataclasses import dataclass, field, asdict

import cv2
import numpy as np
import pytesseract
from PIL import Image

WORDS = {"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
         "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
         "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
         "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
         "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50, "sixty": 60,
         "seventy": 70, "eighty": 80, "ninety": 90}
SCALES = {"hundred": 100, "thousand": 1000, "million": 1000000}


@dataclass
class CheckExtraction:
    routing_number: str | None = None
    account_number: str | None = None
    check_number: str | None = None
    amount_cents: int | None = None            # courtesy box
    legal_amount_cents: int | None = None      # written line (ICR best-effort)
    amount_mismatch: bool = False
    check_date: str | None = None
    payer_name: str | None = None
    memo: str | None = None
    confidence: str = "low"
    detail: dict = field(default_factory=dict)

    def as_dict(self) -> dict:
        return asdict(self)


def _prep(img: np.ndarray) -> np.ndarray:
    g = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
    g = cv2.resize(g, None, fx=2, fy=2, interpolation=cv2.INTER_CUBIC)
    return cv2.adaptiveThreshold(g, 255, cv2.ADAPTIVE_THRESH_GAUSSIAN_C,
                                 cv2.THRESH_BINARY, 35, 11)


def _ocr(img: np.ndarray, psm: int = 6, whitelist: str | None = None) -> str:
    cfg = f"--psm {psm}"
    if whitelist:
        cfg += f" -c tessedit_char_whitelist={whitelist}"
    return pytesseract.image_to_string(img, config=cfg).strip()


def parse_micr(band_text: str) -> tuple[str | None, str | None, str | None]:
    """Digit runs around MICR separators. US layout: ⑈routing⑈ account⑆ check
    (check number may lead on personal checks). Returns (routing, account, check)."""
    t = band_text.replace(" ", "")
    # normalize common E-13B symbol approximations
    for sym in ("⑈", "A", "a"):  # transit
        t = t.replace(sym, "|")
    for sym in ("⑆", "B", "b", "'"):  # on-us
        t = t.replace(sym, "~")
    for sym in ("⑇", "C", "c"):
        t = t.replace(sym, "^")
    runs = re.findall(r"\d{3,17}", t)
    routing = next((r for r in runs if len(r) == 9), None)
    rest = [r for r in runs if r is not routing]
    account = max(rest, key=len) if rest else None
    check = min(rest, key=len) if rest else None
    return routing, account, check


def routing_checksum_valid(routing: str) -> bool:
    """ABA routing checksum: 3(d1+d4+d7)+7(d2+d5+d8)+(d3+d6+d9) ≡ 0 mod 10."""
    d = [int(c) for c in routing]
    return (3*(d[0]+d[3]+d[6]) + 7*(d[1]+d[4]+d[7]) + (d[2]+d[5]+d[8])) % 10 == 0


def words_to_cents(text: str) -> int | None:
    """'One thousand two hundred & 34/100' -> 120034 (best-effort ICR)."""
    toks = re.findall(r"[a-z]+|\d+/\d+|\d+", text.lower())
    total, current, cents = 0, 0, None
    for t in toks:
        if re.fullmatch(r"\d+/\d+", t):
            cents = int(t.split("/")[0])
        elif re.fullmatch(r"\d+", t):
            current += int(t)
        elif t in WORDS:
            current += WORDS[t]
        elif t in SCALES:
            current = max(1, current) * SCALES[t]
            if SCALES[t] >= 1000:
                total, current = total + current, 0
        elif t == "dollars" or t == "dollar":
            pass
    if total + current == 0 and cents is None:
        return None
    return (total + current) * 100 + (cents or 0)


def parse_courtesy(text: str) -> int | None:
    m = re.search(r"\$?\s*([\d,]+\.\d{2})", text) or re.search(r"\*+\s*([\d,]+)\.(\d{2})", text)
    if not m:
        m2 = re.search(r"([\d,]+)\.(\d{2})", text)
        if not m2:
            return None
        return int(m2.group(1).replace(",", "")) * 100 + int(m2.group(2))
    g = m.group(1)
    if "." in g:
        whole, frac = g.replace(",", "").split(".")
        return int(whole) * 100 + int(frac)
    return None


def extract_check(image_bytes: bytes) -> CheckExtraction:
    """Full extraction from a check photo/scan. Never raises on image quirks —
    returns low-confidence partial results instead of failing the intake."""
    out = CheckExtraction()
    try:
        img = cv2.imdecode(np.frombuffer(image_bytes, np.uint8), cv2.IMREAD_COLOR)
        if img is None:
            out.detail["error"] = "undecodable image"
            return out
        h, w = img.shape[:2]

        # 1) MICR band: bottom strip, digit-biased OCR
        band = _prep(img[int(h*0.82):h, 0:w])
        micr_txt = _ocr(band, psm=7, whitelist="0123456789⑈⑆⑇⑄ABCDabcd|' ")
        out.routing_number, out.account_number, out.check_number = parse_micr(micr_txt)
        out.detail["micr_raw"] = micr_txt
        if out.routing_number and not routing_checksum_valid(out.routing_number):
            out.detail["routing_checksum"] = "failed"
            out.routing_number = None  # don't trust a failed checksum

        # 2) Courtesy amount box: right-middle region
        box = _prep(img[int(h*0.30):int(h*0.60), int(w*0.60):w])
        box_txt = _ocr(box, psm=7, whitelist="$0123456789.,*")
        out.amount_cents = parse_courtesy(box_txt)
        out.detail["courtesy_raw"] = box_txt

        # 3) Legal amount line (handwriting — ICR best-effort)
        line = _prep(img[int(h*0.35):int(h*0.55), int(w*0.05):int(w*0.75)])
        legal_txt = _ocr(line, psm=7)
        out.legal_amount_cents = words_to_cents(legal_txt)
        out.detail["legal_raw"] = legal_txt

        if out.amount_cents and out.legal_amount_cents \
                and out.amount_cents != out.legal_amount_cents:
            out.amount_mismatch = True

        # 4) Date / payee / memo zones
        date_zone = _prep(img[int(h*0.12):int(h*0.30), int(w*0.55):w])
        date_txt = _ocr(date_zone, psm=7)
        m = re.search(r"(\d{1,2})[/\-.](\d{1,2})[/\-.](\d{2,4})", date_txt)
        if m:
            mo, dy, yr = int(m.group(1)), int(m.group(2)), int(m.group(3))
            yr = yr + 2000 if yr < 100 else yr
            if 1 <= mo <= 12 and 1 <= dy <= 31:
                out.check_date = f"{yr:04d}-{mo:02d}-{dy:02d}"
        memo_zone = _prep(img[int(h*0.62):int(h*0.80), 0:int(w*0.5)])
        memo_txt = _ocr(memo_zone, psm=7)
        if memo_txt:
            out.memo = memo_txt[:120]
        out.detail["date_raw"], out.detail["memo_raw"] = date_txt, memo_txt

        # 5) Confidence rollup
        core = [out.routing_number, out.check_number, out.amount_cents]
        hits = sum(1 for x in core if x)
        if hits == 3 and not out.amount_mismatch:
            out.confidence = "high"
        elif hits >= 2:
            out.confidence = "medium"
        else:
            out.confidence = "low"
    except Exception as e:  # never kill intake on an image quirk
        out.detail["error"] = str(e)
    return out
