// Author: Patrick Munis at newwave
// auth.js — Keycloak OIDC with PKCE (no library, no secrets in the SPA).
const Auth = (() => {
  const cfg = window.IDRE_CONFIG;
  const base = `${cfg.keycloakUrl}/realms/${cfg.realm}/protocol/openid-connect`;
  const K = { token: "idre.token", refresh: "idre.refresh", verifier: "idre.verifier" };

  const b64url = (buf) =>
    btoa(String.fromCharCode(...new Uint8Array(buf)))
      .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

  async function login() {
    const verifier = b64url(crypto.getRandomValues(new Uint8Array(32)));
    sessionStorage.setItem(K.verifier, verifier);
    const challenge = b64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)));
    const params = new URLSearchParams({
      client_id: cfg.clientId, response_type: "code",
      redirect_uri: location.origin + "/app.html", scope: "openid profile",
      code_challenge: challenge, code_challenge_method: "S256",
    });
    location.href = `${base}/auth?${params}`;
  }

  async function handleCallback() {
    const code = new URLSearchParams(location.search).get("code");
    if (!code) return false;
    const resp = await fetch(`${base}/token`, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: "authorization_code", client_id: cfg.clientId, code,
        redirect_uri: location.origin + "/app.html",
        code_verifier: sessionStorage.getItem(K.verifier),
      }),
    });
    if (!resp.ok) return false;
    const tok = await resp.json();
    sessionStorage.setItem(K.token, tok.access_token);
    sessionStorage.setItem(K.refresh, tok.refresh_token);
    history.replaceState(null, "", location.pathname + location.hash);
    return true;
  }

  async function token() {
    const tok = sessionStorage.getItem(K.token);
    if (!tok) return null;
    const { exp } = JSON.parse(atob(tok.split(".")[1]));
    if (exp * 1000 > Date.now() + 30000) return tok;
    // refresh
    const resp = await fetch(`${base}/token`, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: "refresh_token", client_id: cfg.clientId,
        refresh_token: sessionStorage.getItem(K.refresh),
      }),
    });
    if (!resp.ok) { logout(); return null; }
    const fresh = await resp.json();
    sessionStorage.setItem(K.token, fresh.access_token);
    sessionStorage.setItem(K.refresh, fresh.refresh_token);
    return fresh.access_token;
  }

  function claims() {
    const tok = sessionStorage.getItem(K.token);
    if (!tok) return null;
    const p = JSON.parse(atob(tok.split(".")[1]));
    return {
      sub: p.sub,
      name: p.preferred_username || p.sub,
      roles: (p.realm_access && p.realm_access.roles) || [],
      tenants: (p.groups || []).filter((g) => g.startsWith("/tenant/")).map((g) => g.slice(8)),
    };
  }

  function logout() {
    sessionStorage.clear();
    location.href = `${base}/logout?client_id=${cfg.clientId}&post_logout_redirect_uri=${encodeURIComponent(location.origin + "/app.html")}`;
  }

  return { login, logout, handleCallback, token, claims };
})();
