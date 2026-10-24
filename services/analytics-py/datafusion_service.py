"""DataFusion ad-hoc query service: sub-second SQL over the gold zone without
spinning up Spark. Tenant-scoped, read-only, authenticated.

Security posture (the previous version had none):
  - Every request requires a Keycloak bearer token (RS256, JWKS-verified).
    The tenant scope comes from the token's ``tenant`` claim or a
    ``/tenant/<st>`` group — never from a client-supplied header alone.
    Realm admins (platform-admin / federal-admin) may override the scope
    with X-Tenant-Code for cross-tenant rollup work.
  - SQL passes lakehouse_common.validate_readonly_sql (literal/comment-
    aware single-statement SELECT/CTE gate) and is wrapped in an engine-
    enforced row cap before execution.
  - Results are capped at MAX_ROWS; an over-cap result returns the first
    MAX_ROWS with ``truncated: true`` instead of unbounded memory.

Gold marts are read through the Delta log (deltalake) with a tenant
partition filter — correct snapshot semantics AND physical tenant scoping,
unlike registering the raw parquet directory (which would read stale files
left behind by Delta overwrites).

Run: python datafusion_service.py
Env: KEYCLOAK_URL, KEYCLOAK_REALM, MAX_QUERY_ROWS (default 10000).
"""

from __future__ import annotations

import json
import os
import re
from http.server import BaseHTTPRequestHandler, HTTPServer

import datafusion
import pyarrow as pa

from lakehouse_common import validate_readonly_sql, wrap_with_row_cap

MINIO = "s3://lakehouse/gold"
KEYCLOAK_URL = os.environ.get("KEYCLOAK_URL", "http://keycloak:8080")
KEYCLOAK_REALM = os.environ.get("KEYCLOAK_REALM", "idre")
MAX_ROWS = int(os.environ.get("MAX_QUERY_ROWS", "10000"))
ADMIN_ROLES = {"platform-admin", "federal-admin"}
TENANT_RE = re.compile(r"^[a-z]{2}$")

GOLD_MARTS = (
    "cms_monthly_report",
    "sla_breach_summary",
    "qpa_benchmarks",
    "settlement_positions",
    "geo_jurisdiction_check",
    "geo_coverage_gaps",
)

STORAGE_OPTS = {
    "AWS_ENDPOINT_URL": os.environ.get("S3_ENDPOINT", "http://minio:9000"),
    "AWS_ACCESS_KEY_ID": os.environ.get("S3_ACCESS_KEY", "idre"),
    "AWS_SECRET_ACCESS_KEY": os.environ.get("S3_SECRET_KEY", "idre-secret"),
    "AWS_REGION": "us-east-1",
    "AWS_ALLOW_HTTP": "true",
    "AWS_S3_ALLOW_UNSAFE_RENAME": "true",
}

_JWKS = None


def _jwks_client():
    """PyJWKClient caches the realm keys; constructed lazily so the module
    imports (and unit tests) without Keycloak reachable."""
    global _JWKS
    if _JWKS is None:
        import jwt
        _JWKS = jwt.PyJWKClient(
            f"{KEYCLOAK_URL}/realms/{KEYCLOAK_REALM}/protocol/openid-connect/certs")
    return _JWKS


class AuthError(Exception):
    pass


def authenticate(auth_header: str | None) -> tuple[str | None, set[str]]:
    """Verify the bearer token; return (tenant_from_token, realm_roles).
    tenant_from_token is None for principals with no tenant scope (admins)."""
    import jwt

    if not auth_header or not auth_header.startswith("Bearer "):
        raise AuthError("bearer token required")
    token = auth_header[len("Bearer "):]
    try:
        key = _jwks_client().get_signing_key_from_jwt(token).key
        claims = jwt.decode(token, key, algorithms=["RS256"],
                            options={"verify_aud": False})
    except jwt.PyJWTError as exc:
        raise AuthError(f"invalid token: {exc}") from exc

    roles = set(claims.get("realm_access", {}).get("roles", []) or [])
    tenant = claims.get("tenant")
    if not tenant:
        for group in claims.get("groups", []) or []:
            if group.startswith("/tenant/"):
                tenant = group.rsplit("/", 1)[-1]
                break
    return tenant, roles


def resolve_tenant(claims_tenant: str | None, roles: set[str],
                   header_tenant: str | None) -> str:
    """Header can only NARROW or match the token's scope; widening requires
    an admin realm role."""
    if header_tenant:
        if not TENANT_RE.match(header_tenant):
            raise AuthError("bad tenant code")
        if header_tenant == claims_tenant or roles & ADMIN_ROLES:
            return header_tenant
        raise AuthError("tenant scope not in token")
    if claims_tenant and TENANT_RE.match(claims_tenant):
        return claims_tenant
    raise AuthError("no tenant scope in token")


def make_ctx(tenant: str) -> datafusion.SessionContext:
    """Register every gold mart that exists, tenant partition-filtered via
    the Delta log."""
    from deltalake import DeltaTable

    ctx = datafusion.SessionContext()
    for mart in GOLD_MARTS:
        try:
            dt = DeltaTable(f"s3://lakehouse/gold/{mart}", storage_options=STORAGE_OPTS)
            table = dt.to_pyarrow_table(partitions=[("tenant", "=", tenant)])
        except Exception as exc:  # mart absent or unreadable: skip, don't 500
            print(f"datafusion: skip {mart} for {tenant}: {exc}", flush=True)
            continue
        if table.num_rows:
            ctx.register_table(mart, ctx.from_arrow(table))
    return ctx


class Handler(BaseHTTPRequestHandler):
    def _json(self, code: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/healthz":
            self._json(200, {"ok": True})
        else:
            self.send_error(404)

    def do_POST(self):
        if self.path != "/analytics/query":
            self.send_error(404)
            return
        try:
            claims_tenant, roles = authenticate(self.headers.get("Authorization"))
            tenant = resolve_tenant(claims_tenant, roles,
                                    self.headers.get("X-Tenant-Code"))
        except AuthError as exc:
            self._json(401, {"error": str(exc)})
            return

        try:
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        except (ValueError, KeyError):
            self._json(400, {"error": "invalid JSON body"})
            return
        sql = body.get("sql", "")

        reason = validate_readonly_sql(sql)
        if reason:
            self._json(403, {"error": reason})
            return

        ctx = make_ctx(tenant)
        try:
            df = ctx.sql(wrap_with_row_cap(sql, MAX_ROWS))
            batches = df.collect()
        except Exception as exc:  # noqa: BLE001
            self._json(400, {"error": str(exc)})
            return

        # ALL batches, not just the first (the old rows[0] bug silently
        # dropped every batch after the first for larger results).
        if batches:
            table = pa.Table.from_batches(batches, schema=batches[0].schema)
        else:
            table = pa.table({n: pa.array([], type=pa.null()) for n in df.schema().names})
        truncated = table.num_rows > MAX_ROWS
        if truncated:
            table = table.slice(0, MAX_ROWS)
        self._json(200, {
            "columns": table.column_names,
            "rows": table.to_pydict(),
            "truncated": truncated,
        })


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 8092), Handler).serve_forever()
