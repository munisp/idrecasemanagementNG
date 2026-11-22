// Author: Patrick Munis at newwave
// breaker.go — circuit breakers for external third-party calls (Stripe API,
// SMTP relay) plus the Prometheus metrics registry that powers the SLO
// alerts. One vendor's bad day must degrade the platform gracefully (fast
// failure, queued retry at the caller) instead of cascading into case-api
// thread exhaustion — the lesson of the 2026-11 outage week, where one
// vendor's incident became everyone's outage.
//
// Internal platform services (vault, storage gateway, database) are NOT
// behind breakers: they fail-closed by design and their failure is the
// platform's failure anyway. Breakers exist only for third parties whose
// availability we do not control.
package main

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrBreakerOpen is returned immediately when the breaker is open. Callers
// should map it to "temporarily unavailable, retry later" (503 semantics),
// never to a permanent failure — the work is queued/deferred, not lost.
var ErrBreakerOpen = errors.New("circuit breaker open")

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerHalfOpen
	breakerOpen
)

// breaker trips after consecutiveFailures consecutive errors, stays open for
// resetAfter, then admits one probe (half-open). A successful probe closes
// it; a failed probe re-opens it for another resetAfter window.
type breaker struct {
	name                string
	consecutiveFailures int
	resetAfter          time.Duration

	mu       sync.Mutex
	state    breakerState
	failures int
	openedAt time.Time
}

func newBreaker(name string, consecutiveFailures int, resetAfter time.Duration) *breaker {
	return &breaker{name: name, consecutiveFailures: consecutiveFailures, resetAfter: resetAfter}
}

// do runs fn through the breaker, recording metrics. Only genuine remote
// failures should be reported as failures — call sites pass err, and fn's
// own error is returned verbatim.
func (b *breaker) do(fn func() error) error {
	if err := b.before(); err != nil {
		externalCalls.WithLabelValues(b.name, "breaker_open").Inc()
		return err
	}
	start := time.Now()
	err := fn()
	externalDuration.WithLabelValues(b.name).Observe(time.Since(start).Seconds())
	if err != nil {
		externalCalls.WithLabelValues(b.name, "error").Inc()
		b.after(false)
		return err
	}
	externalCalls.WithLabelValues(b.name, "ok").Inc()
	b.after(true)
	return nil
}

func (b *breaker) before() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerOpen:
		if time.Since(b.openedAt) >= b.resetAfter {
			b.state = breakerHalfOpen // admit exactly one probe
			breakerStateGauge.WithLabelValues(b.name).Set(float64(breakerHalfOpen))
			return nil
		}
		return fmt.Errorf("%s: %w", b.name, ErrBreakerOpen)
	default:
		return nil
	}
}

func (b *breaker) after(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.state = breakerClosed
		b.failures = 0
		breakerStateGauge.WithLabelValues(b.name).Set(float64(breakerClosed))
		return
	}
	switch b.state {
	case breakerHalfOpen: // probe failed — re-open for a full window
		b.state = breakerOpen
		b.openedAt = time.Now()
	case breakerClosed:
		b.failures++
		if b.failures >= b.consecutiveFailures {
			b.state = breakerOpen
			b.openedAt = time.Now()
		}
	}
	breakerStateGauge.WithLabelValues(b.name).Set(float64(b.state))
}

// stateName exposes the current state for /healthz-style diagnostics and tests.
func (b *breaker) stateName() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[breakerState]string{breakerClosed: "closed", breakerHalfOpen: "half-open", breakerOpen: "open"}[b.state]
}

// Platform breakers: tuned for "vendor wobble recovers in tens of seconds".
// Stripe: 3 consecutive failures (payment calls are latency-sensitive) opens
// for 30s. SMTP: 5 failures (mail is naturally queued/retried) opens for 60s.
var (
	stripeBreaker = newBreaker("stripe", 3, 30*time.Second)
	smtpBreaker   = newBreaker("smtp", 5, 60*time.Second)
)
