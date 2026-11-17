# Author: Patrick Munis at newwave
# Runbook: Help-Desk MFA / Credential Reset

Identity is the platform's top attack vector. The 2025–2026 breach wave
(Aflac, Marks & Spencer, Qantas — Scattered Spider playbook) entered through
help-desk social engineering: the attacker calls in, impersonates a staff
member, and talks the help desk into resetting MFA. This runbook exists so a
convincing phone call is never enough.

## Non-negotiable rules

1. **No same-call resets.** An MFA reset or password reset for any staff
   account is never completed on the call that requested it.
2. **Two-person approval.** Every reset requires a second approver holding
   TENANT_ADMIN (for tenant staff) or PLATFORM_ADMIN (for tenant admins and
   platform staff). One approver, one executor, never the same person.
3. **Verified callback.** Before approval, the help desk calls the requester
   back on the phone number **already on file** in the tenant directory (not
   a number supplied during the request), or verifies via the requester's
   manager of record.
4. **Step-up for sensitive changes.** Payout-account details, tenant
   configuration, and role grants require re-authentication in the portal at
   change time, independent of any help-desk action.
5. **SMS is never a factor.** If a user reports their phone lost, the
   replacement factor is a new passkey/TOTP enrolled at verified callback —
   never a fallback to SMS.

## Procedure

1. Log the request (ticketing) with the caller's claimed identity, callback
   number on file, and the manager of record.
2. Terminate the inbound call. Initiate the verified callback.
3. On successful callback verification, route to the approver. The approver
   independently confirms with the manager of record.
4. The executor performs the reset in Keycloak (`Users → Credentials →
   Reset`), sets "Update password"/"Configure OTP" required actions, and
   **revokes all sessions** for the user (`Sessions → Log out all`).
5. Verify the audit trail: Keycloak admin events are enabled on the realm
   (`adminEventsEnabled`), and the reset must appear in the platform audit
   log the same day. If it doesn't, treat as an incident.
6. Notify the user and their manager of record that the reset completed.

## Detection hooks

- Keycloak admin events (MFA credential removal, user update) stream to the
  SIEM (Wazuh ruleset `deploy/wazuh/rules/idre-rules.xml`): any credential
  reset without a matching approved ticket fires an alert.
- More than 3 denied push/OTP attempts in 10 minutes on one account pages
  the on-call.
- A credential reset followed within 1 hour by a login from a new IP or new
  device fingerprint is a **P1 incident**, not a notification.

## If compromise is suspected anyway

1. Disable the account in Keycloak immediately (`Users → Enabled: off`).
2. Revoke all sessions and refresh tokens.
3. Pull the audit log for the account's subject across the last 30 days
   (hash-chained — gaps or hash breaks are themselves evidence).
4. Rotate `VAULT_INTERNAL_TOKEN`, `STORAGE_INTERNAL_TOKEN`, and
   `WORKER_TOKEN` if the account held TENANT_ADMIN or above
   (docs/RUNBOOK-secrets-rotation.md).
5. Open the incident record; notify per the breach-notification matrix
   (state AG + affected tenants; PHI involvement triggers HIPAA timelines).
