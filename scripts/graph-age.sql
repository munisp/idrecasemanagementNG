-- Author: Patrick Munis at newwave
-- graph-age.sql — Apache AGE backend for the Entity 360 relationship graph
-- (optional; only needed when GRAPH_BACKENDS includes "age").
--
-- Prerequisite: the AGE extension must be present in the Postgres image —
-- e.g. swap postgres:16 for apache/age (PG16 build) in docker-compose.yml, or
-- install the extension from https://age.apache.org/ on an existing cluster.
--
-- Per-tenant graphs (e360_<tenant>) are created lazily by the sync projector
-- (POST /internal/entities/graph/sync or the entity backfill hook) — nothing
-- tenant-scoped is provisioned here. The relational tables remain the system
-- of record; the graphs are disposable projections, rebuilt wholesale on
-- every sync.

CREATE EXTENSION IF NOT EXISTS age;

-- The case-api pins a pooled connection and issues LOAD 'age' before every
-- cypher call (AGE's per-session requirement), so no search_path changes or
-- shared_preload_libraries tweaks beyond the extension itself are needed for
-- the application path. For ad-hoc psql cypher, run:
--   LOAD 'age';
--   SET search_path = ag_catalog, "$user", public;
