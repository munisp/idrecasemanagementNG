# Author: Patrick Munis at newwave
# Reference environment launcher for the live DAST run (native, no docker).
# Brings up vault-rs, storage-rs, case-api plus the JWKS harness and the
# temporal stub on loopback. Postgres (schemas + honeytokens seeded) must
# already be listening on 127.0.0.1:5432 — see docs/DAST-FINDINGS-* for the
# pgserver recipe used in the reference run.
#
# All secrets below are DISPOSABLE-STACK-ONLY dev values (runbook hard rule).
set -u
DAST_HOME="${DAST_HOME:-$HOME/dast}"
CASE_API_BIN="${CASE_API_BIN:-$DAST_HOME/case-api}"
VAULT_BIN="${VAULT_BIN:-$DAST_HOME/vault}"
STORAGE_BIN="${STORAGE_BIN:-$DAST_HOME/storage}"
mkdir -p "$DAST_HOME/work" "$DAST_HOME/opendal-data"

cd "$(dirname "$0")"
python3 jwks-harness.py gen .
setsid nohup python3 jwks-serve.py </dev/null >>"$DAST_HOME/work/jwks.log" 2>&1 &
setsid nohup python3 temporal-stub.py </dev/null >>"$DAST_HOME/work/temporal-stub.log" 2>&1 &

env ADDR=127.0.0.1:8081 \
  VAULT_MASTER_SECRET=000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f \
  VAULT_INTERNAL_TOKEN=dev-internal-token \
  setsid nohup "$VAULT_BIN" </dev/null >>"$DAST_HOME/work/vault.log" 2>&1 &

env OPENDAL_SCHEME=fs OPENDAL_ROOT="$DAST_HOME/opendal-data" \
  STORAGE_INTERNAL_TOKEN=dev-storage-token LISTEN_ADDR=127.0.0.1:8094 \
  setsid nohup "$STORAGE_BIN" </dev/null >>"$DAST_HOME/work/storage.log" 2>&1 &

sleep 3  # storage must answer /healthz before case-api boots (gateway ping)

env ADDR=127.0.0.1:8080 \
  DATABASE_URL=postgres://idre:idre@127.0.0.1:5432/idre \
  KEYCLOAK_JWKS_URL=http://127.0.0.1:8085/auth/realms/idre/protocol/openid-connect/certs \
  KEYCLOAK_ISSUER=http://localhost/auth/realms/idre \
  KEYCLOAK_AUDIENCE=idre-api \
  TEMPORAL_HOST=127.0.0.1:7233 \
  TIGERBEETLE_CLUSTER_ID=0 TIGERBEETLE_ADDRESSES=127.0.0.1:3000 \
  WORKER_TOKEN=dev-worker-token \
  VAULT_URL=http://127.0.0.1:8081 VAULT_INTERNAL_TOKEN=dev-internal-token \
  STORAGE_GATEWAY_URL=http://127.0.0.1:8094 STORAGE_INTERNAL_TOKEN=dev-storage-token \
  STRIPE_SECRET_KEY= STRIPE_WEBHOOK_SECRET=whsec_dast_run \
  REDIS_ADDR= PERMIFY_URL= \
  METRICS_PORT=9091 OTEL_SDK_DISABLED=true \
  setsid nohup "$CASE_API_BIN" </dev/null >>"$DAST_HOME/work/case-api.log" 2>&1 &

sleep 8
curl -sS -o /dev/null -w "healthz: %{http_code}\n" http://127.0.0.1:8080/healthz
curl -sS -o /dev/null -w "metrics: %{http_code}\n" http://127.0.0.1:9091/metrics
