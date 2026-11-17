# Author: Patrick Munis at newwave
# Runbook: Backup & Restore

Backup production is `scripts/backup-postgres.sh` (nightly, age-encrypted,
WORM-retained via the storage gateway). This runbook is the other half:
**proving the backups work.**

## Guarantees

- Encryption happens before upload — storage compromise ≠ data breach.
- The `backups/` prefix sits under a WORM/object-lock bucket policy — even
  valid platform credentials cannot delete or overwrite a backup inside the
  retention window (6 years, matching the federal document mandate).
- Backup decryption key is offline (two-person control). Restores are a
  deliberate ceremony, which is exactly what you want at 03:00 under attack.

## Quarterly restore drill (mandatory)

1. Pick the most recent nightly object:
   `GET /objects/backups?prefix=YYYY/MM/DD` via the storage gateway.
2. Fetch and decrypt on an isolated host with the offline age key:
   `age --decrypt -i age.key idre-<ts>.dump.age > restore.dump`
3. Restore into a scratch database (never the live one):
   `createdb idre_restore_drill && pg_restore --dbname=idre_restore_drill restore.dump`
4. Verify, don't assume:
   - row counts on `tenant_<st>.cases` match the source's counts at dump time
   - one known case's `audit_log` chain verifies (`scripts/verify-audit-chain.py`)
   - one document object referenced by the drill DB exists in storage
5. Record the drill (date, backup id, verifier, pass/fail) in the ops
   journal. A missed drill is itself an audit finding.
6. Drop the scratch DB; shred `restore.dump`.

## Disaster scenarios

| Scenario | Recovery path | Target |
|---|---|---|
| Accidental data deletion | Restore last nightly to scratch, surgically copy rows back | RPO 24h, RTO 4h |
| Ransomware on live DB | Fail over to WORM backup; audit chain proves integrity of pre-attack state | RPO 24h |
| Total region loss | age key (offline) + WORM bucket in second region (enable bucket replication in prod) | RPO 24h, RTO 24h |
| Audit-log tampering claim | `verify-audit-chain.py` against backup copy vs live — divergence localizes the tampering point | evidence-grade |

## What is NOT covered by pg_dump (and where it lives instead)

- **Object store documents**: ciphertext objects; protected by WORM +
  versioning + (prod) cross-region replication, not by pg_dump.
- **Kafka topics**: rebuilt from the Postgres outbox on replay.
- **Keycloak realm**: exported config lives in `deploy/keycloak/`; user
  federation re-syncs from the tenant directory — back up Keycloak's own DB
  if local users are created outside IaC.
- **TigerBeetle ledger**: has its own replication/checkpointing; treat the
  ledger volume as separately backed-up infrastructure.
