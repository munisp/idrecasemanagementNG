#!/usr/bin/env bash
# Regenerate the KEDA outbox ScaledObject query from live tenant schemas
# (checklist 2.8). The ScaledObject ships with tx/fl hardcoded; run this after
# each state onboarding and kubectl apply the output.
#
# Usage: scripts/gen-keda-outbox-query.sh > /tmp/scaledobject.yaml && kubectl apply -f /tmp/scaledobject.yaml
set -euo pipefail
DSN="${DATABASE_URL:-postgres://idre:idre@localhost:5432/idre}"

SCHEMAS=$(psql "$DSN" -tAc \
  "SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE 'tenant\_%' ORDER BY 1")
[ -n "$SCHEMAS" ] || { echo "no tenant schemas found" >&2; exit 1; }

QUERY=$(echo "$SCHEMAS" | while read -r s; do
  echo "SELECT count(*) AS cnt FROM ${s}.outbox WHERE published_at IS NULL"
done | paste -sd' ' | sed 's/SELECT count/UNION ALL SELECT count/g; s/^UNION ALL //')

cat <<YAML
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: outbox-relay
  namespace: idre-core
spec:
  scaleTargetRef: { name: outbox-relay }
  minReplicaCount: 1
  maxReplicaCount: 8
  pollingInterval: 10
  cooldownPeriod: 120
  triggers:
    - type: postgresql
      metadata:
        connectionFromEnv: DATABASE_URL
        query: >-
          SELECT COALESCE(SUM(cnt),0) FROM ( ${QUERY} ) t
        targetQueryValue: "500"
        activationQueryValue: "50"
# generated $(date -u +%FT%TZ) from $(echo "$SCHEMAS" | wc -l) tenant schemas
YAML
