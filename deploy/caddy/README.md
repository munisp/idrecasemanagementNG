# Caddy edge tier

## What Caddy adds to this platform

The platform already had a gateway (APISIX), an identity provider (Keycloak),
a static file server for the portal (nginx), and a WAF plan (open-appsec).
What it did **not** have was a single public edge that ties them together.
Caddy fills exactly those holes and nothing more:

| Gap before Caddy | What Caddy adds |
| --- | --- |
| No automated certificate lifecycle — APISIX has no ACME client; TLS meant manual cert provisioning and renewal cron | Automatic HTTPS: ACME issuance and renewal out of the box (and an internal CA if east-west TLS is wanted later) |
| Four public ports (9080 APISIX, 8085 Keycloak, 3001 portal nginx, plus admin 9180) each with its own hardening burden | One public listener (80/443); everything else binds to localhost or the internal network |
| Portal served by a second web server (nginx) with its own config to keep in sync | Caddy serves the PWA directly with SPA fallback and correct cache policy (service worker `no-cache`, immutable assets), eliminating the nginx tier |
| No HTTP security-header enforcement anywhere | Central CSP / X-Frame-Options / Referrer-Policy / HSTS at the edge, one place to audit |
| open-appsec had no native attachment point — it does not plug into APISIX or plain nginx easily | open-appsec ships as a **native Caddy module**: the WAF inspects north-south traffic before it ever reaches APISIX |

What Caddy deliberately does **not** replace: APISIX remains the API policy
gateway (OIDC bearer validation, per-route rate ceilings, upstream routing,
Prometheus). Caddy is transport + edge hygiene; APISIX is API policy. They
complement rather than overlap.

## Integration map

```
                 ┌───────────────────────── CADDY :80/:443 ─────────────────────────┐
                 │  TLS termination · security headers · open-appsec WAF (module)   │
 internet ──────▶│                                                                  │
                 │   /auth/* ───────────▶ Keycloak :8080  (identity)                │
                 │   /v1/* /api/* /ml/* ─▶ APISIX :9080 ──▶ case-api :8080          │
                 │   /* (static) ────────▶ portal PWA     ─▶ vault :8081 (internal) │
                 └──────────────────────────────────────────────────────────────────┘
```

### Keycloak (identity)

- Keycloak runs with `KC_HTTP_RELATIVE_PATH=/auth` and
  `KC_HOSTNAME_URL=<public-base>/auth`, so the **canonical issuer**
  (`<public-base>/auth/realms/idre`) is identical for browsers, for Caddy,
  and for services inside the network. Issuer mismatch is the #1 cause of
  "token works in curl but not through the proxy" — this layout removes it.
- Caddy proxies `/auth/*` to Keycloak; the Admin Console is reachable through
  the same path and the direct port is bound to localhost only.
- APISIX's `openid-connect` plugin discovers via
  `${KEYCLOAK_ISSUER}/.well-known/openid-configuration`; the discovery
  document advertises the public issuer regardless of where it is fetched
  from, so APISIX may fetch it internally while validating the public `iss`.

### APISIX (API policy gateway)

- All `/v1/*`, `/api/*`, `/ml/*` traffic is proxied to APISIX unchanged;
  its routes, OIDC bearer validation, `limit-count` ceilings (60/min rules
  admin, 600/min case traffic, 120/min voice webhooks) and Prometheus plugin
  keep working exactly as before.
- `/internal/*` is refused at the edge with 404 and never reaches APISIX;
  APISIX's own `ip-restriction` on that route is the second layer.
- The APISIX Admin API (9180) stays localhost-only; in k8s it is not
  published at all.

### open-appsec (WAF)

- open-appsec attaches to Caddy as a compiled-in module
  (`docker build --target appsec`). Traffic is inspected **before** APISIX,
  so ML-based detection and API-schema enforcement see the raw client
  request; blocked requests never consume gateway or case-api capacity.
- In Kubernetes the same role is filled by the open-appsec agent sidecar on
  the ingress; Caddy is the compose/bare-metal equivalent so dev and prod
  behave alike.

### Portal PWA

- Served by Caddy from `/srv/portal` (mounted read-only) with SPA fallback
  to `/index.html`. The old nginx tier remains in compose bound to
  `127.0.0.1:3001` purely as a fallback.
- `sw.js` is served `Cache-Control: no-cache` (stale workers strand users on
  old app shells); fingerprinted assets are served immutable.

## Files

| File | Purpose |
| --- | --- |
| `Caddyfile` | Edge routes, security headers, static hosting, prod TLS template |
| `Dockerfile` | Stock build (default) + `appsec` target embedding the WAF module |

## Operational notes

- Production: set `DOMAIN` + `ACME_EMAIL`, uncomment the production block in
  the Caddyfile, and certificates are automatic from there.
- Rotate nothing on cert renewal — Caddy reloads itself without dropping
  connections.
- If the portal's `config.js` points `keycloakUrl` at a public hostname, set
  it to the same origin as the portal (e.g. `https://idre.example.gov/auth`)
  so login redirects stay same-site.
