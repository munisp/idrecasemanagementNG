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


def test_parse_micr_rejects_fake_check_soup():
    # Digit soup from E-13B symbols reading as digit fragments. The soup
    # contains a 9-window passing the ABA checksum BY CHANCE ("789012115"):
    # sliding extraction would fabricate a routing number. Miss, don't guess.
    r, a, c = cp.parse_micr("45678901211565432109812890987654321091")
    assert r is None and a is not None  # partial metadata only


def test_parse_micr_rejects_bad_checksum_run():
    r, _, _ = cp.parse_micr("⑈111000026⑈ 12345678901⑆0001")  # off-by-one digit
    assert r is None


def test_words_to_cents_rejects_wordless_garbage():
    # cursive misread as random digits must not become a phantom amount
    assert cp.words_to_cents("= tee esa : if flesdend 8 Do.") is None
    assert cp.words_to_cents("Seven hundred fifteen and 39/100") == 71539
    assert cp.words_to_cents("715 and 39/100") == 71539  # fraction-marked is ok


def test_parse_check_date_month_names():
    assert cp.parse_check_date("DATE: Aug. 11, 2019") == "2019-08-11"
    assert cp.parse_check_date("08/11/2019") == "2019-08-11"
    assert cp.parse_check_date("March 3, 26") == "2026-03-03"
    assert cp.parse_check_date("no date here") is None


def test_parse_courtesy_handwritten_decimal_comma():
    assert cp.parse_courtesy("715,39") == 71539
    assert cp.parse_courtesy("$1,234.56") == 123456  # thousands comma still wins
    assert cp.parse_courtesy("no amount") is None
