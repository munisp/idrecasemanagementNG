// Author: Patrick Munis at newwave
// rules.go — tenant-scoped rule engine.
//
// Business policy lives in DATA (public.program_rules config.rules[]), not in
// code. A rule is:
//
//   {
//     "name": "intake-day13-incomplete",
//     "event": "sweep.intake",              // which hook fires it
//     "enabled": true,
//     "conditions": [                        // ALL must hold (AND)
//       {"field": "days_since_outreach", "op": "gte", "value": 13},
//       {"field": "status", "op": "in", "value": ["INSTRUCTED","DOCS_RECEIVED"]}
//     ],
//     "actions": [
//       {"type": "set_status", "params": {"status": "INELIGIBLE"}},
//       {"type": "notify", "params": {"kind": "SLA_BREACH",
//          "body": "Intake {{id}} incomplete at day 13 — issue ineligibility letter"}}
//     ]
//   }
//
// Events are fired by code at decision points (intake.advance, doc.upload,
// claims.imported, invoice.settled, sweep.intake). The engine evaluates
// conditions against a facts map; callers execute the returned actions through
// the small registry below. Adding a policy = inserting JSON, not a deploy.
//
// Semantics: conditions are conjunctive; an empty conditions list always
// matches. Unknown ops/fields fail CLOSED for block-type rules and are
// skipped otherwise — a misconfigured rule must never silently allow what an
// operator intended to forbid.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type ruleCond struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

type ruleAction struct {
	Type   string         `json:"type"`
	Params map[string]any `json:"params"`
}

type Rule struct {
	Name       string       `json:"name"`
	Event      string       `json:"event"`
	Enabled    *bool        `json:"enabled"`
	Conditions []ruleCond   `json:"conditions"`
	Actions    []ruleAction `json:"actions"`
}

func (r Rule) active() bool { return r.Enabled == nil || *r.Enabled }

// rulesFor loads the tenant's rules for one event, fresh from config every
// call (config edits take effect without a restart).
func (s *server) rulesFor(r *http.Request, tenant, event string) []Rule {
	var raw []byte
	if err := s.db.QueryRow(r.Context(),
		`SELECT config->'rules' FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw); err != nil {
		return nil
	}
	var all []Rule
	if json.Unmarshal(raw, &all) != nil {
		return nil
	}
	var out []Rule
	for _, ru := range all {
		if ru.Event == event && ru.active() {
			out = append(out, ru)
		}
	}
	return out
}

// toFloat coerces JSON numbers / numeric strings for comparisons.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// evalCond: one condition against the facts map. ok=false + known=true means
// the condition failed; known=false means the op/field was unrecognized.
func evalCond(c ruleCond, facts map[string]any) (ok, known bool) {
	fact, present := facts[c.Field]
	switch c.Op {
	case "is_null":
		return !present || fact == nil || fact == "", true
	case "not_null":
		return present && fact != nil && fact != "", true
	}
	if !present {
		return false, true // condition on an absent fact fails (except *_null ops)
	}
	switch c.Op {
	case "eq":
		return fmt.Sprint(fact) == fmt.Sprint(c.Value), true
	case "neq":
		return fmt.Sprint(fact) != fmt.Sprint(c.Value), true
	case "in":
		list, isList := c.Value.([]any)
		if !isList {
			return false, false
		}
		for _, item := range list {
			if fmt.Sprint(fact) == fmt.Sprint(item) {
				return true, true
			}
		}
		return false, true
	case "contains":
		return strings.Contains(fmt.Sprint(fact), fmt.Sprint(c.Value)), true
	case "gt", "gte", "lt", "lte":
		f, okF := toFloat(fact)
		v, okV := toFloat(c.Value)
		if !okF || !okV {
			return false, false
		}
		switch c.Op {
		case "gt":
			return f > v, true
		case "gte":
			return f >= v, true
		case "lt":
			return f < v, true
		default:
			return f <= v, true
		}
	case "days_older_than":
		// fact is an RFC3339 timestamp; passes when it is more than value days old.
		ts, err := time.Parse(time.RFC3339, fmt.Sprint(fact))
		threshold, okT := toFloat(c.Value)
		if err != nil || !okT {
			return false, false
		}
		return time.Since(ts) > time.Duration(threshold*24)*time.Hour, true
	}
	return false, false
}

// evalRule returns (matched, understood): matched = all conditions hold;
// understood = every condition used a known op (else the rule is suspect).
func evalRule(ru Rule, facts map[string]any) (matched, understood bool) {
	for _, c := range ru.Conditions {
		ok, known := evalCond(c, facts)
		if !known {
			return false, false
		}
		if !ok {
			return false, true
		}
	}
	return true, true
}

// fireRules evaluates a tenant's rules for an event and returns the actions
// of every matching rule. A rule with unrecognized conditions is treated as
// NOT matching, and the caller is told via suspectRules so it can fail closed
// on security-sensitive events (doc.upload blocks) while staying open on
// advisory ones.
func fireRules(rules []Rule, facts map[string]any) (actions []ruleAction, suspectRules []string) {
	actions, _, suspectRules = fireRulesNamed(rules, facts)
	return actions, suspectRules
}

// fireRulesNamed additionally reports WHICH rules fired, so the caller can
// emit a rule.fired audit event per rule (Kafka idre.<tenant>.rules → lakehouse).
func fireRulesNamed(rules []Rule, facts map[string]any) (actions []ruleAction, fired, suspectRules []string) {
	for _, ru := range rules {
		matched, understood := evalRule(ru, facts)
		if !understood {
			suspectRules = append(suspectRules, ru.Name)
			continue
		}
		if matched {
			actions = append(actions, ru.Actions...)
			fired = append(fired, ru.Name)
		}
	}
	return actions, fired, suspectRules
}

// ruleDetailKey matches the set_detail action's target key — conservative
// allowlist so a rule can never overwrite system detail fields.
var ruleDetailKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// fireEventRules is the single integration point between the rule engine and
// every middleware-adjacent call site (upload handlers, claims import, ledger
// settlement, intake advance). It loads the tenant's rules fresh, evaluates
// them, executes side-effect actions, emits one rule.fired outbox event per
// fired rule (Dapr → Kafka → lakehouse), and returns (blocked, message) —
// block_request is the ONLY action that alters the caller's control flow.
//
// Error posture: evaluation is fail-closed (suspect rules never fire); action
// side effects are fail-open (a broken notify action must not reject an
// upload) — every failure is logged + recorded as RULE_SUSPECT activity.
func (s *server) fireEventRules(r *http.Request, tenant, event string, facts map[string]any) (blocked bool, blockMsg string) {
	rules := s.rulesFor(r, tenant, event)
	if len(rules) == 0 {
		return false, ""
	}
	actions, fired, suspect := fireRulesNamed(rules, facts)
	caseID, _ := facts["case_id"].(string)
	for _, n := range suspect {
		slog.Warn("suspect rule skipped (unknown op/field)", "tenant", tenant, "event", event, "rule", n)
		s.logActivity(r.Context(), tenant, caseID, "RULE_SUSPECT",
			fmt.Sprintf("Rule %q skipped on %s: unrecognized condition — fix or disable it in the rules admin", n, event))
	}
	for _, a := range actions {
		switch a.Type {
		case "block_request":
			if !blocked {
				blocked = true
				blockMsg = expandTemplate(fmt.Sprint(a.Params["message"]), facts)
				if blockMsg == "" || blockMsg == "<nil>" {
					blockMsg = "blocked by program rule"
				}
			}
		case "notify":
			body := expandTemplate(fmt.Sprint(a.Params["body"]), facts)
			kind := fmt.Sprint(a.Params["kind"])
			if kind == "" || kind == "<nil>" {
				kind = "MILESTONE"
			}
			if _, err := s.db.Exec(r.Context(), `
				INSERT INTO public.notifications (tenant, user_sub, type, body)
				VALUES ($1,'*',$2,$3)`, tenant, kind, body); err != nil {
				slog.Error("rule notify failed", "tenant", tenant, "event", event, "err", err)
			}
		case "log_activity":
			s.logActivity(r.Context(), tenant, caseID, "RULE_"+strings.ToUpper(strings.ReplaceAll(event, ".", "_")),
				expandTemplate(fmt.Sprint(a.Params["body"]), facts))
		case "set_detail":
			key := fmt.Sprint(a.Params["key"])
			if caseID == "" || !ruleDetailKey.MatchString(key) {
				slog.Warn("rule set_detail rejected", "tenant", tenant, "event", event, "key", key, "case", caseID)
				continue
			}
			val := expandTemplate(fmt.Sprint(a.Params["value"]), facts)
			if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
				UPDATE tenant_%s.cases SET details = jsonb_set(details, $2, to_jsonb($3::text), true), updated_at=now()
				WHERE id=$1`, sanitizeTenant(tenant)), caseID, "{"+key+"}", val); err != nil {
				slog.Error("rule set_detail failed", "tenant", tenant, "event", event, "err", err)
			}
		case "flag_review":
			if docID, _ := facts["doc_id"].(string); docID != "" {
				if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
					UPDATE tenant_%s.documents SET analysis_status='NEEDS_REVIEW'
					WHERE id=$1`, sanitizeTenant(tenant)), docID); err != nil {
					slog.Error("rule flag_review failed", "tenant", tenant, "doc", docID, "err", err)
				}
			}
			s.logActivity(r.Context(), tenant, caseID, "RULE_FLAGGED",
				expandTemplate(fmt.Sprint(a.Params["reason"]), facts))
		case "set_status":
			// Only documents may be status-set by rules on the upload path;
			// case/intake status transitions stay owned by their handlers.
			if event == "doc.upload" {
				if docID, _ := facts["doc_id"].(string); docID != "" {
					st := fmt.Sprint(a.Params["status"])
					if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
						UPDATE tenant_%s.documents SET analysis_status=$2
						WHERE id=$1`, sanitizeTenant(tenant)), docID, st); err != nil {
						slog.Error("rule set_status failed", "tenant", tenant, "doc", docID, "err", err)
					}
				}
			}
		}
	}
	// One audit event per fired rule — Dapr pubsub → Kafka idre.<tenant>.rules
	// → Flink bronze → lakehouse, so rule behavior itself is analyzable.
	for _, n := range fired {
		s.publish(r.Context(), tenant, "rules", map[string]any{
			"type": "rule.fired", "tenant": tenant, "event": event, "rule": n,
			"case_id": caseID, "blocked": blocked, "at": time.Now().UTC(),
		})
	}
	return blocked, blockMsg
}

// expandTemplate fills {{field}} placeholders from facts (notification text).
func expandTemplate(tpl string, facts map[string]any) string {
	for k, v := range facts {
		tpl = strings.ReplaceAll(tpl, "{{"+k+"}}", fmt.Sprint(v))
	}
	return tpl
}
