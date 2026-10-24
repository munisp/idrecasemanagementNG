# Author: Patrick Munis at newwave
"""dewatermark: periodic-background detection + removal.

Synthetic fixtures built in-test: a clean text page must pass through
untouched; the same page with a tiled diagonal watermark lattice added must
be detected and the watermark measurably removed (residual correlation
drops, text pixels survive).
"""

import cv2
import numpy as np

import dewatermark


def _text_page(w: int = 800, h: int = 1000) -> np.ndarray:
    page = np.full((h, w), 220, np.uint8)
    for i, line in enumerate(["HEALTH INSURANCE CLAIM FORM", "Payer Name: Sunshine Coast",
                              "Member ID: SCH884120077", "Claim Number: NGH-2026-004417",
                              "Billed Amount: $1,850.00"]):
        cv2.putText(page, line, (40, 80 + i * 60), cv2.FONT_HERSHEY_SIMPLEX,
                    0.8, 30, 2, cv2.LINE_AA)
    return page


def _watermark_tile(size: int = 120) -> np.ndarray:
    tile = np.zeros((size, size), np.uint8)
    cv2.putText(tile, "VOID", (5, size // 2), cv2.FONT_HERSHEY_SIMPLEX,
                1.0, 60, 2, cv2.LINE_AA)
    m = cv2.getRotationMatrix2D((size // 2, size // 2), -30, 1.0)
    return cv2.warpAffine(tile, m, (size, size))


def _add_watermark(page: np.ndarray) -> np.ndarray:
    h, w = page.shape
    tile = _watermark_tile()
    tiled = np.tile(tile, (h // tile.shape[0] + 2, w // tile.shape[1] + 2))[:h, :w]
    # watermark darkens the page where its ink is
    return np.clip(page.astype(np.int16) - tiled.astype(np.int16), 0, 255).astype(np.uint8)


def test_clean_page_is_untouched():
    page = _text_page()
    assert not dewatermark.detect_periodic_background(page)
    out = dewatermark.remove_periodic_background(page)
    assert np.array_equal(out, page)
    assert dewatermark.lattice_shifts(page) == []


def test_watermark_detected_and_removed():
    page = _text_page()
    wm = _add_watermark(page)
    assert dewatermark.detect_periodic_background(wm)
    out = dewatermark.remove_periodic_background(wm)
    # Text survival: pixels that were dark in the original text stay darkish.
    text_mask = page < 100
    assert text_mask.sum() > 500
    survival = (out[text_mask] < 160).mean()
    assert survival > 0.85, f"text survival {survival:.2%}"
    # Watermark suppression: on originally-clean paper, the watermark's dark
    # ink must be substantially lighter after removal.
    wm_ink = (wm < 150) & (page > 200)
    if wm_ink.sum() > 100:
        assert out[wm_ink].mean() > wm[wm_ink].mean() + 30


def test_remove_with_shifts_matches_detect_then_remove():
    page = _add_watermark(_text_page())
    shifts = dewatermark.lattice_shifts(page)
    assert shifts
    a = dewatermark.remove_periodic_background(page)
    b = dewatermark.remove_with_shifts(page, shifts)
    assert np.array_equal(a, b)


def test_enhance_page_marks_removal():
    page = _add_watermark(_text_page())
    _, removed = dewatermark.enhance_page(page)
    assert removed
    clean, removed2 = dewatermark.enhance_page(_text_page())
    assert not removed2
    assert clean.shape == _text_page().shape
