// Author: Patrick Munis at newwave
// accesspolicy.go — who may reach which part of a tenant's API, decided in
// one place and before any handler runs.
//
// Two gaps made this necessary, both found by calling every route as every
// role instead of reading the menu:
//
//  1. A TPA (an outside organization filing on behalf of others) could read
//     ANY case in the tenant by id — its record, its documents, its timeline
//     — and could find those ids through search. The case list refused a
//     TPA; the routes underneath it did not, because each relied on the list
//     being the only way in.
//  2. Several tenant-wide lists (intake, deliverables, call logs) had no
//     role check at all: the page was hidden from a role's menu, and the
//     data behind it was one typed address away.
//
// Hiding a menu item is a courtesy, not a boundary. The boundary is here.
package main

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// tenantStaffRoles are the roles a tenant's own staff hold (orig lineage
// parity; NG's documents.go map is folder names, not roles).
var tenantStaffRoles = map[string]bool{
	"CASE_MANAGER": true, "PM": true, "FINANCE": true, "ARBITRATOR": true,
	"ATTORNEY": true, "CODER": true, "NURSE_PHYSICIAN": true, "DOCTOR": true,
	"NURSE": true, "TENANT_ADMIN": true,
}

// isExternalOnly reports whether the principal is an account of someone
// outside the office, with no office role: a TPA, a health plan, an invited
// party. Such a principal has standing only in its own area and in the cases
// it filed or was invited to (party_accounts). A health plan's login is made
// by the public registration page, so it must never pass as staff.
func isExternalOnly(p principal) bool {
	for _, r := range p.Roles {
		if tenantStaffRoles[r] || r == "FEDERAL_ADMIN" || r == "PLATFORM_ADMIN" || r == "STATE_AUDITOR" || r == serviceRole {
			return false
		}
	}
	// No office role: outside -- a TPA, a health plan, an invited party, or a
	// login with no role at all (which used to pass as staff; a public
	// registration whose role assignment failed produced exactly that).
	return true
}

// tpaOpenPaths are the parts of the API a TPA uses that are not about one
// case: its own area, the shell every signed-in page needs, and the two
// handlers that already answer a TPA from its own filings only.
var tpaOpenPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/tpa(/|$)`),
	regexp.MustCompile(`^/calendar$`),        // calendar() answers a TPA from its own filings
	regexp.MustCompile(`^/intake/bulk(/|$)`), // batch filing: the handlers scope to the caller's own batches
}

// hpOpenPaths: a health plan's own area (its profile, parties, packages,
// filings and the disputes against it -- every handler scopes to the plan the
// login belongs to).
var hpOpenPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/hp(/|$)`),
}

// outsideShellPaths are what every signed-in page needs, for any outside account.
var outsideShellPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/program$`),           // the portal reads the program's settings to draw itself
	regexp.MustCompile(`^/prefs$`),             // the caller's own preferences
	regexp.MustCompile(`^/notifications(/|$)`), // the caller's own notifications
	regexp.MustCompile(`^/audit/mine$`),        // the caller's own audit trail
	regexp.MustCompile(`^/upload-policy$`),     // what may be uploaded, and how big
	regexp.MustCompile(`^/uploads(/|$)`),       // the caller's own resumable uploads
	regexp.MustCompile(`^/presence/ping$`),
}

// partyOpenPaths: an invited party's own workspace, and the sealed offers of a
// case (submitOffer / listOffers admit a PARTY only on a case its
// party_accounts row names).
var partyOpenPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/party(/|$)`),
	regexp.MustCompile(`^/cases/[0-9a-fA-F-]{36}/offers$`),
}

func outsideOpenPaths(p principal) []*regexp.Regexp {
	out := append([]*regexp.Regexp{}, outsideShellPaths...)
	if hasRole(p, "TPA") {
		out = append(out, tpaOpenPaths...)
	}
	if hasRole(p, "HEALTH_PLAN") {
		out = append(out, hpOpenPaths...)
	}
	if hasRole(p, "PARTY") {
		out = append(out, partyOpenPaths...)
	}
	return out
}

// tpaOwnCasePaths are what a TPA may read of a case IT filed: the record,
// its deadlines and the documents. Nothing internal — no timeline, review
// notes, eligibility evidence, QA drafts or correspondence log.
var tpaOwnCasePaths = regexp.MustCompile(`^/cases/([0-9a-fA-F-]{36})(|/clocks|/documents|/documents/[0-9a-fA-F-]{36}/download)$`)

// pagePolicy is a read endpoint that belongs to one page of the portal, and
// the roles that page is for. The same sets drive the menu (portal/js/app.js
// NAV); a role without the page does not get its data by typing the address.
type pagePolicy struct {
	pattern *regexp.Regexp
	roles   []string
	page    string
}

var (
	intakePageRoles       = []string{"CASE_MANAGER", "ARBITRATOR", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole}
	deliverablesPageRoles = []string{"CASE_MANAGER", "ARBITRATOR", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole}
	voicePageRoles        = []string{"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole}
)

var pagePolicies = []pagePolicy{
	{regexp.MustCompile(`^/intake$`), intakePageRoles, "Intake"},
	{regexp.MustCompile(`^/deliverables$`), deliverablesPageRoles, "Deliverables"},
	{regexp.MustCompile(`^/voice/(intake|logs)$`), voicePageRoles, "Voice console"},
}

// filingDetailKeys are the parts of a case's details its filer supplied or
// may know. What the office adds while working the case -- the dispute
// summary, eligibility conflicts, fraud-waste-abuse flags, how the provider
// type was worked out -- is left out of what an outside filer receives.
var filingDetailKeys = []string{
	"filed_by_tpa", "filing_notes", "filing_party_type", "initiating_party", "initiating_party_contact",
	"requester_contact_name", "requester_email", "requester_org", "provider_org", "health_plan", "disputed_issue",
}

func filingDetails(details map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range filingDetailKeys {
		if v, ok := details[k]; ok {
			out[k] = v
		}
	}
	return out
}

// accessDecision is the pure core of the guard: given who is asking, the
// method and the path below /v1/tenants/{tenant}, it returns
//
//	"allow"      carry on to the handler (which applies its own checks)
//	"own-case"   allow only if the caller filed the case named in caseID
//	"deny"       refuse, with reason
//
// Pure, so every rule above is tested without a database.
func accessDecision(p principal, method, rel string) (decision, caseID, reason string) {
	if isExternalOnly(p) {
		// The federal process: a party starts its own dispute (#/new).
		if hasRole(p, "PARTY") && method == http.MethodPost && rel == "/cases/initiate" {
			return "allow", "", ""
		}
		for _, re := range outsideOpenPaths(p) {
			if re.MatchString(rel) {
				return "allow", "", ""
			}
		}
		if m := tpaOwnCasePaths.FindStringSubmatch(rel); m != nil && method == http.MethodGet {
			return "own-case", m[1], ""
		}
		return "deny", "", "this account can only use its own filing area"
	}
	if method == http.MethodGet {
		for _, pol := range pagePolicies {
			if pol.pattern.MatchString(rel) && !hasAnyRole(p, pol.roles...) {
				return "deny", "", "your role does not have the " + pol.page + " page"
			}
		}
	}
	return "allow", "", ""
}

// accessGuard applies accessDecision to every tenant-scoped request. It runs
// after authentication and the tenant check, and before the handler.
func (s *server) accessGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Context().Value(ctxPrincipal{}).(principal)
		tenant := r.Context().Value(ctxTenant{}).(string)
		rel := r.URL.Path
		if i := strings.Index(rel, "/v1/tenants/"); i >= 0 {
			rest := rel[i+len("/v1/tenants/"):]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				rel = rest[j:]
			} else {
				rel = ""
			}
		}
		decision, caseID, reason := accessDecision(p, r.Method, strings.TrimRight(rel, "/"))
		switch decision {
		case "allow":
			next.ServeHTTP(w, r)
		case "own-case":
			var mine bool
			_ = s.db.QueryRow(r.Context(), `
				SELECT EXISTS(SELECT 1 FROM public.case_origin o
				              JOIN public.tpa_users u ON u.tpa_id = o.tpa_id AND u.tenant = o.tenant
				              WHERE o.tenant=$1 AND o.case_id::text=$2 AND u.user_sub=$3)
				    OR EXISTS(SELECT 1 FROM public.case_origin o
				              JOIN public.health_plan_users u ON u.plan_id = o.health_plan_id AND u.tenant = o.tenant
				              WHERE o.tenant=$1 AND o.case_id::text=$2 AND u.user_sub=$3)
				    OR EXISTS(SELECT 1 FROM public.party_accounts pa
				              WHERE pa.tenant=$1 AND pa.case_id::text=$2 AND pa.kc_subject=$3)`,
				tenant, caseID, p.Subject).Scan(&mine)
			if !mine {
				// Not found, not forbidden: an outside account must not learn which case ids exist.
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			next.ServeHTTP(w, r)
		default:
			// NG lineage: no audit-log sink yet (it arrives with actionlog.go);
			// the denial is still recorded, to the server log.
			log.Printf("ACCESS_DENIED by=%s method=%s path=%s reason=%q", p.Subject, r.Method, rel, reason)
			http.Error(w, fmt.Sprintf(`{"error":%q}`, "forbidden: "+reason), http.StatusForbidden)
		}
	})
}
