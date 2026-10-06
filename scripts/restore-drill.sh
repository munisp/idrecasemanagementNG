#!/usr/bin/env bash
# PITR restore drill (go-live checklist 3.3). Restores the CNPG cluster's S3
# backup into a THROWAWAY cluster (never in-place), runs verification queries,
# prints a drill report, and tears down. Run quarterly and before cutover.
#
# Usage: scripts/restore-drill.sh [recovery-target-time]   # default: latest
set -euo pipefail
NS=idre-core
DRILL=postgres-restore-drill
TARGET="${1:-}"

echo "== 1/4 creating throwaway CNPG cluster from S3 backup =="
kubectl -n "$NS" delete cluster "$DRILL" --ignore-not-found
cat <<YAML | kubectl -n "$NS" apply -f -
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: $DRILL
spec:
  instances: 1
  storage: { size: 50Gi, storageClass: gp3 }
  bootstrap:
    recovery:
      source: origin
      $( [ -n "$TARGET" ] && echo "recoveryTarget: { targetTime: \"$TARGET\" }" )
  externalClusters:
    - name: origin
      barmanObjectStore:
        destinationPath: s3://postgres-backups/main
        endpointURL: http://minio.$NS:9000
        s3Credentials:
          accessKeyId:     { name: minio, key: accesskey }
          secretAccessKey: { name: minio, key: secretkey }
        wal:   { compression: lz4 }
        data:  { compression: lz4 }
YAML

echo "== 2/4 waiting for recovery =="
kubectl -n "$NS" wait --for=condition=Ready cluster/"$DRILL" --timeout=30m

echo "== 3/4 verification queries =="
kubectl -n "$NS" exec "$DRILL-1" -- psql -U idre -d idre -tAc "
  SELECT 'schemas',  count(*) FROM information_schema.schemata WHERE schema_name LIKE 'tenant\_%'
  UNION ALL SELECT 'cases',    count(*) FROM tenant_tx.cases
  UNION ALL SELECT 'audit',    count(*) FROM public.audit_log
  UNION ALL SELECT 'payments', count(*) FROM public.payments;"
python3 scripts/verify-audit-chain.py   # chain must verify on the restore too

echo "== 4/4 teardown =="
kubectl -n "$NS" delete cluster "$DRILL"
echo "DRILL PASSED $(date -u +%FT%TZ) target=${TARGET:-latest}"
