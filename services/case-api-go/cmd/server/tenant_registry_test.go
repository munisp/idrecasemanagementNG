// Author: Patrick Munis at newwave
// tenant_registry_test.go — pins the fail-closed behavior for DAST finding
// F2: a credential whose group claim names an unprovisioned tenant must get
// a precise 503 before any handler runs, never a generic 500 from a failed
// query against a schema that does not exist.
package main

import (
	"net/http"
	"testing"
)

// withTenantExists swaps the registry checker for the duration of a test.
func withTenantExists(t *testing.T, fn func(string) bool) {
	t.Helper()
	prev := tenantExists
	tenantExists = fn
	t.Cleanup(func() { tenantExists = prev })
}

func TestUnprovisionedTenantFailsClosed(t *testing.T) {
	withTenantExists(t, func(string) bool { return false })

	// A member of the (unprovisioned) tenant: membership passes, the
	// registry must stop the request before the handler.
	member := principal{Subject: "u1", Roles: []string{"CASE_MANAGER"}, Tenants: []string{"zz"}}
	code, reached := tenancyAllows(t, http.MethodGet, "zz", member)
	if reached {
		t.Fatal("unprovisioned tenant must never reach the handler")
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unprovisioned tenant must 503, got %d", code)
	}

	// A platform admin gets the same answer: the failure is provisioning,
	// not authorization, so broad roles must not bypass it.
	admin := principal{Subject: "root", Roles: []string{"PLATFORM_ADMIN"}}
	code, reached = tenancyAllows(t, http.MethodGet, "zz", admin)
	if reached || code != http.StatusServiceUnavailable {
		t.Fatalf("admin vs unprovisioned: reached=%v code=%d, want 503", reached, code)
	}
}

func TestProvisionedTenantUnaffected(t *testing.T) {
	withTenantExists(t, func(tenant string) bool { return tenant == "tx" })
	member := principal{Subject: "u1", Roles: []string{"CASE_MANAGER"}, Tenants: []string{"tx"}}
	code, reached := tenancyAllows(t, http.MethodGet, "tx", member)
	if !reached || code != http.StatusOK {
		t.Fatalf("provisioned tenant must pass: reached=%v code=%d", reached, code)
	}
}
