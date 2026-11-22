# Author: Patrick Munis at newwave
# docker-initdb.sh — postgres container first-boot entrypoint (mounted at
# /docker-entrypoint-initdb.d/00-init.sh by docker-compose.yml). Runs once on
# an empty pgdata: base schemas, then the full migration set, so a fresh
# stack converges without human intervention (DAST finding F1).
set -eu
psql -v ON_ERROR_STOP=1 -q -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
  -f /platform-scripts/init-schemas.sql
PSQL="psql -v ON_ERROR_STOP=1 -q -U $POSTGRES_USER -d $POSTGRES_DB" \
  bash /platform-scripts/migrate.sh
echo "initdb: platform schema converged"
