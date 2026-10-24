// Author: Patrick Munis at newwave
package main

// Permify ReBAC client — a second, finer authorization layer beneath Keycloak.
//
// Keycloak RBAC gates roles (COARSE: "is a FINANCE officer").
// Permify gates object-level permission (FINE: "may post to the tx ledger").
// Enabled when PERMIFY_URL is set; disabled (allow) in dev when unset — the
// Keycloak + tenancy middleware remains fail-closed regardless.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type permifyCheck struct {
	Metadata struct {
		SchemaVersion string `json:"schema_version"`
		SnapToken     string `json:"snap_token"`
		Depth         int    `json:"depth"`
	} `json:"metadata"`
	Entity struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"entity"`
	Permission string `json:"permission"`
	Subject    struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"subject"`
}

// allow asks Permify whether subject may perform permission on entity.
// Fail-CLOSED when Permify is configured but unreachable: for ledger and
// reveal operations, an authorization outage must not become authorization.
func (s *server) allow(ctx context.Context, entityType, entityID, permission, userSub string) bool {
	if s.permify == "" {
		return true // dev mode: Permify not deployed; Keycloak RBAC still enforced
	}
	var req permifyCheck
	req.Metadata.Depth = 20
	req.Entity.Type, req.Entity.ID = entityType, entityID
	req.Permission = permission
	req.Subject.Type, req.Subject.ID = "user", userSub

	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.permify+"/v1/tenants/t1/permissions/check", bytes.NewReader(body))
	if err != nil {
		return false
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Can string `json:"can"` // "RESULT_ALLOWED" | "RESULT_DENIED"
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	return out.Can == "RESULT_ALLOWED"
}

// requirePerm is a helper for handlers: 403 when the Permify check fails.
func (s *server) requirePerm(w http.ResponseWriter, r *http.Request, entityType, entityID, permission string) bool {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !s.allow(r.Context(), entityType, entityID, permission, p.Subject) {
		http.Error(w, fmt.Sprintf(`{"error":"permit denied: %s on %s:%s"}`, permission, entityType, entityID),
			http.StatusForbidden)
		return false
	}
	return true
}
