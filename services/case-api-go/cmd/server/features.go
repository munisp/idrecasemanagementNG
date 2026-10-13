package main

import (
	"net/http"
)

// features.go — server-side enforcement of the manifest's feature toggles.
// Until now the manifest validated feature keys and the portal hid the UI,
// but no endpoint actually checked the flag: a UI-only toggle is decoration,
// not a control. Absent manifest / absent key = default ON (same semantics
// as the manifest defaults), so existing tenants change nothing.

// featureEnabled resolves a manifest feature for the tenant, fresh per call
// — an admin edit applies to the very next request.
func (s *server) featureEnabled(r *http.Request, tenant, name string) bool {
	m, err := s.manifestFor(r.Context(), tenant)
	if err != nil || m == nil {
		return true
	}
	if v, ok := m.Features[name]; ok {
		return v
	}
	return true
}

// featureGate rejects with 404 (not 403 — a disabled feature should be
// indistinguishable from one that doesn't exist) when the flag is off.
// Returns true when the caller may proceed.
func (s *server) featureGate(w http.ResponseWriter, r *http.Request, tenant, name string) bool {
	if s.featureEnabled(r, tenant, name) {
		return true
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	return false
}
