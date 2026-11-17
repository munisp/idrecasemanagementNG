// Author: Patrick Munis at newwave
// tenant_isolation_test.go — cross-tenant negative tests: the failure mode
// that ends a multi-state platform is tenant B reading tenant A. These pin
// the middleware contract (authn claim mapping + tenancy enforcement) so a
// refactor can never silently widen access. DB-level defense (Postgres RLS)
// is exercised separately by scripts/test-tenant-isolation.sh against the
// composed stack — these tests need no database.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// tenancyAllows runs the tenancy middleware as the router would deliver it:
// tenant path param set, principal already in context (authn runs first).
func tenancyAllows(t *testing.T, method, tenant string, p principal) (int, bool) {
	t.Helper()
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		// the handler must see the resolved tenant in context
		if got := r.Context().Value(ctxTenant{}); got != tenant {
			t.Errorf("ctxTenant = %v, want %q", got, tenant)
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, "/v1/tenants/"+tenant+"/cases", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("tenant", tenant)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, ctxPrincipal{}, p)
	rec := httptest.NewRecorder()
	tenancy(next).ServeHTTP(rec, req.WithContext(ctx))
	return rec.Code, reached
}

// TestCrossTenantDenied is the core negative matrix: credentials scoped to
// one state must never reach another state's data, on any method.
func TestCrossTenantDenied(t *testing.T) {
	staffTX := principal{Subject: "u1", Roles: []string{"CASE_MANAGER"}, Tenants: []string{"tx"}}
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		code, reached := tenancyAllows(t, m, "ny", staffTX)
		if reached {
			t.Fatalf("tx staff must never reach ny (%s)", m)
		}
		if code != http.StatusForbidden {
			t.Fatalf("cross-tenant %s must 403, got %d", m, code)
		}
	}
}

// TestSameTenantAllowed: the positive control — without it the negative
// matrix above would also pass if tenancy simply denied everything.
func TestSameTenantAllowed(t *testing.T) {
	staffTX := principal{Subject: "u1", Roles: []string{"CASE_MANAGER"}, Tenants: []string{"tx"}}
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		code, reached := tenancyAllows(t, m, "tx", staffTX)
		if !reached || code != http.StatusOK {
			t.Fatalf("tx staff must reach tx (%s): reached=%v code=%d", m, reached, code)
		}
	}
}

// TestPlatformRolesReachEveryTenant pins the deliberate exception: federal
// oversight roles are cross-tenant by design.
func TestPlatformRolesReachEveryTenant(t *testing.T) {
	for _, role := range []string{"PLATFORM_ADMIN", "FEDERAL_ADMIN", serviceRole} {
		p := principal{Subject: "fed", Roles: []string{role}} // no Tenants at all
		code, reached := tenancyAllows(t, http.MethodPost, "ny", p)
		if !reached || code != http.StatusOK {
			t.Fatalf("%s must reach every tenant: reached=%v code=%d", role, reached, code)
		}
	}
}

// TestAuditorReadOnlyEverywhere: STATE_AUDITOR observes every state but
// writes nowhere — not even their home tenant. An auditor who also holds an
// operational role writes only where their group membership allows.
func TestAuditorReadOnlyEverywhere(t *testing.T) {
	auditor := principal{Subject: "aud", Roles: []string{"STATE_AUDITOR"}}
	if code, reached := tenancyAllows(t, http.MethodGet, "fl", auditor); !reached || code != http.StatusOK {
		t.Fatalf("auditor must read any tenant: reached=%v code=%d", reached, code)
	}
	if _, reached := tenancyAllows(t, http.MethodPost, "fl", auditor); reached {
		t.Fatal("auditor must never write, even in a tenant they can read")
	}
	// auditor + operational role: writes only via group membership
	audStaff := principal{Subject: "aud2", Roles: []string{"STATE_AUDITOR", "CASE_MANAGER"}, Tenants: []string{"tx"}}
	if _, reached := tenancyAllows(t, http.MethodPost, "tx", audStaff); !reached {
		t.Fatal("auditor+CASE_MANAGER must write in their own tenant")
	}
	if _, reached := tenancyAllows(t, http.MethodPost, "ny", audStaff); reached {
		t.Fatal("auditor+CASE_MANAGER must not write outside their tenant")
	}
}

// TestTenantParamValidation: the tenant path param is exactly two lowercase
// letters — anything else is a 400, never silently coerced into a lookup.
func TestTenantParamValidation(t *testing.T) {
	p := principal{Subject: "u1", Roles: []string{"PLATFORM_ADMIN"}}
	for _, bad := range []string{"", "t", "tex", "t1", "../x"} {
		code, reached := tenancyAllows(t, http.MethodGet, bad, p)
		if reached || code != http.StatusBadRequest {
			t.Fatalf("tenant %q must 400 before any role check: code=%d reached=%v", bad, code, reached)
		}
	}
}

// TestStaffWithNoTenantGroupsDenied: a valid staff token with zero
// /tenant/<st> groups reaches nothing — group membership is the only
// tenant grant for ordinary staff.
func TestStaffWithNoTenantGroupsDenied(t *testing.T) {
	p := principal{Subject: "u2", Roles: []string{"CASE_MANAGER", "PM"}}
	if code, reached := tenancyAllows(t, http.MethodGet, "tx", p); reached || code != http.StatusForbidden {
		t.Fatalf("staff with no tenant groups must 403: code=%d reached=%v", code, reached)
	}
}

// --- JWT claim mapping: the tenancy decision is only as honest as the
// claims it reads. These sign real tokens and run them through authn. ---

func authnFor(t *testing.T) (*authn, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := jwk.FromRaw(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	pub.Set(jwk.KeyIDKey, "test-key")
	pub.Set(jwk.AlgorithmKey, jwa.RS256)
	keys := jwk.NewSet()
	keys.AddKey(pub)
	return &authn{keys: keys, issuer: "https://issuer.test/realms/idre"}, key
}

func signToken(t *testing.T, key *rsa.PrivateKey, issuer string, expires time.Time, claims map[string]any) string {
	t.Helper()
	b := jwt.NewBuilder().
		Subject("user-123").
		Issuer(issuer).
		Audience([]string{"idre-portal"}).
		IssuedAt(time.Now().Add(-time.Minute)).
		Expiration(expires)
	for k, v := range claims {
		b = b.Claim(k, v)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	signKey, err := jwk.FromRaw(key)
	if err != nil {
		t.Fatal(err)
	}
	signKey.Set(jwk.KeyIDKey, "test-key")
	signKey.Set(jwk.AlgorithmKey, jwa.RS256)
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, signKey))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

func runAuthn(t *testing.T, a *authn, raw string) (int, principal) {
	t.Helper()
	var got principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Context().Value(ctxPrincipal{}).(principal)
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/tx/cases", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	a.middleware(next).ServeHTTP(rec, req)
	return rec.Code, got
}

// TestTenantClaimMapping: only /tenant/<st> group entries become tenant
// grants; realm roles and arbitrary groups must not leak into Tenants.
func TestTenantClaimMapping(t *testing.T) {
	a, key := authnFor(t)
	raw := signToken(t, key, "https://issuer.test/realms/idre", time.Now().Add(5*time.Minute), map[string]any{
		"groups":       []any{"/tenant/tx", "/staff", "/tenant/ny"},
		"realm_access": map[string]any{"roles": []any{"CASE_MANAGER"}},
	})
	code, got := runAuthn(t, a, raw)
	if code != http.StatusOK {
		t.Fatalf("valid token must authenticate, got %d", code)
	}
	want := map[string]bool{"tx": true, "ny": true}
	if len(got.Tenants) != 2 {
		t.Fatalf("Tenants = %v, want exactly [tx ny]", got.Tenants)
	}
	for _, tn := range got.Tenants {
		if !want[tn] {
			t.Fatalf("unexpected tenant grant %q (only /tenant/* groups may grant)", tn)
		}
	}
	if len(got.Roles) != 1 || got.Roles[0] != "CASE_MANAGER" {
		t.Fatalf("Roles = %v, want [CASE_MANAGER]", got.Roles)
	}
}

// TestBadTokensRejected: expired tokens, foreign issuers, and garbage must
// 401 before tenancy is ever consulted.
func TestBadTokensRejected(t *testing.T) {
	a, key := authnFor(t)
	expired := signToken(t, key, "https://issuer.test/realms/idre", time.Now().Add(-time.Minute), nil)
	foreign := signToken(t, key, "https://evil.test/realms/idre", time.Now().Add(5*time.Minute), map[string]any{
		"groups": []any{"/tenant/tx"},
	})
	for name, raw := range map[string]string{
		"expired":        expired,
		"foreign issuer": foreign,
		"garbage":        "not-a-jwt",
	} {
		if code, _ := runAuthn(t, a, raw); code != http.StatusUnauthorized {
			t.Fatalf("%s token must 401, got %d", name, code)
		}
	}
}

// TestWorkerTokenHasNoTenantGroups: the service worker role passes tenancy
// by design (service-to-service), but carries no tenant groups and no
// username — it must never look like a human tenant member.
func TestWorkerTokenHasNoTenantGroups(t *testing.T) {
	a := &authn{keys: jwk.NewSet(), issuer: "https://issuer.test/realms/idre", workerToken: "w"}
	code, got := runAuthn(t, a, "w")
	if code != http.StatusOK {
		t.Fatalf("worker token must authenticate, got %d", code)
	}
	if len(got.Tenants) != 0 {
		t.Fatalf("worker principal must carry no tenant groups, got %v", got.Tenants)
	}
	if !hasRole(got, serviceRole) {
		t.Fatalf("worker principal must hold %s, got %v", serviceRole, got.Roles)
	}
}
