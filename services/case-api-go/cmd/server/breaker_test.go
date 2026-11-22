// Author: Patrick Munis at newwave
package main

import (
	"errors"
	"testing"
	"time"
)

func TestBreakerTripsAfterThreshold(t *testing.T) {
	b := newBreaker("test", 3, time.Minute)
	boom := errors.New("boom")
	for i := 0; i < 3; i++ {
		if err := b.do(func() error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("call %d: want underlying error, got %v", i, err)
		}
	}
	if b.stateName() != "open" {
		t.Fatalf("breaker should be open after 3 failures, state=%s", b.stateName())
	}
	// open: fails fast with ErrBreakerOpen, fn never runs
	ran := false
	if err := b.do(func() error { ran = true; return nil }); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("open breaker must return ErrBreakerOpen, got %v", err)
	}
	if ran {
		t.Fatal("open breaker must not run fn")
	}
}

func TestBreakerRecoversViaProbe(t *testing.T) {
	b := newBreaker("test", 1, 20*time.Millisecond)
	_ = b.do(func() error { return errors.New("boom") })
	if b.stateName() != "open" {
		t.Fatal("should be open")
	}
	time.Sleep(25 * time.Millisecond)
	// half-open probe succeeds -> closed
	if err := b.do(func() error { return nil }); err != nil {
		t.Fatalf("probe should run and succeed, got %v", err)
	}
	if b.stateName() != "closed" {
		t.Fatalf("successful probe must close breaker, state=%s", b.stateName())
	}
}

func TestBreakerProbeFailureReopens(t *testing.T) {
	b := newBreaker("test", 1, 20*time.Millisecond)
	_ = b.do(func() error { return errors.New("boom") })
	time.Sleep(25 * time.Millisecond)
	_ = b.do(func() error { return errors.New("still down") })
	if b.stateName() != "open" {
		t.Fatalf("failed probe must re-open, state=%s", b.stateName())
	}
	// still within the new window: fast-fail
	if err := b.do(func() error { return nil }); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("re-opened breaker must fast-fail, got %v", err)
	}
}

func TestBreakerSuccessResetsFailureCount(t *testing.T) {
	b := newBreaker("test", 3, time.Minute)
	boom := errors.New("boom")
	_ = b.do(func() error { return boom })
	_ = b.do(func() error { return boom })
	_ = b.do(func() error { return nil }) // resets the streak
	_ = b.do(func() error { return boom })
	_ = b.do(func() error { return boom })
	if b.stateName() != "closed" {
		t.Fatalf("interleaved success must reset the streak, state=%s", b.stateName())
	}
}

func TestStripeHTTPErrorMessage(t *testing.T) {
	e := &stripeHTTPError{code: 402, msg: "card declined"}
	if e.Error() != "stripe: card declined" {
		t.Fatalf("got %q", e.Error())
	}
	e2 := &stripeHTTPError{code: 500}
	if e2.Error() != "stripe: HTTP 500" {
		t.Fatalf("got %q", e2.Error())
	}
}
