// Author: Patrick Munis at newwave
package main

import (
	"testing"
	"time"
)

func TestEvalCondOperators(t *testing.T) {
	facts := map[string]any{"days": 13.0, "status": "INSTRUCTED", "name": "memorial", "gone": nil}
	cases := []struct {
		c    ruleCond
		want bool
	}{
		{ruleCond{"days", "gte", 13.0}, true},
		{ruleCond{"days", "gt", 13.0}, false},
		{ruleCond{"days", "lt", 14.0}, true},
		{ruleCond{"status", "eq", "INSTRUCTED"}, true},
		{ruleCond{"status", "neq", "PAID"}, true},
		{ruleCond{"status", "in", []any{"INSTRUCTED", "DOCS_RECEIVED"}}, true},
		{ruleCond{"status", "in", []any{"PAID"}}, false},
		{ruleCond{"name", "contains", "memor"}, true},
		{ruleCond{"gone", "is_null", nil}, true},
		{ruleCond{"status", "not_null", nil}, true},
		{ruleCond{"missing", "eq", "x"}, false}, // absent fact fails
	}
	for i, tc := range cases {
		got, known := evalCond(tc.c, facts)
		if !known || got != tc.want {
			t.Errorf("case %d (%v): got (%v,%v) want %v", i, tc.c, got, known, tc.want)
		}
	}
}

func TestDaysOlderThan(t *testing.T) {
	old := time.Now().Add(-14 * 24 * time.Hour).UTC().Format(time.RFC3339)
	fresh := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	ok, _ := evalCond(ruleCond{"ts", "days_older_than", 13.0}, map[string]any{"ts": old})
	if !ok {
		t.Error("14-day-old timestamp should pass a 13-day gate")
	}
	ok, _ = evalCond(ruleCond{"ts", "days_older_than", 13.0}, map[string]any{"ts": fresh})
	if ok {
		t.Error("2-hour-old timestamp must not pass a 13-day gate")
	}
}

func TestUnknownOpFailsClosed(t *testing.T) {
	_, known := evalCond(ruleCond{"x", "regex", ".*"}, map[string]any{"x": "y"})
	if known {
		t.Error("unknown op must be reported as not-understood")
	}
	rules := []Rule{{Name: "bad", Event: "doc.upload", Conditions: []ruleCond{{Field: "x", Op: "regex", Value: ".*"}}}}
	actions, suspect := fireRules(rules, map[string]any{"x": "y"})
	if len(actions) != 0 || len(suspect) != 1 {
		t.Error("suspect rule must not fire and must be reported")
	}
}

func TestFireRulesCollectsActions(t *testing.T) {
	rules := []Rule{
		{Name: "a", Event: "sweep.intake",
			Conditions: []ruleCond{{Field: "days", Op: "gte", Value: 13.0}},
			Actions:    []ruleAction{{Type: "set_status", Params: map[string]any{"status": "INELIGIBLE"}}}},
		{Name: "disabled", Event: "sweep.intake", Enabled: boolPtr(false),
			Actions: []ruleAction{{Type: "notify"}}},
	}
	actions, _ := fireRules(rules[:1], map[string]any{"days": 20.0})
	if len(actions) != 1 || actions[0].Params["status"] != "INELIGIBLE" {
		t.Errorf("expected set_status action, got %+v", actions)
	}
	// disabled rule never fires even with matching conditions
	var disabled []Rule
	for _, r := range rules {
		if r.active() {
			disabled = append(disabled, r)
		}
	}
	if len(disabled) != 1 {
		t.Error("enabled=false must deactivate the rule")
	}
}

func TestValidateRules(t *testing.T) {
	good := []Rule{{Name: "ok", Event: "intake.advance",
		Conditions: []ruleCond{{Field: "status", Op: "eq", Value: "PAID"}},
		Actions:    []ruleAction{{Type: "block_request"}}}}
	if err := validateRules(good); err != nil {
		t.Errorf("valid rules rejected: %v", err)
	}
	bad := [][]Rule{
		{{Event: "intake.advance"}},                                     // no name
		{{Name: "x", Event: "nope"}},                                    // bad event
		{{Name: "x", Event: "intake.advance", Conditions: []ruleCond{{Field: "f", Op: "regex"}}}}, // bad op
		{{Name: "x", Event: "intake.advance", Actions: []ruleAction{{Type: "delete_db"}}}},        // bad action
		{{Name: "dup", Event: "intake.advance"}, {Name: "dup", Event: "intake.advance"}},          // duplicate
	}
	for i, b := range bad {
		if err := validateRules(b); err == nil {
			t.Errorf("bad rule set %d passed validation", i)
		}
	}
}

func TestExpandTemplate(t *testing.T) {
	got := expandTemplate("Intake {{id}} at day {{days}}", map[string]any{"id": "a1", "days": 13})
	if got != "Intake a1 at day 13" {
		t.Errorf("got %q", got)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestFireRulesNamedReportsFired(t *testing.T) {
	enabled := true
	rules := []Rule{
		{
			Name: "a", Event: "doc.upload", Enabled: &enabled,
			Conditions: []ruleCond{{Field: "folder", Op: "eq", Value: "OFFERS"}},
			Actions:    []ruleAction{{Type: "notify", Params: map[string]any{"body": "x"}}},
		},
		{
			Name: "b", Event: "doc.upload", Enabled: &enabled,
			Conditions: []ruleCond{{Field: "size_bytes", Op: "gt", Value: 100}},
			Actions:    []ruleAction{{Type: "log_activity", Params: map[string]any{"body": "y"}}},
		},
		{
			Name: "c", Event: "doc.upload", Enabled: &enabled,
			Conditions: []ruleCond{{Field: "size_bytes", Op: "bogus_op", Value: 1}},
			Actions:    []ruleAction{{Type: "block_request"}},
		},
	}
	actions, fired, suspect := fireRulesNamed(rules, map[string]any{"folder": "OFFERS", "size_bytes": 50})
	if len(actions) != 1 || actions[0].Type != "notify" {
		t.Fatalf("expected only rule a's action, got %+v", actions)
	}
	if len(fired) != 1 || fired[0] != "a" {
		t.Fatalf("fired = %v", fired)
	}
	if len(suspect) != 1 || suspect[0] != "c" {
		t.Fatalf("suspect = %v — unknown op must fail closed and report", suspect)
	}
}

func TestRuleDetailKeyAllowlist(t *testing.T) {
	for _, ok := range []string{"review_reason", "volume_override2", "x"} {
		if !ruleDetailKey.MatchString(ok) {
			t.Fatalf("%q should be allowed", ok)
		}
	}
	for _, bad := range []string{"Status", "a.b", "a'; DROP TABLE--", "../x", ""} {
		if ruleDetailKey.MatchString(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
