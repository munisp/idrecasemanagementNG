"""Pure-function tests for check extraction (no images needed)."""
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
