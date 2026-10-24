# Author: Patrick Munis at newwave
"""Parity tests for rules_engine.py against the Go engine semantics
(services/case-api-go/cmd/server/rules.go + rules_test.go). If a behavior
changes in one implementation it MUST change in the other — these cases are
the shared contract."""

import unittest
from datetime import datetime, timedelta, timezone

from rules_engine import eval_cond, expand_template, fire_rules


class TestOperators(unittest.TestCase):
    def test_numeric_ops(self):
        f = {"n": 10, "s": "12", "b": True}
        self.assertEqual(eval_cond({"field": "n", "op": "gte", "value": 10}, f), (True, True))
        self.assertEqual(eval_cond({"field": "n", "op": "lt", "value": 10}, f), (False, True))
        self.assertEqual(eval_cond({"field": "s", "op": "gt", "value": 11}, f), (True, True))
        # bools never coerce to numbers (Go: type switch rejects bool)
        self.assertEqual(eval_cond({"field": "b", "op": "gt", "value": 0}, f), (False, True))

    def test_eq_neq_in_contains(self):
        f = {"status": "INSTRUCTED", "tags": ["a", "b"], "name": "itemized-bill.pdf"}
        self.assertEqual(eval_cond({"field": "status", "op": "eq", "value": "INSTRUCTED"}, f), (True, True))
        self.assertEqual(eval_cond({"field": "status", "op": "neq", "value": "CONVERTED"}, f), (True, True))
        self.assertEqual(eval_cond({"field": "status", "op": "in", "value": ["INSTRUCTED", "DOCS_RECEIVED"]}, f), (True, True))
        self.assertEqual(eval_cond({"field": "name", "op": "contains", "value": "bill"}, f), (True, True))
        self.assertEqual(eval_cond({"field": "tags", "op": "contains", "value": "b"}, f), (True, True))

    def test_null_ops(self):
        f = {"x": None}
        self.assertEqual(eval_cond({"field": "x", "op": "is_null"}, f), (True, True))
        self.assertEqual(eval_cond({"field": "missing", "op": "is_null"}, f), (True, True))
        self.assertEqual(eval_cond({"field": "x", "op": "not_null"}, f), (False, True))

    def test_days_older_than(self):
        old = (datetime.now(timezone.utc) - timedelta(days=14)).isoformat()
        new = (datetime.now(timezone.utc) - timedelta(hours=2)).isoformat()
        self.assertEqual(eval_cond({"field": "outreach_at", "op": "days_older_than", "value": 13},
                                   {"outreach_at": old}), (True, True))
        self.assertEqual(eval_cond({"field": "outreach_at", "op": "days_older_than", "value": 13},
                                   {"outreach_at": new}), (False, True))
        # garbage timestamp: condition understood but not met (fail closed on match)
        self.assertEqual(eval_cond({"field": "outreach_at", "op": "days_older_than", "value": 13},
                                   {"outreach_at": "not-a-date"}), (False, True))

    def test_unknown_op_fails_closed_and_marks_suspect(self):
        rules = [{"name": "bad", "event": "doc.analyzed", "enabled": True,
                  "conditions": [{"field": "x", "op": "regex", "value": ".*"}],
                  "actions": [{"type": "flag_review"}]}]
        actions, fired, suspect = fire_rules(rules, {"x": "anything"})
        self.assertEqual(actions, [])
        self.assertEqual(fired, [])
        self.assertEqual(suspect, ["bad"])

    def test_disabled_rule_never_fires(self):
        rules = [{"name": "off", "event": "doc.analyzed", "enabled": False,
                  "conditions": [], "actions": [{"type": "notify"}]}]
        actions, fired, _ = fire_rules(rules, {})
        self.assertEqual((actions, fired), ([], []))

    def test_conjunctive_conditions(self):
        rules = [{"name": "both", "event": "doc.analyzed", "enabled": True,
                  "conditions": [{"field": "doc_type", "op": "eq", "value": "ITEMIZED_BILL"},
                                 {"field": "ungrounded_count", "op": "gt", "value": 0}],
                  "actions": [{"type": "flag_review"}]}]
        self.assertEqual(fire_rules(rules, {"doc_type": "ITEMIZED_BILL", "ungrounded_count": 2})[1], ["both"])
        self.assertEqual(fire_rules(rules, {"doc_type": "ITEMIZED_BILL", "ungrounded_count": 0})[1], [])

    def test_expand_template(self):
        self.assertEqual(expand_template("Doc {{doc_id}} on case {{case_id}}", {"doc_id": "d1", "case_id": "c9"}),
                         "Doc d1 on case c9")


if __name__ == "__main__":
    unittest.main()
