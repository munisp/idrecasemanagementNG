// app.js — hash router + bootstrap. Role-aware navigation (Meridian shell).
(async function () {
  const view = document.getElementById("view");
  const nav = document.getElementById("nav");

  // Service worker (PWA)
  if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js");

  // OIDC callback
  if (await Auth.handleCallback()) location.hash = "#/dashboard";

  const me = Auth.claims();
  if (!me) { Auth.login(); return; }

  // Tenant selection: first tenant group; platform/federal admins may switch later.
  if (!Api.getTenant() || (!me.tenants.includes(Api.getTenant()) && !me.roles.includes("FEDERAL_ADMIN") && !me.roles.includes("PLATFORM_ADMIN"))) {
    if (me.tenants.length) Api.setTenant(me.tenants[0]);
  }
  if (!Api.getTenant()) {
    view.innerHTML = `<h1>No tenant access</h1><p class="muted">Your account is not assigned to any state tenant. Contact your case manager.</p>`;
    return;
  }

  // Server-side user prefs: device-local localStorage is the offline cache;
  // public.user_prefs is the cross-device source of truth (PWA/desktop/native).
  window.Prefs = {
    push(key, value) {
      localStorage.setItem("idre." + key, typeof value === "string" ? value : JSON.stringify(value));
      Api.prefs.put(key, value).catch(() => {}); // offline: server catches up next login
    },
  };
  Api.prefs.all().then((sv) => {
    if (!sv) return;
    if (sv.theme && sv.theme !== (localStorage.getItem("idre.theme") || "")) {
      localStorage.setItem("idre.theme", sv.theme);
      document.documentElement.dataset.theme = sv.theme === "dark" ? "dark" : "";
    }
    if (sv.density) {
      localStorage.setItem("idre.density", sv.density);
      document.body.classList.toggle("density-compact", sv.density === "compact");
    }
    if (sv.recents) localStorage.setItem("idre.recents", JSON.stringify(sv.recents));
    if (sv.last_tenant && sv.last_tenant !== Api.getTenant()) Prefs.push("last_tenant", Api.getTenant());
  }).catch(() => {});

  // Theme (user preference layer; persisted locally)
  const savedTheme = localStorage.getItem("idre.theme");
  if (savedTheme === "dark") document.documentElement.dataset.theme = "dark";
  document.getElementById("theme-toggle").onclick = () => {
    const dark = document.documentElement.dataset.theme !== "dark";
    document.documentElement.dataset.theme = dark ? "dark" : "";
    Prefs.push("theme", dark ? "dark" : "light");
  };

  // Tenant identity chip (always visible, non-removable). Cross-tenant users
  // (PLATFORM_ADMIN / FEDERAL_ADMIN read+write; STATE_AUDITOR read-only) get a
  // switcher; everyone else sees a fixed chip.
  const STATES = ["al","ak","az","ar","ca","co","ct","de","fl","ga","hi","id","il","in","ia","ks","ky","la","me","md","ma","mi","mn","ms","mo","mt","ne","nv","nh","nj","nm","ny","nc","nd","oh","ok","or","pa","ri","sc","sd","tn","tx","ut","vt","va","wa","wv","wi","wy","dc"];
  const crossTenant = me.roles.includes("PLATFORM_ADMIN") || me.roles.includes("FEDERAL_ADMIN");
  const auditorOnly = me.roles.includes("STATE_AUDITOR") &&
    !["CASE_MANAGER", "ARBITRATOR", "FINANCE", "PARTY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"].some((r) => me.roles.includes(r));
  const chip = document.getElementById("tenant-chip");
  const paintChip = () => {
    const cur = Api.getTenant();
    document.getElementById("t-avatar").textContent = cur.slice(0, 2).toUpperCase();
    if (crossTenant || auditorOnly) {
      document.getElementById("t-name").innerHTML =
        `<select id="t-switch" class="t-select" aria-label="Switch state tenant">` +
        STATES.map((s) => `<option value="${s}"${s === cur ? " selected" : ""}>${s.toUpperCase()} IDRE</option>`).join("") +
        `</select>${auditorOnly ? '<span class="badge s-audit">read-only audit</span>' : ""}`;
      document.getElementById("t-switch").addEventListener("change", (e) => {
        Api.setTenant(e.target.value);
        Prefs.push("last_tenant", e.target.value);
        UI.toast(`Switched to ${e.target.value.toUpperCase()} tenant${auditorOnly ? " (read-only)" : ""}`);
        paintChip();
        location.hash = "#/dashboard";
        location.reload();
      });
    } else {
      document.getElementById("t-name").textContent = `${cur.toUpperCase()} IDRE`;
    }
  };
  paintChip();

  // Demo-mode banner (only when the backend is stubbed)
  if (window.IDRE_DEMO) {
    const bar = document.createElement("div");
    bar.className = "demo-bar";
    bar.innerHTML = `⚠ <b>Demo mode</b> — real portal code rendering built-in sample data; no backend connected. Set <code>demoMode:false</code> and point <code>apiBase</code>/<code>keycloakUrl</code> at your deployment for live data.`;
    document.getElementById("body").insertBefore(bar, view);
  }

  // ---- Program Manifest: sector-agnostic terminology + lifecycle ----------
  // The manifest (fetched per tenant) declares every user-facing noun and the
  // lifecycle pipeline — "Case"/"Provider"/"Health Plan" never live in code.
  // No manifest (404) = legacy FL/NSA defaults, identical to prior behavior.
  const DEFAULT_TERMS = {
    case_noun: "Case", case_plural: "Disputes", party_a: "Provider",
    party_b: "Health Plan", neutral: "Arbitrator", intake_noun: "Intake request",
    amount_label: "QPA", service_label: "Service",
  };
  window.App = {
    manifest: null,
    t: (key) => (App.manifest && App.manifest.terminology && App.manifest.terminology[key]) || DEFAULT_TERMS[key] || key,
    // Pipeline stages for kanban views: [[STATUS, label], ...] or null = legacy
    pipeline: () => {
      const cs = App.manifest && App.manifest.lifecycle && App.manifest.lifecycle.case_statuses;
      return cs && cs.length ? cs.map((s) => [s.name, s.label]) : null;
    },
    loadManifest: async () => {
      App.manifest = await Api.program.manifest().catch(() => null);
    },
    // Sector mechanics toggles (manifest features). Defaults preserve NSA
    // behavior: sealed offers + negotiation window ON, everything else OFF.
    feature: (name) => {
      if (!App.manifest) return true; // legacy tenant: everything on (unchanged behavior)
      const f = App.manifest.features || {};
      if (name in f) return f[name];
      return name === "sealed_offers" || name === "negotiation_window"; // NSA defaults
    },
  };
  await App.loadManifest();

  // Role-aware rail navigation (mirrors docs/STAKEHOLDERS.md coverage matrix)
  const has = (...rs) => rs.some((r) => me.roles.includes(r));
  const links = [
    ["#/dashboard", "▤", "Home"], ["#/cases", "▦", App.t("case_plural")], ["#/pipeline", "▥", "Pipeline"],
    ["#/crm/accounts", "◈", "Accounts"], ["#/crm/leads", "◎", "Leads"], ["#/crm/tasks", "☑", "Tasks"],
  ];
  if (has("PARTY", "CASE_MANAGER")) links.push(["#/new", "＋", `New ${App.t("case_noun").toLowerCase()}`]);
  links.push(["#/ask", "✦", "Ask the graph"]);
  if (has("CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/assistant", "❖", "Assistant"]);
  if (has("CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
    links.push(["#/qa", "✓", "QA gate"]);
    links.push(["#/intake", "⇥", "Intake"]);
    links.push(["#/deliverables", "⎘", "Deliverables"]);
  }
  if (has("FINANCE", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR")) links.push(["#/finance", "◍", "Financials"]);
  if (has("CASE_MANAGER", "ARBITRATOR", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR")) links.push(["#/ops", "◔", "Ops"]);
  links.push(["#/calendar", "▨", "Calendar"]);
  links.push(["#/onboarding", "⚑", "Onboarding"]);
  if (has("CASE_MANAGER") && App.feature("voice_console")) links.push(["#/voice", "☎", "Voice console"]);
  if (has("FEDERAL_ADMIN", "STATE_AUDITOR", "PLATFORM_ADMIN")) links.push(["#/reports", "◫", "Reports"]);
  if (has("CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/time", "⏱", "Team time"]);
  if (has("CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
    links.push(["#/billing", "🧾", "Billing"]);
    links.push(["#/arap", "⚖️", "AR / AP"]);
    links.push(["#/recon", "🔁", "Reconciliation"]);
  }
  if (has("FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/rules", "§", "Rules"]);
  nav.innerHTML = links.map(([h, i, l]) =>
    `<a href="${h}" data-route="${h.slice(2).split("/")[0]}"><span class="ri">${i}</span><span class="rl">${l}</span></a>`).join("");

  // Notifications
  document.getElementById("bell").onclick = async () => {
    const n = await Api.cm.notifications().catch(() => []);
    const w = window.__notifPanel || (window.__notifPanel = document.createElement("div"));
    w.className = "notif-panel";
    w.innerHTML = `<h3>Notifications</h3>` + (n.length ? n.map((x) =>
      `<div class="notif ${x.read_at ? "" : "unread"}" onclick="Api.cm.readNotif('${x.id}').then(()=>location.reload())">
         <b>${x.type}</b> — ${x.message} <span class="muted">${new Date(x.created_at).toLocaleString()}</span></div>`).join("")
      : `<p class="muted">No notifications.</p>`);
    document.body.appendChild(w);
    setTimeout(() => document.addEventListener("click", (e) => { if (!w.contains(e.target) && e.target.id !== "bell") w.remove(); }, { once: true }), 0);
  };
  (async () => {
    const n = await Api.cm.notifications().catch(() => []);
    const unread = n.filter((x) => !x.read_at).length;
    const b = document.getElementById("bell-n");
    if (unread) { b.textContent = unread; b.style.display = "block"; }
  })();

  // Identity
  const initials = (me.name || "?").split(/[\s._-]+/).map((s) => s[0]).join("").slice(0, 2).toUpperCase();
  document.getElementById("whoami").innerHTML =
    `<span class="avatar">${initials}</span><span class="who-txt">${me.name} · <a href="javascript:void(0)" id="lo">sign out</a></span>`;
  // Global search: live dropdown (debounced) + full-results page on Enter.
  // The field used to respond to Enter ONLY — typing showed nothing, which
  // read as "search does nothing". Now: 2+ chars queries /search after a
  // 250ms debounce, arrows select, Enter opens the hit or the full page.
  (() => {
    const gq = document.getElementById("gq"), drop = document.getElementById("gq-drop");
    const escHtml = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
    const linkFor = (h) => h.kind === "case" ? `#/cases/${h.id}`
      : h.kind === "document" ? `#/cases/${h.case_id || ""}`
      : h.kind === "account" ? `#/crm/accounts/${h.id}`
      : h.kind === "lead" ? "#/crm/leads" : "#/crm/accounts";
    const iconFor = (h) => h.kind === "case" ? "▦" : h.kind === "document" ? "🗎" : "◈";
    let timer = 0, items = [], sel = -1;
    const closeDrop = () => { drop.hidden = true; items = []; sel = -1; };
    async function refresh() {
      const q = gq.value.trim();
      if (q.length < 2) { closeDrop(); return; }
      let hits = [];
      try { hits = (await Api.crm.search(q)).slice(0, 8); } catch { hits = []; }
      if (gq.value.trim() !== q) return; // a newer keystroke owns the box
      items = hits; sel = -1;
      drop.innerHTML = hits.map((h, i) =>
        `<div class="palette-item gq-item" data-i="${i}" role="option">
           <span class="ri">${iconFor(h)}</span><span class="pl">${escHtml(h.label)}</span>
           ${h.detail ? `<span class="pd">${escHtml(h.detail)}</span>` : ""}</div>`).join("") ||
        `<div class="palette-empty">No matches — press Enter for full search</div>`;
      drop.hidden = false;
      drop.querySelectorAll(".gq-item").forEach((el) =>
        el.addEventListener("mousedown", () => { location.hash = linkFor(items[+el.dataset.i]); closeDrop(); }));
    }
    gq.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(refresh, 250); });
    gq.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        if (sel >= 0 && items[sel]) { location.hash = linkFor(items[sel]); closeDrop(); }
        else location.hash = `#/search/${encodeURIComponent(gq.value)}`;
      } else if (e.key === "Escape") closeDrop();
      else if ((e.key === "ArrowDown" || e.key === "ArrowUp") && !drop.hidden && items.length) {
        e.preventDefault();
        sel = (sel + (e.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
        drop.querySelectorAll(".gq-item").forEach((el, i) => el.classList.toggle("sel", i === sel));
      }
    });
    gq.addEventListener("blur", () => setTimeout(closeDrop, 150)); // mousedown lands first
  })();
  document.getElementById("lo").onclick = Auth.logout;

  const routes = [
    [/^#\/dashboard$/, Views.dashboard],
    [/^#\/pipeline$/, CrmViews.pipeline],
    [/^#\/cases(\?.*)?$/, Views.cases],
    [/^#\/cases\/([\w-]+)$/, (m) => Views.caseDetail(m[1])],
    [/^#\/new$/, Views.newDispute],
    [/^#\/crm\/accounts$/, CrmViews.accounts],
    [/^#\/crm\/accounts\/new$/, CrmViews.accountNew],
    [/^#\/crm\/accounts\/([\w-]+)$/, (m) => CrmViews.account360(m[1])],
    [/^#\/crm\/leads$/, CrmViews.leads],
    [/^#\/crm\/tasks$/, CrmViews.tasks],
    [/^#\/search\/(.+)$/, (m) => CrmViews.search(decodeURIComponent(m[1]))],
    [/^#\/ask$/, Views.askGraph],
    [/^#\/assistant$/, () => Views.assistant("")],
    [/^#\/assistant\/([\w-]+)$/, (m) => Views.assistant(m[1])],
    [/^#\/qa$/, Views.qaQueue],
    [/^#\/intake$/, Views.intake],
    [/^#\/deliverables$/, Views.deliverables],
    [/^#\/finance$/, Views.finance],
    [/^#\/ops$/, Views.opsDashboard],
    [/^#\/calendar$/, CrmViews.calendar],
    [/^#\/onboarding$/, Views.onboarding],
    [/^#\/onboarding\/new$/, Views.onboardingNew],
    [/^#\/voice$/, Views.voice],
    [/^#\/reports$/, Views.reports],
    [/^#\/time$/, Views.timeReport],
    [/^#\/billing$/, Views.billingInvoices],
    [/^#\/arap$/, Views.arapView],
    [/^#\/recon$/, Views.reconView],
    [/^#\/rules$/, Views.rulesAdmin],
  ];

  async function render() {
    const h = location.hash || "#/dashboard";
    nav.querySelectorAll("a").forEach((a) => {
      const r = a.dataset.route;
      a.classList.toggle("active", h.startsWith("#/" + r) || (r === "cases" && h.startsWith("#/new")));
    });
    for (const [re, fn] of routes) {
      const m = h.match(re);
      if (m) {
        view.innerHTML = await fn(m);
        view.classList.remove("view-in");
        void view.offsetWidth; // restart the enter animation on every render
        view.classList.add("view-in");
        view.focus({ preventScroll: true });
        return;
      }
    }
    view.innerHTML = `<h1>Not found</h1>`;
  }
  // Soft refresh: re-render the current route without a full page reload
  // (no Keycloak round-trip, no shell flash, preserves rail/scroll context).
  window.App.rerender = render;
  // Last-resort feedback net: any async handler that still throws without its
  // own try/catch surfaces as an error toast instead of failing silently.
  addEventListener("unhandledrejection", (e) => {
    UI.toast(e.reason?.message || "Unexpected error", { kind: "error" });
  });
  // Immediate feedback when any compact select changes, before the async
  // handler's own toast lands — the control flashes so the user sees the
  // change registered even on slow networks.
  document.addEventListener("change", (e) => {
    if (e.target.matches?.("select.mini")) UI.flash(e.target);
  });
  addEventListener("hashchange", render);
  if (!location.hash) location.hash = "#/dashboard";
  render();

  // Presence heartbeat: report this user as active every 45s; the ops
  // dashboard counts anyone seen within the last 3 minutes as online.
  const pingPresence = () => Api.program.pingPresence(me?.name).catch(() => {});
  pingPresence();
  setInterval(pingPresence, 45000);
})();
