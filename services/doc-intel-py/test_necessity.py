# Author: Patrick Munis at newwave
"""necessity screen: conservative tripwire semantics — flags only on curated
code families with a clear mismatch; unknown codes and absent dx never flag."""
import necessity


def test_ed_em_flagged_on_routine_exam_dx():
    flags = necessity.screen(["99285"], ["Z00.00"])
    assert flags and flags[0]["code"] == "99285"


def test_ed_em_passes_with_acute_dx():
    assert necessity.screen(["99285"], ["R07.9"]) == []
    assert necessity.screen(["99285"], ["S06.0X0A", "I10"]) == []


def test_office_visit_flagged_on_z00_only():
    flags = necessity.screen(["99213"], ["Z00.129"])
    assert flags
    # but a Z13 screening dx is legitimately linkable
    assert necessity.screen(["99213"], ["Z13.220"]) == []


def test_unknown_code_never_flags():
    assert necessity.screen(["99999"], ["Z00.00"]) == []


def test_no_dx_means_nothing_to_screen():
    assert necessity.screen(["99285"], []) == []


def test_prefix_ownership_first_family_wins():
    # 99285 matches the "9928" family; must not also be screened as "992" anything
    flags = necessity.screen(["99281"], ["Z00.00"])
    assert len(flags) == 1
