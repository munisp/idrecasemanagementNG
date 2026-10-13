"""Verdict logic for TPA onboarding packages: slot expectations, conservative
fail-open (illegible/ambiguous -> NEEDS_REVIEW, confident wrong-type ->
REJECTED), and keyword floors for the non-classified slots."""

import tpa_docs
from pipeline import classify_text
from fields import extract_fields


def verdict(slot, text, filename="doc.pdf"):
    return tpa_docs._verdict(slot, filename, text, classify_text, extract_fields)


EOB_TEXT = ("EXPLANATION OF BENEFITS. Claim Number: CLM-99812. Patient responsibility "
            "$412.00. Allowed amount $1,050.00. Adjustment amount $638.00. CPT 99285. "
            "Coinsurance and deductible applied.") * 2

ITEMIZED_TEXT = ("ITEMIZED BILL UB-04. CMS-1500 claim form. HCPCS CPT 99283 71046. "
                 "Place of service 23. Billed amount $2,400.00. Diagnosis code S06.0X0A. "
                 "Service date 01/14/2026. NPI 1234567893. Revenue code 0450.") * 2


def test_eob_accepted():
    status, reasons = verdict("EOB", EOB_TEXT)
    assert status == "ACCEPTED", reasons


def test_itemized_bill_accepted():
    status, reasons = verdict("ITEMIZED_BILL", ITEMIZED_TEXT)
    assert status == "ACCEPTED", reasons


def test_type_mismatch_rejected():
    # A clear EOB uploaded into the ITEMIZED_BILL slot is confidently wrong.
    status, reasons = verdict("ITEMIZED_BILL", EOB_TEXT, filename="eob.pdf")
    assert status == "REJECTED"
    assert any(r["code"] == "TYPE_MISMATCH" for r in reasons)


def test_illegible_needs_review():
    status, reasons = verdict("EOB", "")
    assert status == "NEEDS_REVIEW"
    assert reasons[0]["code"] == "LEGIBILITY_LOW"


def test_unrelated_needs_review_not_rejected():
    status, reasons = verdict("EOB", "Dear patient, thank you for visiting our clinic. " * 10)
    assert status == "NEEDS_REVIEW"
    assert reasons[0]["code"] in ("TYPE_UNCLEAR", "FIELDS_MISSING")


def test_attestation_keyword_floor():
    status, _ = verdict("IDRE_ATTESTATION",
                        "I hereby attest and certify under penalty of law that the "
                        "information provided is true and correct. " * 2)
    assert status == "ACCEPTED"


def test_open_negotiation_keyword_floor():
    status, _ = verdict("OPEN_NEGOTIATION_NOTICE",
                        "Notice of initiation of the open negotiation period. "
                        "The 30 business days negotiation window begins on receipt. " * 2)
    assert status == "ACCEPTED"


def test_coverage_keyword_floor():
    status, _ = verdict("PROOF_OF_COVERAGE",
                        "This letter confirms coverage eligibility for the member and "
                        "subscriber for the dates of service below. " * 2)
    assert status == "ACCEPTED"


def test_keyword_floor_missing():
    status, reasons = verdict("IDRE_ATTESTATION", "Quarterly newsletter. " * 20)
    assert status == "NEEDS_REVIEW"
    assert reasons[0]["code"] == "KEYWORDS_MISSING"


def test_six_slots():
    assert len(tpa_docs.SLOTS) == 6
    assert set(tpa_docs.SLOT_TYPE) == set(tpa_docs.SLOTS)
    for slot in tpa_docs.SLOTS:
        assert tpa_docs.SLOT_TYPE[slot] is not None or slot in tpa_docs.SLOT_KEYWORDS


QPA_SUBSTANTIATION = ("OFFER SUBSTANTIATION. The qualifying payment amount (QPA) for CPT 99285 "
                      "was derived as the median contracted rate for the service in the region "
                      "as of January 31 2019, trended forward. CPT 99285. Billed amount $2,400.00. "
                      "Itemized calculation attached.") * 2

PLAIN_SUBSTANTIATION = ("OFFER SUBSTANTIATION. We believe our offer is fair given the complexity "
                        "of the case. CPT 99285. Billed amount $2,400.00.") * 2


def test_healthplan_substantiation_with_qpa_accepted():
    status, reasons = tpa_docs._verdict("OFFER_SUBSTANTIATION", "offer_substantiation.pdf",
                                        QPA_SUBSTANTIATION, classify_text, extract_fields,
                                        actor="healthplan")
    assert status == "ACCEPTED", reasons


def test_healthplan_substantiation_without_qpa_needs_review():
    status, reasons = tpa_docs._verdict("OFFER_SUBSTANTIATION", "offer_substantiation.pdf",
                                        PLAIN_SUBSTANTIATION, classify_text, extract_fields,
                                        actor="healthplan")
    assert status == "NEEDS_REVIEW"
    assert any(r["code"] == "QPA_DOC_MISSING" for r in reasons)


def test_tpa_substantiation_has_no_qpa_floor():
    # The QPA floor is payer-side only; a provider-side TPA package is fine.
    status, _ = tpa_docs._verdict("OFFER_SUBSTANTIATION", "offer_substantiation.pdf",
                                  PLAIN_SUBSTANTIATION, classify_text, extract_fields,
                                  actor="tpa")
    assert status == "ACCEPTED"
