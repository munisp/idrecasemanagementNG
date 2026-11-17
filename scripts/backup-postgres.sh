#!/usr/bin/env bash
# Author: Patrick Munis at newwave
# backup-postgres.sh — encrypted, immutable, off-blast-radius backup.
#
# Design rules (ransomware-grade):
#   1. Dump is encrypted with age BEFORE it leaves the host — the storage
#      account never holds plaintext, so a storage compromise is not a
#      data breach.
#   2. Upload goes through the storage gateway into a WORM bucket prefix
#      (backups/...): object-lock retention means even valid platform
#      credentials cannot delete or overwrite a backup.
#   3. Backup credentials differ from runtime credentials: the age public
#      key encrypts, only the offline-held private key decrypts.
#   4. A backup you haven't restored from is not a backup — see
#      docs/RUNBOOK-backup-restore.md for the quarterly restore drill.
#
# Env:  DATABASE_URL            postgres://idre:idre@postgres:5432/idre
#       BACKUP_AGE_PUBLIC_KEY   age1... (public half only, safe to store)
#       STORAGE_GATEWAY_URL     http://storage:8094
#       STORAGE_INTERNAL_TOKEN  shared with storage-rs
#
# Cron (k8s CronJob or host cron), nightly 02:00 UTC:
#   0 2 * * * /opt/idre/scripts/backup-postgres.sh
set -euo pipefail

: "${DATABASE_URL:?required}"
: "${BACKUP_AGE_PUBLIC_KEY:?required — age public key (age1...)}" 
: "${STORAGE_GATEWAY_URL:?required}"
: "${STORAGE_INTERNAL_TOKEN:?required}"

TS="$(date -u +%Y%m%dT%H%M%SZ)"
KEY="backups/$(date -u +%Y/%m/%d)/idre-${TS}.dump.age"

echo "[backup] dumping postgres -> age-encrypted stream -> $KEY"
pg_dump "$DATABASE_URL" --format=custom --compress=6 \
  | age --encrypt --recipient "$BACKUP_AGE_PUBLIC_KEY" \
  | curl -fsS -X PUT \
      -H "X-Internal-Auth: ${STORAGE_INTERNAL_TOKEN}" \
      -H "Content-Type: application/octet-stream" \
      -H "X-Object-Meta-kind: postgres-backup" \
      -H "X-Object-Meta-created: ${TS}" \
      --data-binary @- \
      "${STORAGE_GATEWAY_URL}/objects/${KEY}"

# Verify the upload is retrievable and intact (size > 0) before declaring success.
SIZE=$(curl -fsSI -H "X-Internal-Auth: ${STORAGE_INTERNAL_TOKEN}" \
  "${STORAGE_GATEWAY_URL}/objects/${KEY}" | tr -d '\r' | awk 'tolower($1)=="content-length:"{print $2}')
if [ -z "${SIZE:-}" ] || [ "$SIZE" -lt 1024 ]; then
  echo "[backup] FATAL: uploaded object missing or suspiciously small (${SIZE:-none} bytes)" >&2
  exit 1
fi
echo "[backup] OK: $KEY (${SIZE} bytes, age-encrypted, WORM-retained)"

# Audit trail: the nightly chain verifier and SIEM both watch this line.
echo "[backup] audit: action=BACKUP_COMPLETED key=$KEY bytes=$SIZE"
