/* telemetry.js — Phase 1: the interaction baseline. Batches portal events
 * and flushes every 15s (+ sendBeacon on hide) to /telemetry/events.
 * Privacy contract: keystroke COUNTS only (never content), control labels
 * (button text) but never form values. Also the accept/override channel for
 * prefill and next-action suggestions — that stream is the training data
 * for calibrating (and eventually learning) the recommenders. */
const Telemetry = (() => {
  const SID = sessionStorage.getItem("telem_sid") ||
    (() => { const s = Math.random().toString(36).slice(2) + Date.now().toString(36);
             sessionStorage.setItem("telem_sid", s); return s; })();
  let queue = [];
  let keys = 0; // keystrokes since last flush

  const workflowOf = () => {
    const h = location.hash || "#/";
    if (h.startsWith("#/new")) return "case_initiate";
    if (h.startsWith("#/qa")) return "qa_queue";
    if (h.startsWith("#/intake")) return "intake";
    if (h.startsWith("#/doc-review")) return "doc_review";
    if (h.startsWith("#/cases/")) return "case_detail";
    if (h.startsWith("#/crm")) return "crm";
    if (h === "#/" || h.startsWith("#/dashboard")) return "dashboard";
    return "other";
  };
  const track = (action, target, meta) => {
    queue.push({ workflow: workflowOf(), action, target: String(target || "").slice(0, 120), meta: meta || {} });
    if (queue.length >= 40) flush();
  };
  const flush = async (keepalive) => {
    if (keys > 0) { queue.push({ workflow: workflowOf(), action: "keystroke", target: "", meta: { keys } }); keys = 0; }
    if (!queue.length) return;
    const body = JSON.stringify({ session_id: SID, events: queue.splice(0, queue.length) });
    const tenant = localStorage.getItem("idre.tenant");
    const tok = await Auth.token();
    if (!tenant || !tok) return;
    // keepalive fetch (not sendBeacon) so the Authorization header survives
    // page-hide flushes — beacons can't carry headers.
    fetch(window.IDRE_CONFIG.apiBase + `/v1/tenants/${tenant}/telemetry/events`, {
      method: "POST", body, keepalive: !!keepalive,
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${tok}` },
    }).catch(() => {});
  };

  document.addEventListener("click", (ev) => {
    const el = ev.target.closest("button, a, [data-track]");
    if (!el) return;
    const label = (el.dataset.track || el.textContent || "").trim().slice(0, 60);
    if (label) track("click", label);
  }, true);
  document.addEventListener("keydown", (ev) => {
    if (ev.target.matches("input, textarea, select")) keys++;
  }, true);
  document.addEventListener("submit", (ev) => track("submit", ev.target.id || ev.target.name || "form"), true);
  window.addEventListener("hashchange", () => track("navigate", location.hash));
  document.addEventListener("visibilitychange", () => { if (document.visibilityState === "hidden") flush(true); });
  setInterval(() => flush(false), 15000);

  // role: the single role this user acts as, for per-role learning cells.
  // SAME canonical order as canonicalRoleOrder in the API's nextaction.go —
  // event-time stamps and request-time lookups must agree or the cells split.
  const ROLE_ORDER = ["PLATFORM_ADMIN", "FEDERAL_ADMIN", "TENANT_ADMIN",
    "CASE_MANAGER", "ARBITRATOR", "ATTORNEY", "FINANCE", "PM",
    "DOCTOR", "NURSE", "NURSE_PHYSICIAN", "CODER", "BULK_SUBMITTER", "PARTY"];
  const role = () => {
    const have = (window.Auth && Auth.claims() && Auth.claims().roles) || [];
    for (const want of ROLE_ORDER) if (have.includes(want)) return want;
    return have[0] || "";
  };

  return { track, flush, role };
})();
