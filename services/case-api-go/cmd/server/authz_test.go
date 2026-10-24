// Author: Patrick Munis at newwave
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lestrrat-go/jwx/v2/jwk"
)

func TestHasAnyRole(t *testing.T) {
	p := principal{Roles: []string{"ARBITRATOR"}}
	if hasAnyRole(p, "FINANCE", "CASE_MANAGER") {
		t.Fatal("ARBITRATOR must not match a FINANCE/CASE_MANAGER-only gate")
	}
	if !hasAnyRole(p, "FINANCE", "ARBITRATOR") {
		t.Fatal("ARBITRATOR must match when it's one of the allowed roles")
	}
}

// Reproduces the live finding (sibling deployment, main branch, same code):
// before the RBAC floor existed, an ARBITRATOR token reached
// postFeeTransfer/escalateCase/reveal with no role check at all (Permify,
// the only other gate, allows everyone when undeployed).
func TestFeeTransferRoleFloorRejectsWrongRole(t *testing.T) {
	cases := []struct {
		roles []string
		want  bool
	}{
		{[]string{"ARBITRATOR"}, false},
		{[]string{"PARTY"}, false},
		{[]string{"FINANCE"}, true},
		{[]string{"CASE_MANAGER"}, true},
		{[]string{"FEDERAL_ADMIN"}, true},
		{[]string{"PLATFORM_ADMIN"}, true},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, "FINANCE", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
		if got != c.want {
			t.Errorf("roles=%v: got allowed=%v, want %v", c.roles, got, c.want)
		}
	}
}

// Reproduces the second live finding: WORKER_TOKEN was never validated
// anywhere, so the Temporal worker's own service-to-service escalate call
// always 401'd. A valid worker token must authenticate as serviceRole; any
// other value must fall through to (and fail) ordinary JWT parsing.
func TestWorkerTokenAuthenticates(t *testing.T) {
	keys := jwk.NewSet()
	a := &authn{keys: keys, issuer: "https://issuer.test/realms/idre", workerToken: "the-worker-secret"}

	var gotPrincipal principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal = r.Context().Value(ctxPrincipal{}).(principal)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/tx/cases/x/escalate", nil)
	req.Header.Set("Authorization", "Bearer the-worker-secret")
	rec := httptest.NewRecorder()
	a.middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("worker token must authenticate, got status %d body %s", rec.Code, rec.Body.String())
	}
	if !hasRole(gotPrincipal, serviceRole) {
		t.Fatalf("worker token must grant %s, got roles %v", serviceRole, gotPrincipal.Roles)
	}
}

func TestWrongTokenFallsThroughToJWTAndFails(t *testing.T) {
	keys := jwk.NewSet()
	a := &authn{keys: keys, issuer: "https://issuer.test/realms/idre", workerToken: "the-worker-secret"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/tx/cases", nil)
	req.Header.Set("Authorization", "Bearer not-the-worker-secret-and-not-a-jwt")
	rec := httptest.NewRecorder()
	a.middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a non-matching, non-JWT token must 401, got %d", rec.Code)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if isUniqueViolation(nil) {
		t.Fatal("nil error is not a unique violation")
	}
	if isUniqueViolation(&pgconn.PgError{Code: "23503"}) { // foreign_key_violation
		t.Fatal("a different pg error code must not be treated as a unique violation")
	}
	if !isUniqueViolation(&pgconn.PgError{Code: "23505"}) { // unique_violation
		t.Fatal("pg error code 23505 must be recognized as a unique violation")
	}
}
