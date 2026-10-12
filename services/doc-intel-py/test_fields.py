"""Tests for deterministic field extraction: EOB money labels, identifiers,
remark/diagnosis codes, extraction_score read-selection, and cross_check
remittance arithmetic."""

import fields


EOB_TEXT = """
Gulfstream Health Plan of Florida
Explanation of Benefits
CLAIM INFORMATION
Claim Number: MBC-2026-T-8841
Provider: Bayfront Medical Center
Provider NPI: 1164517486
Member Name: Marisol Ortega
Member ID: SCH-884120077
Service Date: 03/12/2026
Determination Date: 04/08/2026
Network Status: Out-of-Network
SERVICE LINES
Billed Amount $21,800.00
Allowed Amount $18,650.00
Plan Paid $17,337.60
Patient Responsibility $612.50
Denial codes: CO-45, CO-97
Diagnosis: I10
"""


def test_canonical_money_labels():
    ex = fields.extract_fields(EOB_TEXT)
    canon = {a["canonical"]: a["cents"] for a in ex["amounts"]}
    assert canon["billed"] == 2_180_000
    assert canon["allowed"] == 1_865_000
    assert canon["paid"] == 1_733_760
    assert canon["patient_resp"] == 61_250


def test_member_id_and_taxonomy():
    ex = fields.extract_fields(EOB_TEXT)
    assert ex["identifiers"]["member_id"] == "SCH-884120077"
    assert "npi" in ex["identifiers"]
    ex2 = fields.extract_fields("Taxonomy 207Q00000X Family Medicine")
    assert ex2["identifiers"]["taxonomy"] == "207Q00000X"


def test_remark_codes_normalized():
    ex = fields.extract_fields(EOB_TEXT)
    assert "CO-45" in ex["adjustment_codes"]
    assert "CO-97" in ex["adjustment_codes"]
    # unhyphenated input normalizes to hyphenated form
    ex2 = fields.extract_fields("adjustment CO45 and PR2")
    assert "CO-45" in ex2["adjustment_codes"]
    assert "PR-2" in ex2["adjustment_codes"]


def test_diagnosis_requires_context():
    ex = fields.extract_fields(EOB_TEXT)
    assert "I10" in ex["diagnosis_codes"]
    # a bare token without diagnostic context must not be picked up
    ex2 = fields.extract_fields("the form I10 was filed in box 4")
    assert "diagnosis_codes" not in ex2 or "I10" not in ex2["diagnosis_codes"]


def test_tagged_dates():
    ex = fields.extract_fields(EOB_TEXT)
    dates = {d["label"]: d["iso"] for d in ex["dates"]}
    assert dates["service date"] == "2026-03-12"
    assert dates["determination date"] == "2026-04-08"


def test_extra_labels_learned_synonym():
    text = "Carrier Allowance $412.00\nTotal Billed $900.00"
    base = fields.extract_fields(text)
    assert not any(a["canonical"] == "allowed" for a in base["amounts"])
    learned = fields.extract_fields(text, extra_labels={"allowed": ["carrier allowance"]})
    assert any(a["canonical"] == "allowed" and a["cents"] == 41_200
               for a in learned["amounts"])


def test_extraction_score_prefers_fields_over_noise():
    noise = " ".join(f"x{i}#" for i in range(400))
    assert fields.extraction_score(EOB_TEXT) > fields.extraction_score(noise)
    assert fields.extraction_score("") == 0.0


def test_cross_check_balanced_eob_is_quiet():
    ex = fields.extract_fields(EOB_TEXT)
    # paid + patient_resp = 1,733,760 + 61,250 = 1,795,010 vs allowed 1,865,000
    # -> genuinely unbalanced fixture, so this one SHOULD flag
    issues = fields.cross_check(ex)
    assert any("does not balance" in i for i in issues)


def test_cross_check_balanced_passes():
    text = ("Allowed Amount $100.00\nPlan Paid $80.00\n"
            "Patient Responsibility $20.00\nTotal Billed $150.00")
    issues = fields.cross_check(fields.extract_fields(text))
    assert not any("does not balance" in i or "exceeds" in i for i in issues)


def test_cross_check_impossible_arithmetic():
    text = "Total Billed $100.00\nAllowed Amount $250.00"
    issues = fields.cross_check(fields.extract_fields(text))
    assert any("exceeds billed" in i for i in issues)


def test_cross_check_bad_npi_flagged():
    text = "Provider NPI: 1234567890"
    ex = fields.extract_fields(text)
    assert ex["identifiers"]["npi"]["luhn_valid"] is False
    assert any("Luhn" in i for i in fields.cross_check(ex))


def test_valid_npi_passes_luhn():
    # Luhn-valid with the 80840 healthcare prefix per the CMS NPI spec
    assert fields.valid_npi("1487652392")
    assert not fields.valid_npi("1487652390")
