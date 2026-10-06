// views.js — one render function per screen; role-aware actions.
// Meridian upgrade: statutory-clock projections, L4 peek panel, bulk bar,
// density toggle, modal+toast everywhere (no prompt/alert), sealed-offer flow.
const Views = (() => {
  const $ = (sel) => document.querySelector(sel);
  // Bind after the router injects innerHTML: queueMicrotask races ahead of the
  // `view.innerHTML = await fn()` continuation (microtask order) and bound null;
  // a macrotask runs strictly after it. Every form/handler binding MUST use this.
  const afterRender = (fn) => setTimeout(fn, 0);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const fmtDate = (d) => (d ? new Date(d).toLocaleString("en-US", { dateStyle: "medium", timeStyle: "short" }) : "—");
  const badge = (s) => `<span class="badge s-${esc(s).toLowerCase().replace(/_/g, "-")}">${esc(s)}</span>`;
  const err = (e) => `<div class="err-box"><b>Something didn't load.</b> ${esc(e.message)} <button class="mini" onclick="App.rerender()">Retry</button></div>`;
  const role = (r) => (Auth.claims()?.roles || []).includes(r);
  const can = (...rs) => rs.some(role);

  // ---- Statutory clock helpers -------------------------------------------------
  const clockChip = (c) => {
    const txt = c.remaining >= 0 ? `${c.remaining} ${c.basis === "business" ? "bd" : "cd"} left` : `Breached ${Math.abs(c.remaining)}d ago`;
    return `<span class="sla ${c.state}" title="${esc(c.label)} · ${esc(c.cite)} · basis: ${esc(c.basis_note)}">◷ ${txt}</span>`;
  };
  const nearestClock = (clocks) => clocks && clocks.length
    ? clocks.reduce((a, b) => (a.remaining < b.remaining ? a : b)) : null;

  async function clockMap() {
    try {
      const all = await Api.cm.allClocks();
      return Object.fromEntries(all.map((x) => [x.case_id, x.clocks]));
    } catch { return {}; }
  }

  // ---- Dashboard -----------------------------------------------------------
  async function dashboard() {
    const me = Auth.claims();
    let html = `<div class="view-head"><h1>Good day, ${esc((me.name || "").split(/[.\s]/)[0] || me.name)}</h1>
      <span class="muted">tenant <b>${esc(Api.getTenant()).toUpperCase()}</b> · ${me.roles.map(esc).join(", ")}</span></div>`;
    try {
      const [{ cases }, summary, clocks] = await Promise.all([Api.cases.list({ limit: 200 }), Api.reports.summary(), clockMap()]);
      html += `<div class="cards">` + summary.map((s) =>
        `<div class="card"><div class="num">${s.count}</div><div class="lbl">${badge(s.status)}</div>
         <div class="muted">avg ${esc(App.t("amount_label"))} $${s.avg_qpa_usd.toFixed(0)}</div></div>`).join("") + `</div>`;
      const open = cases.filter((c) => !String(c.status).startsWith("CLOSED"));
      const attention = open.filter((c) => clocks[c.id] && clocks[c.id].length)
        .sort((a, b) => nearestClock(clocks[a.id]).remaining - nearestClock(clocks[b.id].remaining));
      html += `<h2>Needs your attention (${attention.length})</h2>`;
      html += attention.length
        ? `<div class="panel">` + attention.slice(0, 8).map((c) => {
            const cl = nearestClock(clocks[c.id]);
            return `<div class="att-item" onclick="location.hash='#/cases/${c.id}'">
              <span class="att-id">${esc(c.case_number)}</span>
              <span class="att-title">${esc(c.service_line)} · ${esc(App.t("amount_label"))} $${(c.qpa_cents / 100).toLocaleString()}</span>
              ${badge(c.status)} ${clockChip(cl)}</div>`;
          }).join("") + `</div>`
        : `<p class="muted">Nothing needs you right now. New assignments and deadline risk appear here.</p>`;
      html += `<h2>Open disputes (${open.length})</h2>` + caseTable(open, clocks);
    } catch (e) { html += err(e); }
    return html;
  }

  // ---- Disputes grid: clocks, selection, peek, density ------------------------
  let selection = new Set();

  // Server-driven column sort, stored in the hash so it survives refresh and
  // is shareable: '#/cases?sort=-qpa_cents' (prefix '-' = descending).
  const SORTABLE = new Set(["case_number", "status", "service_line", "qpa_cents", "opened_at"]);
  function sortableTh(col, label, cur) {
    if (!cur && cur !== "") return `<th>${label}</th>`;
    const active = cur === col || cur === "-" + col;
    const arrow = cur === col ? " ↑" : cur === "-" + col ? " ↓" : "";
    return `<th class="sortable${active ? " sorted" : ""}" onclick="Views.sortCases('${col}')">${label}${arrow}</th>`;
  }
  function sortCases(col) {
    if (!SORTABLE.has(col)) return;
    const q = new URLSearchParams(location.hash.split("?")[1] || "");
    const cur = q.get("sort") || "";
    // Cycle: none -> asc -> desc -> none
    const next = cur === col ? "-" + col : cur === "-" + col ? "" : col;
    next ? q.set("sort", next) : q.delete("sort");
    const s = q.toString();
    location.hash = "#/cases" + (s ? "?" + s : "");
  }
  const caseTable = (rows, clocks = {}, sort = "") => rows.length ? `
    <div class="dg-wrap">
      <table class="dg"><thead><tr>
        <th class="selcol"><input type="checkbox" id="sel-all" aria-label="Select all"></th>
        ${[["case_number", "Case #"], ["status", "Status"], ["service_line", App.t("service_label")], ["qpa_cents", App.t("amount_label")]]
          .map(([c, l]) => sortableTh(c, l, sort)).join("")}
        <th>Statutory clock</th>${sortableTh("opened_at", "Opened", sort)}<th></th></tr></thead><tbody>` +
      rows.map((c) => `<tr class="click" data-case="${c.id}">
        <td class="selcol"><input type="checkbox" class="sel-one" data-id="${c.id}" ${selection.has(c.id) ? "checked" : ""} aria-label="Select ${esc(c.case_number)}"></td>
        <td class="mono">${esc(c.case_number)}</td><td>${badge(c.status)}</td><td>${esc(c.service_line)}</td>
        <td class="num">$${(c.qpa_cents / 100).toLocaleString()}</td>
        <td>${clocks[c.id] && clocks[c.id].length ? clockChip(nearestClock(clocks[c.id])) : '<span class="muted">—</span>'}</td>
        <td class="muted">${fmtDate(c.opened_at)}</td>
        <td><button class="mini peek" data-id="${c.id}" title="Peek without losing your place">▸</button></td></tr>`).join("") +
      `</tbody></table></div>` : `<p class="muted">No disputes.</p>`;

  function bindGrid(rows) {
    $("#sel-all")?.addEventListener("change", (e) => {
      selection = e.target.checked ? new Set(rows.map((c) => c.id)) : new Set();
      document.querySelectorAll(".sel-one").forEach((cb) => (cb.checked = e.target.checked));
      paintBulkBar();
    });
    document.querySelectorAll(".sel-one").forEach((cb) =>
      cb.addEventListener("change", () => { cb.checked ? selection.add(cb.dataset.id) : selection.delete(cb.dataset.id); paintBulkBar(); }));
    document.querySelectorAll("tr.click").forEach((tr) =>
      tr.addEventListener("click", (e) => {
        if (e.target.closest("input,button")) return;
        location.hash = `#/cases/${tr.dataset.case}`;
      }));
    document.querySelectorAll(".peek").forEach((b) => b.addEventListener("click", () => peek(b.dataset.id)));
  }

  function paintBulkBar() {
    document.querySelector(".bulk-bar")?.remove();
    if (!selection.size) return;
    const bar = document.createElement("div");
    bar.className = "bulk-bar";
    bar.setAttribute("aria-live", "polite");
    bar.innerHTML = `<b>${selection.size} selected</b>
      <button class="mini" id="bk-assign">Assign to me</button>
      <button class="mini" id="bk-status">Set status…</button>
      <button class="mini" id="bk-clear">Clear</button>`;
    document.body.appendChild(bar);
    $("#bk-clear").onclick = () => { selection.clear(); document.querySelectorAll(".sel-one,#sel-all").forEach((cb) => (cb.checked = false)); paintBulkBar(); };
    $("#bk-assign").onclick = async (ev) => {
      await UI.run(ev.currentTarget, async () => {
        const r = await Api.cm.bulk({ action: "assign", case_ids: [...selection] });
        const ok = r.results.filter((x) => x.ok).length;
        UI.toast(`Assigned ${ok} of ${r.results.length} disputes to you`);
        selection.clear(); App.rerender();
      }, "Assigning…");
    };
    $("#bk-status").onclick = async (ev) => {
      const v = await UI.modal({ title: `Set status for ${selection.size} disputes`, fields: [
        { name: "status", label: "New status", options: [["INITIATED", "Initiated"], ["OFFER_WINDOW_OPEN", "Offer window open"], ["IN_REVIEW", "In review"], ["DETERMINED", "Determined"], ["PAYMENT_PENDING", "Payment pending"], ["CLOSED_DISMISSED", "Closed — dismissed"]], required: true },
      ], submitLabel: "Apply to selection" });
      if (!v) return;
      await UI.run(ev.currentTarget, async () => {
        const r = await Api.cm.bulk({ action: "status", status: v.status, case_ids: [...selection] });
        const ok = r.results.filter((x) => x.ok).length;
        UI.toast(`Status updated on ${ok} of ${r.results.length} disputes`);
        selection.clear(); App.rerender();
      }, "Applying…");
    };
  }

  // L4 peek panel — preview a case without losing list position
  async function peek(id) {
    document.querySelector(".peek-panel")?.remove();
    const p = document.createElement("aside");
    p.className = "peek-panel";
    p.setAttribute("role", "complementary");
    p.setAttribute("aria-label", "Case preview");
    p.innerHTML = `<div class="peek-h"><b>Preview</b><button class="icon-btn" aria-label="Close preview">✕</button></div>
      <div class="peek-b"><p class="muted">Loading…</p></div>`;
    document.body.appendChild(p);
    p.querySelector(".icon-btn").onclick = () => p.remove();
    p.addEventListener("keydown", (e) => { if (e.key === "Escape") p.remove(); });
    try {
      const [c, clocks] = await Promise.all([Api.cases.get(id), Api.cm.clocks(id).catch(() => [])]);
      p.querySelector(".peek-b").innerHTML = `
        <div class="mono muted">${esc(c.case_number)}</div>
        <h3>${esc(c.service_line)} · $${(c.qpa_cents / 100).toLocaleString()}</h3>
        <p>${badge(c.status)}</p>
        ${clocks.map((cl) => `<div class="sla-card"><span class="sla ${cl.state}">${clockChip(cl)}</span>
          <span class="muted" style="font-size:11px">${esc(cl.label)} · ${esc(cl.cite)}</span></div>`).join("")}
        <p style="margin-top:14px"><button class="mini" onclick="location.hash='#/cases/${c.id}'">Open workspace →</button></p>`;
    } catch (e) { p.querySelector(".peek-b").innerHTML = err(e); }
  }

  // ---- Disputes list -----------------------------------------------------------
  // Server-side keyset pagination: page size 50, "Load more" appends the next
  // page; saved-view status filters are pushed to the server so a filter never
  // applies to a partial page. Counts come from the server total, not the page.
  const PAGE_SIZE = 50;
  async function cases() {
    try {
      const hq = new URLSearchParams(location.hash.split("?")[1] || "");
      const v = hq.get("view");
      const sort = hq.get("sort") || "";
      const saved = await Api.cm.views().catch(() => []);
      const sv = saved.find((s) => s.id === v);
      const status = sv && sv.filters && sv.filters.status ? sv.filters.status : "";
      const reqParams = { limit: PAGE_SIZE, ...(status ? { status } : {}), ...(sort ? { sort } : {}) };
      const [page, clocks] = await Promise.all([Api.cases.list(reqParams), clockMap()]);
      let loaded = page.cases.slice();   // accumulated rows across pages
      let cursor = page.next_cursor;     // "" when no more pages
      const total = page.total;
      const opts = saved.map((s) => `<option value="${s.id}" ${s.id === v ? "selected" : ""}>${s.pinned ? "★ " : ""}${esc(s.name)}</option>`).join("");
      const density = localStorage.getItem("idre.density") || "comfortable";
      document.body.classList.toggle("density-compact", density === "compact");
      const pager = () => `<p class="pager"><span class="muted">Showing ${loaded.length} of ${total}</span>
        ${cursor ? `<button class="mini" id="more">Load more (${Math.min(PAGE_SIZE, total - loaded.length)} of ${total - loaded.length} remaining)</button>` : ""}</p>`;
      afterRender(() => {
        bindGrid(loaded);
        $("#density").onclick = () => {
          const next = (localStorage.getItem("idre.density") || "comfortable") === "comfortable" ? "compact" : "comfortable";
          (window.Prefs ? Prefs.push("density", next) : localStorage.setItem("idre.density", next));
          document.body.classList.toggle("density-compact", next === "compact");
          UI.toast(`Density: ${next}`);
        };
        $("#grab").onclick = async (ev) => {
          await UI.run(ev.currentTarget, async () => {
            const r = await Api.cm.grabNext();
            if (r.claimed) { UI.toast(`Claimed ${r.case_number} from the queue`); location.hash = `#/cases/${r.case_id}`; }
            else UI.toast(r.message, { kind: "warn" });
          }, "Grabbing…");
        };
        const loadMore = async (ev) => {
          await UI.run(ev.currentTarget, async () => {
            try {
              const more = { limit: PAGE_SIZE, ...(status ? { status } : {}), ...(sort ? { sort } : {}) };
              if (cursor.startsWith("offset:")) more.offset = cursor.slice(7); else more.cursor = cursor;
              const next = await Api.cases.list(more);
              loaded = loaded.concat(next.cases);
              cursor = next.next_cursor;
              document.querySelector(".dg-wrap").outerHTML = caseTable(loaded, clocks, sort || "opened_at");
              $("#pg").innerHTML = pager();
              bindGrid(loaded);
              $("#more")?.addEventListener("click", loadMore);
              UI.toast(`Showing ${loaded.length} of ${total}`, { kind: "info" });
            } catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }, "Loading…");
        };
        $("#more")?.addEventListener("click", loadMore);
      });
      return `<div class="view-head"><h1>${esc(App.t("case_plural"))}</h1><span class="muted">${total} total</span>
        <span style="flex:1"></span>
        <button class="mini" id="grab">⇪ Grab next</button>
        <button class="mini" id="density">Density: ${density}</button></div>
        <p class="viewbar">
          <select onchange="location.hash='#/cases?view='+this.value" aria-label="Saved views">
            <option value="">All disputes</option>${opts}</select>
          <button class="mini" onclick="Views.saveCurrentView()">Save current view</button>
          ${sv ? `<span class="muted">filter: status = ${esc(sv.filters.status)}</span>` : ""}</p>` +
        caseTable(loaded, clocks, sort || "opened_at") + `<div id="pg">${pager()}</div>`;
    } catch (e) { return `<h1>${esc(App.t("case_plural"))}</h1>` + err(e); }
  }

  // ---- Actions (all modal-based now) ----------------------------------------------
  async function escalate(caseId) {
    const v = await UI.modal({ title: "Escalate case", danger: true, submitLabel: "Escalate",
      body: "Supervisors and federal administrators are notified immediately. This is logged to the audit trail.",
      fields: [
        { name: "clock", label: "Statutory clock", options: [["DETERMINATION_30BD", "Determination (30bd)"], ["OFFER_WINDOW_10BD", "Offer window (10bd)"], ["PAYMENT_30CD", "Payment (30cd)"], ["NEGOTIATION_30BD", "Negotiation (30bd)"], ["INITIATION_4BD", "Initiation (4bd)"]], required: true },
        { name: "detail", label: "Reason", type: "textarea", placeholder: "Why does this need supervisory attention?", required: true },
      ] });
    if (!v) return;
    try {
      await Api.cm.escalate(caseId, v.clock, v.detail);
      UI.toast("Escalated — supervisors and federal administrators notified");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function relate(caseId) {
    const v = await UI.modal({ title: "Link related case", fields: [
      { name: "related", label: "Related case ID", placeholder: "UUID", required: true },
      { name: "type", label: "Relationship", options: [["BATCH", "Batch"], ["PARENT_CHILD", "Parent / child"], ["DUPLICATE", "Duplicate"]], required: true },
    ] });
    if (!v) return;
    try {
      await Api.cm.relate({ case_id: caseId, related_case_id: v.related, rel_type: v.type });
      UI.toast("Cases linked");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function feeTransfer(caseId) {
    const v = await UI.modal({ title: "Post fee transfer", submitLabel: "Post to ledger",
      body: "Double-entry transfer on the tenant's TigerBeetle ledger. Idempotent; subject to the Permify ledger permission.",
      fields: [
        { name: "kind", label: "Transfer kind", options: [["ADMIN_FEE", "Admin fee ($15)"], ["IDRE_FEE_RESERVE", "IDRE fee reserve"], ["REFUND", "Refund"], ["SETTLEMENT", "Settlement"]], required: true },
        { name: "party", label: "Party ID", value: "party", required: true },
        { name: "amount", label: "Amount (USD)", type: "number", step: "0.01", value: "15.00", required: true },
        { name: "post", label: "Posting", options: [["PENDING", "Pending (two-phase)"], ["POST", "Post immediately"], ["VOID", "Void pending"]], required: true },
      ] });
    if (!v) return;
    try {
      await Api.fees.transfer({ case_id: caseId, kind: v.kind, party_id: v.party,
        amount_cents: Math.round(parseFloat(v.amount) * 100), post_kind: v.post });
      UI.toast("Ledger transfer created (double-entry, idempotent)");
      App.rerender();
    } catch (e) { UI.toast("Ledger rejected the transfer: " + e.message, { kind: "warn", duration: 9000 }); }
  }

  async function saveCurrentView() {
    const v = await UI.modal({ title: "Save current view", fields: [
      { name: "name", label: "View name", required: true },
      { name: "status", label: "Filter by status", placeholder: "leave blank for all" },
    ], submitLabel: "Save view" });
    if (!v) return;
    try {
      await Api.cm.saveView({ object: "CASES", name: v.name, filters: v.status ? { status: v.status } : {} });
      UI.toast("View saved");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function assign(caseId) {
    const v = await UI.modal({ title: "Assign / route", fields: [
      { name: "role", label: "Assign as", options: [["CASE_MANAGER", "Case manager"], ["ARBITRATOR", "Arbitrator"]], required: true },
      { name: "assignee", label: "Assignee", placeholder: "blank = workload-balanced auto" },
    ] });
    if (!v) return;
    try {
      const r = await Api.cm.assign(caseId, v.role, v.assignee || "");
      UI.toast(`Assigned to ${r.assigned_to}`);
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function letter(caseId, template) {
    let qs = "";
    if (template === "determination_letter") {
      const v = await UI.modal({ title: "Determination letter", fields: [
        { name: "winning", label: "Winning party", required: true },
        { name: "rationale", label: "Rationale", type: "textarea", required: true },
      ], submitLabel: "Generate letter" });
      if (!v) return;
      qs = `?winning_party=${encodeURIComponent(v.winning)}&rationale=${encodeURIComponent(v.rationale)}`;
    }
    try { await Api.cm.letter(caseId, template, qs); UI.toast("Letter generated — see Documents"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // Sealed offer — value goes only to the vault via the signal; never rendered back.
  async function offerForm(caseId) {
    const v = await UI.modal({ title: "Submit sealed offer", submitLabel: "Seal and submit",
      body: "Your offer is encrypted in the vault the moment you submit. It is visible to no one — including arbitrators — until the offer window closes and the lawful-reveal gate passes.",
      fields: [{ name: "amount", label: "Offer amount (USD)", type: "number", step: "0.01", required: true }] });
    if (!v) return;
    try {
      await Api.cases.signal(caseId, "OFFER_SUBMITTED", { party_id: Auth.claims().sub, amount_cents: Math.round(parseFloat(v.amount) * 100) });
      UI.toast("Offer received and sealed — receipt logged");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function determinationForm(caseId) {
    const v = await UI.modal({ title: "Issue determination", submitLabel: "Issue determination", danger: true,
      body: "This selects the prevailing offer under 45 CFR 149.510(c)(4) and starts the 30-calendar-day payment clock. It cannot be undone.",
      fields: [
        { name: "winning", label: "Winning offer (party id)", required: true },
        { name: "rationale", label: "Determination rationale", type: "textarea", required: true,
          hint: "Required — becomes part of the certified determination letter." },
      ] });
    if (!v) return;
    try {
      await Api.cases.signal(caseId, "DETERMINATION_ISSUED", { winning_offer_party: v.winning, rationale: v.rationale });
      UI.toast("Determination issued — payment clock started");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- Case detail workspace -------------------------------------------------
  async function caseDetail(id) {
    try {
      const [c, docs, activities, checklist, rels, clocks] = await Promise.all([
        Api.cases.get(id), Api.cases.documents(id), Api.cases.activities(id),
        Api.cm.checklist(id), Api.cm.relationships(id), Api.cm.clocks(id).catch(() => []),
      ]);
      Palette.remember("case", c.id, c.case_number);
      let html = `<div class="view-head"><h1><span class="mono">${esc(c.case_number)}</span></h1>
        ${badge(c.status)}</div>
        <p class="muted">${esc(c.service_line)} · ${esc(App.t("amount_label"))} $${(c.qpa_cents / 100).toLocaleString()} · opened ${fmtDate(c.opened_at)}</p>`;

      // Statutory clock cluster (server-projected)
      if (clocks.length)
        html += `<div class="sla-cluster">` + clocks.map((cl) => `
          <div class="sla-card"><span class="sla ${cl.state}" style="font-size:16px">${clockChip(cl)}</span>
            <span class="lbl">${esc(cl.label)}</span>
            <span class="cite">${esc(cl.cite)} · due ${esc(cl.due)} · ${esc(cl.basis_note)}</span></div>`).join("") + `</div>`;

      // Workflow actions by role + status
      const acts = [];
      if (can("PARTY") && c.status === "INITIATED")
        acts.push(["Respond filed", () => Api.cases.signal(id, "RESPONSE_FILED", {})]);
      if (can("PARTY") && ["OFFER_WINDOW_OPEN", "INITIATED"].includes(c.status))
        if (App.feature("sealed_offers")) acts.push(["Submit sealed offer", () => offerForm(id)]);
      if (can("PARTY"))
        acts.push(["Mark fees paid", () => Api.cases.signal(id, "FEES_PAID", { party_id: Auth.claims().sub })]);
      if (can("ARBITRATOR"))
        acts.push(["Issue determination", () => determinationForm(id)]);
      if (can("CASE_MANAGER", "FEDERAL_ADMIN"))
        acts.push(["Finalize IDRE selection", () => Api.cases.signal(id, "SELECTION_FINALIZED", {})]);
      if (can("CASE_MANAGER", "FEDERAL_ADMIN"))
        acts.push(["Escalate", () => Views.escalate(id)]);
      if (can("CASE_MANAGER", "FEDERAL_ADMIN"))
        acts.push(["Link related case", () => Views.relate(id)]);
      if (can("FINANCE") && ["DETERMINED", "CLOSED_PAID"].includes(c.status))
        acts.push(["Record payment", () => Api.cases.signal(id, "PAYMENT_RECORDED", { amount_cents: c.qpa_cents })]);
      if (can("FINANCE"))
        acts.push(["Post fee transfer", () => Views.feeTransfer(id)]);
      if (acts.length)
        html += `<div class="actions">` + acts.map((a, i) =>
          `<button data-act="${i}">${a[0]}</button>`).join("") + `</div>`;

      // Documents — docket grouped by folder with full metadata + RBAC controls
      const FOLDERS = ["GENERAL", "INTAKE", "EVIDENCE", "CORRESPONDENCE", "OFFERS", "DETERMINATION", "INVOICES", "PARTY_UPLOADS"];
      html += `<h2>Docket</h2>
        <p><a class="button" href="${Api.cases.zipUrl(id)}" target="_blank">⬇ Download all as zip (plan-notification bundle)</a></p>
        <form id="up" class="upload"><input type="file" name="file" required aria-label="Choose file" />
        <select name="folder" aria-label="Folder">${FOLDERS.map((f) => `<option>${f}</option>`).join("")}</select>
        <label><input type="checkbox" name="sealed" /> sealed (offer justification — encrypted in the vault)</label>
        <button>Upload</button></form>`;
      if (docs.length) {
        const byFolder = {};
        docs.forEach((d) => { (byFolder[d.folder || "GENERAL"] = byFolder[d.folder || "GENERAL"] || []).push(d); });
        html += Object.entries(byFolder).map(([folder, items]) => `
          <h3 class="doc-folder">📁 ${esc(folder)} <span class="muted">(${items.length})</span></h3>
          <table><thead><tr><th>File</th><th>Type</th><th>Size</th><th>Scan</th><th>Sealed</th><th>Uploaded</th><th>Analysis</th><th></th></tr></thead><tbody>` +
          items.map((d) => `<tr>
            <td class="mono">${esc(d.filename || d.doc_id.slice(0, 8) + "…")}</td>
            <td>${esc(d.content_type || "—")}</td>
            <td>${d.size_bytes > 1048576 ? (d.size_bytes / 1048576).toFixed(1) + " MB" : (d.size_bytes / 1024).toFixed(0) + " KB"}</td>
            <td>${d.scan_status === "CLEAN" ? '<span class="badge s-paid">✓ clean</span>' : badge(d.scan_status || "PENDING")}</td>
            <td>${d.sealed ? '<span class="badge s-sealed">🔒 sealed</span>' : "—"}</td>
            <td class="muted">${esc(d.uploaded_by || "")} · ${fmtDate(d.uploaded_at)}</td>
            <td>${badge(d.analysis_status)}${d.doc_type ? " · " + esc(d.doc_type) : ""}</td>
            <td><a href="${Api.cases.downloadUrl(id, d.doc_id)}" target="_blank">download</a>
            ${!d.sealed ? ` · <a href="javascript:void(0)" onclick="Views.showAnalysis('${id}','${d.doc_id}')">analysis</a>` : ""}
            ${can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ?
              ` · <select class="mini" onchange="Views.moveDoc('${id}','${d.doc_id}',this.value,this)"><option value="">move…</option>
                ${FOLDERS.filter((f) => f !== d.folder).map((f) => `<option>${f}</option>`).join("")}</select>` : ""}</td></tr>`).join("") +
          `</tbody></table>`).join("");
      } else html += `<p class="muted">No documents yet.</p>`;
      html += `<div id="analysis"></div>`;

      // Program panels (per-state rules: eligibility, correspondence/QA,
      // invoices, claims, dual status, program dates, opt-out) — rendered
      // only when the tenant runs a custom program.
      html += `<div id="prog"></div>`;
      programPanel(id).then((h) => { const b = document.getElementById("prog"); if (b) b.innerHTML = h; });

      if (rels.length)
        html += `<h2>Related cases</h2><table><tbody>` + rels.map((r) =>
          `<tr><td>${badge(r.rel_type)}</td><td class="mono">${esc(r.related_case_id)}</td></tr>`).join("") + `</tbody></table>`;

      // GNN link-prediction panel (graph-intel; degrades gracefully offline)
      html += `<div id="gnn-rel"></div>`;
      Api.graph.related(id).then((g) => {
        const box = document.getElementById("gnn-rel");
        if (!box) return;
        const preds = (g && g.predictions) || [];
        box.innerHTML = `<h2>Suggested related disputes <span class="muted">· GraphSAGE ${esc(g.model_version || "")}</span></h2>` +
          (preds.length ? `<table><thead><tr><th>Case</th><th>Link score</th><th></th></tr></thead><tbody>` +
            preds.map((p) => `<tr><td class="mono">${esc(p.case_id.slice(0, 8))}…</td>
              <td><span class="gnn-score" style="--s:${p.score}">${(p.score * 100).toFixed(0)}%</span></td>
              <td><a href="#/cases/${esc(p.case_id)}">open</a> ·
                  <a href="javascript:void(0)" onclick="Views.relate('${id}')">confirm link</a></td></tr>`).join("") +
            `</tbody></table>` :
            `<p class="muted">${esc(g.reason || "No link predictions yet — train the model (POST /graph/train) after a few cases exist.")}</p>`);
      }).catch(() => { const b = document.getElementById("gnn-rel"); if (b) b.innerHTML = ""; });
      const stages = {};
      checklist.forEach((i) => { (stages[i.stage] = stages[i.stage] || []).push(i); });
      html += `<h2>Stage checklists</h2>` + Object.entries(stages).map(([stage, items]) =>
        `<h3>${esc(stage)}</h3><ul class="checklist">` + items.map((i) =>
          `<li>${i.done ? "✅" : (can("CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN")
            ? `<button class="mini" onclick="Views.check('${i.id}')">check</button>` : "⬜")}
            ${esc(i.item)}${i.required ? " *" : ""}${i.done_by ? ` <span class="muted">(${esc(i.done_by)})</span>` : ""}</li>`).join("") +
        `</ul>`).join("");
      if (can("CASE_MANAGER", "FEDERAL_ADMIN"))
        html += `<div class="actions">
          <button onclick="Views.assign('${id}')">Assign / route</button>
          <button onclick="Views.letter('${id}','offer_window_notice')">Generate offer-window notice</button>
          <button onclick="Views.letter('${id}','determination_letter')">Generate determination letter</button>
        </div>`;

      html += `<h2>Activity timeline</h2>` + (activities.length ? `<table><tbody>` +
        activities.map((a) => `<tr><td>${badge(a.type)}</td><td>${esc(a.body)}</td>
          <td class="muted">${fmtDate(a.at)}</td></tr>`).join("") +
        `</tbody></table>` : `<p class="muted">No activity yet — voice calls and milestones attach automatically.</p>`);

      afterRender(() => {
        acts.forEach((a, i) =>
          document.querySelector(`[data-act="${i}"]`)?.addEventListener("click", async () => {
            try { await a[1](); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }));
        $("#up").addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = ev.target.file.files[0];
          await UI.run(ev.target.querySelector("button"), async () => {
            try { await Api.cases.upload(id, f, ev.target.sealed.checked, ev.target.folder.value); UI.toast("Document uploaded"); App.rerender(); }
            catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }, "Uploading…");
        });
      });
      return html;
    } catch (e) { return err(e); }
  }

  async function moveDoc(caseId, docId, folder, sel) {
    if (!folder) return;
    await UI.run(sel, async () => {
      try { await Api.cases.moveDoc(caseId, docId, folder); UI.toast(`Moved to ${folder}`); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  async function showAnalysis(caseId, docId) {
    const box = $("#analysis");
    box.innerHTML = `<p class="muted">Loading analysis…</p>`;
    try {
      const a = await Api.cases.analysis(caseId, docId);
      const r = a.result || {};
      box.innerHTML = `<h3>Document analysis ${badge(a.status)}</h3>
        <p class="muted">type: ${esc(a.doc_type)} · seal detected: ${r.seal_detected ? "yes" : "no"} · tables: ${r.table_count ?? 0}</p>
        <pre>${esc(JSON.stringify(r.extracted || {}, null, 2))}</pre>
        ${(r.findings || []).length ? `<p class="error">Findings: ${esc(JSON.stringify(r.findings))}</p>` : ""}`;
    } catch (e) {
      box.innerHTML = e.status === 404
        ? `<h3>Document analysis</h3><p class="muted">No analysis result yet — the document is still queued for the OCR/extraction pipeline (or was uploaded before doc-intel processed it). Try again shortly.</p>`
        : err(e);
    }
  }

  async function check(itemId) {
    try { await Api.cm.checkItem(itemId); UI.toast("Checklist item completed"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- New dispute -------------------------------------------------------------
  function newDispute() {
    afterRender(() => $("#nd").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = Object.fromEntries(new FormData(ev.target));
      await UI.run(ev.target.querySelector("button"), async () => {
        try {
          const r = await Api.cases.initiate({
            case_number: f.case_number, service_line: f.service_line, plan_type: f.plan_type,
            qpa_cents: Math.round(parseFloat(f.qpa) * 100), provider_id: f.provider_id,
            payer_id: f.payer_id, open_negotiation_end: f.one_end,
          });
          UI.toast("Dispute initiated — statutory clocks started");
          location.hash = `#/cases/${r.case_id}`;
        } catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
      }, "Initiating…");
    }));
    return `<h1>New ${esc(App.t("case_noun").toLowerCase())}</h1><form id="nd" class="form">
      <label>CMS case number <input name="case_number" required placeholder="CMS-TX-2026-00002" /></label>
      <label>Service line <select name="service_line"><option>ER</option><option>AIR_AMBULANCE</option><option>ANESTHESIA</option><option>RADIOLOGY</option><option>LAB</option><option>OTHER</option></select></label>
      <label>Plan type <select name="plan_type"><option>SELF_FUNDED</option><option>FULLY_INSURED</option></select></label>
      <label>QPA (USD) <input name="qpa" type="number" step="0.01" required /></label>
      <label>Provider ID <input name="provider_id" required /></label>
      <label>Payer ID <input name="payer_id" required /></label>
      <label>Open negotiation end <input name="one_end" type="date" required /></label>
      <button>Initiate (starts statutory clocks)</button></form>`;
  }

  // ---- Onboarding --------------------------------------------------------------
  async function onboarding() {
    let html = `<h1>Onboarding</h1>`;
    if (can("CASE_MANAGER", "FEDERAL_ADMIN")) {
      try {
        const apps = await Api.onboarding.list();
        html += `<h2>Review queue</h2>` + (apps.length ? `<table><thead><tr>
          <th>Legal name</th><th>Type</th><th>Status</th><th>Submitted</th><th></th></tr></thead><tbody>` +
          apps.map((a) => `<tr><td>${esc(a.legal_name)}</td><td>${esc(a.type)}</td>
            <td>${badge(a.status)}</td><td>${fmtDate(a.submitted_at)}</td>
            <td>${["PENDING_APPROVAL"].includes(a.status) ? `
              <button onclick="Views.decide('${a.id}','APPROVE')">Approve</button>
              <button class="danger" onclick="Views.decide('${a.id}','REJECT')">Reject</button>` : ""}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">Queue empty.</p>`);
      } catch (e) { html += err(e); }
    }
    html += `<h2>New application</h2><p class="muted">Self-service registration for provider, payer, IDRE and auditor organizations.</p>
      <p><a class="button" href="#/onboarding/new">Start application</a></p>`;
    return html;
  }

  function onboardingNew() {
    afterRender(() => $("#ob").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = Object.fromEntries(new FormData(ev.target));
      const payload = {};
      f.requirements.split(",").map((s) => s.trim()).filter(Boolean).forEach((k) => (payload[k] = "provided"));
      try {
        await Api.onboarding.submit({
          type: f.type, legal_name: f.legal_name, ein: f.ein, npi: f.npi || undefined,
          payload: { ...payload, contact_email: f.contact_email },
        });
        UI.toast("Application submitted for verification");
        location.hash = "#/onboarding";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }));
    return `<h1>Organization application</h1><form id="ob" class="form">
      <label>Organization type <select name="type">
        <option value="PROVIDER_ORG">Provider / facility organization</option>
        <option value="PAYER_ORG">Payer / health plan organization</option>
        <option value="IDRE_ENTITY">IDRE entity (dispute resolution)</option>
        <option value="STATE_AUDITOR_ORG">State auditor organization</option>
      </select></label>
      <label>Legal name <input name="legal_name" required /></label>
      <label>EIN <input name="ein" placeholder="12-3456789" required /></label>
      <label>NPI (providers) <input name="npi" /></label>
      <label>Contact email <input name="contact_email" type="email" required /></label>
      <label>Documents provided (comma-separated keys, e.g. w9, npi, fee_schedule) <input name="requirements" placeholder="w9, npi" /></label>
      <button>Submit for verification</button></form>`;
  }

  async function decide(id, decision) {
    let reason = "";
    if (decision === "REJECT") {
      const v = await UI.modal({ title: "Reject application", danger: true, submitLabel: "Reject",
        fields: [{ name: "reason", label: "Rejection reason", type: "textarea", required: true,
          hint: "Sent to the applicant and recorded in the audit trail." }] });
      if (!v) return;
      reason = v.reason;
    } else if (!(await UI.confirm("Approve application?", "The organization is provisioned into its tenant and notified.", "Approve"))) return;
    try { await Api.onboarding.decide(id, decision, reason); UI.toast(`Application ${decision.toLowerCase()}d`); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- Voice console -------------------------------------------------------------
  async function voice() {
    try {
      const [intake, logs] = await Promise.all([Api.voice.intake(), Api.voice.logs()]);
      afterRender(() => $("#ob-call")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = Object.fromEntries(new FormData(ev.target));
        await UI.run(ev.target.querySelector("button"), async () => {
          try {
            const r = await Api.voice.outbound(f.to, f.case_number, f.script);
            UI.toast(`Outbound call ${r.status}`);
            App.rerender();
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        }, "Dialing…");
      }));
      return `<h1>Voice console</h1>
        <h2>Outbound call</h2>
        <form id="ob-call" class="form">
          <label>Phone number <input name="to" placeholder="+1…" required /></label>
          <label>Case number <input name="case_number" placeholder="CMS-TX-2026-00001" /></label>
          <label>Script <select name="script">
            <option value="window_closing">Offer window closing reminder</option>
            <option value="determination_issued">Determination issued</option>
            <option value="fee_reminder">Fee payment reminder</option>
            <option value="general">General update</option>
          </select></label>
          <button>Place outbound call</button>
          <p class="muted">Requires outbound calling enabled in the tenant voice config.</p>
        </form>
        <h2>Intake requests (${intake.length})</h2>` +
        (intake.length ? `<table><thead><tr><th>Caller</th><th>Organization</th><th>Summary</th><th>Status</th><th>At</th></tr></thead><tbody>` +
          intake.map((v) => `<tr><td>${esc(v.caller_name)}<br/><span class="muted">${esc(v.caller_phone)}</span></td>
            <td>${esc(v.organization)}</td><td>${esc(v.summary)}</td><td>${badge(v.status)}</td><td>${fmtDate(v.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No intake requests.</p>`) +
        `<h2>Call log</h2>` +
        (logs.length ? `<table><thead><tr><th>Direction</th><th>Tool/event</th><th>Case</th><th>Status</th><th>At</th></tr></thead><tbody>` +
          logs.map((l) => `<tr><td>${esc(l.direction)}</td><td>${esc(l.tool)}</td><td>${esc(l.case_number)}</td>
            <td>${badge(l.status)}</td><td>${fmtDate(l.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No calls yet.</p>`);
    } catch (e) { return `<h1>Voice console</h1>` + err(e); }
  }

  // ---- Reports ---------------------------------------------------------------------
  async function reports() {
    try {
      const [sla, summary] = await Promise.all([Api.reports.sla(), Api.reports.summary()]);
      const geoLink = (window.IDRE_CONFIG.geoMapUrl || "")
        ? `<p><a class="button" href="${window.IDRE_CONFIG.geoMapUrl}" target="_blank">Geospatial audit map (GeoLibre) ↗</a>
           <span class="muted"> jurisdiction checks + coverage gaps from the lakehouse gold zone</span></p>` : "";
      return `<h1>Compliance reports</h1>${geoLink}<h2>Case status rollup</h2>` +
        `<table><thead><tr><th>Status</th><th>Count</th><th>Avg QPA</th></tr></thead><tbody>` +
        summary.map((s) => `<tr><td>${badge(s.status)}</td><td>${s.count}</td><td>$${s.avg_qpa_usd.toFixed(0)}</td></tr>`).join("") +
        `</tbody></table><h2>Statutory SLA breaches (${sla.length})</h2>` +
        (sla.length ? `<table><thead><tr><th>Case</th><th>Clock</th><th>Detail</th><th>At</th></tr></thead><tbody>` +
          sla.map((b) => `<tr><td class="mono">${esc(b.case_id)}</td><td>${badge(b.clock)}</td><td>${esc(b.detail)}</td><td>${fmtDate(b.at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No breaches recorded.</p>`);
    } catch (e) { return `<h1>Compliance reports</h1>` + err(e); }
  }

  // ---- Ask the graph (EPR-KGQA) ------------------------------------------------
  function askGraph() {
    const html = `<div class="view-head"><h1>Ask the dispute graph</h1></div>
      <p class="muted">Natural-language questions over this state's dispute graph — entity linking, path retrieval,
      GraphSAGE ranking, and a local LLM answer. Every claim cites its graph path; nothing leaves the platform.</p>
      <form id="kgqa" class="kgqa-bar">
        <input name="q" required minlength="6" autocomplete="off"
          placeholder="e.g. Which disputes share a payer with case IDR-2026-0004?" aria-label="Ask the dispute graph" />
        <button>Ask</button>
      </form>
      <div id="kgqa-out" aria-live="polite"></div>
      <h3>Try asking</h3>
      <div class="kgqa-hints">
        ${["Which cases are most related to IDR-2026-0004?",
           "What connects Lone Star Imaging and BlueShield of Texas?",
           "Which disputes involve air ambulance service lines?",
           "Show cases related to IDR-2026-0002 and why"].map((q) =>
          `<button class="kgqa-hint" data-q="${esc(q)}">${esc(q)}</button>`).join("")}
      </div>`;
    afterRender(() => {
      const out = $("#kgqa-out");
      const run = async (q) => {
        out.innerHTML = `<p class="muted">Linking entities, retrieving paths, ranking, generating…</p>`;
        try {
          const r = await Api.graph.ask(q, 5);
          Palette.remember("kgqa", r.log_id, q.slice(0, 60));
          out.innerHTML = `<div class="kgqa-answer">
              <div class="kgqa-head"><b>Answer</b>
                <span class="muted">${esc(r.generator)} · ${r.latency_ms} ms · log <span class="mono">${esc(r.log_id.slice(0, 8))}</span></span>
                <span class="kgqa-fb">
                  <button class="mini" data-fb="1" title="Helpful">▲ helpful</button>
                  <button class="mini" data-fb="0" title="Not helpful">▽</button>
                </span></div>
              <p>${esc(r.answer).replace(/\n/g, "<br/>")}</p></div>
            ${(r.entities || []).length ? `<h3>Entities linked</h3><div class="kgqa-ents">` +
              r.entities.map((e) => `<span class="kgqa-ent">${e.kind === "Case"
                ? `<a href="#/cases/${esc(e.id)}">${esc(e.label)}</a>` : esc(e.label)}
                <span class="muted">${esc(e.kind)}${e.detail ? " · " + esc(e.detail) : ""}</span></span>`).join("") + `</div>` : ""}
            ${(r.citations || []).length ? `<h3>Evidence paths <span class="muted">· every claim traces to one of these</span></h3>
              <ol class="kgqa-paths">` + r.citations.map((c) => `<li class="mono">${esc(c.path)}</li>`).join("") + `</ol>` : ""}
            ${(r.gnn_ranked || []).length ? `<h3>GNN-ranked related cases</h3><table><tbody>` +
              r.gnn_ranked.map((g) => `<tr><td class="mono"><a href="#/cases/${esc(g.case_id)}">${esc(g.case_id.slice(0, 8))}…</a></td>
                <td><span class="gnn-score" style="--s:${g.score}">${(g.score * 100).toFixed(0)}%</span></td></tr>`).join("") +
              `</tbody></table>` : ""}`;
          out.querySelectorAll("[data-fb]").forEach((b) => b.addEventListener("click", async () => {
            try {
              const fb = await Api.graph.feedback(r.log_id, Number(b.dataset.fb));
              UI.toast(b.dataset.fb === "1"
                ? `Thanks — ${fb.edges_reinforced || 0} evidence edge(s) reinforced for the next GNN round`
                : "Thanks — recorded on the interaction log");
            } catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }));
        } catch (e) {
          out.innerHTML = `<p class="error">${esc(e.message)}</p>
            <p class="muted">Is graph-intel running? Admins: trigger a sync first (⌘K → “Sync dispute graph”).</p>`;
        }
      };
      $("#kgqa").addEventListener("submit", (ev) => { ev.preventDefault(); run(ev.target.q.value.trim()); });
      document.querySelectorAll(".kgqa-hint").forEach((b) =>
        b.addEventListener("click", () => { $("#kgqa").q.value = b.dataset.q; run(b.dataset.q); }));
    });
    return html;
  }

  // ---- Program rules UI (G1–G15 surfaces) ---------------------------------

  async function programPanel(id) {
    const prog = await Api.program.get().catch(() => null);
    if (!prog || !prog.config) return ""; // federal NSA tenants: nothing extra
    const cfg = prog.config;
    const c = await Api.cases.get(id).catch(() => ({}));
    const det = c.details || {};
    const facts = [];
    if (det.filing_party_type) facts.push(`<span class="chip">Filed by: ${det.filing_party_type === "HEALTH_PLAN" ? "Health plan" : "Provider"}</span>`);
    if (det.packet_complete_at) facts.push(`<span class="chip">Packet complete: ${fmtDate(det.packet_complete_at)} (10-day review clock basis)</span>`);
    if (det.volume_large) {
      const vv = det.volume_violations || [];
      facts.push(vv.length
        ? `<span class="badge warn" title="${esc(vv.join("; "))}">⚠ Large-volume policy: ${vv.length} violation(s) — ineligible per Capitol Bridge policy, resubmission permitted</span>`
        : `<span class="chip">Large-volume dispute — policy compliant</span>`);
    }
    let html = `<h2>Program — ${esc(prog.program || "state rules")}</h2>
      ${facts.length ? `<p class="fact-row">${facts.join(" ")}</p>` : ""}
      <details class="prog-sec" open><summary>Dual status</summary>
      <form id="p-status" class="inline-form">
        <select name="internal"><option value="">internal status…</option>
          ${(cfg.statuses?.internal || []).map((s) => `<option>${esc(s)}</option>`).join("")}</select>
        <select name="agency"><option value="">agency status…</option>
          ${(cfg.statuses?.agency || []).map((s) => `<option>${esc(s)}</option>`).join("")}</select>
        <button>Update status</button></form></details>

      <details class="prog-sec"><summary>Program dates (clock bases)</summary>
      <form id="p-date" class="inline-form">
        <select name="key">${(cfg.clocks || []).map((cl) => `<option value="${esc(cl.basis)}">${esc(cl.basis)} (${esc(cl.label)})</option>`).join("")}</select>
        <input type="date" name="value" required /><button>Record date</button></form></details>

      <details class="prog-sec"><summary>Eligibility review</summary>
      <form id="p-elig" class="inline-form">
        <input name="provider_type" placeholder="provider type (e.g. hospital_inpatient)" required />
        <label>contracted <input type="checkbox" name="contracted" /></label>
        <input name="amount" type="number" step="0.01" placeholder="disputed $" required />
        <input name="fd" type="date" title="final determination date" />
        <input name="flags" placeholder="flags (comma-separated reason codes)" />
        <button>Compute eligibility</button></form><div id="p-elig-out"></div></details>

      <details class="prog-sec"><summary>Correspondence</summary>
      <form id="p-send" class="inline-form">
        <select name="template">${(cfg.correspondence?.templates || []).map((tp) =>
          `<option value="${esc(tp.key)}">${esc(tp.key)}${tp.qa_role ? " (QA: " + esc(tp.qa_role) + ")" : ""}</option>`).join("")}</select>
        <input name="to" placeholder="to emails (comma-separated)" required />
        <input name="cc" placeholder="cc emails" />
        <textarea name="body" rows="3" placeholder="message body" required></textarea>
        <button>Send / submit for QA</button></form><div id="p-corr"></div></details>

      <details class="prog-sec"><summary>Invoices & claims</summary>
      <form id="p-inv" class="inline-form">
        <select name="party"><option>HEALTH_PLAN</option><option>PROVIDER</option></select>
        <select name="kind"><option>INITIAL_FEE</option><option>FULL_REVIEW</option><option>DEFAULT</option></select>
        <input name="amount" type="number" step="0.01" placeholder="amount $" required />
        <button>Issue invoice</button></form><div id="p-inv-list"></div>
      <form id="p-claims" class="inline-form">
        <textarea name="csv" rows="3" placeholder="claim_number,cpt,billed,paid per line (bulk import)"></textarea>
        <button>Import claims</button></form><div id="p-claims-list"></div></details>

      <details class="prog-sec"><summary>Share links & opt-out</summary>
      <div class="actions">
        <button id="p-share-up">Create upload link</button>
        <button id="p-share-dl">Create download link</button></div><div id="p-share-out"></div>
      <form id="p-optout" class="inline-form">
        <label>eligible to opt out <input type="checkbox" name="eligible" /></label>
        <input name="rationale" placeholder="rationale" required /><button>Record opt-out decision</button></form></details>`;

    afterRender(() => {
      const money = (v) => Math.round(parseFloat(v) * 100);
      $("#p-status")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.setStatus(id, { internal_status: f.internal.value, agency_status: f.agency.value });
          UI.toast("Status updated"); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      $("#p-date")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.setDate(id, f.key.value, f.value.value); UI.toast("Program date recorded — clocks re-projected"); App.rerender(); }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      $("#p-elig")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try {
          const r = await Api.program.eligibility(id, {
            provider_type: f.provider_type.value, contracted: f.contracted.checked,
            disputed_amount_cents: money(f.amount.value),
            final_determination_at: f.fd.value || "",
            flags: f.flags.value ? f.flags.value.split(",").map((x) => x.trim()).filter(Boolean) : [],
          });
          document.getElementById("p-elig-out").innerHTML =
            `<p>${badge(r.result)} ${r.reason ? esc(r.reason) : ""} <span class="muted">review ${esc(r.review_id)}</span></p>`;
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      $("#p-send")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        const split = (v) => v.split(",").map((x) => x.trim()).filter(Boolean);
        try {
          const r = await Api.program.send(id, { template: f.template.value, body: f.body.value, to: split(f.to.value), cc: split(f.cc.value) });
          UI.toast(r.status === "PENDING" ? "Draft submitted to QA gate" : "Sent — logged to correspondence");
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      Api.program.correspondence(id).then((r) => {
        const el = document.getElementById("p-corr"); if (!el) return;
        const rows = r.correspondence || [];
        el.innerHTML = rows.length ? `<table><tbody>` + rows.slice(0, 10).map((m) =>
          `<tr><td>${badge(m.direction)}</td><td>${esc(m.subject || "")}</td>
           <td class="muted">${esc(m.template || "")} · ${fmtDate(m.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : "";
      }).catch(() => {});
      $("#p-inv")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.issueInvoice(id, { party: f.party.value, kind: f.kind.value, amount_cents: money(f.amount.value) });
          UI.toast("Invoice issued (number = case number)"); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      Api.program.invoices(id).then((r) => {
        const el = document.getElementById("p-inv-list"); if (!el) return;
        el.innerHTML = (r.invoices || []).length ? `<table><tbody>` + r.invoices.map((v) =>
          `<tr><td class="mono">${esc(v.invoice_no)}</td><td>${esc(v.party)}</td><td>$${(v.amount_cents / 100).toFixed(2)}</td>
           <td>${badge(v.status)}</td>
           <td>${v.status === "OPEN" ? `<a href="javascript:void(0)" onclick="Views.payInvoice('${v.id}')">💳 pay by card</a> ·
             <a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','PAY',this)">mark paid</a> ·
             <a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','REFUND',this)">refund</a>` : ""}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No invoices on this case.</p>`;
      }).catch(() => {});
      $("#p-claims")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const lines = ev.target.csv.value.split("\n").map((l) => l.trim()).filter(Boolean).map((l) => {
          const [claim_number, cpt, billed, paid] = l.split(",");
          return { claim_number, cpt, billed_cents: money(billed || 0), paid_cents: money(paid || 0) };
        });
        try {
          const r = await Api.program.importClaims(id, lines);
          const vc = r.volume_check;
          if (vc && vc.large_volume && (vc.violations || []).length) {
            UI.toast(`⚠ ${r.imported} imported — large-volume policy violation(s): ${vc.violations.join("; ")}`, { kind: "warn", sticky: true });
          } else if (vc && vc.large_volume) {
            UI.toast(`${r.imported} claim lines imported — large-volume dispute, policy compliant`, { kind: "info" });
          } else {
            UI.toast(`${r.imported} claim lines imported`);
          }
          App.rerender();
        }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      const share = async (kind) => {
        try { const r = await Api.program.shareLink(id, kind, 7);
          document.getElementById("p-share-out").innerHTML =
            `<p class="mono">share link: ${esc(window.IDRE_CONFIG.apiBase)}${esc(r.path)}</p>`;
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      };
      document.getElementById("p-share-up")?.addEventListener("click", () => share("upload"));
      document.getElementById("p-share-dl")?.addEventListener("click", () => share("download"));
      $("#p-optout")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        try { await Api.program.optOut(id, ev.target.eligible.checked, ev.target.rationale.value);
          UI.toast("Opt-out decision recorded"); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
    });
    return html;
  }

  async function payInvoice(invId) {
    try {
      const r = await Api.program.checkout(invId);
      if (r.checkout_url) { location.href = r.checkout_url; return; } // Stripe-hosted checkout
      UI.toast("Checkout session created");
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function settleInvoice(invId, action, el) {
    let ref = "";
    if (action === "PAY") {
      const v = await UI.modal({ title: "Mark invoice paid", submitLabel: "Mark paid",
        fields: [{ name: "ref", label: "Remittance reference (RA/ERA)", placeholder: "Optional" }] });
      if (!v) return;
      ref = v.ref;
    } else if (!(await UI.confirm("Refund this invoice?", "A refund entry is posted to the ledger and the payer is notified.", "Refund", true))) return;
    await UI.run(el, async () => {
      try { await Api.program.settleInvoice(invId, action, ref); UI.toast(`Invoice ${action === "PAY" ? "paid" : "refunded"}`); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  async function qaQueue() {
    try {
      const r = await Api.program.qaQueue();
      const q = r.queue || [];
      return `<div class="view-head"><h1>QA gate</h1>
        <span class="muted">nothing reaches a party without approval on gated templates</span></div>` +
        (q.length ? `<table><thead><tr><th>Subject</th><th>Case</th><th>Drafted by</th><th></th></tr></thead><tbody>` +
          q.map((i) => `<tr><td>${esc(i.subject)}</td><td class="mono">${esc((i.case_id || "").slice(0, 8))}…</td>
            <td>${esc(i.drafted_by)}</td>
            <td><button class="mini" onclick="Views.qaReview('${i.id}')">review</button></td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">Queue empty — no drafts awaiting review.</p>`) + `<div id="qa-detail"></div>`;
    } catch (e) { return err(e); }
  }

  async function qaReview(qaId) {
    const box = $("#qa-detail");
    try {
      const d = await Api.program.qaGet(qaId);
      const to = (d.to_recipients || []).join(", ");
      box.innerHTML = `<div class="card"><h3>${esc(d.subject)}</h3>
        <p class="muted">to: ${esc(to)} · channel ${esc(d.channel)}</p>
        <pre class="qa-body">${esc(d.body)}</pre>
        <div class="actions">
          <button onclick="Views.qaDecide('${qaId}','APPROVE',this)">Approve & send</button>
          <button class="danger" onclick="Views.qaDecide('${qaId}','REJECT',this)">Reject</button></div></div>`;
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function qaDecide(qaId, decision, el) {
    let note = "";
    if (decision === "REJECT") {
      const v = await UI.modal({ title: "Reject draft", danger: true, submitLabel: "Reject",
        fields: [{ name: "note", label: "Rejection note", type: "textarea", required: true,
          hint: "Returned to the drafter with the draft." }] });
      if (!v) return;
      note = v.note;
    } else if (!(await UI.confirm("Approve and send?", "The email is delivered to all recipients now and logged to correspondence.", "Approve & send"))) return;
    await UI.run(el, async () => {
      try { await Api.program.qaDecision(qaId, decision, note);
          UI.toast(decision === "APPROVE" ? "Approved — sent and logged" : "Rejected"); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  // Intake statuses that end the lifecycle — no further advancement.
  // Terminal/advance sets come from the manifest's intake lifecycle when
  // configured (sector-agnostic); legacy FL/NSA set is the fallback.
  const LEGACY_INTAKE_TERMINAL = ["CONVERTED", "CLOSED_REFUNDED", "INELIGIBLE"];
  const intakeTerminalSet = () => {
    const st = (App.manifest && App.manifest.lifecycle && App.manifest.lifecycle.intake_statuses) || [];
    const t = st.filter((x) => x.terminal).map((x) => x.name);
    return t.length ? t : LEGACY_INTAKE_TERMINAL;
  };
  const intakeAdvanceOptions = (current) => {
    const st = (App.manifest && App.manifest.lifecycle && App.manifest.lifecycle.intake_statuses) || [];
    if (!st.length) return `<option value="">advance…</option><option>DOCS_RECEIVED</option>
                <option value="PACKET_COMPLETE">PACKET_COMPLETE (starts 10-day review)</option>
                <option>PAID</option><option>CONVERTED</option>
                <option>INELIGIBLE</option><option>CLOSED_REFUNDED</option>`;
    return `<option value="">advance…</option>` + st.filter((x) => x.name !== current)
      .map((x) => `<option value="${esc(x.name)}">${esc(x.name)} — ${esc(x.label)}</option>`).join("");
  };
  // Day-13 completeness gate (AHCA): docs not received within 13 days of
  // outreach => case found incomplete, ineligible letter issues. The backend
  // sweep enforces it; this countdown makes it visible before it bites.
  function day13Countdown(i) {
    if (i.packet_complete_at || intakeTerminalSet().includes(i.status) || !i.outreach_at) return "";
    const left = 13 - Math.floor((Date.now() - new Date(i.outreach_at)) / 864e5);
    if (left < 0) return `<span class="badge warn">past day 13</span>`;
    return `<span class="${left <= 3 ? "badge warn" : "muted"}">day 13 in ${left}d</span>`;
  }

  // Filing-party select labeled from the manifest's party codes/terminology
  // (POLICYHOLDER/INSURER for appraisal; PROVIDER/HEALTH_PLAN legacy default).
  function intakePartySelect() {
    const t = (App.manifest && App.manifest.terminology) || {};
    const a = t.party_a_code || "PROVIDER", b = t.party_b_code || "HEALTH_PLAN";
    const la = t.party_a || "Provider", lb = t.party_b || "Health Plan";
    return `<select name="filing_party_type" title="filing party">
      <option value="${esc(a)}">${esc(la)} files</option>
      <option value="${esc(b)}">${esc(lb)} files</option></select>`;
  }

  // Sector-specific intake fields declared by the manifest (phase 2) —
  // rendered dynamically; the API enforces required + stores in details.
  function intakeExtraFields() {
    const fields = (App.manifest && App.manifest.intake_fields) || [];
    return fields.map((f) => {
      const req = f.required ? "required" : "";
      if (f.type === "select")
        return `<select name="fld_${esc(f.name)}" title="${esc(f.label)}" ${req}>
          <option value="">${esc(f.label)}…</option>
          ${f.options.map((o) => `<option>${esc(o)}</option>`).join("")}</select>`;
      return `<input name="fld_${esc(f.name)}" type="${f.type === "number" ? "number" : f.type === "date" ? "date" : f.type === "email" ? "email" : "text"}"
        placeholder="${esc(f.label)}${f.required ? " *" : ""}" title="${esc(f.label)}" ${req} />`;
    }).join("");
  }

  async function intake() {
    try {
      const r = await Api.program.intake();
      const rows = r.intake || [];
      return `<div class="view-head"><h1>Pre-case intake</h1>
        <span class="muted">instruction requests awaiting documents and fees — the 10-day initial review starts at PACKET_COMPLETE</span></div>
        <form class="inline-form" onsubmit="return Views.newIntake(this)">
          <input name="email" type="email" placeholder="requester email" required />
          <input name="contact_name" placeholder="contact" /><input name="org" placeholder="organization" />
          ${intakePartySelect()}
          ${intakeExtraFields()}
          <button>New ${esc(App.t("intake_noun").toLowerCase())}</button></form>` +
        (rows.length ? `<table><thead><tr><th>Email</th><th>Org</th><th>Filing party</th><th>Status</th><th>Outreach</th><th>Packet complete</th><th></th></tr></thead><tbody>` +
          rows.map((i) => `<tr><td>${esc(i.email)}</td><td>${esc(i.org || "")}</td>
            <td>${i.filing_party_type === "HEALTH_PLAN" ? badge("HEALTH_PLAN") : `<span class="muted">Provider</span>`}</td>
            <td>${badge(i.status)} ${day13Countdown(i)}</td>
            <td class="muted">${fmtDate(i.outreach_at)}</td>
            <td class="muted">${i.packet_complete_at ? fmtDate(i.packet_complete_at) : "—"}</td>
            <td>${!intakeTerminalSet().includes(i.status) ?
              `<select onchange="Views.advanceIntake('${i.id}', this.value, this)">
                ${intakeAdvanceOptions(i.status)}</select>` : ""}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No intake requests.</p>`);
    } catch (e) { return err(e); }
  }

  async function newIntake(form) {
    try {
      const fields = {};
      ((App.manifest && App.manifest.intake_fields) || []).forEach((f) => {
        const el = form.elements["fld_" + f.name];
        if (el && el.value !== "") fields[f.name] = el.value;
      });
      await Api.program.createIntake({ email: form.email.value, contact_name: form.contact_name.value, org: form.org.value, filing_party_type: form.filing_party_type.value, fields });
      UI.toast(`${App.t("intake_noun")} opened — submission instructions queued`); App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    return false;
  }

  async function advanceIntake(id, status, sel) {
    if (!status) return;
    let caseId = "";
    if (status === "PACKET_COMPLETE") {
      const ok = await UI.modal({ title: "Mark packet complete", submitLabel: "Start the 10-day clock",
        body: "The 10-day initial-review clock starts NOW (complete-packet receipt). This first mark is permanent — it cannot be restarted." });
      if (ok === null) { if (sel) sel.value = ""; return; }
    }
    if (status === "INELIGIBLE") {
      const ok = await UI.modal({ title: "Find intake ineligible", submitLabel: "Mark ineligible", danger: true,
        body: "The party may resubmit once the ineligibility reason is cured. Staff must issue the ineligibility letter." });
      if (ok === null) { if (sel) sel.value = ""; return; }
    }
    if (status === "CONVERTED") {
      const v = await UI.modal({ title: "Convert intake to case", submitLabel: "Link case",
        fields: [{ name: "case_id", label: "Case ID to link", required: true, placeholder: "UUID" }] });
      if (!v) { if (sel) sel.value = ""; return; }
      caseId = v.case_id;
    }
    await UI.run(sel, async () => {
      try { await Api.program.advanceIntake(id, status, caseId); UI.toast(`Intake → ${status}`); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); if (sel) sel.value = ""; }
    });
  }

  async function deliverables() {
    try {
      const r = await Api.program.deliverables();
      const d = r.deliverables || [];
      return `<div class="view-head"><h1>Contract deliverables</h1>
        <span class="muted">program report schedule</span></div>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.requestDeliverable(new FormData(event.target), event.target.querySelector('button'))">
          <input name="name" required placeholder="Ad hoc report name" />
          <input name="ref" placeholder="Contract ref (optional)" size="10" />
          <button class="mini">request ad hoc (due +10 business days)</button></form>` +
        (d.length ? `<table><thead><tr><th>Deliverable</th><th>Rule</th><th>Next due</th><th></th></tr></thead><tbody>` +
          d.map((x) => `<tr><td>${esc(x.name)}</td><td class="mono">${esc(x.due_rule)}</td><td>${esc(x.next_due)}</td>
            <td><button class="mini" onclick="Views.submitDeliverable('${esc(x.name)}', this)">mark delivered</button></td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No deliverables configured for this program.</p>`) +
        ((r.history || []).length ? `<h2>Delivery history</h2><table><tbody>` +
          r.history.map((h) => `<tr><td>${esc(h.name)}</td><td>${badge(h.status)}</td>
            <td class="muted">${fmtDate(h.delivered_at)}</td></tr>`).join("") + `</tbody></table>` : "");
    } catch (e) { return err(e); }
  }

  async function requestDeliverable(f, btn) {
    await UI.run(btn, async () => {
      try { const r = await Api.program.requestDeliverable(f.get("name"), f.get("ref"));
        UI.toast("Ad hoc report requested — due " + r.due_date); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Requesting…");
  }

  async function submitDeliverable(name, el) {
    await UI.run(el, async () => {
      try { await Api.program.submitDeliverable({ name }); UI.toast("Deliverable recorded"); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  // ---- Financial dashboard (all money movement through the platform) -------

  async function finance() {
    try {
      const [fin, pays] = await Promise.all([
        Api.program.financial(), Api.program.payments().catch(() => ({ payments: [] })),
      ]);
      const k = fin.kpi || {};
      const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
      let html = `<div class="view-head"><h1>Financials</h1>
        <span class="muted">every payment and transaction through the platform · tenant <b>${esc(Api.getTenant()).toUpperCase()}</b>
        ${fin.stripe_enabled ? ' · <span class="badge s-paid">stripe live</span>' : ' · <span class="badge">stripe not configured</span>'}</span></div>
        <div class="kpi-row">
          <div class="kpi"><span class="kpi-n">${usd(k.collected_cents)}</span><span class="kpi-l">Collected (all time)</span></div>
          <div class="kpi"><span class="kpi-n">${usd(k.collected_30d_cents)}</span><span class="kpi-l">Collected (30 days)</span></div>
          <div class="kpi"><span class="kpi-n">${usd(k.refunded_cents)}</span><span class="kpi-l">Refunded</span></div>
          <div class="kpi"><span class="kpi-n">${esc(String(k.payments_count ?? 0))}</span><span class="kpi-l">Payments settled</span></div>
        </div>`;

      // Receivables + aging
      const rec = fin.receivables || [];
      if (rec.length)
        html += `<h2>Receivables by status</h2><table><thead><tr><th>Status</th><th>Party</th><th>#</th><th>Total</th></tr></thead><tbody>` +
          rec.map((x) => `<tr><td>${badge(x.status)}</td><td>${esc(x.party)}</td><td>${x.n}</td><td>${usd(x.total_cents)}</td></tr>`).join("") +
          `</tbody></table>`;
      const aging = fin.aging || [];
      if (aging.length)
        html += `<h2>A/R aging (open invoices)</h2><div class="aging-row">` +
          ["current", "1-30", "31-60", "60+"].map((b) => {
            const row = aging.find((a) => a.bucket === b);
            return `<div class="aging-cell ${b === "60+" ? "bad" : b === "current" ? "" : "warn"}">
              <span class="kpi-n">${row ? usd(row.total_cents) : "$0"}</span>
              <span class="kpi-l">${b === "current" ? "Current" : b + " days past due"}${row ? ` · ${row.n} inv` : ""}</span></div>`;
          }).join("") + `</div>`;

      // Payment rails
      const bm = fin.by_method || [];
      if (bm.length)
        html += `<h2>Payment rails</h2><table><thead><tr><th>Provider</th><th>Status</th><th>#</th><th>Total</th></tr></thead><tbody>` +
          bm.map((x) => `<tr><td>${esc(x.provider)}</td><td>${badge(x.status)}</td><td>${x.n}</td><td>${usd(x.total_cents)}</td></tr>`).join("") +
          `</tbody></table>`;

      // Card payments
      const plist = pays.payments || [];
      if (plist.length)
        html += `<h2>Card payments</h2><table><thead><tr><th>Case</th><th>Payer</th><th>Amount</th><th>Status</th><th>Stripe ref</th><th>When</th></tr></thead><tbody>` +
          plist.slice(0, 25).map((p) => `<tr><td class="mono">${esc((p.case_id || "").slice(0, 8))}…</td>
            <td>${esc(p.payer_email || "—")}</td><td>${usd(p.amount_cents)}</td><td>${badge(p.status)}</td>
            <td class="mono">${esc(p.payment_intent || p.session_id || "")}</td>
            <td class="muted">${fmtDate(p.created_at)}</td></tr>`).join("") + `</tbody></table>`;

      // Unified event stream
      const ev = fin.events || [];
      html += `<h2>Transaction stream</h2>` +
        (ev.length ? `<table><thead><tr><th>Event</th><th>Dir</th><th>Amount</th><th>Party</th><th>Reference</th><th>Actor</th><th>When</th></tr></thead><tbody>` +
          ev.map((e) => `<tr><td>${badge(e.kind)}</td>
            <td>${e.direction === "IN" ? "↓ in" : e.direction === "OUT" ? "↑ out" : "—"}</td>
            <td>${usd(e.amount_cents)}</td><td>${esc(e.party || "—")}</td>
            <td class="mono">${esc(e.ref || "")}</td><td>${esc(e.actor || "")}</td>
            <td class="muted">${fmtDate(e.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No financial events yet — issue an invoice to start the stream.</p>`);
      return html;
    } catch (e) { return err(e); }
  }

  // ---- Rules admin (FEDERAL_ADMIN / PLATFORM_ADMIN; every save audited) -----
  let rulesDraft = null; // working copy; saved as one unit so the audit diff is meaningful

  const condText = (c) => `${esc(c.field)} ${esc(c.op)} ${c.value === undefined ? "" : esc(JSON.stringify(c.value))}`;
  const actText = (a) => `${esc(a.type)} ${Object.entries(a.params || {}).map(([k, v]) => `${esc(k)}=${esc(String(v))}`).join(" ")}`;

  // Manifest summary card: the program's sector-agnostic definition
  // (terminology, lifecycle, clocks) with admin edit + audit. 404 = legacy tenant.
  function manifestCard(m) {
    if (!m) return `<div class="rule-card"><div class="rule-head"><b>Program manifest</b> ${badge("legacy")}</div>
      <p class="muted">No manifest configured — this tenant runs the built-in lifecycle and terminology.
      Install one via a sector pack or paste JSON below.</p>
      <button class="mini" onclick="Views.manifestEdit()">Install manifest</button></div>`;
    const t = m.terminology || {}, lc = m.lifecycle || {};
    const stageList = (stages) => (stages || []).map((s) =>
      `<span class="chip">${esc(s.label)}${s.terminal ? " · terminal" : ""}${s.completed ? " · completed" : ""}</span>`).join(" ");
    return `<div class="rule-card"><div class="rule-head"><b>Program manifest</b> ${badge(m.sector || "program")}
      <span class="muted mono">${esc(m.program)}@${esc(m.version)}</span><span style="flex:1"></span>
      <button class="mini" onclick="Views.manifestEdit()">edit manifest</button></div>
      <p class="fact-row">${["case_noun", "party_a", "party_b", "neutral", "intake_noun"].map((k) =>
        `<span class="chip">${k.replace(/_/g, " ")}: <b>${esc(t[k] || "—")}</b></span>`).join("")}</p>
      <div class="rule-cols"><div><h4>Intake lifecycle</h4><p>${stageList(lc.intake_statuses)}</p></div>
      <div><h4>${esc(t.case_noun || "Case")} lifecycle</h4><p>${stageList(lc.case_statuses)}</p></div></div>
      ${(m.clocks || []).length ? `<h4>Statutory clocks</h4><p>${m.clocks.map((c) =>
        `<span class="chip">${esc(c.name)}: ${c.days} ${esc(c.day_type)} days from ${esc(c.basis)}</span>`).join(" ")}</p>` : ""}
      <p class="fact-row">
        ${m.determination && m.determination.engine ? `<span class="chip">valuation engine: <b>${esc(m.determination.engine)}</b></span>` : ""}
        ${m.roster && (m.roster.certifications || []).length ? `<span class="chip">${esc(m.neutral_label || (m.terminology||{}).neutral || "Neutral")} roster: ${m.roster.certifications.map(esc).join(", ")}${m.roster.min_cases ? `, ${m.roster.min_cases}+ cases` : ""}</span>` : ""}
        ${m.letters && (m.letters.templates || []).length ? `<span class="chip">letter pack: ${(m.letters.templates).map(esc).join(", ")}</span>` : ""}
        ${m.intake_fields && m.intake_fields.length ? `<span class="chip">${m.intake_fields.length} sector intake field(s)</span>` : ""}
      </p>
    </div>`;
  }

  async function manifestEdit() {
    const cur = App.manifest ? JSON.stringify(App.manifest, null, 2) : "";
    const v = await UI.modal({ title: "Program manifest", submitLabel: "Validate & save", wide: true,
      fields: [
        { name: "json", label: "Manifest (JSON)", type: "textarea", required: true, value: cur,
          hint: "terminology · lifecycle (intake/case statuses, terminal flags) · clocks · intake_fields · documents.schemas · determination.engine · letters · roster — validated before save; every save is audited" },
        { name: "note", label: "Change note (audit trail)", required: true, placeholder: "e.g. Install TX auto appraisal pack v1.0.2026" }] });
    if (v === null) return;
    let parsed;
    try { parsed = JSON.parse(v.json); } catch (e) { UI.toast(`Invalid JSON: ${e.message}`, { kind: "error", sticky: true }); return; }
    try {
      const r = await Api.program.saveManifest(parsed, v.note);
      await App.loadManifest();
      UI.toast(`Manifest ${r.program}@${r.version} installed — change recorded in the audit trail`);
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  async function rulesAdmin() {
    try {
      const [lr, ar] = await Promise.all([Api.program.rules(), Api.program.rulesAudit().catch(() => ({ changes: [] }))]);
      rulesDraft = (lr.rules || []).map((r) => ({ ...r }));
      afterRender(bindRulesAdmin);
      const audit = ar.changes || [];
      const manifestHtml = manifestCard(App.manifest);
      return `<div class="view-head"><h1>Program rules</h1>
        <span class="muted">live policy — changes take effect immediately and are permanently audited</span></div>
        <p><button id="rule-add">＋ New rule</button>
           <button id="rules-save" class="btn-primary">Save all changes</button></p>` +
        manifestHtml +
        `<div id="rules-list">` +
        (rulesDraft.length ? rulesDraft.map((r, i) => `
          <div class="rule-card ${r.enabled === false ? "rule-off" : ""}">
            <div class="rule-head"><b>${esc(r.name || "(unnamed)")}</b> ${badge(r.event)}
              <label class="rule-toggle"><input type="checkbox" data-idx="${i}" class="rule-en" ${r.enabled === false ? "" : "checked"} /> enabled</label>
              <span style="flex:1"></span>
              <button class="mini" onclick="Views.ruleEdit(${i})">edit</button>
              <button class="mini danger" onclick="Views.ruleDelete(${i})">delete</button></div>
            ${r._basis ? `<p class="muted rule-basis">${esc(r._basis)}</p>` : ""}
            <div class="rule-cols"><div><h4>When (all must hold)</h4><ul>${
              (r.conditions || []).map((c) => `<li class="mono">${condText(c)}</li>`).join("") || "<li class='muted'>always</li>"}</ul></div>
            <div><h4>Then</h4><ul>${
              (r.actions || []).map((a) => `<li class="mono">${actText(a)}</li>`).join("")}</ul></div></div>
          </div>`).join("") : `<p class="muted">No rules configured — hardcoded defaults apply.</p>`) +
        `</div>
        <h2>Change history <span class="muted">append-only audit trail</span></h2>` +
        (audit.length ? `<table><thead><tr><th>When</th><th>Changed by</th><th>Note</th><th>Rules</th><th></th></tr></thead><tbody>` +
          audit.map((c) => `<tr><td class="muted">${fmtDate(c.changed_at)}</td><td class="mono">${esc(c.changed_by.slice(0, 12))}…</td>
            <td>${esc(c.note || "—")}</td><td>${(c.before || []).length} → ${(c.after || []).length}</td>
            <td><details><summary class="mini">diff</summary><pre class="rule-diff">${esc(JSON.stringify(c.after, null, 1))}</pre></details></td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No changes recorded yet.</p>`);
    } catch (e) { return err(e); }
  }

  function ruleEdit(idx) {
    const r = rulesDraft[idx];
    UI.modal({
      title: `Edit rule: ${r.name || ""}`, submitLabel: "Apply", wide: true,
      fields: [{ name: "json", label: "Rule definition (JSON)", type: "textarea", required: true,
        value: JSON.stringify(r, null, 2),
        hint: "events: intake.advance | doc.upload | doc.analyzed | claims.imported | invoice.settled | sweep.intake · ops: eq neq in contains gt gte lt lte is_null not_null days_older_than · actions: set_status set_detail notify log_activity block_request flag_review" }],
    }).then((v) => {
      if (v === null) return;
      try {
        const parsed = JSON.parse(v.json);
        if (!parsed.name || !parsed.event) throw new Error("rule needs name + event");
        rulesDraft[idx] = parsed;
        UI.toast(`Rule "${parsed.name}" staged — Save all changes to apply`);
        App.rerender();
      } catch (e) { UI.toast(`Invalid rule JSON: ${e.message}`, { kind: "error" }); }
    });
  }

  function ruleDelete(idx) {
    UI.modal({ title: `Delete rule "${rulesDraft[idx].name}"?`, danger: true, submitLabel: "Delete",
      body: "The deletion is staged until you Save all changes; the audit trail records it permanently." })
      .then((ok) => { if (ok === null) return; rulesDraft.splice(idx, 1); UI.toast("Rule staged for deletion"); App.rerender(); });
  }

  async function rulesSave() {
    const v = await UI.modal({ title: "Save program rules", submitLabel: "Save & audit", fields: [
      { name: "note", label: "Change note (audit trail)", required: true, placeholder: "e.g. AHCA memo 2026-03: day-13 gate" }] });
    if (!v) return;
    try {
      const r = await Api.program.saveRules(rulesDraft, v.note);
      UI.toast(`${r.saved} rule(s) saved — change recorded in the audit trail`);
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  function bindRulesAdmin() {
    document.querySelectorAll(".rule-en").forEach((cb) => cb.addEventListener("change", () => {
      rulesDraft[+cb.dataset.idx].enabled = cb.checked;
      UI.flash(cb.closest(".rule-card"));
    }));
    document.getElementById("rule-add")?.addEventListener("click", () => {
      rulesDraft.push({ name: "new-rule", event: "intake.advance", enabled: false, conditions: [], actions: [] });
      App.rerender();
      ruleEdit(rulesDraft.length - 1);
    });
    document.getElementById("rules-save")?.addEventListener("click", () => UI.run(document.getElementById("rules-save"), rulesSave, "Saving…"));
  }

  return { dashboard, cases, caseDetail, newDispute, sortCases, onboarding, onboardingNew, decide, voice, reports, showAnalysis, check, assign, letter, saveCurrentView, escalate, relate, feeTransfer, peek, askGraph, settleInvoice, qaQueue, qaReview, qaDecide, intake, newIntake, advanceIntake, deliverables, submitDeliverable, requestDeliverable, finance, payInvoice, moveDoc, rulesAdmin, ruleEdit, ruleDelete, rulesSave, bindRulesAdmin, manifestEdit };
})();
