#!/usr/bin/env bash
# Provision a new state tenant end-to-end:
#   1. Postgres schema (DDL template) + state_config row
#   2. TigerBeetle ledger + core accounts
#   3. Kafka topics (idre.<state>.{cases,offers,fees,audit,voice}) + ACLs
#   4. Keycloak tenant group
# Usage: ./scripts/provision-tenant.sh <two-letter-state-code> [tb_ledger_id]
set -euo pipefail

DRY_RUN=0
if [ "${1:-}" = "--dry-run" ]; then DRY_RUN=1; shift; fi
TENANT="${1:?state code required}"
LEDGER="${2:-$((RANDOM % 50 + 150))}"
if [ "$DRY_RUN" = "1" ]; then
  PG="echo [dry-run] psql:"
  # shellcheck disable=SC2034
  TB="echo [dry-run] tigerbeetle:"
  echo "== DRY RUN tenant=${TENANT} ledger=${LEDGER} — nothing will be mutated =="
else
  PG=${PG:-psql "$DATABASE_URL"}
fi

$PG -v ON_ERROR_STOP=1 <<SQL
  SELECT public.provision_tenant('${TENANT}');
  INSERT INTO public.state_config (tenant, tb_ledger_id)
  VALUES ('${TENANT}', ${LEDGER}) ON CONFLICT (tenant) DO NOTHING;
SQL

run() { if [ "$DRY_RUN" = "1" ]; then echo "[dry-run] $*"; else "$@"; fi }

# TigerBeetle core accounts (escrow / admin / idre / refund) on the tenant ledger.
run tbctl --addresses "${TB_ADDRESSES:-localhost:3000}" create-accounts \
  --ledger "${LEDGER}" --code 700 \
  --account "${TENANT}:1000:escrow" --account "${TENANT}:3000:admin" \
  --account "${TENANT}:4000:idre"  --account "${TENANT}:5000:refund"

# Kafka topics with per-tenant prefix ACLs.
# Shared ops topic: Flink SLA early-warning sink produces here (tenant
# carried in the payload; produce-only grant for the flink principal).
run kafka-topics.sh --bootstrap-server "${KAFKA_BROKERS:-localhost:9092}"   --create --if-not-exists --topic idre.alerts   --partitions 3 --replication-factor 3 --config min.insync.replicas=2
for domain in cases offers fees audit voice documents rules; do
  run kafka-topics.sh --bootstrap-server "${KAFKA_BROKERS:-localhost:9092}" \
    --create --if-not-exists --topic "idre.${TENANT}.${domain}" \
    --partitions 6 --replication-factor 3
done

# Keycloak tenant group (k6 = keycloak admin CLI or REST call).
run curl -sf -X POST "${KEYCLOAK_URL:-http://keycloak:8080}/admin/realms/idre/groups" \
  -H "Authorization: Bearer ${KC_ADMIN_TOKEN:-}" \
  -H 'Content-Type: application/json' \
  -d "{\"name\": \"tenant/${TENANT}\"}"

[ "$DRY_RUN" = "1" ] && echo "== dry run complete ==" || echo "tenant ${TENANT} provisioned (ledger ${LEDGER})"
