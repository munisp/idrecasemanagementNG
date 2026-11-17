#!/usr/bin/env bash
# Author: Patrick Munis at newwave
# test-tenant-isolation.sh — live cross-tenant negative test against a
# running stack (docker compose). The Go unit tests pin the middleware;
# this pins the whole path end to end: JWT -> authn -> tenancy -> RLS.
#
# Usage:
#   KEYCLOAK_URL=http://localhost:8080/auth BASE=http://localhost:8080 \
#     bash scripts/test-tenant-isolation.sh
#
# Prerequisites: stack up, realm seeded, one staff user per tenant:
#   tx-tester (group /tenant/tx, role CASE_MANAGER, password from env
#   TX_TESTER_PW) and ny-tester (group /tenant/ny). The script creates a
#   case as tx-tester, then proves ny-tester cannot see, read, or mutate it
#   — at the API layer (403) and the row layer (RLS: empty result, not an
#   error leak).
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"
KC="${KEYCLOAK_URL:-http://localhost:8080/auth}"
REALM="${KEYCLOAK_REALM:-idre}"
CLIENT_ID="${KEYCLOAK_CLIENT_ID:-idre-portal}"
: "${TX_TESTER_PW:?set the tx-tester password}"
: "${NY_TESTER_PW:?set the ny-tester password}"

token() {  # token <user> <pw>
  curl -fsS -X POST "$KC/realms/$REALM/protocol/openid-connect/token" \
    -d "grant_type=password&client_id=$CLIENT_ID&username=$1&password=$2" \
    | python3 -c "import sys,json;print(json.load(sys.stdin)['access_token'])"
}

TX_TOK=$(token tx-tester "$TX_TESTER_PW")
NY_TOK=$(token ny-tester "$NY_TESTER_PW")
fail=0
check() { # check <desc> <want_code> <actual_code>
  if [ "$2" = "$3" ]; then echo "PASS  $1 ($3)"; else echo "FAIL  $1 (want $2, got $3)"; fail=1; fi
}

# 1. tx-tester creates a case in tx
CODE=$(curl -s -o /tmp/iso-case.json -w "%{http_code}" -X POST "$BASE/v1/tenants/tx/cases" \
  -H "Authorization: Bearer $TX_TOK" -H "Content-Type: application/json" \
  -d '{"service_line":"IDR","details":{"test":"tenant-isolation"}}')
check "create case in tx as tx-tester" 201 "$CODE"
CASE_ID=$(python3 -c "import json;print(json.load(open('/tmp/iso-case.json'))['case_id'])")

# 2. ny-tester must not read it
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/tenants/ny/cases/$CASE_ID" -H "Authorization: Bearer $NY_TOK")
check "read tx case from ny" 403 "$CODE"

# 3. ny-tester must not list it (RLS: the case must not appear at all)
curl -s "$BASE/v1/tenants/ny/cases" -H "Authorization: Bearer $NY_TOK" > /tmp/iso-list.json
if grep -q "$CASE_ID" /tmp/iso-list.json; then echo "FAIL  ny list leaked tx case"; fail=1; else echo "PASS  ny list contains no tx case"; fi

# 4. ny-tester must not mutate it
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/tenants/ny/cases/$CASE_ID/advance" -H "Authorization: Bearer $NY_TOK")
check "advance tx case from ny" 403 "$CODE"

# 5. ny-tester must not fetch its documents
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/tenants/ny/cases/$CASE_ID/documents" -H "Authorization: Bearer $NY_TOK")
check "list tx case documents from ny" 403 "$CODE"

# 6. tenant path confusion: valid token, garbage tenant code
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/tenants/t1/cases" -H "Authorization: Bearer $TX_TOK")
check "malformed tenant code" 400 "$CODE"

# 7. no token at all
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/tenants/tx/cases")
check "unauthenticated request" 401 "$CODE"

# 8. tx-tester reading own tenant still works (guard against over-blocking)
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/tenants/tx/cases/$CASE_ID" -H "Authorization: Bearer $TX_TOK")
check "tx-tester reads own case" 200 "$CODE"

rm -f /tmp/iso-case.json /tmp/iso-list.json
if [ "$fail" = 0 ]; then echo "TENANT ISOLATION: ALL PASS"; else echo "TENANT ISOLATION: FAILURES PRESENT"; exit 1; fi
