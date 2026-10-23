package main

// intake_nextstep.go — rule-based next step for each row of the intake
// pipeline, computed inside listIntake (no extra queries: status + timestamps
// are already on the row). Same philosophy as the case next-action engine:
// hand-set rules with explicit reasons, advisory only, and the portal emits
// telemetry when staff follow a suggestion — the accept rates calibrate
// (and later replace) these rules.

import "time"

type intakeNextStep struct {
	ActionID  string `json:"action_id"`
	Label     string `json:"label"`
	Reason    string `json:"reason"`
	AdvanceTo string `json:"advance_to,omitempty"` // advanceIntake target when the step is a status move
}

// recommendIntakeStep: nil for terminal states and for "waiting is correct"
// (a fresh INSTRUCTED row is not attention-worthy — the party was just emailed).
func recommendIntakeStep(status string, createdAt time.Time, packetCompleteAt *time.Time, now time.Time) *intakeNextStep {
	switch status {
	case "DOCS_RECEIVED":
		return &intakeNextStep{"verify_packet", "Verify packet & start clock",
			"Documents arrived — verifying packet completeness starts the 10-day initial-review clock", "PACKET_COMPLETE"}
	case "PACKET_COMPLETE":
		if packetCompleteAt != nil {
			daysLeft := 10 - int(now.Sub(*packetCompleteAt).Hours()/24)
			if daysLeft < 0 {
				return &intakeNextStep{"review_packet", "Initial review OVERDUE",
					"The 10-day initial-review clock has expired — the review is late", ""}
			}
			return &intakeNextStep{"review_packet", "Complete initial review",
				"Day " + itoa(10-daysLeft) + " of the 10-day initial-review clock — " + itoa(daysLeft) + " day(s) left", ""}
		}
		return &intakeNextStep{"review_packet", "Complete initial review", "Packet is complete — the review clock is running", ""}
	case "PAID":
		return &intakeNextStep{"convert", "Convert to case",
			"Fee paid — conversion opens the dispute and starts the statutory clocks", "CONVERTED"}
	case "INSTRUCTED":
		if now.Sub(createdAt) > 5*24*time.Hour {
			return &intakeNextStep{"send_reminder", "Send packet reminder",
				"Instructions went out over 5 days ago and no documents have arrived — a reminder usually unblocks it", ""}
		}
		return nil
	default: // CONVERTED, CLOSED_REFUNDED, INELIGIBLE — terminal
		return nil
	}
}

// itoa is defined in graph.go.
