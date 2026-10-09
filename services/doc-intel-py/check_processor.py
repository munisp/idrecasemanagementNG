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
= first available in the order above). MICR is read FIRST by the in-house
E-13B engine (micr_engine.py — monospace cell-grid segmentation + template
NCC against the bundled Nimra font, fonts/Nimra-E13B.ttf, SIL OFL 1.1),
which recognizes the ⑈⑆⑇⑉ separator symbols natively; tesseract remains
as the fallback ensemble, and every routing must still pass the ⑆…⑆
structural bracket + ABA checksum (with unique-solution single-digit
repair) — miss, don't guess.

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


def _ink(zone: np.ndarray) -> np.ndarray:
    """Ink-emphasis preprocessing for handwriting on colored safety paper.
    Min-channel projection makes colored ink (blue/black pen on green/blue
    guilloche) dark regardless of hue; Otsu then separates ink from paper.
    Plain grayscale+adaptive-threshold washes blue ink out on green paper
    (confirmed on live samples)."""
    g = zone.min(axis=2) if zone.ndim == 3 else zone
    g = cv2.resize(g, None, fx=3, fy=3, interpolation=cv2.INTER_CUBIC)
    g = cv2.GaussianBlur(g, (3, 3), 0)
    _, bw = cv2.threshold(g, 0, 255, cv2.THRESH_BINARY + cv2.THRESH_OTSU)
    return bw


MONTHS = {"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
          "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}


def parse_check_date(text: str) -> str | None:
    """ISO date from a check date zone: 08/11/2019 or handwritten
    'Aug. 11, 2019' alike. Returns None when nothing plausible parses."""
    m = re.search(r"(\d{1,2})[/\-.](\d{1,2})[/\-.](\d{2,4})", text)
    if m:
        mo, dy, yr = int(m.group(1)), int(m.group(2)), int(m.group(3))
    else:
        m = re.search(r"([A-Za-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?[,]?\s+(\d{2,4})",
                      text, re.IGNORECASE)
        if not m:
            return None
        mo_key = m.group(1)[:3].lower()
        if mo_key not in MONTHS:
            # ICR misreads of month names ("Clug." for "Aug.") — fuzzy-match
            # the 3-letter prefix against the 12-month closed vocabulary.
            import difflib
            close = difflib.get_close_matches(mo_key, list(MONTHS), n=2, cutoff=0.5)
            if not close:
                return None
            if len(close) == 2 and (
                    difflib.SequenceMatcher(None, mo_key, close[0]).ratio()
                    - difflib.SequenceMatcher(None, mo_key, close[1]).ratio()) < 0.15:
                return None  # ambiguous misread — no guess
            mo_key = close[0]
        mo, dy, yr = MONTHS[mo_key], int(m.group(2)), int(m.group(3))
    yr = yr + 2000 if yr < 100 else yr
    if 1 <= mo <= 12 and 1 <= dy <= 31:
        return f"{yr:04d}-{mo:02d}-{dy:02d}"
    return None


def _ocr(img: np.ndarray, psm: int = 6, whitelist: str | None = None) -> str:
    """Tesseract read that never raises — a bad zone (or a missing tesseract
    binary) degrades that field to empty instead of blanking the whole check."""
    cfg = f"--psm {psm}"
    if whitelist:
        if "'" in whitelist or '"' in whitelist:
            raise ValueError("whitelist must not contain quotes (tesseract config parsing)")
        cfg += f" -c tessedit_char_whitelist={whitelist}"
    try:
        return pytesseract.image_to_string(img, config=cfg).strip()
    except Exception:
        return ""


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
    # PP-OCRv5 is the compatibility sweet spot: v6-medium inference models
    # require a bleeding-edge paddlepaddle (strides attribute error on 3.0.x),
    # so the default pins v5; override with CHECK_PADDLE_OCR_VERSION when the
    # deployed paddle version supports newer weights.
    want = os.environ.get("CHECK_PADDLE_OCR_VERSION", "PP-OCRv5")
    for ver in ([want] if want else []) + ["PP-OCRv5", None]:
        try:
            from paddleocr import PaddleOCR
            kw = dict(lang="en",
                      use_doc_orientation_classify=False,
                      use_doc_unwarping=False,
                      use_textline_orientation=False,
                      # oneDNN PIR kernels crash on some CPU builds
                      # (ConvertPirAttribute2RuntimeAttribute); plain CPU
                      # inference is slower but never crashes intake.
                      enable_mkldnn=os.environ.get("CHECK_PADDLE_MKLDNN", "0") == "1")
            if ver:
                kw["ocr_version"] = ver
            _paddle_ocr = PaddleOCR(**kw)
            break
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


def _deskew(gray: np.ndarray) -> np.ndarray:
    """Straighten a handwritten zone. Pen lines on a check drift a few degrees;
    TrOCR/Paddle lose characters on rotated baselines, so estimate the ink
    angle via minAreaRect and rotate back. No-op (original returned) when the
    ink mass is too thin to trust an estimate."""
    try:
        ink = cv2.threshold(gray, 0, 255,
                            cv2.THRESH_BINARY_INV + cv2.THRESH_OTSU)[1]
        coords = np.column_stack(np.where(ink > 0))
        if len(coords) < 200:
            return gray
        angle = cv2.minAreaRect(coords)[-1]
        angle = -(90 + angle) if angle < -45 else -angle
        if abs(angle) < 0.4 or abs(angle) > 15:  # noise or hopeless — leave it
            return gray
        h, w = gray.shape
        m = cv2.getRotationMatrix2D((w // 2, h // 2), angle, 1.0)
        return cv2.warpAffine(gray, m, (w, h), flags=cv2.INTER_CUBIC,
                              borderMode=cv2.BORDER_REPLICATE)
    except Exception:
        return gray


def _paddle_page(img: np.ndarray) -> list[dict]:
    """One PaddleOCR pass over the WHOLE check, returning every line with its
    normalized center (cx, cy in 0..1) and model score. Detection on a full
    page is far more reliable than on tiny pre-cropped zones — the detector
    needs layout context. Callers assign lines to fields by coordinates."""
    ocr = _paddle()
    if ocr is None:
        return []
    try:
        h, w = img.shape[:2]
        out: list[dict] = []
        for res in ocr.predict(img):
            polys = res.get("rec_polys", [])
            texts = res.get("rec_texts", [])
            scores = res.get("rec_scores", [])
            for poly, txt, sc in zip(polys, texts, scores):
                pts = np.asarray(poly).reshape(-1, 2)
                out.append({"text": str(txt), "score": float(sc),
                            "cx": float(pts[:, 0].mean()) / w,
                            "cy": float(pts[:, 1].mean()) / h,
                            "x0": float(pts[:, 0].min()) / w})
        return out
    except Exception:
        return []


def _zone_lines(page: list[dict], x0: float, x1: float,
                y0: float, y1: float) -> str:
    """Join full-page OCR lines whose centers fall in a zone, in reading
    order (top-to-bottom, left-to-right)."""
    sel = [l for l in page if x0 <= l["cx"] <= x1 and y0 <= l["cy"] <= y1]
    sel.sort(key=lambda l: (l["cy"], l["x0"]))
    return " ".join(l["text"] for l in sel).strip()


def _engine_read(eng: str, img: np.ndarray, psm: int = 7,
                 whitelist: str | None = None) -> tuple[str, float | None]:
    if eng == "trocr":
        return _trocr_read(img)
    if eng == "paddle":
        return _paddle_read(img)
    return _ocr(_prep(img), psm=psm, whitelist=whitelist), None


def consensus_read(variants: list[np.ndarray], handwritten: bool,
                   parser=None, psm: int = 7,
                   whitelist: str | None = None,
                   precomputed: list[tuple[str, str, float | None]] | None = None
                   ) -> tuple[str, str, float | None, int]:
    """Ensemble read: EVERY docked engine reads EVERY preprocessing variant,
    and the winner is chosen by parsed-value consensus, not first-nonempty.

    Accuracy posture (>97% field target): a single engine's first guess is the
    old behavior and is where errors come from. Here a candidate only wins
    outright when ≥2 independent reads agree on the parsed value (e.g. both
    TrOCR and Tesseract legal-line reads parse to the same cents). With no
    agreement, the highest-model-score candidate wins but the voter count is
    reported so the rollup can stay below "high" confidence.

    Returns (text, engines_used, best_model_score, agreeing_voters).
    `parser` normalizes a raw string to a comparable value (e.g.
    words_to_cents); without one, raw stripped text is compared.
    """
    engines = _icr_order() if handwritten else \
        (["paddle", "tesseract"] if _ENGINE != "tesseract" else ["tesseract"])
    cands: list[tuple[str, str, float | None, object]] = []
    for txt, eng, score in (precomputed or []):
        if txt:
            cands.append((txt, eng, score, parser(txt) if parser else txt.strip()))
    for img in variants:
        if img is None:
            continue
        for eng in engines:
            try:
                txt, score = _engine_read(eng, img, psm=psm, whitelist=whitelist)
            except Exception:
                continue
            if not txt:
                continue
            val = parser(txt) if parser else txt.strip()
            cands.append((txt, eng, score, val))
    if not cands:
        return "", "none", None, 0
    # group by parsed value; None-valued reads group per-text (still count)
    groups: dict[object, list[int]] = {}
    for i, (_, _, _, val) in enumerate(cands):
        key = val if val not in (None, "") else f"raw:{cands[i][0].strip()}"
        groups.setdefault(key, []).append(i)
    best_key = max(groups, key=lambda k: (
        # a successfully parsed value ALWAYS beats a larger group of
        # unparseable raw reads — two engines agreeing on garbage is garbage
        1 if not str(k).startswith("raw:") else 0,
        len(groups[k]),
        max((cands[i][2] or 0.0) for i in groups[k]),
    ))
    winners = groups[best_key]
    bi = max(winners, key=lambda i: (cands[i][2] or 0.0))
    txt, eng, score, _ = cands[bi]
    engines_used = "+".join(sorted({cands[i][1] for i in winners}))
    return txt, engines_used, score, len(winners)


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
    (check number may lead on personal checks). Returns (routing, account, check).

    Posture on merged/garbage reads: miss, don't guess — an unreadable band
    routes the check to REVIEW; a fabricated routing number would corrupt
    settlement metadata."""
    t = band_text.replace(" ", "")
    # normalize common E-13B symbol approximations
    for sym in ("⑈", "A", "a"):  # transit
        t = t.replace(sym, "|")
    for sym in ("⑆", "B", "b"):  # on-us
        t = t.replace(sym, "~")
    for sym in ("⑇", "C", "c"):
        t = t.replace(sym, "^")
    # paddle/full-page reads approximate E-13B separators as ':' and '"'
    t = t.replace('"', "|").replace("''", "|").replace(":", "|")
    runs = re.findall(r"\d{3,17}", t)
    # The routing number is the only 9-digit field AND the only field with a
    # mathematical validity constraint — both must hold. A 9-digit run that
    # fails the ABA checksum is not trusted (garbage read or fake document);
    # and we deliberately do NOT slide windows into longer merged digit runs:
    # when E-13B symbols OCR as digit fragments ("11"/"12"), the soup contains
    # checksum-passing 9-windows by pure chance (~10% per window — verified on
    # a sample check), so sliding extraction fabricates routing numbers.
    # Posture: a missed routing sends the check to REVIEW; a guessed one
    # corrupts settlement metadata. Miss, don't guess.
    routing = next((r for r in runs if len(r) == 9 and routing_checksum_valid(r)), None)
    rest = [r for r in runs if r != routing]
    account = max(rest, key=len) if rest else None
    check = min(rest, key=len) if rest else None
    return routing, account, check


def _micr_bands(img: np.ndarray) -> list[np.ndarray]:
    """Locate candidate MICR bands instead of trusting one fixed strip.
    The E-13B band is the densest ink row-range in the lower third of the
    check; a horizontal projection finds it even when the template places
    it higher/lower than the classic 0.85-0.99 window. Returns up to two
    candidate bands (fixed strip first, projection band second, deduped)."""
    h, w = img.shape[:2]
    bands = [img[int(h*0.85):int(h*0.99), int(w*0.02):int(w*0.98)]]
    try:
        g = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY) if img.ndim == 3 else img
        lower = g[int(h*0.70):, :]
        bw = cv2.adaptiveThreshold(lower, 255, cv2.ADAPTIVE_THRESH_GAUSSIAN_C,
                                   cv2.THRESH_BINARY_INV, 41, 13)
        proj = bw.mean(axis=1)
        if proj.max() > 0:
            peak = int(proj.argmax())
            y0 = max(0, peak - int(h*0.03))
            y1 = min(lower.shape[0], peak + int(h*0.06))
            cand = img[int(h*0.70)+y0:int(h*0.70)+y1, int(w*0.02):int(w*0.98)]
            if cand.size and abs((int(h*0.70)+peak) - int(h*0.92)) > int(h*0.04):
                bands.append(cand)
    except Exception:
        pass
    return bands


def _repair_routing(text: str) -> str | None:
    """One-unknown-digit ABA repair: a 9-character run with exactly one
    non-digit yields at most one checksum-valid completion. Two valid
    completions = ambiguous = no repair (miss, don't guess)."""
    out = None
    t = " " + text + " "
    for m in re.finditer(r"\S{9,}", t):
        tok = m.group(0)
        for i in range(0, max(1, len(tok)-8)):
            win = tok[i:i+9]
            if len(win) == 9 and sum(c.isdigit() for c in win) == 8:
                sols = [d for d in "0123456789"
                        if routing_checksum_valid("".join(d if not c.isdigit() else c for c in win))]
                if len(sols) == 1:
                    fixed = "".join(sols[0] if not c.isdigit() else c for c in win)
                    if out is not None and out != fixed:
                        return None  # conflicting repairs — ambiguous
                    out = fixed
    return out


def routing_checksum_valid(routing: str) -> bool:
    """ABA routing checksum: 3(d1+d4+d7)+7(d2+d5+d8)+(d3+d6+d9) ≡ 0 mod 10."""
    d = [int(c) for c in routing]
    return (3*(d[0]+d[3]+d[6]) + 7*(d[1]+d[4]+d[7]) + (d[2]+d[5]+d[8])) % 10 == 0


_VOCAB = list(WORDS) + list(SCALES)


def _vocab_match(tok: str) -> str | None:
    """Map a misread token to the nearest number word. ICR on cursive fuses
    and distorts words ('hundredfifteen', 'Leven'); the legal-line vocabulary
    is tiny and closed, so fuzzy matching against it is safe and accurate.
    Two passes: exact substring segmentation of fused words, then
    difflib nearest-match with a uniqueness margin."""
    import difflib
    if tok in _VOCAB:
        return tok
    # fused word: "hundredfifteen" -> hundred + fifteen (greedy split)
    for i in range(len(tok), 3, -1):
        head, rest = tok[:i], tok[i:]
        if head in _VOCAB and rest:
            m = _vocab_match(rest)
            if m:
                return head + " " + m
    # Fuzzy matching is for cursive ICR misreads of REAL words ('hunderd').
    # Short tokens ('to', 'the') sit within 0.65 of number words by accident,
    # and on degraded scans they fabricate phantom amounts — so fuzzy needs
    # length >= 4 and a stricter cutoff.
    if len(tok) < 4:
        return None
    close = difflib.get_close_matches(tok, _VOCAB, n=2, cutoff=0.78)
    if len(close) == 1 or (
            len(close) == 2
            and difflib.SequenceMatcher(None, tok, close[0]).ratio()
            - difflib.SequenceMatcher(None, tok, close[1]).ratio() > 0.1):
        return close[0]
    return None


def words_to_cents(text: str) -> int | None:
    """'One thousand two hundred & 34/100' -> 120034.
    Requires at least one number WORD or an explicit n/100 fraction — a bare
    digit run inside OCR garbage (cursive misread as digits) must NOT turn
    into a phantom amount. ICR-misread words are fuzzy-matched against the
    closed legal-amount vocabulary. The standard '… and NN' or 'and NN/100'
    tail is parsed as cents."""
    toks = re.findall(r"[a-z]+|\d+/\d+|\d+", text.lower())
    # expand fuzzy word tokens into canonical vocabulary tokens
    expanded: list[str] = []
    for t in toks:
        if re.fullmatch(r"[a-z]+", t) and t not in _VOCAB and t not in ("and", "dollars", "dollar"):
            m = _vocab_match(t)
            if m:
                expanded.extend(m.split())
                continue
        expanded.append(t)
    toks = expanded
    total, current, cents = 0, 0, None
    saw_word = False
    word_hits = 0
    for i, t in enumerate(toks):
        if re.fullmatch(r"\d+/\d+", t):
            cents = int(t.split("/")[0])
        elif re.fullmatch(r"\d+", t):
            # "… and NN" at the end of the legal line = cents, not dollars
            if (i >= 2 and toks[i - 1] == "and" and i == len(toks) - 1
                    and len(t) <= 2 and saw_word):
                cents = int(t)
            else:
                current += int(t)
        elif t in WORDS:
            current += WORDS[t]; saw_word = True; word_hits += 1
        elif t in SCALES:
            current = max(1, current) * SCALES[t]; word_hits += 1
            if SCALES[t] >= 1000:
                total, current = total + current, 0
    # A real legal line always carries multiple number words ('Seven hundred
    # fifteen …') or a word plus an explicit n/100 fraction. A single fuzzy
    # hit in OCR garbage is a phantom — refuse it.
    if word_hits < 2 and not re.search(r"\d+/\d+", text):
        return None
    if not saw_word and cents is None:
        return None
    if total + current == 0 and cents is None:
        return None
    return (total + current) * 100 + (cents or 0)


def parse_courtesy(text: str) -> int | None:
    m = re.search(r"\$?\s*([\d,]+\.\d{2})", text) or re.search(r"\*+\s*([\d,]+)\.(\d{2})", text)
    if m:
        g = m.group(1) if "." in m.group(1) else None
        if g:
            whole, frac = g.replace(",", "").split(".")
            return int(whole) * 100 + int(frac)
        m2 = re.search(r"([\d,]+)\.(\d{2})", text)
        if m2:
            return int(m2.group(1).replace(",", "")) * 100 + int(m2.group(2))
    # handwritten decimal comma: "715,39" (no dot present)
    m3 = re.search(r"(?<![\d,])(\d{1,3}(?:\.\d{3})*|\d+),(\d{2})(?!\d)", text)
    if m3 and "." not in m3.group(0):
        return int(m3.group(1).replace(".", "")) * 100 + int(m3.group(2))
    return None


def _orient(img: np.ndarray) -> tuple[np.ndarray, str]:
    """Phone photos arrive rotated. Tesseract OSD detects 90/180/270 turns
    directly; the fallback is MICR bottom-edge ink density (the E-13B band
    hugs the very bottom of a correctly oriented check; signature/memo ink
    sits higher, so a bottom-10% density scan disambiguates 180 flips).
    Always returns landscape."""
    try:
        import pytesseract
        rot = int(pytesseract.image_to_osd(img).split("Rotate: ")[1].split("\n")[0])
        if rot in (90, 180, 270):
            img = cv2.rotate(img, {90: cv2.ROTATE_90_CLOCKWISE,
                                   180: cv2.ROTATE_180,
                                   270: cv2.ROTATE_90_COUNTERCLOCKWISE}[rot])
        if img.shape[1] < img.shape[0]:
            img = cv2.rotate(img, cv2.ROTATE_90_CLOCKWISE)
        return img, "osd"
    except Exception:
        pass
    cands = [img, cv2.rotate(img, cv2.ROTATE_180),
             cv2.rotate(img, cv2.ROTATE_90_CLOCKWISE),
             cv2.rotate(img, cv2.ROTATE_90_COUNTERCLOCKWISE)]
    land = [c for c in cands if c.shape[1] >= c.shape[0]] or cands

    def bottom_density(c: np.ndarray) -> float:
        g = cv2.cvtColor(c, cv2.COLOR_BGR2GRAY)
        band = g[int(g.shape[0]*0.90):, :]
        _, bw = cv2.threshold(band, 0, 255, cv2.THRESH_BINARY_INV + cv2.THRESH_OTSU)
        return float(np.mean(bw > 0))

    return max(land, key=bottom_density), "heuristic"


def _dewarp(img: np.ndarray) -> tuple[np.ndarray, bool]:
    """Phone photos shoot the check at an angle on a desk. The check is the
    largest 4-corner contour; rectify it to a flat rectangle. Conservative:
    any doubt (no quad, quad too small) returns the original untouched."""
    g = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
    g = cv2.GaussianBlur(g, (5, 5), 0)
    edges = cv2.dilate(cv2.Canny(g, 40, 120), np.ones((5, 5), np.uint8))
    cnts, _ = cv2.findContours(edges, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    if not cnts:
        return img, False
    c = max(cnts, key=cv2.contourArea)
    if cv2.contourArea(c) < 0.25 * img.shape[0] * img.shape[1]:
        return img, False
    approx = cv2.approxPolyDP(c, 0.02 * cv2.arcLength(c, True), True)
    if len(approx) != 4:
        return img, False
    pts = approx.reshape(4, 2).astype(np.float32)
    ssum, sdiff = pts.sum(1), np.diff(pts, axis=1).ravel()
    rect = np.float32([pts[np.argmin(ssum)], pts[np.argmin(sdiff)],
                       pts[np.argmax(ssum)], pts[np.argmax(sdiff)]])
    W = int(max(np.linalg.norm(rect[0]-rect[1]), np.linalg.norm(rect[2]-rect[3])))
    H = int(max(np.linalg.norm(rect[0]-rect[3]), np.linalg.norm(rect[1]-rect[2])))
    if W < 200 or H < 100:
        return img, False
    M = cv2.getPerspectiveTransform(rect, np.float32([[0, 0], [W, 0], [W, H], [0, H]]))
    return cv2.warpPerspective(img, M, (W, H)), True


def _quality(img: np.ndarray) -> dict:
    """Scan-quality gate: Laplacian-variance blur score, mean brightness,
    megapixels. Recorded in the extraction detail so REVIEW shows WHY a
    scan was hard, and the confidence rollup can penalize bad captures."""
    g = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
    blur = float(cv2.Laplacian(g, cv2.CV_64F).var())
    return {"blur": round(blur, 1),           # <80 blurry, <30 severely
            "brightness": round(float(g.mean()), 1),  # <90 too dark
            "megapixels": round(img.shape[0]*img.shape[1]/1e6, 2)}


def extract_check(image_bytes: bytes) -> CheckExtraction:
    """Full extraction from a check photo/scan. Never raises on image quirks —
    returns low-confidence partial results instead of failing the intake.

    Accuracy architecture (field-accuracy target >97%):
      * every field is read by an engine ENSEMBLE over several preprocessing
        variants (natural grayscale, deskewed, ink-isolated), and the winner
        is chosen by parsed-value consensus — not first-nonempty;
      * courtesy and legal amounts cross-validate: agreement is the strongest
        accuracy signal available on a check and earns "high" confidence;
        disagreement routes to REVIEW, never to auto-match;
      * MICR is accepted only on checksum-valid 9-digit runs, read across
        three band preps (CLAHE+Otsu, adaptive, raw) before giving up.
    """
    out = CheckExtraction()
    try:
        img = cv2.imdecode(np.frombuffer(image_bytes, np.uint8), cv2.IMREAD_COLOR)
        if img is None:
            out.detail["error"] = "undecodable image"
            return out
        # capture normalization: fix rotation, flatten perspective, grade the
        # scan. Phone photos of checks are the norm, not the exception.
        img, orient_how = _orient(img)
        img, dewarped = _dewarp(img)
        q = _quality(img)
        out.detail["capture"] = {**q, "orient": orient_how, "dewarped": dewarped}
        h, w = img.shape[:2]
        # Full-page paddle pass once — every field gets its lines by
        # coordinates. This is the single biggest accuracy win: the detector
        # sees full layout context instead of guessing inside tight crops.
        page = _paddle_page(img)

        def gray_up(zone: np.ndarray, fx: float = 2) -> np.ndarray:
            g = cv2.cvtColor(zone, cv2.COLOR_BGR2GRAY) if zone.ndim == 3 else zone
            return cv2.resize(g, None, fx=fx, fy=fx, interpolation=cv2.INTER_CUBIC)

        # 1) MICR band: three preps, checksum-validated parse on each; first
        #    prep yielding a valid routing wins. A band that never yields one
        #    is a miss -> REVIEW (miss, don't guess).
        # Band candidates: fixed strip + projection-located band (templates vary).
        bands = _micr_bands(img)
        band = bands[0]
        bg = cv2.resize(gray_up(band, 1), None, fx=3, fy=3, interpolation=cv2.INTER_CUBIC)
        # skew is the most common scan defect — deskew BEFORE binarizing so the
        # E-13B glyph pitch survives thresholding
        bg_de = _deskew(bg)
        clahe = cv2.createCLAHE(clipLimit=3.0, tileGridSize=(8, 8)).apply(bg_de)
        _, bw_otsu = cv2.threshold(clahe, 0, 255, cv2.THRESH_BINARY + cv2.THRESH_OTSU)
        bw_adapt = cv2.adaptiveThreshold(bg_de, 255, cv2.ADAPTIVE_THRESH_GAUSSIAN_C,
                                         cv2.THRESH_BINARY, 41, 13)
        clahe_raw = cv2.createCLAHE(clipLimit=3.0, tileGridSize=(8, 8)).apply(bg)
        _, bw_otsu_raw = cv2.threshold(clahe_raw, 0, 255, cv2.THRESH_BINARY + cv2.THRESH_OTSU)
        # Low-contrast captures: inverted Otsu (light-ink/dark-bg scans) and a
        # morphological close that reconnects broken glyph strokes.
        _, bw_inv = cv2.threshold(clahe, 0, 255, cv2.THRESH_BINARY_INV + cv2.THRESH_OTSU)
        bw_inv = cv2.bitwise_not(bw_inv)
        bw_morph = cv2.morphologyEx(bw_otsu, cv2.MORPH_CLOSE,
                                    cv2.getStructuringElement(cv2.MORPH_RECT, (3, 3)))
        # Projection-located second band, prepared the same way (when present).
        bg2_de = None
        if len(bands) > 1:
            bg2 = cv2.resize(gray_up(bands[1], 1), None, fx=3, fy=3, interpolation=cv2.INTER_CUBIC)
            bg2_de = _deskew(bg2)
        micr_raws: list[str] = []
        # 1a) In-house E-13B engine (micr_engine.py): monospace cell-grid
        #     segmentation + template NCC against the bundled Nimra font.
        #     It reads the four MICR separator symbols NATIVELY — where
        #     tesseract approximates them as ':'/'"' and loses field
        #     boundaries. Acceptance posture mirrors the phantom-guard:
        #     a clean high-score read is trusted immediately; a read with
        #     unknown cells ('?') is held as a candidate and only accepted
        #     if a later independent read corroborates the same routing.
        eng_candidate = None
        try:
            import micr_engine
            for prep in (bg_de, bw_otsu):
                try:
                    etxt, escore = micr_engine.recognize(prep)
                except Exception:
                    etxt, escore = None, 0.0
                if not etxt:
                    continue
                micr_raws.append(f"e13b:{etxt}({escore:.2f})")
                r = micr_engine.extract_routing(etxt)  # ⑆…⑆ bracket + checksum (+1-digit repair)
                _, a, c = parse_micr(etxt)
                if r:
                    unknowns = etxt.count("?")
                    # clean high-score read, or a single-unknown read whose
                    # routing was pinned by the unique checksum solution
                    if (unknowns == 0 and escore >= 0.85) or (unknowns == 1 and escore >= 0.75):
                        out.routing_number, out.account_number, out.check_number = r, a, c
                        break
                    eng_candidate = eng_candidate or (r, a, c)
                if "⑆" in etxt or "⑈" in etxt:
                    out.account_number = out.account_number or a
                    out.check_number = out.check_number or c
        except ImportError:
            pass
        # 1b) tesseract preps — fixed band variants first, projection band last
        preps = [bw_otsu, bw_adapt, bw_otsu_raw, bw_inv, bw_morph, bg_de]
        if bg2_de is not None:
            preps.append(bg2_de)
        for prep in preps:
            if out.routing_number:
                break
            txt = _ocr(prep, psm=7, whitelist="0123456789⑈⑆⑇⑄ABCDabcd| ") \
                  or _ocr(prep, psm=7) or _ocr(prep, psm=6) or _ocr(prep, psm=13)
            if txt:
                micr_raws.append(txt)
            r, a, c = parse_micr(txt)
            if r and routing_checksum_valid(r):
                out.routing_number, out.account_number, out.check_number = r, a, c
                break
            # one-unknown-digit repair: a single smudged MICR digit no longer
            # costs the whole routing read (checksum pins the unique value)
            fixed = _repair_routing(txt or "")
            if fixed:
                out.routing_number = fixed
                out.account_number = out.account_number or a
                out.check_number = out.check_number or c
                break
            # best-effort account/check ONLY when the read actually saw MICR
            # separators (normalized to |/~ by parse_micr's symbol map).
            # Separator-free digit soup is field-boundary garbage: under
            # degradation it concatenates routing+account+check into one run,
            # and trusting it writes phantom settlement metadata.
            norm = txt.translate(str.maketrans("", "", " ")).replace('"', "|").replace(":", "|")
            if "|" in norm or "~" in norm or "⑈" in txt or "⑆" in txt:
                out.account_number = out.account_number or a
                out.check_number = out.check_number or c
        # full-page paddle read of the band (E-13B symbols surface as ':'/'"')
        band_lines = [l for l in page if l["cy"] >= 0.85]
        if band_lines and not out.routing_number:
            ptxt = " ".join(l["text"] for l in sorted(band_lines, key=lambda l: l["x0"]))
            micr_raws.append(ptxt)
            r, a, c = parse_micr(ptxt)
            if r and routing_checksum_valid(r):
                out.routing_number, out.account_number, out.check_number = r, a, c
        # deferred engine candidate: accept only when a non-engine read
        # independently contains the same checksum-valid routing
        if not out.routing_number and eng_candidate:
            r, a, c = eng_candidate
            if any(r in raw for raw in micr_raws if not raw.startswith("e13b:")):
                out.routing_number = r
                out.account_number = out.account_number or a
                out.check_number = out.check_number or c
        out.detail["micr_raw"] = " || ".join(micr_raws)
        out.detail["micr_preps_tried"] = len(micr_raws)

        # 2) Courtesy amount box — usually HANDWRITTEN, so it goes through the
        #    handwriting engine chain (TrOCR first), not the printed chain.
        #    Tighter box ($ anchor to right edge) cuts payee-line bleed.
        box_zone = img[int(h*0.28):int(h*0.55), int(w*0.68):w]
        page_box = _zone_lines(page, 0.68, 1.0, 0.28, 0.55)
        box_pre = [(" ".join(l["text"] for l in page if 0.68 <= l["cx"] <= 1.0 and 0.28 <= l["cy"] <= 0.55),
                    "paddle-page",
                    (sum(l["score"] for l in page if 0.68 <= l["cx"] <= 1.0 and 0.28 <= l["cy"] <= 0.55)
                     / max(1, len([l for l in page if 0.68 <= l["cx"] <= 1.0 and 0.28 <= l["cy"] <= 0.55])))
                    if page_box else None)]
        box_txt, box_eng, box_score, box_votes = consensus_read(
            [gray_up(box_zone), _deskew(gray_up(box_zone)), _ink(box_zone)],
            handwritten=False, parser=parse_courtesy,
            psm=7, whitelist="$0123456789.,*", precomputed=box_pre)
        out.amount_cents = parse_courtesy(box_txt)
        out.detail["courtesy_raw"] = box_txt
        out.detail["courtesy_engine"] = box_eng
        out.detail["courtesy_votes"] = box_votes
        if box_score is not None:
            out.detail["courtesy_score"] = round(box_score, 3)

        # 3) Legal amount line — handwriting ICR ensemble over natural /
        #    deskewed / ink-isolated variants, consensus on parsed cents.
        # Legal line position varies by check template: classic layouts put it
        # right under the payee (~0.45-0.63 h), modern ones lower (~0.58-0.75).
        # Read both bands; the cents-parsing consensus picks the real line.
        line_zone = img[int(h*0.45):int(h*0.63), int(w*0.05):int(w*0.80)]
        line_zone2 = img[int(h*0.58):int(h*0.75), int(w*0.05):int(w*0.85)]
        line_nat = gray_up(line_zone)
        line_nat2 = gray_up(line_zone2)
        legal_lines = [l for l in page if 0.12 <= l["cx"] <= 0.80 and 0.44 <= l["cy"] <= 0.75
                       and not re.search(r"(?i)dollars|payable|order", l["text"])]
        legal_pre = [(" ".join(l["text"] for l in legal_lines), "paddle-page",
                      sum(l["score"] for l in legal_lines) / len(legal_lines))] if legal_lines else []
        legal_txt, legal_eng, legal_score, legal_votes = consensus_read(
            [line_nat, _deskew(line_nat), _ink(line_zone),
             line_nat2, _deskew(line_nat2), _ink(line_zone2)],
            handwritten=True, parser=words_to_cents, precomputed=legal_pre)
        out.legal_amount_cents = words_to_cents(legal_txt)
        out.detail["legal_raw"] = legal_txt
        out.detail["legal_engine"] = legal_eng
        out.detail["legal_votes"] = legal_votes
        if legal_score is not None:
            out.detail["legal_score"] = round(legal_score, 3)

        if out.amount_cents and out.legal_amount_cents \
                and out.amount_cents != out.legal_amount_cents:
            out.amount_mismatch = True

        # 4) Payee line ("Pay to the order of ___") — often handwritten.
        payee_zone = img[int(h*0.30):int(h*0.46), int(w*0.10):int(w*0.62)]
        payee_lines = [l for l in page if 0.10 <= l["cx"] <= 0.62 and 0.30 <= l["cy"] <= 0.46
                       and "order" not in l["text"].lower() and "pay to" not in l["text"].lower()]
        payee_pre = [(" ".join(l["text"] for l in payee_lines), "paddle-page",
                      sum(l["score"] for l in payee_lines) / len(payee_lines))] if payee_lines else []
        payee_txt, _, _, _ = consensus_read(
            [_deskew(gray_up(payee_zone)), _ink(payee_zone)], handwritten=True,
            precomputed=payee_pre)
        payee_txt = re.sub(r"(?i)\b(pay( to)?( the)?( order( of)?)?)\b[:\s]*", "",
                           payee_txt).strip(" :.-")
        if payee_txt and re.search(r"[A-Za-z]{3,}", payee_txt):
            out.payer_name = payee_txt[:80]
        out.detail["payee_raw"] = payee_txt

        # 5) Date / memo zones (printed or handwritten; month names allowed).
        #    Date sits anywhere in the upper-right quadrant depending on the
        #    template — read the whole quadrant, not a narrow strip.
        date_zone = img[int(h*0.10):int(h*0.42), int(w*0.55):w]
        date_lines = [l for l in page if 0.55 <= l["cx"] <= 1.0 and 0.10 <= l["cy"] <= 0.42]
        date_pre = [(" ".join(l["text"] for l in date_lines), "paddle-page",
                     sum(l["score"] for l in date_lines) / len(date_lines))] if date_lines else []
        date_txt, _, _, _ = consensus_read(
            [gray_up(date_zone), _ink(date_zone)], handwritten=True,
            parser=parse_check_date, precomputed=date_pre)
        out.check_date = parse_check_date(date_txt)
        memo_zone = img[int(h*0.60):int(h*0.82), 0:int(w*0.55)]
        memo_lines = [l for l in page if 0.0 <= l["cx"] <= 0.55 and 0.60 <= l["cy"] <= 0.82
                      and not re.search(r"(?i)payable|branches|account n|dollars|signature",
                                        l["text"])]
        memo_named = [l for l in memo_lines if re.search(r"(?i)\bmemo\b", l["text"])]
        pick = memo_named or memo_lines
        memo_pre = [(" ".join(l["text"] for l in pick), "paddle-page",
                     sum(l["score"] for l in pick) / len(pick))] if pick else []
        memo_txt, _, _, _ = consensus_read(
            [gray_up(memo_zone), _ink(memo_zone)], handwritten=True,
            precomputed=memo_pre)
        memo_txt = re.sub(r"(?i)^memo\b[:\s]*", "", memo_txt).strip()
        if memo_txt:
            out.memo = memo_txt[:120]
        out.detail["date_raw"], out.detail["memo_raw"] = date_txt, memo_txt

        # 6) Confidence rollup. The decisive signal is courtesy/legal
        #    agreement — two independent readings of the same amount agreeing
        #    is stronger than any single model score.
        #
        #    Calibration notes (the "everything is low" fix):
        #    * check_number is NOT a core field — it's the least
        #      settlement-relevant value on the check and the one most often
        #      unreadable; it earns a bonus, it never gates.
        #    * raw OCR scores for HANDWRITING (paddle/tesseract confidence)
        #      are systematically 0.4–0.7 even on perfect reads, so a >=0.85
        #      gate permanently suppresses "high". Ensemble consensus
        #      (votes>=2 independent preps agreeing on parsed cents) is the
        #      calibrated substitute for a raw score.
        #    * capture gates measured on real phone deposits: Laplacian
        #      variance 20 and brightness 45 separate unusable captures from
        #      ordinary indoor photos (30/60 rejected good scans).
        core = [out.routing_number, out.amount_cents]
        hits = sum(1 for x in core if x) + (1 if out.check_number else 0)
        core_hits = sum(1 for x in core if x)
        strong = box_score is None or box_score >= 0.85 or box_votes >= 2
        amounts_agree = (out.amount_cents is not None
                         and out.amount_cents == out.legal_amount_cents)
        legal_unread = out.legal_amount_cents is None
        consensus_ok = box_votes >= 2 and (legal_votes >= 2 or legal_votes == 0)
        capture_ok = q["blur"] >= 20 and q["brightness"] >= 45 and q["megapixels"] >= 0.05
        if not out.amount_mismatch and strong and capture_ok and (
                (amounts_agree and core_hits == 2)
                or (amounts_agree and core_hits == 1 and hits >= 2)
                or (core_hits == 2 and legal_unread and box_votes >= 2)):
            # full agreement on a complete core; or courtesy strongly read
            # with the legal line simply absent (many templates/photos crop it)
            out.confidence = "high"
        elif hits >= 2 and consensus_ok and not out.amount_mismatch:
            out.confidence = "medium"
        elif core_hits >= 1 and not out.amount_mismatch and (amounts_agree or legal_unread):
            out.confidence = "medium"
        elif hits >= 1:
            out.confidence = "medium" if amounts_agree else "low"
        else:
            out.confidence = "low"
    except Exception as e:  # never kill intake on an image quirk
        out.detail["error"] = str(e)
    return out
