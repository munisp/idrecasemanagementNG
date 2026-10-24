// Author: Patrick Munis at newwave
package main

import (
	"strings"
	"testing"
)

func TestPrefillFeedbackAdjust(t *testing.T) {
	base := prefillValue{Value: "Acme", Confidence: "high", Source: "doc", Evidence: "extracted from EOB"}

	// Below the sample threshold: untouched, even at a terrible rate.
	got := prefillFeedbackAdjust(base, 2, 27)
	if got.Confidence != "high" || got.Evidence != base.Evidence {
		t.Errorf("below threshold must not move: %+v", got)
	}

	// At threshold with a healthy rate: untouched.
	got = prefillFeedbackAdjust(base, 20, 10)
	if got.Confidence != "high" {
		t.Errorf("67%% accept at n=30 must not demote: %+v", got)
	}

	// At threshold, mostly overridden: capped low, evidence explains why.
	got = prefillFeedbackAdjust(base, 9, 21)
	if got.Confidence != "low" {
		t.Errorf("30%% accept at n=30 must demote to low: %+v", got)
	}
	if !strings.Contains(got.Evidence, "override this suggestion 70% of the time (n=30)") {
		t.Errorf("evidence must carry rate and sample size: %q", got.Evidence)
	}
	if !strings.HasPrefix(got.Evidence, base.Evidence) {
		t.Errorf("original evidence must be preserved, not replaced: %q", got.Evidence)
	}

	// Value and source survive demotion — the suggestion stays usable.
	if got.Value != base.Value || got.Source != base.Source {
		t.Errorf("demotion must not touch value/source: %+v", got)
	}

	// Exactly half is not "less than half".
	got = prefillFeedbackAdjust(base, 15, 15)
	if got.Confidence != "high" {
		t.Errorf("50%% accept must not demote: %+v", got)
	}
}
