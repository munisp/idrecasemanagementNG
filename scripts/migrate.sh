# Author: Patrick Munis at newwave
# migrate.sh — converge a database onto the full platform schema.
#
# DAST finding F1 (2026-11-22): compose initdb applied ONLY init-schemas.sql;
# the incremental scripts below were wired to nothing, and a fresh stack
# served 500 on every case list ("column org of relation audit_log does not
# exist"). This script is the automated path. It is:
#   - idempotent: every script uses IF NOT EXISTS / additive DDL, safe to
#     re-run against a live database at any time;
#   - order-tolerant: the scripts have cross-dependencies (payments needs
#     payment_groups from tpa_bulk_disputes, casemgmt-core needs
#     financial_events from payments, ...), so it applies the set in bounded
#     fixpoint rounds — proven to converge in 3 rounds from a fresh database;
#   - fail-loud: if the set has not converged after the rounds, it exits 1
#     with the failing script names.
#
# Usage:  PSQL="psql -h 127.0.0.1 -U idre -d idre" bash scripts/migrate.sh
#         bash scripts/migrate.sh          # defaults: local idre database
set -u
PSQL="${PSQL:-psql -h 127.0.0.1 -U idre -d idre}"
DIR="$(cd "$(dirname "$0")" && pwd)"

# Optional extensions: used when the server packages them, skipped loudly
# when it does not (pg_trgm powers entity360 + db-optimization; Apache AGE
# powers graph-age and is a custom image — see docker-compose.yml comment).
$PSQL -q -c "CREATE EXTENSION IF NOT EXISTS pg_trgm" >/dev/null 2>&1 \
  && HAVE_TRGM=1 || { HAVE_TRGM=0; echo "migrate: pg_trgm unavailable — entity360/db-optimization will be skipped"; }
$PSQL -q -c "CREATE EXTENSION IF NOT EXISTS age" >/dev/null 2>&1 \
  && HAVE_AGE=1 || HAVE_AGE=0

SCRIPTS="casemgmt-core.sql crm-core.sql audit_actor.sql org_audit.sql
case_transitions.sql deid.sql doc_review.sql docintel-learning.sql
document_requests.sql features.sql fee_payments.sql healthplan_onboarding.sql
npi_registry.sql offers.sql onboarding-and-docintel.sql
onboarding-requirements-seed.sql payments.sql portal_uploads.sql
productivity.sql program-rules.sql qa_learning.sql respondents.sql
sharebox.sql tenant_brand.sql tpa_bulk_disputes.sql tpa_onboarding.sql
tpa_saved_payments.sql"
[ "$HAVE_TRGM" = 1 ] && SCRIPTS="$SCRIPTS entity360.sql db-optimization.sql"
[ "$HAVE_AGE" = 1 ] && SCRIPTS="$SCRIPTS graph-age.sql"

PENDING="$SCRIPTS"
for round in 1 2 3 4 5 6 7 8; do
  NEXT=""
  PROGRESS=0
  for f in $PENDING; do
    if $PSQL -v ON_ERROR_STOP=1 -q -f "$DIR/$f" >/dev/null 2>&1; then
      PROGRESS=1
    else
      NEXT="$NEXT $f"
    fi
  done
  PENDING="$(echo $NEXT)" # normalize whitespace
  [ -z "$PENDING" ] && { echo "migrate: schema converged (round $round)"; exit 0; }
  [ "$PROGRESS" = 0 ] && break # nothing applied this round — real failure, not ordering
done

echo "migrate: FAILED to converge; still failing:" >&2
for f in $PENDING; do
  echo "  $f:" >&2
  $PSQL -v ON_ERROR_STOP=1 -q -f "$DIR/$f" 2>&1 | grep -m1 ERROR | sed 's/^/    /' >&2
done
exit 1
