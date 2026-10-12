"""Tests for the learning loop: label-phrase harvesting, champion/challenger
promotion and retirement over a fake DB connection (no postgres, no search)."""

import learning
import fields


# --- harvest_label_phrase ----------------------------------------------------

def test_harvest_finds_label_before_value():
    text = "Carrier Allowance $412.00\nTotal Billed $900.00"
    assert learning.harvest_label_phrase(text, 41_200) == "carrier allowance"


def test_harvest_without_dollar_sign():
    text = "Plan Allowance: 17,337.60 paid per contract"
    assert learning.harvest_label_phrase(text, 1_733_760) == "plan allowance"


def test_harvest_missing_value_returns_none():
    assert learning.harvest_label_phrase("nothing here", 41_200) is None


def test_harvest_drops_trailing_stopwords():
    text = "Amount of the $100.00"
    phrase = learning.harvest_label_phrase(text, 10_000)
    assert phrase == "amount"


def test_corrected_cents_parsing():
    assert learning._corrected_cents("$1,234.56") == 123_456
    assert learning._corrected_cents("900") == 90_000
    assert learning._corrected_cents("not a number") is None
    assert learning._corrected_cents("") is None


# --- mine() over a fake connection -------------------------------------------

class _Cursor:
    def __init__(self, db):
        self._db = db
        self._rows = []

    def execute(self, sql, params=None):
        s = " ".join(sql.split()).lower()
        if s.startswith("select id, tenant, doc_id"):
            self._rows = list(self._db["feedback"])
        elif s.startswith("select id, field, extracted_value, corrected_value from public.check_feedback"):
            self._rows = list(self._db.get("check_feedback", []))
        elif s.startswith("select pattern_type, key, value from public.doc_learned_patterns"):
            self._rows = [(pt, k, v) for (pt, k, v), p in self._db["patterns"].items()
                          if p["status"] == "ACTIVE"]
        elif s.startswith("select id, pattern_type, key, value, support, status"):
            self._rows = [(p["id"], pt, k, v, p["support"], p["status"])
                          for (pt, k, v), p in self._db["patterns"].items()
                          if pt in ("MEMO_NOISE_PHRASE", "PAYER_ALIAS")
                          and p["status"] in ("CANDIDATE", "ACTIVE")]
        elif s.startswith("select id, key, value, support, status"):
            self._rows = [(p["id"], k, v, p["support"], p["status"])
                          for (pt, k, v), p in self._db["patterns"].items()
                          if pt == "LABEL_SYNONYM" and p["status"] in ("CANDIDATE", "ACTIVE")]
        elif s.startswith("insert into public.doc_learned_patterns"):
            if "'memo_noise_phrase'" in s:
                pt, key_, phrase, support = "MEMO_NOISE_PHRASE", "memo", params[0], params[1]
            elif "'payer_alias'" in s:
                pt, key_, phrase, support = "PAYER_ALIAS", params[0], params[1], params[2]
            else:
                pt, key_, phrase, support = "LABEL_SYNONYM", params[0], params[1], params[2]
            key = (pt, key_, phrase)
            p = self._db["patterns"].setdefault(
                key, {"id": len(self._db["patterns"]) + 1, "status": "CANDIDATE",
                      "precision": None})
            p["support"] = support
        elif s.startswith("update public.doc_learned_patterns set status='active'"):
            precision, pid = params
            for p in self._db["patterns"].values():
                if p["id"] == pid:
                    p["status"], p["precision"] = "ACTIVE", precision
        elif s.startswith("update public.doc_learned_patterns set status='retired'"):
            precision, pid = params
            for p in self._db["patterns"].values():
                if p["id"] == pid:
                    p["status"], p["precision"] = "RETIRED", precision
        elif s.startswith("update public.doc_learned_patterns set precision"):
            precision, pid = params
            for p in self._db["patterns"].values():
                if p["id"] == pid:
                    p["precision"] = precision
        else:
            raise AssertionError(f"unexpected SQL: {sql}")
        return self

    def fetchall(self):
        return self._rows


class _FakeConn:
    def __init__(self, db):
        self._db = db

    def execute(self, sql, params=None):
        return _Cursor(self._db).execute(sql, params)


class _FakeSearch:
    """OpenSearch stand-in: doc_id -> indexed text."""
    def __init__(self, docs):
        self._docs = docs

    def search(self, index, body):
        doc_id = body["query"]["term"]["doc_id"]
        text = self._docs.get(doc_id)
        hits = [{"_source": {"text": text}}] if text else []
        return {"hits": {"hits": hits}}


def _feedback(n, field, corrected, doc_prefix="doc"):
    return [(i, "FL", f"{doc_prefix}{i}", "eob", field, "0.00", corrected)
            for i in range(n)]


def test_mine_promotes_supported_accurate_synonym():
    docs = {f"doc{i}": "Carrier Allowance $412.00\nTotal Billed $900.00"
            for i in range(3)}
    db = {"feedback": _feedback(3, "allowed_amount_usd", "412.00"), "patterns": {}}
    stats = learning.mine(_FakeConn(db), _FakeSearch(docs))
    assert stats["candidates"] == 1
    assert stats["promoted"] == 1
    p = db["patterns"][("LABEL_SYNONYM", "allowed", "carrier allowance")]
    assert p["status"] == "ACTIVE"
    assert p["precision"] == 1.0
    # and the ACTIVE cache now makes the phrase extractable
    labels = learning.get_active_labels()
    assert "carrier allowance" in labels.get("allowed", [])
    ex = fields.extract_fields(docs["doc0"], extra_labels=labels)
    assert any(a["canonical"] == "allowed" and a["cents"] == 41_200
               for a in ex["amounts"])


def test_mine_does_not_promote_below_min_support():
    docs = {"doc0": "Carrier Allowance $412.00"}
    db = {"feedback": _feedback(1, "allowed_amount_usd", "412.00"), "patterns": {}}
    stats = learning.mine(_FakeConn(db), _FakeSearch(docs))
    assert stats["promoted"] == 0
    p = db["patterns"][("LABEL_SYNONYM", "allowed", "carrier allowance")]
    assert p["status"] == "CANDIDATE"


def test_mine_does_not_promote_inaccurate_synonym():
    # 3 docs support the phrase, but in 1 of them extraction with the synonym
    # does not recover the corrected value -> precision 2/3 < 0.95
    docs = {f"doc{i}": "Carrier Allowance $412.00" for i in range(3)}
    db = {"feedback": _feedback(2, "allowed_amount_usd", "412.00")
                      + [(9, "FL", "doc2", "eob", "allowed_amount_usd", "0.00", "500.00")],
          "patterns": {}}
    stats = learning.mine(_FakeConn(db), _FakeSearch(docs))
    assert stats["promoted"] == 0


def test_mine_retires_degraded_active_pattern():
    key = ("LABEL_SYNONYM", "allowed", "carrier allowance")
    db = {"feedback": _feedback(3, "allowed_amount_usd", "999.00"), "patterns": {
        key: {"id": 1, "status": "ACTIVE", "support": 5, "precision": 0.97}}}
    docs = {f"doc{i}": "Carrier Allowance $412.00" for i in range(3)}
    stats = learning.mine(_FakeConn(db), _FakeSearch(docs))
    assert stats["retired"] == 1
    assert db["patterns"][key]["status"] == "RETIRED"


def test_mine_degrades_gracefully_without_search():
    db = {"feedback": _feedback(3, "allowed_amount_usd", "412.00"), "patterns": {}}
    stats = learning.mine(_FakeConn(db), None)
    assert stats["candidates"] == 0  # no doc text -> no new candidates, no crash


# --- check-extraction learning ----------------------------------------------

def test_harvest_memo_noise_prefix():
    assert learning.harvest_memo_noise(
        "THIS IS NOT A INVOICE IDRE-2026-000123", "IDRE-2026-000123"
    ) == "this is not a invoice"  # contiguous prefix of words absent from the correction


def test_harvest_memo_noise_suffix():
    assert learning.harvest_memo_noise(
        "IDRE-2026-000123 VOID STAMP", "IDRE-2026-000123"
    ) == "void stamp"


def test_harvest_memo_noise_interior_edit_not_learned():
    # A content correction (different words in the middle) isn't noise.
    assert learning.harvest_memo_noise("inv 0042 final", "inv 0043 final") is None
    assert learning.harvest_memo_noise("", "x") is None
    assert learning.harvest_memo_noise("same words", "same words") is None


def test_strip_memo_noise_word_bounded():
    assert learning.strip_memo_noise(
        "VOID STAMP Inv IDRE-2026-000123", ["void stamp"]) == "Inv IDRE-2026-000123"
    # word boundaries: "stamp" inside "stampede" must not be eaten
    assert learning.strip_memo_noise("stampede 42", ["stamp"]) == "stampede 42"


def _check_feedback(n, field, extracted, corrected):
    return [(i, field, extracted, corrected) for i in range(n)]


def test_mine_check_promotes_payer_alias():
    db = {"feedback": [], "patterns": {},
          "check_feedback": _check_feedback(3, "payer_name", "Aetna lnc", "Aetna Inc")}
    stats = learning.mine(_FakeConn(db), None)
    key = ("PAYER_ALIAS", "aetna lnc", "Aetna Inc")
    assert stats["check_promoted"] == 1
    assert db["patterns"][key]["status"] == "ACTIVE"
    assert db["patterns"][key]["precision"] == 1.0
    # and the applied cache exposes it
    pats = learning.get_active_check_patterns()
    assert pats["payer_alias"]["aetna lnc"] == "Aetna Inc"


def test_mine_check_promotes_memo_noise_phrase():
    db = {"feedback": [], "patterns": {},
          "check_feedback": _check_feedback(
              4, "memo", "VOID STAMP IDRE-2026-000123", "IDRE-2026-000123")}
    stats = learning.mine(_FakeConn(db), None)
    key = ("MEMO_NOISE_PHRASE", "memo", "void stamp")
    assert stats["check_promoted"] == 1
    assert db["patterns"][key]["status"] == "ACTIVE"
    assert "void stamp" in learning.get_active_check_patterns()["memo_strip"]


def test_mine_check_no_promotion_below_min_support():
    db = {"feedback": [], "patterns": {},
          "check_feedback": _check_feedback(2, "payer_name", "Aetna lnc", "Aetna Inc")}
    stats = learning.mine(_FakeConn(db), None)
    assert stats["check_promoted"] == 0
    assert db["patterns"][("PAYER_ALIAS", "aetna lnc", "Aetna Inc")]["status"] == "CANDIDATE"


def test_mine_check_no_promotion_when_corrections_disagree():
    # Same OCR misread corrected two different ways -> precision below gate.
    fb = [(0, "payer_name", "Aetna lnc", "Aetna Inc"),
          (1, "payer_name", "Aetna lnc", "Aetna Inc"),
          (2, "payer_name", "Aetna lnc", "Aetna Life")]
    db = {"feedback": [], "patterns": {}, "check_feedback": fb}
    stats = learning.mine(_FakeConn(db), None)
    assert stats["check_promoted"] == 0


def test_mine_check_retires_degraded_alias():
    key = ("PAYER_ALIAS", "aetna lnc", "Aetna Inc")
    db = {"feedback": [],
          "patterns": {key: {"id": 1, "status": "ACTIVE", "support": 5, "precision": 0.99}},
          "check_feedback": [(i, "payer_name", "Aetna lnc", "Aetna Life") for i in range(4)]}
    stats = learning.mine(_FakeConn(db), None)
    assert stats["check_retired"] == 1
    assert db["patterns"][key]["status"] == "RETIRED"
