# Author: Patrick Munis at newwave
# Runbook: Secrets Rotation

Every credential the platform holds, where it lives, how to rotate it, and
what breaks if you don't. Standing rule: **rotate on schedule (90 days),
rotate on suspicion (immediately), rotate on departure (same day).**

## Inventory

| Secret | Consumers | Rotation procedure | Blast if leaked |
|---|---|---|---|
| `VAULT_INTERNAL_TOKEN` | case-api, lettergen, doc-intel, idre-workflows → vault | 1. Generate 32B random. 2. Set on vault, restart vault (vault accepts ONE token — brief overlap window is not supported, so restart vault first, then consumers). 3. Update consumers, rolling restart. 4. Verify: `curl -H "X-Internal-Auth: $NEW" http://vault:8081/healthz` 200. | Full decrypt oracle for all documents |
| `STORAGE_INTERNAL_TOKEN` | case-api, doc-intel, lettergen → storage-rs | Same order: storage-rs first, then consumers. storage-rs fails closed when set — mismatch = hard 401s. | Full object-store read/write |
| `WORKER_TOKEN` | idre-workflows, doc-intel → case-api | Update case-api env + workers; rolling restart; old tokens die at once (single value). | Service-role API access (passes tenancy) |
| Keycloak `client_secret` (per client) | portal, service clients | Keycloak Admin → Clients → Credentials → Regenerate; update consumer; revoke old. | Token minting as that client |
| Postgres `idre` password | all services | `ALTER ROLE idre PASSWORD`; rolling restart; update secrets store. | Full DB read/write (RLS still applies per-role) |
| MinIO root + app keys | storage-rs, lakehouse | Create new access key, update storage-rs + analytics, restart, delete old key. Root key: console rotation, only for ops. | Raw ciphertext store access |
| `STRIPE_SECRET_KEY` / webhook secret | case-api | Stripe dashboard → roll key; update; verify a test webhook signature. | Payment initiation |
| `VAULT_MASTER_SECRET` | vault-rs only | **Key ceremony**, not a restart: generate new master, re-wrap all tenant DEKs (vault re-wrap endpoint), then swap. Practice in staging first. | With ciphertext store: full plaintext |
| `DEID_DATE_SHIFT_SEED` | deid | Rotate only with a documented migration note — old shifted dates must remain reproducible for cross-referencing prior exports. | Re-identification of de-id exports |
| SMTP / ClamAV / Redis creds | case-api, workers | Update both sides, rolling restart. | Email spoofing as platform |

## Mechanics

- Secrets live in the platform secrets store (sops-encrypted files or
  External Secrets + cloud KMS in prod) — never in git, never in chat.
  Compose files hold dev-only defaults clearly marked as such.
- Rotation evidence: log the rotation (who/when/which secret class) in the
  ops journal; the SIEM watches for old-token use for 72h post-rotation
  (rule 100261 fires on repeated internal-token 401s).
- Age backup key (`BACKUP_AGE_PUBLIC_KEY`): the **private** half lives
  offline (hardware token or sealed envelope, two-person control). It is
  never on any running host; restores require the ceremony in
  RUNBOOK-backup-restore.md.

## On suspicion of compromise

Skip the schedule. Rotate the affected class **and everything it can reach**
(e.g. a leaked TENANT_ADMIN account triggers internal-token rotation per
RUNBOOK-helpdesk-mfa-reset.md §"If compromise is suspected"). Note the
correlation: an internal token alone is not enough inside the mesh (mTLS),
but on the compose deployment it is — treat internal-token leaks as P1.
