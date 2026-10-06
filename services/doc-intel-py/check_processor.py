"""Physical check extraction: MICR line (E-13B band), courtesy amount box,
legal (handwritten) amount line, date, payee, memo — with per-field
confidence and an honest ICR posture.

MICR: the bottom band of a US check is magnetic-ink printed in E-13B; it OCRs
reliably with Tesseract once the band is isolated, thresholded, and upscaled.
Symbols map: ⑈=transit(A)  ⑆=on-us(B)  ⑇=amount(C)  ⑄=dash(D) — Tesseract
approximates them as letters/symbols; we parse the digit runs around them.

Amounts: the courtesy box (printed digits) OCRs well. The legal line is
usually HANDWRITTEN — this module reads it with a real ICR engine chain:

  1. TrOCR (microsoft/trocr-base-handwritten) — a transformer trained on the
     IAM handwriting corpus; the strongest open-source single-line ICR model.
     Used when `transformers` is installed (CPU torch already ships in the
     doc-intel image for Docling).
  2. PaddleOCR (PP-OCRv4, already docked for scanned-page OCR) — strong on
     printed/clear hand-print and returns real per-line confidence scores.
  3. Tesseract — last-resort fallback; reading marked low confidence.

Engine selection: CHECK_ICR_ENGINE=trocr|paddle|tesseract|auto (default auto
= first available in the order above). MICR stays on Tesseract regardless:
Paddle/TrOCR charsets don't cover the E-13B ⑈⑆⑇ symbols, and the digit-run
parser + ABA checksum already self-validates.

A courtesy/legal mismatch always routes the check to REVIEW (never
auto-match on conflicting amounts).
"""

from __future__ import annotations

import os
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
    # accepts BGR or already-grayscale crops (ICR chain passes grayscale)
    g = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY) if img.ndim == 3 else img
    g = cv2.resize(g, None, fx=2, fy=2, interpolation=cv2.INTER_CUBIC)
    return cv2.adaptiveThreshold(g, 255, cv2.ADAPTIVE_THRESH_GAUSSIAN_C,
                                 cv2.THRESH_BINARY, 35, 11)


def _ocr(img: np.ndarray, psm: int = 6, whitelist: str | None = None) -> str:
    cfg = f"--psm {psm}"
    if whitelist:
        cfg += f" -c tessedit_char_whitelist={whitelist}"
    return pytesseract.image_to_string(img, config=cfg).strip()


# ---------------------------------------------------------------------------
# ICR/OCR engine chain. All engines are lazily docked on first use and any
# import/load failure degrades to the next engine — an extraction returns
# low-confidence partials rather than ever failing intake.
# ---------------------------------------------------------------------------

_ENGINE = os.environ.get("CHECK_ICR_ENGINE", "auto").lower()
_paddle_ocr = None
_paddle_tried = False
_trocr_pipe = None
_trocr_tried = False


def _paddle():
    """Dock PaddleOCR (already a service dependency). Light config: crops are
    pre-zoned, so orientation classify/unwarping are wasted cycles."""
    global _paddle_ocr, _paddle_tried
    if _paddle_tried:
        return _paddle_ocr
    _paddle_tried = True
    try:
        from paddleocr import PaddleOCR
        _paddle_ocr = PaddleOCR(
            lang="en",
            use_doc_orientation_classify=False,
            use_doc_unwarping=False,
            use_textline_orientation=False,
        )
    except Exception:
        _paddle_ocr = None
    return _paddle_ocr


def _trocr():
    """Dock TrOCR handwriting pipeline if transformers is installed."""
    global _trocr_pipe, _trocr_tried
    if _trocr_tried:
        return _trocr_pipe
    _trocr_tried = True
    try:
        from transformers import pipeline
        _trocr_pipe = pipeline("image-to-text",
                               model="microsoft/trocr-base-handwritten")
    except Exception:
        _trocr_pipe = None
    return _trocr_pipe


def _paddle_read(img: np.ndarray) -> tuple[str, float | None]:
    """Returns (joined_text, mean_rec_score). PaddleOCR 3.x predict() yields
    result dicts with rec_texts / rec_scores."""
    ocr = _paddle()
    if ocr is None:
        return "", None
    try:
        texts: list[str] = []
        scores: list[float] = []
        for res in ocr.predict(img):
            texts.extend(res.get("rec_texts", []))
            scores.extend(float(s) for s in res.get("rec_scores", []))
        return " ".join(texts).strip(), (sum(scores) / len(scores) if scores else None)
    except Exception:
        return "", None


def _trocr_read(img: np.ndarray) -> tuple[str, float | None]:
    pipe = _trocr()
    if pipe is None:
        return "", None
    try:
        pil = Image.fromarray(img)
        out = pipe(pil)
        return (out[0].get("generated_text", "") if out else "").strip(), None
    except Exception:
        return "", None


def _icr_order() -> list[str]:
    return {"trocr": ["trocr", "paddle", "tesseract"],
            "paddle": ["paddle", "tesseract"],
            "tesseract": ["tesseract"]}.get(_ENGINE, ["trocr", "paddle", "tesseract"])


def read_handwritten_line(img: np.ndarray) -> tuple[str, str, float | None]:
    """Legal-amount line ICR: (text, engine_used, model_score). Engines expect
    a natural (non-binarized) crop — aggressive thresholding destroys cursive
    strokes, so callers pass a grayscale/upscaled region, not _prep() output."""
    for eng in _icr_order():
        if eng == "trocr":
            txt, score = _trocr_read(img)
        elif eng == "paddle":
            txt, score = _paddle_read(img)
        else:
            txt, score = _ocr(_prep(img), psm=7), None
        if txt:
            return txt, eng, score
    return "", "none", None


def read_printed_zone(img: np.ndarray, psm: int = 7,
                      whitelist: str | None = None) -> tuple[str, str, float | None]:
    """Printed zones (courtesy box, date, memo): Paddle first for real
    confidence scores, Tesseract fallback (whitelist support kept for the
    courtesy box's narrow charset)."""
    if _ENGINE != "tesseract":
        txt, score = _paddle_read(img)
        if txt:
            return txt, "paddle", score
    return _ocr(_prep(img), psm=psm, whitelist=whitelist), "tesseract", None


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

        # 2) Courtesy amount box: right-middle region (printed — Paddle first,
        #    narrow-charset Tesseract fallback)
        box_txt, box_eng, box_score = read_printed_zone(
            img[int(h*0.30):int(h*0.60), int(w*0.60):w], psm=7,
            whitelist="$0123456789.,*")
        out.amount_cents = parse_courtesy(box_txt)
        out.detail["courtesy_raw"] = box_txt
        out.detail["courtesy_engine"] = box_eng
        if box_score is not None:
            out.detail["courtesy_score"] = round(box_score, 3)

        # 3) Legal amount line — handwriting ICR. Natural grayscale crop
        #    (upscaled), NOT _prep()'s adaptive threshold: binarization
        #    destroys cursive strokes that TrOCR/Paddle rely on.
        line_zone = img[int(h*0.35):int(h*0.55), int(w*0.05):int(w*0.75)]
        line_gray = cv2.cvtColor(line_zone, cv2.COLOR_BGR2GRAY)
        line_nat = cv2.resize(line_gray, None, fx=2, fy=2,
                              interpolation=cv2.INTER_CUBIC)
        legal_txt, legal_eng, legal_score = read_handwritten_line(line_nat)
        out.legal_amount_cents = words_to_cents(legal_txt)
        out.detail["legal_raw"] = legal_txt
        out.detail["legal_engine"] = legal_eng
        if legal_score is not None:
            out.detail["legal_score"] = round(legal_score, 3)

        if out.amount_cents and out.legal_amount_cents \
                and out.amount_cents != out.legal_amount_cents:
            out.amount_mismatch = True

        # 4) Date / memo zones (printed or hand-printed)
        date_txt, _, _ = read_printed_zone(img[int(h*0.12):int(h*0.30), int(w*0.55):w], psm=7)
        m = re.search(r"(\d{1,2})[/\-.](\d{1,2})[/\-.](\d{2,4})", date_txt)
        if m:
            mo, dy, yr = int(m.group(1)), int(m.group(2)), int(m.group(3))
            yr = yr + 2000 if yr < 100 else yr
            if 1 <= mo <= 12 and 1 <= dy <= 31:
                out.check_date = f"{yr:04d}-{mo:02d}-{dy:02d}"
        memo_txt, _, _ = read_printed_zone(img[int(h*0.62):int(h*0.80), 0:int(w*0.5)], psm=7)
        if memo_txt:
            out.memo = memo_txt[:120]
        out.detail["date_raw"], out.detail["memo_raw"] = date_txt, memo_txt

        # 5) Confidence rollup — model scores count where available. A
        #    low-scoring courtesy read (<0.85) doesn't earn "high".
        core = [out.routing_number, out.check_number, out.amount_cents]
        hits = sum(1 for x in core if x)
        strong = box_score is None or box_score >= 0.85
        if hits == 3 and not out.amount_mismatch and strong:
            out.confidence = "high"
        elif hits >= 2:
            out.confidence = "medium"
        else:
            out.confidence = "low"
    except Exception as e:  # never kill intake on an image quirk
        out.detail["error"] = str(e)
    return out
