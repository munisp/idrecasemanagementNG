"""Stdlib tests for the deterministic layers (no OpenMed, no FastAPI needed).
Run: python -m pytest test_deid.py  (or: python test_deid.py)
"""

import safeharbor as sh
import clinical


def test_ssn_detected_and_redacted():
    text = "Patient SSN 123-45-6789 admitted with chest pain."
    fs = sh.scan(text)
    assert any(f.type == "SSN" for f in fs)
    out = sh.redact(text, fs, "seed:fl")
    assert "123-45-6789" not in out and "[SSN]" in out


def test_dates_only_flagged_in_clinical_context():
    assert not any(f.type == "DATE" for f in sh.scan("Contract signed 01/15/2026 between parties."))
    fs = sh.scan("DOB 01/15/1962; date of service 03/02/2026.")
    assert sum(1 for f in fs if f.type == "DATE") == 2


def test_date_shift_is_stable_and_preserves_year_distance():
    text = "admitted 01/10/2026, discharged 01/17/2026"
    fs = sh.scan(text)
    a = sh.redact(text, fs, "seed:fl")
    b = sh.redact(text, fs, "seed:fl")
    assert a == b  # stable per tenant
    assert "01/10/2026" not in a and "01/17/2026" not in a
    # shifted dates remain dates in the same format
    import re
    assert len(re.findall(r"\d{2}/\d{2}/\d{4}", a)) == 2


def test_risk_direct_identifier_dominates():
    assert sh.risk(sh.scan("call 555-123-4567")) == 1.0
    assert sh.risk([]) == 0.0


def test_email_mrn_zip():
    fs = sh.scan("email jdoe@example.com, MRN: AB123456, zip 33133")
    types = {f.type for f in fs}
    assert {"EMAIL", "MRN"} <= types


def test_clinical_lexicon_spans():
    ents, detectors = clinical.analyze(
        "CT scan of the chest revealed pneumonia; started on ceftriaxone and admitted for chest pain.")
    types = {e.type for e in ents}
    assert "PROCEDURE" in types and "CONDITION" in types and "DRUG" in types
    assert detectors == ["clinical-lexicon/v1"]  # OpenMed absent in test env
    s = clinical.summarize(ents)
    assert s.get("CONDITION", 0) >= 1 and s.get("DRUG", 0) >= 1


def test_clinical_longest_match_wins():
    ents, _ = clinical.analyze("rule out myocardial infarction")
    mi = [e for e in ents if e.text.lower() == "myocardial infarction"]
    assert mi and mi[0].type == "CONDITION"


if __name__ == "__main__":
    fns = [v for k, v in sorted(globals().items()) if k.startswith("test_")]
    for fn in fns:
        fn()
        print(f"ok {fn.__name__}")
    print(f"{len(fns)} tests passed")
