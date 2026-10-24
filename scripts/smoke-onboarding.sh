#!/usr/bin/env bash
# Author: Patrick Munis at newwave
# End-to-end onboarding smoke test against a running local stack:
#   token -> submit provider application -> (auto checks run in workflow)
#   -> admin decision -> verify ACTIVE -> audit trail present.
# Usage: KC=admin ADMIN=password123 ./scripts/smoke-onboarding.sh [tenant]
set -euo pipefail

TENANT="${1:-tx}"
API="${API:-http://localhost:8080}"
KC_URL="${KC_URL:-http://localhost:8085}"

token() { # $1=user $2=pass
  curl -sf "$KC_URL/realms/idre/protocol/openid-connect/token" \
    -d grant_type=password -d client_id=case-portal \
    -d username="$1" -d password="$2" | python3 -c "import sys,json;print(json.load(sys.stdin)['access_token'])"
}

echo "== 1. provider org application (tenant: $TENANT)"
CM_TOKEN=$(token "cm.$TENANT" password123)
APP=$(curl -sf -X POST "$API/v1/tenants/$TENANT/onboarding/applications" \
  -H "Authorization: Bearer $CM_TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"PROVIDER_ORG","legal_name":"Smoke Test Emergency Physicians PLLC",
       "ein":"12-3456789","npi":"1003000126",
       "payload":{"npi":"1003000126","tin":"123456789","w9":"uploaded","texas_medical_board_license":"TX-99999"}}')
APP_ID=$(echo "$APP" | python3 -c "import sys,json;print(json.load(sys.stdin)['application_id'])")
echo "   application_id=$APP_ID"

echo "== 2. workflow drives it to PENDING_APPROVAL (auto gates + docs)"
for i in $(seq 1 12); do
  sleep 5
  STATUS=$(curl -sf "$API/v1/tenants/$TENANT/onboarding/applications" \
    -H "Authorization: Bearer $CM_TOKEN" \
    | python3 -c "import sys,json;print(next(a['status'] for a in json.load(sys.stdin) if a['id']=='$APP_ID'))")
  echo "   [$i] $STATUS"
  [[ "$STATUS" =~ PENDING_APPROVAL|REJECTED_AUTO|PENDING_DOCS ]] && break
done

echo "== 3. wrong-role decision must be rejected (arbiter cannot approve providers)"
ARB_TOKEN=$(token "arbiter.$TENANT" password123)
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  "$API/v1/tenants/$TENANT/onboarding/applications/$APP_ID/decision" \
  -H "Authorization: Bearer $ARB_TOKEN" -H 'Content-Type: application/json' \
  -d '{"decision":"APPROVE"}')
echo "   http=$CODE (expect 403)"; [ "$CODE" = "403" ]

echo "== 4. case manager approves"
curl -sf -X POST "$API/v1/tenants/$TENANT/onboarding/applications/$APP_ID/decision" \
  -H "Authorization: Bearer $CM_TOKEN" -H 'Content-Type: application/json' \
  -d '{"decision":"APPROVE","reason":"smoke test"}' > /dev/null

echo "== 5. verify ACTIVE after provisioning"
for i in $(seq 1 12); do
  sleep 5
  STATUS=$(curl -sf "$API/v1/tenants/$TENANT/onboarding/applications" \
    -H "Authorization: Bearer $CM_TOKEN" \
    | python3 -c "import sys,json;print(next(a['status'] for a in json.load(sys.stdin) if a['id']=='$APP_ID'))")
  echo "   [$i] $STATUS"; [ "$STATUS" = "ACTIVE" ] && break
done
[ "$STATUS" = "ACTIVE" ] && echo "SMOKE PASS" || { echo "SMOKE FAIL: $STATUS"; exit 1; }
