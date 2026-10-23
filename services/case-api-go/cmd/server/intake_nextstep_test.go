package main

import (
	"testing"
	"time"
)

func TestRecommendIntakeStep(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-10 * 24 * time.Hour)
	fresh := now.Add(-2 * 24 * time.Hour)
	recentPacket := now.Add(-3 * 24 * time.Hour)
	overduePacket := now.Add(-11 * 24 * time.Hour)

	cases := []struct {
		name       string
		status     string
		created    time.Time
		packet     *time.Time
		wantAction string // "" expects nil
		wantAdv    string
	}{
		{"docs received starts clock", "DOCS_RECEIVED", old, nil, "verify_packet", "PACKET_COMPLETE"},
		{"packet complete mid-clock", "PACKET_COMPLETE", old, &recentPacket, "review_packet", ""},
		{"packet complete overdue", "PACKET_COMPLETE", old, &overduePacket, "review_packet", ""},
		{"paid converts", "PAID", old, nil, "convert", "CONVERTED"},
		{"stale instructed reminds", "INSTRUCTED", old, nil, "send_reminder", ""},
		{"fresh instructed waits", "INSTRUCTED", fresh, nil, "", ""},
		{"converted terminal", "CONVERTED", old, nil, "", ""},
		{"refunded terminal", "CLOSED_REFUNDED", old, nil, "", ""},
		{"ineligible terminal", "INELIGIBLE", old, nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := recommendIntakeStep(tc.status, tc.created, tc.packet, now)
			if tc.wantAction == "" {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %q, got nil", tc.wantAction)
			}
			if got.ActionID != tc.wantAction || got.AdvanceTo != tc.wantAdv {
				t.Errorf("got %+v", got)
			}
			if got.Label == "" || got.Reason == "" {
				t.Errorf("missing label/reason: %+v", got)
			}
		})
	}
}

func TestRecommendIntakeStepOverdueWording(t *testing.T) {
	now := time.Now().UTC()
	overdue := now.Add(-12 * 24 * time.Hour)
	got := recommendIntakeStep("PACKET_COMPLETE", overdue, &overdue, now)
	if got == nil || got.Label != "Initial review OVERDUE" {
		t.Errorf("overdue wording: %+v", got)
	}
}
