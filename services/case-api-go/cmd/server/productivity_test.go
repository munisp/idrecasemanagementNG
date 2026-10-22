package main

import "testing"

func TestRecommendNextAction(t *testing.T) {
	cases := []struct {
		name string
		in   nextActionInput
		want string
	}{
		{"payment lock wins over everything", nextActionInput{Lock: "AWAITING_PAYMENT", PendingQA: 3, IsReviewer: true, UnreadDocs: 2}, "send_payment_reminder"},
		{"reviewer with pending QA", nextActionInput{Status: "OPEN", PendingQA: 1, IsReviewer: true}, "qa_review"},
		{"pending QA ignored for non-reviewer", nextActionInput{Status: "OPEN", PendingQA: 1, IsReviewer: false}, "review_case"},
		{"unread doc findings", nextActionInput{Status: "OPEN", UnreadDocs: 2}, "review_documents"},
		{"duplicate clinical overlap", nextActionInput{Status: "OPEN", HasOverlaps: true}, "review_duplicates"},
		{"offer window with decider", nextActionInput{Status: "OFFER_WINDOW_OPEN", OfferWindowOpen: true, CanDecide: true}, "submit_offer"},
		{"offer window without decider falls through", nextActionInput{Status: "OFFER_WINDOW_OPEN", OfferWindowOpen: true}, "review_case"},
		{"checklist before negotiation", nextActionInput{Status: "OPEN_NEGOTIATION", ChecklistDue: 2}, "clear_checklist"},
		{"negotiation logging", nextActionInput{Status: "OPEN_NEGOTIATION"}, "log_negotiation"},
		{"fresh case triage", nextActionInput{Status: "INITIATED"}, "triage"},
		{"default", nextActionInput{Status: "PAID"}, "review_case"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := recommendNextAction(tc.in, "case-1")
			if got.ActionID != tc.want {
				t.Errorf("got %q want %q", got.ActionID, tc.want)
			}
			if got.Label == "" || got.Href == "" || got.Reason == "" {
				t.Errorf("action %q missing label/href/reason: %+v", tc.want, got)
			}
			if got.Confidence <= 0 || got.Confidence > 1 {
				t.Errorf("confidence out of range: %v", got.Confidence)
			}
		})
	}
}

func TestRecommendNextActionIneligbleLock(t *testing.T) {
	got := recommendNextAction(nextActionInput{Lock: "INELIGIBLE"}, "case-9")
	if got.ActionID != "review_eligibility" {
		t.Errorf("got %q want review_eligibility", got.ActionID)
	}
}
