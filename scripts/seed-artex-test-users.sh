#!/usr/bin/env bash
# Author: Patrick Munis at newwave
# seed-artex-test-users.sh — provision the four DAST test identities ARTEX
# uses (see deploy/artex/task-pack.md) into the Keycloak `idre` realm, plus
# the two users scripts/test-tenant-isolation.sh already expects. Idempotent:
# safe to re-run; existing users keep their roles and get their password
# reset to the env value.
#
# Identities (all passwords from env, dev defaults shown):
#   tx-tester       CASE_MANAGER   /tenant/tx   TX_TESTER_PW     (default: tx-tester-pw)
#   ny-tester       CASE_MANAGER   /tenant/ny   NY_TESTER_PW     (default: ny-tester-pw)
#   auditor-tester  STATE_AUDITOR  (read-only every tenant)  AUDITOR_TESTER_PW
#   admin-tester    PLATFORM_ADMIN (read+write every tenant) ADMIN_TESTER_PW
#
# These are WEAK, well-known credentials on purpose: they exist so a scanner
# can authenticate as each role. They must NEVER exist outside the disposable
# compose stack — the realm used in k8s is a different, operator-managed one.
#
# Usage:  bash scripts/seed-artex-test-users.sh
# Requires: platform stack up (keycloak container healthy), docker, python3.
set -euo pipefail

REALM="${KEYCLOAK_REALM:-idre}"
KC_CONTAINER="${KC_CONTAINER:-$(docker compose ps -q keycloak)}"
[ -n "$KC_CONTAINER" ] || { echo "keycloak container not found — is the stack up?" >&2; exit 1; }
KCADM=(docker exec "$KC_CONTAINER" /opt/keycloak/bin/kcadm.sh)
ADMIN_PW="${KEYCLOAK_ADMIN_PASSWORD:-admin}"

TX_TESTER_PW="${TX_TESTER_PW:-tx-tester-pw}"
NY_TESTER_PW="${NY_TESTER_PW:-ny-tester-pw}"
AUDITOR_TESTER_PW="${AUDITOR_TESTER_PW:-auditor-tester-pw}"
ADMIN_TESTER_PW="${ADMIN_TESTER_PW:-admin-tester-pw}"

echo "== authenticating kcadm (container: $KC_CONTAINER) =="
"${KCADM[@]}" config credentials --server http://localhost:8080/auth \
  --realm master --user admin --password "$ADMIN_PW" >/dev/null

user_id() {  # user_id <username> -> prints id or empty
  "${KCADM[@]}" get users -r "$REALM" -q "username=$1" 2>/dev/null \
    | python3 -c "import sys,json; u=[x for x in json.load(sys.stdin) if x['username']=='$1']; print(u[0]['id'] if u else '')"
}

group_id() { # group_id <path like tenant/tx>
  "${KCADM[@]}" get groups -r "$REALM" 2>/dev/null \
    | python3 -c "
import sys,json
want='/$1'
def walk(gs):
    for g in gs:
        if g['path']==want: print(g['id']); return True
        if walk(g.get('subGroups',[])): return True
    return False
walk(json.load(sys.stdin))"
}

ensure_user() { # ensure_user <username> <password> <realm-role> [group-path]
  local u="$1" pw="$2" role="$3" grp="${4:-}"
  local id; id=$(user_id "$u")
  if [ -z "$id" ]; then
    "${KCADM[@]}" create users -r "$REALM" -s "username=$u" -s enabled=true >/dev/null
    id=$(user_id "$u")
    echo "  created $u"
  else
    echo "  exists  $u (id $id)"
  fi
  "${KCADM[@]}" set-password -r "$REALM" --userid "$id" --new-password "$pw" >/dev/null
  "${KCADM[@]}" add-roles -r "$REALM" --uusername "$u" --rolename "$role" 2>/dev/null || true
  if [ -n "$grp" ]; then
    local gid; gid=$(group_id "$grp")
    [ -n "$gid" ] || { echo "  ERROR: group /$grp not found in realm $REALM" >&2; exit 1; }
    "${KCADM[@]}" update "users/$id/groups/$gid" -r "$REALM" -n 2>/dev/null || true
    echo "  $u -> role $role, group /$grp"
  else
    echo "  $u -> role $role (no tenant group)"
  fi
}

echo "== provisioning DAST test identities in realm '$REALM' =="
ensure_user tx-tester       "$TX_TESTER_PW"       CASE_MANAGER   tenant/tx
ensure_user ny-tester       "$NY_TESTER_PW"       CASE_MANAGER   tenant/ny
ensure_user auditor-tester  "$AUDITOR_TESTER_PW"  STATE_AUDITOR
ensure_user admin-tester    "$ADMIN_TESTER_PW"    PLATFORM_ADMIN

echo "== done. Tokens: grant_type=password, client_id=case-portal, realm $REALM =="
echo "   (scripts/test-tenant-isolation.sh uses KEYCLOAK_CLIENT_ID; set it to case-portal)"
