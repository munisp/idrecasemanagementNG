// Author: Patrick Munis at newwave
// metrics.go — Prometheus metrics + instrumentation middleware. These feed
// the SLO alerts in deploy/prometheus/idre-alerts.yml: degradation is
// detected regardless of cause (attack, vendor outage, or self-inflicted
// change) because the 2026-11 outage week proved attribution always lags.
//
// No tenant label anywhere: tenant codes in public metrics would leak the
// tenant list and multiply cardinality 50x. Tenant-scoped investigation
// belongs in logs/traces, not in the metrics plane.
package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "idre", Name: "http_requests_total",
		Help: "HTTP requests by route template, method and status code.",
	}, []string{"route", "method", "code"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "idre", Name: "http_request_duration_seconds",
		Help:    "HTTP request latency by route template.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"route", "method"})

	externalCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "idre", Name: "external_call_total",
		Help: "Outbound third-party calls by target and result (ok|error|breaker_open).",
	}, []string{"target", "result"})

	externalDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "idre", Name: "external_call_duration_seconds",
		Help:    "Outbound third-party call latency.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"target"})

	breakerStateGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "idre", Name: "breaker_state",
		Help: "Circuit breaker state per target: 0=closed 1=half-open 2=open.",
	}, []string{"target"})

	outboxBacklog = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "idre", Name: "outbox_unpublished",
		Help: "Unpublished rows in the Postgres outbox (Kafka relay lag).",
	})

	vaultRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "idre", Name: "vault_call_total",
		Help: "Vault seal/open calls by result (ok|error).",
	}, []string{"result"})
)

// instrument records request count + latency per chi route template. It
// wraps ResponseWriter to capture the status code without changing handler
// behavior (handlers write exactly as before).
func instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(ww, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		code := strconv.Itoa(ww.code)
		httpRequests.WithLabelValues(route, r.Method, code).Inc()
		httpDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}

// statusWriter captures the status code without changing handler behavior;
// Flush/Unwrap pass through for streaming handlers.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serveMetrics runs the dedicated metrics listener (started as a goroutine
// from main). Kept off the API port deliberately: the API port is published
// in compose and routed at the edge in k8s, while this listener is bound
// internally only — not published in compose, Cilium-locked to the
// observability namespace in k8s. Carries no tenant labels, so scraping
// leaks nothing beyond route names and rates.
func serveMetrics(port string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("metrics listener failed: %v", err)
	}
}

// pollOutboxBacklog refreshes the outbox gauge every 15s. Runs for the life
// of the process; a dead Kafka relay shows up here before users notice
// missing events. The outbox is per-tenant (tenant_<st>.outbox), so the
// gauge sums unpublished rows across every tenant schema — the same set the
// KEDA generator (scripts/gen-keda-outbox-query.sh) enumerates.
func (s *server) pollOutboxBacklog() {
	go func() {
		for {
			if n, err := s.countOutboxBacklog(); err == nil {
				outboxBacklog.Set(float64(n))
			}
			time.Sleep(15 * time.Second)
		}
	}()
}

// countOutboxBacklog sums unpublished outbox rows over all tenant schemas.
// Schema names come from the catalog and are validated against the tenant
// pattern before interpolation (identifiers can't be query parameters).
func (s *server) countOutboxBacklog() (int64, error) {
	ctx := context.Background()
	rows, err := s.db.Query(ctx, `SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE 'tenant\_%'`)
	if err != nil {
		return 0, err
	}
	var schemas []string
	for rows.Next() {
		var sch string
		if err := rows.Scan(&sch); err != nil {
			rows.Close()
			return 0, err
		}
		schemas = append(schemas, sch)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, sch := range schemas {
		if !validTenantSchema(sch) {
			continue
		}
		var n int64
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM "`+sch+`".outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
			continue // one broken tenant must not blind the whole gauge
		}
		total += n
	}
	return total, nil
}

// validTenantSchema gates catalog values before SQL interpolation:
// exactly tenant_<st> with a two-letter lowercase state code.
func validTenantSchema(sch string) bool {
	if len(sch) != len("tenant_")+2 || sch[:len("tenant_")] != "tenant_" {
		return false
	}
	st := sch[len("tenant_"):]
	return st[0] >= 'a' && st[0] <= 'z' && st[1] >= 'a' && st[1] <= 'z'
}
