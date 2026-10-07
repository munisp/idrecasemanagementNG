"""In-house MICR E-13B recognition engine (inspired by ultimateMICR's
one-shot detector+recognizer design, rebuilt dependency-free).

Why in-house: the E-13B alphabet is CLOSED — 14 glyphs total (0-9 plus
transit ⑆, amount ⑇, on-us ⑈, dash ⑉) printed by a single magnetic-ink
font with fixed geometry. A general OCR (tesseract/paddle) wastes its
capacity approximating the four separator symbols as ':' / '"' / 'A' —
which is exactly where field boundaries get lost and phantom digit soup
gets fabricated. A purpose-built recognizer only needs to answer one
question: which of the 14 known glyph shapes is this ink blob?

Architecture (v1 — template matching, no training data required):
  1. binarize the MICR band (Otsu on the input, which may be gray or an
     already-thresholded prep from the caller)
  2. crop to the vertical ink extent
  3. segment glyphs by vertical-projection valleys; over-wide segments
     (touching glyphs) are recursively split at their weakest column
  4. classify each segment against font-rendered templates with
     normalized cross-correlation (TM_CCOEFF_NORMED), aspect-ratio
     penalized — scale-invariant because both sides are normalized to a
     fixed ink height
  5. a segment scoring below the acceptance floor emits '?' (which
     SPLITS digit runs in parse_micr) — miss, don't guess

Templates are rendered from the bundled Nimra E-13B typeface
(fonts/Nimra-E13B.ttf, SIL OFL 1.1 — see fonts/OFL-Nimra.txt), so the
shapes are true E-13B geometry rather than approximations.

The module also ships a synthetic-line generator (render_line/degrade)
used by the test-suite to produce clean and torturously degraded MICR
bands — that pair doubles as the training-data generator should a v2
CRNN (CNN+BiLSTM+CTC on torch, already a doc-intel dependency) replace
template matching later.
"""

from __future__ import annotations

import os

import cv2
import numpy as np

_FONT_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                          "fonts", "Nimra-E13B.ttf")

TRANSIT, AMOUNT, ONUS, DASH = "⑆", "⑇", "⑈", "⑉"
CHARSET = "0123456789" + TRANSIT + AMOUNT + ONUS + DASH

TMPL_H = 64           # template ink height in px
ACCEPT_FLOOR = 0.40   # below this NCC the glyph is unknown -> '?'
MIN_MARGIN = 0.06     # top-2 NCC gap; below it the glyph is ambiguous -> '?'
_ASPECT_PENALTY = 0.80

_templates: dict | None = None


def _render_glyph(ch: str) -> np.ndarray | None:
    """Render one glyph from the bundled font, tight-cropped and scaled
    to TMPL_H ink height. Ink is white (255) on black (0)."""
    from PIL import Image, ImageDraw, ImageFont
    font = ImageFont.truetype(_FONT_PATH, TMPL_H * 2)
    img = Image.new("L", (TMPL_H * 4, TMPL_H * 3), 0)
    ImageDraw.Draw(img).text((TMPL_H // 2, TMPL_H // 2), ch, font=font, fill=255)
    a = np.asarray(img)
    ys, xs = np.nonzero(a > 100)
    if xs.size == 0:
        return None
    crop = a[ys.min():ys.max() + 1, xs.min():xs.max() + 1]
    w = max(4, int(round(crop.shape[1] * TMPL_H / crop.shape[0])))
    return cv2.resize(crop, (w, TMPL_H), interpolation=cv2.INTER_AREA)


def templates() -> dict:
    """Lazily rendered {char: float32 template}. Empty if the font is
    missing — callers then treat the engine as unavailable."""
    global _templates
    if _templates is None:
        _templates = {}
        if os.path.exists(_FONT_PATH):
            for ch in CHARSET:
                t = _render_glyph(ch)
                if t is not None:
                    _templates[ch] = t.astype(np.float32)
    return _templates


def _binarize(img: np.ndarray) -> np.ndarray:
    """Any input (BGR, gray, or binary prep) -> binary, ink=255, with
    scan-speckle suppression: median pre-filter, then connected-component
    area culling so dust noise can't fake the ink extent or pitch grid."""
    g = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY) if img.ndim == 3 else img
    if len(np.unique(g)) <= 4:  # already a thresholded prep — normalize polarity
        bw = g if g.mean() > 127 else 255 - g
    else:
        g = cv2.medianBlur(g, 3)
        clahe = cv2.createCLAHE(clipLimit=3.0, tileGridSize=(8, 8)).apply(g)
        _, t = cv2.threshold(clahe, 0, 255, cv2.THRESH_BINARY + cv2.THRESH_OTSU)
        bw = 255 - t  # THRESH_BINARY puts ink low; we want ink=255
    n, labels, stats, _ = cv2.connectedComponentsWithStats(bw, connectivity=8)
    if n <= 1:
        return bw
    # scale the speckle floor to the largest glyph component, not the
    # canvas (the deskew pad inflates canvas height; glyph pieces like
    # the transit square run ~1/6 of a big digit's area)
    min_area = max(8, int(stats[1:, cv2.CC_STAT_AREA].max() * 0.012))
    keep = np.nonzero(stats[1:, cv2.CC_STAT_AREA] >= min_area)[0] + 1
    return np.where(np.isin(labels, keep), 255, 0).astype(np.uint8)


def _deskew_gray(g: np.ndarray) -> np.ndarray:
    """Straighten the band: the cell grid assumes horizontal baselines.
    Projection-profile sweep — a horizontal text line maximizes the
    variance of its row projection; far more robust on long thin MICR
    bands than minAreaRect (whose rect angle is ambiguous when the ink
    is 20x wider than tall)."""
    _, t = cv2.threshold(g, 0, 255, cv2.THRESH_BINARY_INV + cv2.THRESH_OTSU)
    if cv2.countNonZero(t) < 50:
        return g
    h, w = g.shape
    small = cv2.resize(t, (min(w, 900), max(8, int(h * min(w, 900) / w))),
                       interpolation=cv2.INTER_AREA)
    best_a, best_v = 0.0, -1.0
    for a10 in range(-100, 101, 5):  # -10.0°..+10.0° in 0.5° steps
        ang = a10 / 10.0
        M = cv2.getRotationMatrix2D((small.shape[1] / 2, small.shape[0] / 2), ang, 1.0)
        rot = cv2.warpAffine(small, M, (small.shape[1], small.shape[0]), borderValue=0)
        v = float(rot.sum(axis=1).var())
        if v > best_v:
            best_v, best_a = v, ang
    if abs(best_a) < 0.3:
        return g
    # rotate on a vertically padded canvas — otherwise the correction
    # itself swings off-center glyph bottoms out of frame and clips them
    p = int(h * 0.8)
    gp = cv2.copyMakeBorder(g, p, p, 0, 0, cv2.BORDER_CONSTANT, value=255)
    M = cv2.getRotationMatrix2D((w / 2, (h + 2 * p) / 2), best_a, 1.0)
    return cv2.warpAffine(gp, M, (w, h + 2 * p), borderValue=255)


def _cells(bw: np.ndarray, pitch_prior: float) -> tuple[list[tuple[int, int]], float]:
    """Cell-grid segmentation exploiting E-13B's strictly monospace pitch
    (every glyph — digits AND the four multi-part symbols — occupies one
    1250-unit advance). Projection-gap segmentation shatters the transit
    symbol (three disjoint blobs) and fuses touching digits; the cell
    grid cuts at pitch boundaries instead.

    Pitch and phase are found by grid search: the correct grid puts its
    cut lines in the inter-glyph gaps, i.e. minimizes ink on cut lines.
    The pitch prior (advance/cap-height ≈ 1250/1170 ≈ 1.068 × ink height)
    bounds the search to ±12%."""
    h, w = bw.shape
    proj = bw.astype(np.float32).sum(axis=0) / 255.0
    cols = np.nonzero(proj > max(1.0, h * 0.03))[0]
    if cols.size == 0:
        return []
    first, last = int(cols[0]), int(cols[-1])
    span = last - first + 1
    # Pitch from projection autocorrelation: a MICR line is periodic at
    # the glyph advance. The search is BOUNDED around the cap-height
    # prior (advance/cap = 1250/1170) — an unbounded autocorrelation
    # locks onto the 2x harmonic whenever digit runs repeat structure.
    lo_lag = max(4, int(pitch_prior * 0.82))
    hi_lag = int(pitch_prior * 1.18)
    if span < lo_lag:
        return [], 0.0
    pj = proj - proj.mean()
    ac = np.correlate(pj, pj, mode="full")[len(pj) - 1:]
    hi_lag = min(hi_lag, len(ac) - 1)
    if hi_lag <= lo_lag:
        return [], 0.0
    pitch0 = lo_lag + int(np.argmax(ac[lo_lag:hi_lag]))
    best = None  # (cut_ink, pitch, phase)
    for p in np.linspace(pitch0 * 0.94, pitch0 * 1.06, 25):
        for k in range(12):
            phase = first - p / 2 + k * p / 11
            cuts = []
            i = 0
            while True:
                c = int(round(phase + i * p))
                i += 1
                if c > last:
                    break
                if c >= first:
                    cuts.append(c)
            ink = sum(proj[c] for c in cuts if 0 <= c < w)
            if best is None or ink < best[0]:
                best = (ink, p, phase)
    if best is None:
        return [], 0.0
    _, p, phase = best
    # Extend the grid past the ink span: the first/last glyph's ink does
    # not fill its cell, so span-based cell counts truncate edge glyphs.
    # Empty cells are dropped later by the classifier.
    i0 = int(np.floor((first - phase) / p)) - 1
    i1 = int(np.ceil((last - phase) / p)) + 1
    cells = []
    for i in range(i0, i1 + 1):
        x0 = int(round(phase + i * p))
        x1 = int(round(phase + (i + 1) * p)) - 1
        x0, x1 = max(0, x0), min(w - 1, x1)
        if x1 > x0:
            cells.append((x0, x1))
    # Trim edge cells holding only ink slivers (grid overhang): a real
    # glyph cell carries ink comparable to the median cell.
    if cells:
        areas = np.array([proj[a:b + 1].sum() for a, b in cells])
        med = np.median(areas[areas > 0]) if (areas > 0).any() else 0.0
        lo, hi = 0, len(cells) - 1
        while lo <= hi and areas[lo] < med * 0.25:
            lo += 1
        while hi >= lo and areas[hi] < med * 0.25:
            hi -= 1
        cells = cells[lo:hi + 1]
    return cells, p


def _classify(bw: np.ndarray, x0: int, x1: int) -> tuple[str, float]:
    seg_full = bw[:, x0:x1 + 1]
    ys, xs = np.nonzero(seg_full > 100)
    if xs.size == 0 or xs.size < bw.shape[0] * 0.5:  # empty cell (space)
        return "", 0.0
    seg = seg_full[ys.min():ys.max() + 1, xs.min():xs.max() + 1].astype(np.float32)
    seg_aspect = seg.shape[1] / max(1, seg.shape[0])
    best_ch, best, second = "?", 0.0, 0.0
    for ch, t in templates().items():
        t_aspect = t.shape[1] / t.shape[0]
        resized = cv2.resize(seg, (t.shape[1], t.shape[0]), interpolation=cv2.INTER_AREA)
        score = float(cv2.matchTemplate(resized, t, cv2.TM_CCOEFF_NORMED)[0, 0])
        if abs(seg_aspect - t_aspect) > 0.35:
            score *= _ASPECT_PENALTY
        if score > best:
            second, best, best_ch = best, score, ch
        elif score > second:
            second = score
    # Ambiguity guard: a blurred glyph that sits between two templates
    # must not be committed — substitution errors on digits fabricate
    # checksum-valid routing numbers. '?' splits the run instead.
    if best < ACCEPT_FLOOR or best - second < MIN_MARGIN:
        return "?", best
    return best_ch, best


def recognize(band_img: np.ndarray) -> tuple[str | None, float]:
    """Recognize a MICR band image -> (text with ⑆⑇⑈⑉ symbols, mean NCC
    score). Returns (None, 0.0) when nothing readable — the caller keeps
    its miss-don't-guess posture."""
    tmpls = templates()
    if not tmpls or band_img is None or band_img.size == 0:
        return None, 0.0
    g = cv2.cvtColor(band_img, cv2.COLOR_BGR2GRAY) if band_img.ndim == 3 else band_img
    g = _deskew_gray(g)
    bw = _binarize(g)
    # crop to the dominant ink row-band (glyph height), not the raw ink
    # extent — residual speckle rows would otherwise stretch it
    rows = bw.sum(axis=1) / 255.0
    peak = rows.max()
    if peak < 2:
        return None, 0.0
    ys = np.nonzero(rows > peak * 0.12)[0]
    if ys.size == 0 or ys.max() - ys.min() < 8:
        return None, 0.0
    band_h = ys.max() - ys.min() + 1
    pad_v = max(3, int(band_h * 0.35))  # residual skew shifts edge glyphs off-band
    bw = bw[max(0, ys.min() - pad_v):ys.max() + pad_v + 1, :]
    bw = np.pad(bw, ((0, 0), (4, 4)), constant_values=0)
    cells, pitch = _cells(bw, 1.068 * band_h)
    # Per-cell realignment: cumulative pitch error and residual skew shift
    # late cells off their glyphs; nudging each cell ±8% of the pitch and
    # keeping the best-scoring alignment absorbs the drift. Without this,
    # a drifted cell straddles two glyphs and template-matching commits
    # to the WRONG digit with high confidence — a phantom factory.
    nudge = max(1, int(round(pitch * 0.08))) if pitch else 0
    chars, scores = [], []
    for x0, x1 in cells:
        ch, s = _classify(bw, x0, x1)
        if ch != "" and nudge:
            for dx in (-2 * nudge, -nudge, nudge, 2 * nudge):
                a, b = x0 + dx, x1 + dx
                if a < 0 or b >= bw.shape[1]:
                    continue
                ch2, s2 = _classify(bw, a, b)
                if ch2 != "" and s2 > s + 0.02:
                    ch, s = ch2, s2
        if ch == "":
            continue
        chars.append(ch)
        scores.append(s)
    text = "".join(chars).strip()
    if not text or text == "?" * len(text):
        return None, 0.0
    return text, float(np.mean(scores)) if scores else 0.0


def _aba_checksum_valid(routing: str) -> bool:
    """ABA 3-7-1 checksum (mirrors check_processor.routing_checksum_valid;
    duplicated here to keep this module import-cycle-free)."""
    d = [int(c) for c in routing]
    return (3 * (d[0] + d[3] + d[6]) + 7 * (d[1] + d[4] + d[7])
            + (d[2] + d[5] + d[8])) % 10 == 0


def extract_routing(text: str | None) -> str | None:
    """Structurally validated routing extraction. On a real US check the
    routing field is bracketed by TRANSIT symbols on BOTH sides
    (⑆123456789⑆). Degraded template reads occasionally substitute a
    digit into a checksum-valid-looking 9-run (~10% chance per random
    run) — requiring the ⑆…⑆ bracket on top of the checksum kills those
    phantoms structurally: a substituted digit can't forge a symbol."""
    if not text:
        return None
    import re
    # Primary: ⑆…⑆. Fallback: left bracket read as ⑈ (the line-initial
    # glyph is the one most often clipped by band cropping — a half-inked
    # transit loses its lower square and looks like an on-us). The right
    # bracket stays strict: account fields end in ⑈, so relaxing THAT
    # side would admit account-field phantoms.
    for m in re.finditer(r"(?:^|[^0-9?])[⑈⑆]([0-9?]{9})⑆", text):
        run = m.group(1)
        if "?" not in run:
            if _aba_checksum_valid(run):
                return run
            continue
        # Checksum repair: the ABA checksum is linear with coefficients
        # 3/7/1, all coprime to 10 — so exactly ONE unknown digit has a
        # UNIQUE solution. This is the same error-correction real MICR
        # readers perform; two unknowns have 10 solutions -> ambiguous.
        if run.count("?") == 1:
            idx = run.index("?")
            coef = (3, 7, 1)[idx % 3]
            total = sum((3, 7, 1)[i % 3] * int(d)
                        for i, d in enumerate(run) if d != "?")
            need = (-total * pow(coef, -1, 10)) % 10
            fixed = run[:idx] + str(need) + run[idx + 1:]
            if _aba_checksum_valid(fixed):
                return fixed
    return None


# ---------------------------------------------------------------------------
# Synthetic line generator (tests today, CRNN training data tomorrow)
# ---------------------------------------------------------------------------

def render_line(text: str, ink_height: int = 30, pad: int = 24) -> np.ndarray:
    """Render a MICR line with the bundled font -> grayscale image
    (black ink on white paper), ink_height in px."""
    from PIL import Image, ImageDraw, ImageFont
    font = ImageFont.truetype(_FONT_PATH, int(ink_height * 1.7))
    width = int(font.getlength(text)) + pad * 2 + 8
    img = Image.new("L", (width, ink_height * 3), 255)
    ImageDraw.Draw(img).text((pad, ink_height), text, font=font, fill=0)
    return np.asarray(img)


def degrade(img: np.ndarray, *, blur: float = 0.0, skew_deg: float = 0.0,
            noise: float = 0.0, gamma: float = 1.0, jpeg: int = 0,
            seed: int = 42) -> np.ndarray:
    """Apply scan-degradation to a synthetic band: gaussian blur, affine
    skew, gaussian noise, brightness gamma, optional JPEG round-trip."""
    out = img.copy()
    if skew_deg:
        h, w = out.shape
        M = cv2.getRotationMatrix2D((w / 2, h / 2), skew_deg, 1.0)
        out = cv2.warpAffine(out, M, (w, h), borderValue=255)
    if blur:
        k = max(3, int(blur) | 1)
        out = cv2.GaussianBlur(out, (k, k), blur / 2)
    if noise:
        rng = np.random.default_rng(seed)
        out = np.clip(out.astype(np.float32)
                      + rng.normal(0, noise, out.shape), 0, 255).astype(np.uint8)
    if gamma != 1.0:
        out = np.clip((out.astype(np.float32) / 255) ** gamma * 255, 0, 255).astype(np.uint8)
    if jpeg:
        _, enc = cv2.imencode(".jpg", out, [cv2.IMWRITE_JPEG_QUALITY, jpeg])
        out = cv2.imdecode(enc, cv2.IMREAD_GRAYSCALE)
    return out
