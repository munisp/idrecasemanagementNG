# Author: Patrick Munis at newwave
"""necessity.py — conservative dx->CPT plausibility screen.

This is NOT a medical-necessity adjudication (that is the LCD/NCD policy
universe and belongs to the human reviewer). It is a tripwire: when a claim's
documented diagnosis codes cannot plausibly support the billed procedure —
e.g. a high-complexity ED E&M billed against a routine-exam Z-code — the case
gets flagged for the clinical queue instead of waiting for someone to notice.

Design rules:
  * curated, deliberately small rule set — only code families where a mismatch
    is unambiguous;
  * a procedure code absent from the table is NEVER flagged (unknown != wrong);
  * a claim is flagged only when it HAS diagnosis codes and none of them is
    plausible for the billed code;
  * ICD-10 matching is by prefix (chapter letter + stem), matching how coders
    actually read linkage.
"""

# procedure prefix -> plausible ICD-10 prefixes. Prefix match: "E11" covers
# E11.9, E11.65, ... Chapters: A-B infectious, C-D neoplasm/blood, E endocrine,
# F mental, G neuro, H eye/ear, I circulatory, J respiratory, K digestive,
# L skin, M musculoskeletal, N GU, O pregnancy, P perinatal, Q congenital,
# R symptoms, S-T injury, Z factors.
_RULES: list[tuple[str, frozenset[str], str]] = [
    # ED E&M 99281-99285: needs an acute presentation — symptoms, injury,
    # circulatory/respiratory/digestive/neuro chapters. A bare Z00 routine
    # exam cannot support 99285.
    ("9928", frozenset({"R", "S", "T", "I", "J", "K", "G", "M", "N", "A", "B", "O", "E", "F"}),
     "ED E&M billed without an acute-presenting diagnosis"),
    # Office/outpatient 99202-99215: anything clinical EXCEPT pure Z00/Z01
    # (routine exams have their own codes) — flag only when every dx is Z00/Z01.
    ("9921", frozenset({"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L",
                        "M", "N", "O", "P", "Q", "R", "S", "T", "Z02", "Z12", "Z13"}),
     "established-patient E&M billed against routine-exam diagnoses only"),
    ("9920", frozenset({"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L",
                        "M", "N", "O", "P", "Q", "R", "S", "T", "Z02", "Z12", "Z13"}),
     "new-patient E&M billed against routine-exam diagnoses only"),
    # Chest X-ray 71045/71046: respiratory / circulatory / injury / symptoms.
    ("7104", frozenset({"J", "I", "R", "S", "T", "C"}),
     "chest radiograph without a thoracic/symptom diagnosis"),
    # CT head 70450-70470: neuro / injury / symptoms / neoplasm.
    ("704", frozenset({"G", "S", "T", "R", "C", "I"}),
     "head CT without a neurologic/injury/symptom diagnosis"),
    # Comprehensive metabolic panel 80053: broad — endocrine, renal (N),
    # circulatory, digestive, symptoms, screening Z13.
    ("80053", frozenset({"E", "N", "I", "K", "R", "Z", "C", "D"}),
     "metabolic panel without a metabolic/screening diagnosis"),
    # Urinalysis 81001-81003: GU / symptoms / pregnancy / endocrine.
    ("8100", frozenset({"N", "R", "O", "E", "Z"}),
     "urinalysis without a GU/symptom/screening diagnosis"),
]


def _plausible(rule_dx: frozenset[str], dx_codes: list[str]) -> bool:
    for dx in dx_codes:
        dx = dx.strip().upper()
        for pref in rule_dx:
            if dx.startswith(pref):
                return True
    return False


def screen(cpt_codes: list[str], dx_codes: list[str]) -> list[dict]:
    """Return flags: [{code, diagnoses, reason}]. Empty when plausible,
    when no dx codes exist (nothing to screen against), or when no billed
    code is in the curated set."""
    if not cpt_codes or not dx_codes:
        return []
    flags: list[dict] = []
    for cpt in cpt_codes:
        cpt = (cpt or "").strip()
        for pref, rule_dx, reason in _RULES:
            if cpt.startswith(pref):
                if not _plausible(rule_dx, dx_codes):
                    flags.append({"code": cpt, "diagnoses": dx_codes[:10], "reason": reason})
                break  # first matching rule family owns the code
    return flags
