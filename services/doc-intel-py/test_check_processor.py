"""Pure-function tests for check extraction (no images needed)."""
import numpy as np

import check_processor as cp


def test_routing_checksum():
    assert cp.routing_checksum_valid("111000025")   # real ABA example
    assert not cp.routing_checksum_valid("111000026")


def test_parse_courtesy():
    assert cp.parse_courtesy("$1,234.56") == 123456
    assert cp.parse_courtesy("**1800.00**") == 180000
    assert cp.parse_courtesy("no amount") is None


def test_words_to_cents():
    assert cp.words_to_cents("One thousand two hundred thirty four & 56/100 dollars") == 123456
    assert cp.words_to_cents("Nine hundred") == 90000
    assert cp.words_to_cents("Twenty-five & 00/100") == 2500
    assert cp.words_to_cents("") is None


def test_parse_micr_transit_first():
    r, a, c = cp.parse_micr("⑈111000025⑈ 12345678901⑆0001")
    assert (r, a, c) == ("111000025", "12345678901", "0001")


def test_icr_engine_order():
    # explicit selection is honored; unknown values fall back to auto chain
    cp._ENGINE = "paddle"
    assert cp._icr_order() == ["paddle", "tesseract"]
    cp._ENGINE = "trocr"
    assert cp._icr_order()[0] == "trocr"
    cp._ENGINE = "bogus"
    assert cp._icr_order() == ["trocr", "paddle", "tesseract"]
    cp._ENGINE = "auto"


def test_handwritten_line_graceful_without_engines(monkeypatch):
    # Neither TrOCR nor Paddle available (CI) and a blank image — the chain
    # must degrade to tesseract/none and never raise.
    monkeypatch.setattr(cp, "_trocr", lambda: None)
    monkeypatch.setattr(cp, "_paddle", lambda: None)
    monkeypatch.setattr(cp, "_ocr", lambda *a, **k: "")
    txt, eng, score = cp.read_handwritten_line(np.zeros((40, 400), np.uint8))
    assert txt == "" and eng in ("tesseract", "none") and score is None


def test_handwritten_line_prefers_first_engine_that_reads(monkeypatch):
    monkeypatch.setattr(cp, "_trocr_read", lambda img: ("Sixty & 00/100", None))
    monkeypatch.setattr(cp, "_paddle_read", lambda img: ("WRONG", 0.99))
    txt, eng, _ = cp.read_handwritten_line(np.zeros((40, 400), np.uint8))
    assert eng == "trocr" and cp.words_to_cents(txt) == 6000


def test_printed_zone_falls_back_to_tesseract(monkeypatch):
    monkeypatch.setattr(cp, "_paddle_read", lambda img: ("", None))
    monkeypatch.setattr(cp, "_ocr", lambda *a, **k: "$412.00")
    txt, eng, score = cp.read_printed_zone(np.zeros((40, 400), np.uint8))
    assert (txt, eng, score) == ("$412.00", "tesseract", None)
    assert cp.parse_courtesy(txt) == 41200
