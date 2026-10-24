// Author: Patrick Munis at newwave
// onboarding.go — stakeholder onboarding: applications, role-based approval
// chains, and status. The durable state machine lives in Temporal
// (StakeholderOnboardingWorkflow); these endpoints are its front door.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	temporalclient "go.temporal.io/sdk/client"
)

// Stakeholder types: IDRE_ENTITY (certified dispute-resolution entity),
// PROVIDER_ORG, PAYER_ORG, STATE_AUDITOR_ORG, ADMIN_STAFF.
type OnboardingApplication struct {
	ID          string         `json:"id"`
	Tenant      string         `json:"tenant"`
	Type        string         `json:"type"`        // IDRE_ENTITY | PROVIDER_ORG | PAYER_ORG | ...
	LegalName   string         `json:"legal_name"`
	EIN         string         `json:"ein"`         // masked at rest
	NPI         string         `json:"npi,omitempty"`
	Payload     map[string]any `json:"payload"`     // type-specific fields (fee schedule, COI, banking…)
	Status      string         `json:"status"`
	SubmittedAt time.Time      `json:"submitted_at"`
}

// createApplication inserts the application row and starts the durable
// onboarding workflow. Shared by the authenticated and public front doors.
func (s *server) createApplication(r *http.Request, tenant string, in OnboardingApplication) (appID, wfID string, err error) {
	payload, _ := json.Marshal(in.Payload)
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.stakeholder_applications
		  (tenant, type, legal_name, ein_masked, npi, payload, status)
		VALUES ($1,$2,$3,$4,$5,$6,'SUBMITTED') RETURNING id`,
		tenant, in.Type, in.LegalName, maskEIN(in.EIN), in.NPI, payload).Scan(&appID)
	if err != nil {
		return "", "", err
	}
	wfID = fmt.Sprintf("ONB-%s-%s", tenant, appID)
	_, err = s.tc.ExecuteWorkflow(r.Context(), temporalclient.StartWorkflowOptions{
		ID: wfID, TaskQueue: "idre-onboarding",
	}, "StakeholderOnboardingWorkflow", map[string]any{
		"tenant": tenant, "application_id": appID, "type": in.Type,
		"legal_name": in.LegalName, "ein": in.EIN, "npi": in.NPI, "payload": in.Payload,
	})
	if err != nil {
		return "", "", err
	}
	return appID, wfID, nil
}

// submitApplication: POST /v1/tenants/{tenant}/onboarding/applications
// Creates the application row and starts the durable onboarding workflow.
func (s *server) submitApplication(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in OnboardingApplication
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.LegalName == "" || in.Type == "" {
		http.Error(w, `{"error":"legal_name and type required"}`, http.StatusBadRequest)
		return
	}
	appID, wfID, err := s.createApplication(r, tenant, in)
	if err != nil {
		http.Error(w, `{"error":"workflow start failed"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"application_id": appID, "workflow_id": wfID, "status": "SUBMITTED",
	})
}

// publicApply: POST /api/public/apply — the no-login front door for external
// stakeholders (provider orgs, payer orgs, IDRE entities, auditors) from the
// landing site. Abuse controls: strict per-IP throttle (10/hour), tenant
// validated against state_config, type whitelist, body size cap. ADMIN_STAFF is
// deliberately NOT accepted here — staff are provisioned by admins, never by
// self-service. Applicant contact details ride in payload.
func (s *server) publicApply(w http.ResponseWriter, r *http.Request) {
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.Split(fwd, ",")[0]
	}
	if !s.rateLimit("apply:"+ip, 10, 3600) {
		http.Error(w, `{"error":"rate limited — try again later"}`, http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in OnboardingApplication
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.LegalName == "" || in.Type == "" || in.Tenant == "" {
		http.Error(w, `{"error":"tenant, type and legal_name required"}`, http.StatusBadRequest)
		return
	}
	switch in.Type {
	case "IDRE_ENTITY", "PROVIDER_ORG", "PAYER_ORG", "STATE_AUDITOR_ORG":
	default:
		http.Error(w, `{"error":"unsupported organization type"}`, http.StatusBadRequest)
		return
	}
	tenant := strings.ToLower(strings.TrimSpace(in.Tenant))
	var exists bool
	if err := s.db.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM public.state_config WHERE tenant=$1)`, tenant).Scan(&exists); err != nil || !exists {
		http.Error(w, `{"error":"unknown state program"}`, http.StatusBadRequest)
		return
	}
	if in.Payload == nil {
		in.Payload = map[string]any{}
	}
	email, _ := in.Payload["contact_email"].(string)
	if email == "" || !strings.Contains(email, "@") {
		http.Error(w, `{"error":"contact_email required"}`, http.StatusBadRequest)
		return
	}
	in.Payload["source"] = "PUBLIC_LANDING"
	appID, wfID, err := s.createApplication(r, tenant, in)
	if err != nil {
		http.Error(w, `{"error":"submission failed"}`, http.StatusBadGateway)
		return
	}
	s.notify(r, tenant, "*", "ONBOARDING",
		fmt.Sprintf("New public %s application: %s", in.Type, in.LegalName), "#/onboarding")
	writeJSON(w, http.StatusCreated, map[string]any{
		"application_id": appID, "workflow_id": wfID, "status": "SUBMITTED",
	})
}

func (s *server) listApplications(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), `
		SELECT id, tenant, type, legal_name, npi, status, submitted_at
		FROM public.stakeholder_applications
		WHERE tenant=$1 ORDER BY submitted_at DESC LIMIT 200`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []OnboardingApplication{}
	for rows.Next() {
		var a OnboardingApplication
		if rows.Scan(&a.ID, &a.Tenant, &a.Type, &a.LegalName, &a.NPI, &a.Status, &a.SubmittedAt) == nil {
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// decideApplication: approve/reject signals the workflow; the workflow performs
// provisioning (Keycloak account + tenant group, TB accounts for IDREs, invite email).
func (s *server) decideApplication(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		Decision string `json:"decision"` // APPROVE | REJECT
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.Decision != "APPROVE" && in.Decision != "REJECT") {
		http.Error(w, `{"error":"decision must be APPROVE or REJECT"}`, http.StatusBadRequest)
		return
	}
	appID := chi.URLParam(r, "appId")

	// Approval authority matrix (enforced again inside the workflow):
	//   IDRE_ENTITY  -> FEDERAL_ADMIN only
	//   others       -> CASE_MANAGER of that tenant, or FEDERAL_ADMIN
	var appType, tenant, legalName string
	if err := s.db.QueryRow(r.Context(),
		`SELECT type, tenant, legal_name FROM public.stakeholder_applications WHERE id=$1`, appID).
		Scan(&appType, &tenant, &legalName); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	authorized := hasRole(p, "FEDERAL_ADMIN")
	if appType != "IDRE_ENTITY" && hasRole(p, "CASE_MANAGER") {
		for _, t := range p.Tenants {
			if t == tenant {
				authorized = true
			}
		}
	}
	if !authorized {
		http.Error(w, `{"error":"insufficient approval authority"}`, http.StatusForbidden)
		return
	}

	wfID := fmt.Sprintf("ONB-%s-%s", tenant, appID)
	if err := s.tc.SignalWorkflow(r.Context(), wfID, "", "DECISION", map[string]any{
		"decision": in.Decision, "reason": in.Reason, "approver": p.Subject,
	}); err != nil {
		http.Error(w, `{"error":"signal failed"}`, http.StatusBadGateway)
		return
	}
	// Case-management integration: broadcast the decision to the tenant's staff.
	s.notify(r, tenant, "*", "ONBOARDING_"+in.Decision,
		fmt.Sprintf("Onboarding application %s (%s) %s — %s", appID, appType, in.Decision, truncate(in.Reason, 200)),
		"#/onboarding")
	if in.Decision == "APPROVE" {
		// OFAC sanctions screening (watchman-backed, warn-only): the approval
		// stands, but a hit is recorded and broadcast to staff.
		s.screenOnboardingSubject(r, tenant, strings.ToUpper(appType), appID, legalName)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "decision signaled"})
}

func hasRole(p principal, role string) bool {
	for _, rr := range p.Roles {
		if rr == role {
			return true
		}
	}
	return false
}

func maskEIN(ein string) string {
	if len(ein) < 4 {
		return "****"
	}
	return "*****" + ein[len(ein)-4:]
}
