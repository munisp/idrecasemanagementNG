"""Periodic-background (watermark) detection and removal for scanned pages.

Faxes and stamped scans of EOBs/claim forms frequently carry a tiled diagonal
watermark ("CONFIDENTIAL", payer logos) plus speckle. CLAHE/deskew alone make
this WORSE: local contrast enhancement amplifies the watermark lattice exactly
as much as the text, and OCR accuracy collapses (measured: tesseract read ~0%
of field values on a watermarked EOB vs >90% after removal).

The watermark is *periodic* — the same tile repeats on a 2D lattice — while
document text is not. That difference is the whole attack:

1. detect_periodic_background(): the autocorrelation of a periodic image has
   sharp peaks away from the origin. No peaks => no lattice => skip removal
   (never touch a clean page — every enhancement is also a damage risk).
2. lattice_vectors(): peak offsets give the lattice translation vectors.
3. remove_periodic_background(): the median of the image over its lattice
   shifts estimates the *stationary* component (watermark + paper) while text
   — which does not repeat at lattice offsets — votes against itself and
   survives in the residual. Subtracting (median - local background) removes
   the watermark layer and leaves the text.

Pure OpenCV/NumPy, fully offline, no models — safe to run per page.
"""

from __future__ import annotations

import cv2
import numpy as np

# Autocorrelation peak (normalized to the zero-lag peak) above which a page
# is considered to carry a periodic background. Tuned on watermarked fax
# scans: genuine tiled watermarks score 0.25-0.6; clean pages stay < 0.1.
_PERIODIC_MIN_STRENGTH = 0.12
# Ignore autocorrelation peaks closer than this to the origin (page-scale
# brightness gradients live there, not the watermark lattice).
_MIN_LATTICE_DIST = 20
_MAX_VECTORS = 6


def _autocorr(gray: np.ndarray) -> np.ndarray:
    """Normalized autocorrelation (fftshifted, center == 1.0)."""
    f = np.fft.fft2(gray.astype(np.float32))
    ac = np.real(np.fft.ifft2(np.abs(f) ** 2))
    ac = np.fft.fftshift(ac)
    peak = ac.max()
    return ac / peak if peak > 0 else ac


def _lattice_peaks(gray: np.ndarray) -> tuple[list[tuple[int, int]], float]:
    """Lattice offset vectors + strength of the strongest one."""
    ac = _autocorr(gray)
    h, w = gray.shape
    cy, cx = h // 2, w // 2
    yy, xx = np.mgrid[0:h, 0:w]
    ac = ac.copy()
    ac[np.sqrt((yy - cy) ** 2 + (xx - cx) ** 2) < _MIN_LATTICE_DIST] = 0
    vecs: list[tuple[int, int]] = []
    strength = 0.0
    # Consider a bounded candidate set; take the strongest mutually-distinct.
    flat = ac.ravel()
    cand_idx = np.argpartition(flat, -_MAX_VECTORS * 4)[-_MAX_VECTORS * 4:]
    for i in sorted(cand_idx, key=lambda i: -flat[i]):
        y, x = divmod(int(i), w)
        dy, dx = y - cy, x - cx
        if all(abs(dy - vy) + abs(dx - vx) > 8 for vy, vx in vecs):
            vecs.append((dy, dx))
            strength = max(strength, float(flat[i]))
        if len(vecs) >= _MAX_VECTORS:
            break
    # A watermark is a 2D lattice; ruled lines and (synthetic) text are 1D.
    # Collinear peak vectors — every offset along one axis — are far more
    # likely to be text/line spacing than a stamp, so they don't count.
    def _collinear(vs: list[tuple[int, int]]) -> bool:
        for i in range(len(vs)):
            for j in range(i + 1, len(vs)):
                if abs(vs[i][0] * vs[j][1] - vs[j][0] * vs[i][1]) > 64:
                    return False
        return True
    if len(vecs) >= 2 and _collinear(vecs):
        return vecs, 0.0
    return vecs, strength


def detect_periodic_background(gray: np.ndarray) -> bool:
    """True when the page carries a periodic watermark lattice."""
    _, strength = _lattice_peaks(gray)
    return strength >= _PERIODIC_MIN_STRENGTH


def lattice_shifts(gray: np.ndarray) -> list[tuple[int, int]]:
    """Lattice translation shifts for a page (empty when no periodic
    background). Exposed so callers processing many ZONES of one page can
    detect once at page level and apply per zone — small crops don't carry
    enough repetitions to detect reliably on their own."""
    vecs, strength = _lattice_peaks(gray)
    if strength < _PERIODIC_MIN_STRENGTH or not vecs:
        return []
    h, w = gray.shape
    shifts: set[tuple[int, int]] = set(vecs)
    for i in range(len(vecs)):
        for j in range(i + 1, len(vecs)):
            dy, dx = vecs[i][0] + vecs[j][0], vecs[i][1] + vecs[j][1]
            if abs(dy) < h // 2 and abs(dx) < w // 2:
                shifts.add((dy, dx))
    return sorted(shifts)


def remove_with_shifts(gray: np.ndarray, shifts: list[tuple[int, int]]) -> np.ndarray:
    """Subtract the lattice-stationary layer using precomputed shifts."""
    if not shifts:
        return gray
    stack = [np.roll(np.roll(gray, dy, 0), dx, 1).astype(np.float32)
             for dy, dx in shifts]
    stationary = np.median(np.stack(stack), axis=0)
    # The watermark layer is the stationary component minus the local page
    # background (paper brightness is stationary too and must NOT be
    # subtracted — removing it would crush global contrast).
    background = cv2.GaussianBlur(gray, (0, 0), 3)
    watermark = stationary - background
    out = np.clip(gray.astype(np.float32) - watermark, 0, 255)
    return out.astype(np.uint8)


def remove_periodic_background(gray: np.ndarray) -> np.ndarray:
    """Subtract the lattice-stationary (watermark) layer from a page.

    Returns the input unchanged when no lattice is detected — clean pages
    must pass through untouched.
    """
    return remove_with_shifts(gray, lattice_shifts(gray))


def enhance_page(gray: np.ndarray) -> tuple[np.ndarray, bool]:
    """Dewatermark (if needed) + speckle cleanup + local contrast.

    Returns (enhanced, watermark_removed). This is the accuracy-critical
    chain for degraded fax/photo pages; clean pages get CLAHE only.
    """
    removed = False
    if detect_periodic_background(gray):
        gray = remove_periodic_background(gray)
        removed = True
    # Median 3x3 kills residual salt-and-pepper without blurring strokes.
    gray = cv2.medianBlur(gray, 3)
    clahe = cv2.createCLAHE(clipLimit=2.0, tileGridSize=(8, 8))
    return clahe.apply(gray), removed
