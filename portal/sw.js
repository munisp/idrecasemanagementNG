// Author: Patrick Munis at newwave
// Service worker: app-shell PWA. Static assets cache-first; API network-first
// with offline JSON fallback; navigation requests fall back to the cached shell.
const SHELL = "idre-shell-v1";
const API = "idre-api-v1";
const SHELL_ASSETS = [
  "/", "/index.html", "/css/app.css", "/manifest.webmanifest",
  "/js/config.js", "/js/auth.js", "/js/api.js", "/js/views.js", "/js/crm-views.js", "/js/app.js",
  "/icons/icon.svg",
];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(SHELL).then((c) => c.addAll(SHELL_ASSETS)));
  self.skipWaiting();
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => ![SHELL, API].includes(k)).map((k) => caches.delete(k)))
    )
  );
  self.clients.claim();
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);

  // Never cache auth or mutations.
  if (url.pathname.includes("/realms/") || e.request.method !== "GET") return;

  if (url.pathname.startsWith("/v1/")) {
    e.respondWith(
      fetch(e.request)
        .then((resp) => {
          if (resp.ok) {
            const clone = resp.clone();
            caches.open(API).then((c) => c.put(e.request, clone));
          }
          return resp;
        })
        .catch(() =>
          caches.match(e.request).then(
            (hit) =>
              hit ||
              new Response(JSON.stringify({ error: "offline", offline: true }), {
                status: 503,
                headers: { "Content-Type": "application/json" },
              })
          )
        )
    );
    return;
  }

  // App shell + static: cache-first, navigation falls back to shell.
  e.respondWith(
    caches.match(e.request).then(
      (hit) =>
        hit ||
        fetch(e.request)
          .then((resp) => {
            if (resp.ok && url.origin === location.origin) {
              const clone = resp.clone();
              caches.open(SHELL).then((c) => c.put(e.request, clone));
            }
            return resp;
          })
          .catch(() => (e.request.mode === "navigate" ? caches.match("/index.html") : Response.error()))
    )
  );
});
