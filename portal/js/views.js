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
  const SORTABLE = new Set(["case_number", "status", "service_line", "qpa_cents", "opened_at", "sla"]);
  // Lever 2/3/4: triage lane chip, signed SLA badge, batch group headers.
  const laneChip = (lane) => lane === "AUTO_REVIEW" ? `<span class="chip lane-auto" title="Everything needed for a one-click confirm is present">AUTO</span>`
    : lane === "COMPLEX" ? `<span class="chip lane-complex" title="Batched, duplicated, escalated, or high-dollar">COMPLEX</span>`
    : `<span class="chip lane-std">STANDARD</span>`;
  const slaBadge = (days) => {
    if (days === undefined || days === null) return '<span class="muted">—</span>';
    const cls = days < 0 ? "sla breach" : days <= 3 ? "sla risk" : days <= 7 ? "sla watch" : "sla ok";
    const label = days < 0 ? `${-days}bd overdue` : days === 0 ? "due today" : `${days}bd left`;
    return `<span class="${cls}" title="Statutory determination clock (exact business days, tenant holidays)">${label}</span>`;
  };
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
  const caseTable = (rows, clocks = {}, sort = "", groupBatch = false) => {
    if (!rows.length) return `<p class="muted">No disputes.</p>`;
    const row = (c) => `<tr class="click" data-case="${c.id}">
        <td class="selcol"><input type="checkbox" class="sel-one" data-id="${c.id}" ${selection.has(c.id) ? "checked" : ""} aria-label="Select ${esc(c.case_number)}"></td>
        <td class="mono">${esc(c.case_number)}</td><td>${badge(c.status)}</td><td>${esc(c.service_line)}</td>
        <td class="num">$${((c.qpa_cents || 0) / 100).toLocaleString()}</td>
        <td>${laneChip(c.triage_lane)}</td>
        <td>${slaBadge(c.sla_days_remaining)}</td>
        <td>${clocks[c.id] && clocks[c.id].length ? clockChip(nearestClock(clocks[c.id])) : '<span class="muted">—</span>'}</td>
        <td class="muted">${fmtDate(c.opened_at)}</td>
        <td><button class="mini peek" data-id="${c.id}" title="Peek without losing your place">▸</button></td></tr>`;
    let body = "";
    if (groupBatch) {
      // Lever 3: batch-as-one — batched disputes collapse under a group header
      // so a 40-claim batch reads as ONE unit of work, not 40 queue rows.
      const groups = new Map();
      const singles = [];
      for (const c of rows) {
        if (c.batch_id) {
          if (!groups.has(c.batch_id)) groups.set(c.batch_id, []);
          groups.get(c.batch_id).push(c);
        } else singles.push(c);
      }
      for (const [bid, members] of groups) {
        const sum = members.reduce((a, c) => a + (c.qpa_cents || 0), 0);
        const worst = Math.min(...members.map((c) => (c.sla_days_remaining ?? 9999)));
        body += `<tr class="batch-head"><td colspan="10">▦ Batch ${esc(bid.slice(0, 8))} — ${members.length} ${esc(App.t("case_plural"))} ·
          $${(sum / 100).toLocaleString()} combined · ${slaBadge(worst)}
          <span class="muted">(review as one: same payer, same fact pattern)</span></td></tr>` +
          members.map((c) => row(c).replace('<tr class="click"', '<tr class="click batched"')).join("");
      }
      body += singles.map(row).join("");
    } else {
      body = rows.map(row).join("");
    }
    return `
    <div class="dg-wrap">
      <table class="dg"><thead><tr>
        <th class="selcol"><input type="checkbox" id="sel-all" aria-label="Select all"></th>
        ${[["case_number", "Case #"], ["status", "Status"], ["service_line", App.t("service_label")], ["qpa_cents", App.t("amount_label")]]
          .map(([c, l]) => sortableTh(c, l, sort)).join("")}
        <th>Lane</th>${sortableTh("sla", "SLA", sort)}
        <th>Statutory clock</th>${sortableTh("opened_at", "Opened", sort)}<th></th></tr></thead><tbody>` +
      body + `</tbody></table></div>`;
  };

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
      const lane = hq.get("lane") || "";
      const groupBatch = hq.get("batch") === "1";
      const saved = await Api.cm.views().catch(() => []);
      const sv = saved.find((s) => s.id === v);
      const status = sv && sv.filters && sv.filters.status ? sv.filters.status : "";
      const reqParams = { limit: PAGE_SIZE, ...(status ? { status } : {}), ...(sort ? { sort } : {}), ...(lane ? { lane } : {}) };
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
        // Lever 2: lane tabs re-filter server-side; Lever 3: batch grouping
        // toggles batch-as-one rows. Both live in the hash = shareable.
        document.querySelectorAll(".lane-tab").forEach((b) => b.addEventListener("click", () => {
          const q = new URLSearchParams(location.hash.split("?")[1] || "");
          b.dataset.lane ? q.set("lane", b.dataset.lane) : q.delete("lane");
          const s = q.toString();
          location.hash = "#/cases" + (s ? "?" + s : "");
        }));
        $("#batch-tgl")?.addEventListener("click", () => {
          const q = new URLSearchParams(location.hash.split("?")[1] || "");
          groupBatch ? q.delete("batch") : q.set("batch", "1");
          const s = q.toString();
          location.hash = "#/cases" + (s ? "?" + s : "");
        });
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
              const more = { limit: PAGE_SIZE, ...(status ? { status } : {}), ...(sort ? { sort } : {}), ...(lane ? { lane } : {}) };
              if (cursor.startsWith("offset:")) more.offset = cursor.slice(7); else more.cursor = cursor;
              const next = await Api.cases.list(more);
              loaded = loaded.concat(next.cases);
              cursor = next.next_cursor;
              document.querySelector(".dg-wrap").outerHTML = caseTable(loaded, clocks, sort || "opened_at", groupBatch);
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
        <p class="lane-tabs" role="tablist" aria-label="Triage lanes">
          ${[["", "All"], ["AUTO_REVIEW", "Auto-review"], ["STANDARD", "Standard"], ["COMPLEX", "Complex"]]
            .map(([l, lab]) => `<button class="mini lane-tab${lane === l ? " active" : ""}" data-lane="${l}" role="tab" aria-selected="${lane === l}">${lab}</button>`).join("")}
          <button class="mini" id="batch-tgl" aria-pressed="${groupBatch}" title="Collapse batched disputes into one reviewable group">${groupBatch ? "▦ Batches: grouped" : "▦ Group batches"}</button>
        </p>
        <p class="viewbar">
          <select onchange="location.hash='#/cases?view='+this.value" aria-label="Saved views">
            <option value="">All disputes</option>${opts}</select>
          <button class="mini" onclick="Views.saveCurrentView()">Save current view</button>
          ${sv ? `<span class="muted">filter: status = ${esc(sv.filters.status)}</span>` : ""}</p>` +
        caseTable(loaded, clocks, sort || "opened_at", groupBatch) + `<div id="pg">${pager()}</div>`;
    } catch (e) { return `<h1>${esc(App.t("case_plural"))}</h1>` + err(e); }
  }

  // ---- Actions (all modal-based now) ----------------------------------------------
  // Copilot brief (Phase 1) — renders the latest grounded, advisory-only
  // brief plus the (re)generate action. The server returns 502 with facts
  // when the local model is unreachable; surface that via the catch path.
  function copilotCard(brief, generatedAt, caseId) {
    const genBtn = `<button class="mini" onclick="Views.copilotBrief('${caseId}', this)">↻ ${brief ? "Regenerate" : "Generate"} brief</button>`;
    if (!brief)
      return `<p class="muted">No brief generated yet. The copilot drafts a grounded eligibility brief, evidence comparison, and uncertainty list from platform-verified case facts only — it never changes case state.</p><p>${genBtn}</p>`;
    return `<pre class="copilot-brief" style="white-space:pre-wrap;font:inherit;line-height:1.45">${esc(brief)}</pre>
      <p class="muted">Generated ${generatedAt ? new Date(generatedAt).toLocaleString() : "—"} · advisory only, not a determination · audit-logged</p>
      <p>${genBtn}</p>`;
  }

  async function copilotBrief(caseId, btn) {
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.copilotBrief(caseId);
        const b = document.getElementById("copilot");
        if (b) b.innerHTML = copilotCard(r.brief, r.generated_at, caseId);
        UI.toast("Copilot brief generated (DRAFT — advisory only)");
      } catch (e) {
        const b = document.getElementById("copilot");
        if (e.data && e.data.facts) {
          // 502 fallback: model unreachable, server returned verified facts
          if (b) b.innerHTML = copilotCard(null, null, caseId) +
            `<p class="muted">Model unreachable — raw verified facts returned instead.</p>
             <pre style="white-space:pre-wrap;font:12px monospace">${esc(JSON.stringify(e.data.facts, null, 2))}</pre>`;
          UI.toast("Copilot model unreachable — showing verified facts only", { kind: "warn" });
        } else {
          if (b) b.innerHTML = copilotCard(null, null, caseId);
          UI.toast(e.message, { kind: "warn" });
        }
      }
    }, "Drafting brief…");
  }

  // Phase 2: copilot drafts enter the QA gate — accept/edit/reject there.
  async function copilotDraftQA(caseId, kind, btn) {
    await UI.run(btn, async () => {
      try {
        await Api.program.copilotDraft(caseId, kind);
        UI.toast(`${kind === "correspondence" ? "Correspondence" : "Determination rationale"} draft queued for QA review`);
        location.hash = "#/qa";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Drafting for QA…");
  }

  // Phase 3: bounded action batches — propose, list, approve/reject.
  function copilotBatchCard(caseId, b) {
    const acts = (b.actions || []).map((a) => {
      const param = a.params ? Object.values(a.params).filter((v) => typeof v === "string").join(" — ") : "";
      const outcome = a.result ? ` <span class="ok">✓ ${esc(a.result)}</span>`
        : a.error ? ` <span class="err">✗ ${esc(a.error)}</span>` : "";
      return `<li><b>${esc(a.type.replace(/_/g, " "))}</b>${param ? ` — ${esc(param)}` : ""}${outcome}</li>`;
    }).join("");
    const pending = b.status === "PENDING_APPROVAL";
    return `<div class="card"><p><b>Batch ${esc(b.batch_id.slice(0, 8))}…</b>
        <span class="badge ${pending ? "s-review" : b.status === "APPLIED" ? "s-ok" : "s-closed"}">${esc(b.status)}</span></p>
      ${b.rationale ? `<p class="muted">${esc(b.rationale)}</p>` : ""}
      <ul>${acts || "<li class='muted'>no actions proposed</li>"}</ul>
      <p class="muted">proposed by ${esc(b.proposed_by)} · model ${esc(b.model)}${b.decided_by ? ` · decided by ${esc(b.decided_by)}` : ""}</p>
      ${pending ? `<div class="actions">
        <button class="mini" onclick="Views.copilotDecideBatch('${caseId}','${b.batch_id}','APPROVE',this)">✓ Approve & execute</button>
        <button class="mini danger" onclick="Views.copilotDecideBatch('${caseId}','${b.batch_id}','REJECT',this)">✗ Reject</button></div>` : ""}
    </div>`;
  }

  async function copilotLoadBatches(caseId) {
    try {
      const r = await Api.program.copilotListActions(caseId);
      const box = document.getElementById("copilot-batches");
      if (!box) return;
      box.innerHTML = (r.batches || []).length
        ? `<h3>Action batches</h3>` + r.batches.map((b) => copilotBatchCard(caseId, b)).join("")
        : "";
    } catch { /* list is best-effort on load */ }
  }

  async function copilotPropose(caseId, btn) {
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.copilotProposeActions(caseId);
        UI.toast(`Copilot proposed ${(r.actions || []).length} action(s) — review and approve below`);
        copilotLoadBatches(caseId);
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Proposing actions…");
  }

  async function copilotDecideBatch(caseId, batchId, decision, btn) {
    if (decision === "APPROVE" &&
        !(await UI.confirm("Approve and execute?", "The listed actions run now via the workflow — each is applied and recorded on the case.", "Approve & execute"))) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.copilotDecideActions(caseId, batchId, decision);
        if (r.error) UI.toast(r.error, { kind: "warn", sticky: true });
        else UI.toast(decision === "APPROVE" ? "Approved — actions executing via workflow" : "Batch rejected");
        copilotLoadBatches(caseId);
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, decision === "APPROVE" ? "Executing…" : "Rejecting…");
  }

  // ---- Assistant (conversational surface) -----------------------------------
  // The per-case thread over the copilot primitives: grounded Q&A in plain
  // language, with the Phase 1-3 actions as chips — the thread advises, the
  // chips act, and every action still passes its human gate.
  async function assistant(caseId) {
    if (!can("CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Assistant</h1></div><p class="muted">Requires a case staff role.</p>`;
    if (!caseId) {
      // Case picker: most recent cases first — the thread always belongs to
      // one case, so grounding never drifts across records.
      try {
        const r = await Api.cases.list({ limit: 50 });
        const rows = (r.cases || []).map((c) =>
          `<tr class="click" onclick="location.hash='#/assistant/${c.id}'"><td class="mono">${esc(c.case_number)}</td>
           <td>${esc(c.service_line || "")}</td><td>${badge(c.status)}</td></tr>`).join("");
        return `<div class="view-head"><h1>Assistant</h1>
          <span class="muted">grounded on platform-verified case facts · advisory only · every turn is on the record</span></div>
          <p>Pick a case to open its thread:</p>
          <table><thead><tr><th>Case</th><th>Line</th><th>Status</th></tr></thead><tbody>${rows}</tbody></table>`;
      } catch (e) { return err(e); }
    }
    afterRender(async () => {
      const thread = document.getElementById("asst-thread");
      try {
        const c = await Api.cases.get(caseId);
        document.getElementById("asst-case").innerHTML =
          `Case <a href="#/cases/${caseId}">${esc(c.case_number)}</a> · ${esc(c.status)}`;
        const h = await Api.program.copilotChatHistory(caseId);
        thread.innerHTML = (h.turns || []).map(asstTurnHtml).join("") ||
          `<div class="muted" style="padding:12px">No turns yet — ask anything about this case, or use a chip below.</div>`;
        thread.scrollTop = thread.scrollHeight;
      } catch (e) {
        thread.innerHTML = `<div class="muted" style="padding:12px">${esc(e.message)}</div>`;
      }
      const form = document.getElementById("asst-form");
      form?.addEventListener("submit", (e) => {
        e.preventDefault();
        const input = form.message;
        const msg = input.value.trim();
        if (msg) { input.value = ""; assistantSend(caseId, msg); }
      });
    });
    return `<div class="view-head"><h1>Assistant</h1>
      <span class="muted" id="asst-case">loading case…</span></div>
      <div id="asst-thread" class="asst-thread"></div>
      <div class="asst-chips">
        <button class="mini" onclick="Views.assistantChip('${caseId}','brief',this)">▤ Brief me</button>
        <button class="mini" onclick="Views.assistantChip('${caseId}','determination_rationale',this)">✍ Draft rationale</button>
        <button class="mini" onclick="Views.assistantChip('${caseId}','correspondence',this)">✉ Draft correspondence</button>
        <button class="mini" onclick="Views.assistantChip('${caseId}','actions',this)">⚙ Propose actions</button>
      </div>
      <form id="asst-form" class="asst-form">
        <input name="message" autocomplete="off" placeholder="Ask about this case… (e.g. what's blocking eligibility?)" aria-label="Message the assistant" />
        <button>Send</button>
      </form>
      <p class="muted" style="margin-top:6px">Advisory only — the assistant cannot change case state; chips route through the same gates as the screens. Every turn is recorded.</p>`;
  }

  function asstTurnHtml(t) {
    const who = t.role === "user" ? "you" : `assistant${t.model ? ` · ${esc(t.model)}` : ""}`;
    return `<div class="asst-turn ${t.role === "user" ? "asst-user" : "asst-ai"}">
      <div class="asst-who">${who}</div>
      <div class="asst-body">${esc(t.body)}</div></div>`;
  }

  function asstAppend(caseId, role, body, model) {
    const thread = document.getElementById("asst-thread");
    if (!thread) return;
    thread.insertAdjacentHTML("beforeend", asstTurnHtml({ role, body, model }));
    thread.scrollTop = thread.scrollHeight;
  }

  async function assistantSend(caseId, msg) {
    asstAppend(caseId, "user", msg);
    asstAppend(caseId, "assistant", "…", "");
    try {
      const r = await Api.program.copilotChat(caseId, msg);
      const thread = document.getElementById("asst-thread");
      thread?.querySelector(".asst-turn:last-child")?.remove();
      asstAppend(caseId, "assistant", r.reply, r.model);
    } catch (e) {
      const thread = document.getElementById("asst-thread");
      thread?.querySelector(".asst-turn:last-child")?.remove();
      asstAppend(caseId, "assistant", `⚠ ${e.message}`, "");
    }
  }

  // Chips run the Phase 1-3 primitives and narrate the outcome into the
  // thread — same endpoints, same gates, conversational surface.
  async function assistantChip(caseId, kind, btn) {
    await UI.run(btn, async () => {
      try {
        if (kind === "brief") {
          asstAppend(caseId, "user", "Brief me on this case.");
          const r = await Api.program.copilotBrief(caseId);
          asstAppend(caseId, "assistant", r.brief, r.model || "");
        } else if (kind === "actions") {
          asstAppend(caseId, "user", "Propose an action batch.");
          const r = await Api.program.copilotProposeActions(caseId);
          const acts = (r.actions || []).map((a) => `• ${a.type.replace(/_/g, " ")}`).join("\n") || "• (none)";
          asstAppend(caseId, "assistant",
            `Proposed ${(r.actions || []).length} action(s) — review and approve on the case page:\n${r.rationale || ""}\n${acts}`, r.model || "");
        } else {
          asstAppend(caseId, "user", kind === "correspondence" ? "Draft correspondence." : "Draft a determination rationale.");
          const r = await Api.program.copilotDraft(caseId, kind);
          asstAppend(caseId, "assistant",
            `Draft queued in the QA gate (${r.subject}). Approve, edit, or reject it there — nothing is sent or filed automatically.`, "");
        }
      } catch (e) { asstAppend(caseId, "assistant", `⚠ ${e.message}`, ""); }
    }, "Working…");
  }

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

      // Copilot (Phases 1-3): grounded brief; QA-gated drafts; bounded,
      // human-approved action batches via Temporal. Everything the model
      // produces is advisory until a person approves it.
      if (can("CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        html += `<h2>Copilot <span class="badge s-review">DRAFT · advisory</span></h2>
          <div id="copilot"><p class="muted">Loading…</p></div>
          <div class="actions">
            <button class="mini" onclick="Views.copilotDraftQA('${id}','determination_rationale',this)">✍ Draft determination rationale</button>
            <button class="mini" onclick="Views.copilotDraftQA('${id}','correspondence',this)">✉ Draft correspondence</button>
            <button class="mini" onclick="Views.copilotPropose('${id}',this)">⚙ Propose action batch</button>
          </div>
          <div id="copilot-batches"></div>`;
        Api.program.copilotBriefLatest(id).then((r) => {
          const b = document.getElementById("copilot");
          if (b) b.innerHTML = copilotCard(r.brief, r.generated_at, id);
        }).catch(() => {
          const b = document.getElementById("copilot");
          if (b) b.innerHTML = copilotCard(null, null, id);
        });
        copilotLoadBatches(id);
      }

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
  // CSV download helper — every report card offers one (stakeholder handoff).
  function csvDownload(name, headers, rows) {
    const q = (v) => `"${String(v ?? "").replace(/"/g, '""')}"`;
    const csv = [headers.map(q).join(",")]
      .concat(rows.map((r) => r.map(q).join(","))).join("\n");
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([csv], { type: "text/csv" }));
    a.download = name;
    a.click();
    URL.revokeObjectURL(a.href);
  }

  // Reports: runnable on-platform, each card runs its query, renders a chart
  // and a table, and exports CSV. No external BI tool needed for the
  // statutory reporting loop.
  async function reports() {
    const PALETTE = ["#2E6B52", "#B08D3E", "#1D4E7E", "#5B3E8C", "#9C2B1F", "#3F7E6B", "#7A5C2E"];
    const geoLink = (window.IDRE_CONFIG.geoMapUrl || "")
      ? `<a class="button" href="${window.IDRE_CONFIG.geoMapUrl}" target="_blank">Geospatial audit map (GeoLibre) ↗</a>` : "";
    afterRender(() => {
      // 1) Case status rollup
      document.getElementById("rpt-status")?.addEventListener("click", async (ev) => {
        await UI.run(ev.currentTarget, async () => {
          try {
            const rows = await Api.reports.summary();
            const el = document.getElementById("rpt-status-out");
            el.innerHTML = `<div class="chart-card">` +
              chartDonut(rows.map((s, i) => ({ label: s.status, value: s.count, color: PALETTE[i % PALETTE.length] }))) +
              `</div><table><thead><tr><th>Status</th><th>Count</th><th>Avg QPA</th></tr></thead><tbody>` +
              rows.map((s) => `<tr><td>${badge(s.status)}</td><td>${s.count}</td><td>$${Number(s.avg_qpa_usd || 0).toFixed(0)}</td></tr>`).join("") +
              `</tbody></table>
               <p><button class="mini" id="rpt-status-csv">⬇ CSV</button></p>`;
            document.getElementById("rpt-status-csv").onclick = () =>
              csvDownload("case_status_rollup.csv", ["status", "count", "avg_qpa_usd"],
                rows.map((s) => [s.status, s.count, Number(s.avg_qpa_usd || 0).toFixed(2)]));
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        }, "Running…");
      });
      // 2) SLA breaches
      document.getElementById("rpt-sla")?.addEventListener("click", async (ev) => {
        await UI.run(ev.currentTarget, async () => {
          try {
            const sla = await Api.reports.sla();
            const byClock = {};
            sla.forEach((b) => { byClock[b.clock] = (byClock[b.clock] || 0) + 1; });
            const el = document.getElementById("rpt-sla-out");
            el.innerHTML = (sla.length
              ? `<div class="chart-card">` + chartHBars(
                  Object.entries(byClock).map(([k, v]) => ({ label: k, value: v }))) + `</div>` +
                `<table><thead><tr><th>Case</th><th>Clock</th><th>Detail</th><th>At</th></tr></thead><tbody>` +
                sla.map((b) => `<tr><td class="mono">${esc(b.case_id)}</td><td>${badge(b.clock)}</td>
                  <td>${esc(b.detail)}</td><td>${fmtDate(b.at)}</td></tr>`).join("") + `</tbody></table>
                 <p><button class="mini" id="rpt-sla-csv">⬇ CSV</button></p>`
              : `<p class="muted">No breaches recorded — all statutory clocks held. ✔</p>`);
            const b = document.getElementById("rpt-sla-csv");
            if (b) b.onclick = () => csvDownload("sla_breaches.csv", ["case_id", "clock", "detail", "at"],
              sla.map((x) => [x.case_id, x.clock, x.detail, x.at]));
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        }, "Running…");
      });
      // 3) Financial summary
      document.getElementById("rpt-fin")?.addEventListener("click", async (ev) => {
        await UI.run(ev.currentTarget, async () => {
          try {
            const fin = await Api.program.financial();
            const k = fin.kpi || {};
            const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
            const buckets = ["current", "1-30", "31-60", "60+"].map((b) => {
              const row = (fin.aging || []).find((a) => a.bucket === b);
              return { label: b === "current" ? "Current" : b + "d", value: row ? row.total_cents / 100 : 0 };
            });
            document.getElementById("rpt-fin-out").innerHTML =
              `<div class="kpi-row">
                 <div class="kpi"><span class="kpi-n">${usd(k.collected_cents)}</span><span class="kpi-l">Collected</span></div>
                 <div class="kpi"><span class="kpi-n">${usd(k.collected_30d_cents)}</span><span class="kpi-l">Last 30 days</span></div>
                 <div class="kpi"><span class="kpi-n">${usd(k.refunded_cents)}</span><span class="kpi-l">Refunded</span></div></div>
               <div class="chart-card">${chartHBars(buckets, (v) => "$" + v.toLocaleString())}</div>
               <p class="muted">A/R aging, open invoices, USD</p>
               <p><button class="mini" id="rpt-fin-csv">⬇ CSV</button></p>`;
            document.getElementById("rpt-fin-csv").onclick = () =>
              csvDownload("financial_summary.csv", ["metric", "value_usd"], [
                ["collected_all_time", (k.collected_cents || 0) / 100],
                ["collected_30d", (k.collected_30d_cents || 0) / 100],
                ["refunded", (k.refunded_cents || 0) / 100],
                ["payments_settled", k.payments_count ?? 0]]);
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        }, "Running…");
      });
      // 4) Caseload & throughput trend
      document.getElementById("rpt-trend")?.addEventListener("click", async (ev) => {
        await UI.run(ev.currentTarget, async () => {
          try {
            const d = await Api.program.opsDashboard();
            const mk = (arr) => (arr || []).map((p) => ({ x: p.d || p.day || "", y: p.n || 0 }));
            document.getElementById("rpt-trend-out").innerHTML =
              `<div class="chart-grid">
                 <div class="chart-card"><h3>Intake — last 30 days</h3>${chartArea(mk(d.cases_trend), { label: "rpt-intake" })}</div>
                 <div class="chart-card"><h3>Throughput (tasks completed)</h3>${chartArea(mk(d.throughput_trend), { label: "rpt-thru", color: "#1D4E7E" })}</div>
                 <div class="chart-card"><h3>Collections</h3>${chartArea(mk(d.collections_trend), { label: "rpt-coll", color: "#B08D3E", fmt: (v) => "$" + (v / 100).toLocaleString() })}</div>
               </div>
               <p><button class="mini" id="rpt-trend-csv">⬇ CSV</button></p>`;
            document.getElementById("rpt-trend-csv").onclick = () =>
              csvDownload("trends_30d.csv", ["day", "intake", "tasks_done", "collections_cents"],
                (d.cases_trend || []).map((p, i) => [p.d || p.day, p.n,
                  (d.throughput_trend || [])[i]?.n ?? 0, (d.collections_trend || [])[i]?.n ?? 0]));
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        }, "Running…");
      });
    });
    return `<div class="view-head"><h1>Reports</h1>
      <span class="muted">run on demand, charted on-platform, exportable — statutory reporting without external BI</span></div>
      ${geoLink ? `<p>${geoLink}</p>` : ""}
      <div class="rpt-grid">
        <div class="rpt-card"><h2>Case status rollup</h2>
          <p class="muted">dispute counts and average QPA by lifecycle status</p>
          <button id="rpt-status">▶ Run report</button><div id="rpt-status-out"></div></div>
        <div class="rpt-card"><h2>Statutory SLA breaches</h2>
          <p class="muted">every clock breach, grouped by clock, with case references</p>
          <button id="rpt-sla">▶ Run report</button><div id="rpt-sla-out"></div></div>
        <div class="rpt-card"><h2>Financial summary</h2>
          <p class="muted">collections, refunds, A/R aging — the money picture</p>
          <button id="rpt-fin">▶ Run report</button><div id="rpt-fin-out"></div></div>
        <div class="rpt-card"><h2>Caseload & throughput trend</h2>
          <p class="muted">30-day intake, completed tasks, collections</p>
          <button id="rpt-trend">▶ Run report</button><div id="rpt-trend-out"></div></div>
      </div>`;
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
      <p class="muted">Statutory clocks run <b>from</b> these dates — recording one starts or restarts its clock.
        Record the date once; every deadline that depends on it re-projects automatically.</p>
      <div id="p-dates-list"></div>
      <div id="p-clock-proj"></div>
      <form id="p-date" class="inline-form">
        <select name="key">${(cfg.clocks || []).map((cl) => `<option value="${esc(cl.basis)}">${esc(cl.basis)} (${esc(cl.label)})</option>`).join("")}</select>
        <input type="date" name="value" required /><button>Record date</button></form></details>

      <details class="prog-sec"><summary>Eligibility review</summary>
      <p class="muted">The engine applies this program's rules in order and records the evidence with the verdict:
        <b>1)</b> an explicit ineligibility flag wins immediately; <b>2)</b> the filing window (e.g. final
        determination + 12 months) is checked; <b>3)</b> the disputed amount is tested against the provider-type
        threshold matrix (contracted status included); <b>4)</b> an invalid AOR puts the case on attorney hold.
        Result is ELIGIBLE, INELIGIBLE (with reason code), or HOLD_AOR — and INELIGIBLE/HOLD move the case status
        automatically.</p>
      <div id="p-elig-history"></div>
      <div class="actions"><button id="p-elig-auto" title="Derives every rule input from the case record and analyzed documents, then decides — or tells you exactly which inputs are still missing">⚡ Auto-adjudicate from case + documents</button></div>
      <div id="p-elig-auto-out"></div>
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
        <textarea name="body" rows="3" placeholder="message body — {share_link} inserts a secure upload link, {download_link} a link to the latest generated document; both are minted on send" required></textarea>
        <label><input type="checkbox" name="auto_share" checked /> Attach a secure upload link (minted on send)</label>
        <label><input type="checkbox" name="auto_download" /> Attach a download link to the latest generated document (minted on send)</label>
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
      const loadDates = () => {
        Api.cases.get(id).then((cc) => {
          const el = document.getElementById("p-dates-list"); if (!el) return;
          const pd = cc.program_dates || {};
          const rows = (cfg.clocks || []).map((cl) => {
            const rec = pd[cl.basis];
            return `<tr><td>${esc(cl.label)}</td><td class="mono">${esc(cl.basis)}</td>
              <td>${rec ? `<b>${esc(rec)}</b>` : `<span class="muted">not recorded</span>`}</td></tr>`;
          });
          el.innerHTML = `<table><thead><tr><th>Clock basis</th><th>Key</th><th>Recorded</th></tr></thead>
            <tbody>${rows.join("")}</tbody></table>`;
        }).catch(() => {});
        Api.cm.clocks(id).then((cls) => {
          const el = document.getElementById("p-clock-proj"); if (!el) return;
          const arr = Array.isArray(cls) ? cls : (cls.clocks || []);
          if (!arr.length) { el.innerHTML = ""; return; }
          el.innerHTML = `<p><b>Projected deadlines</b></p><table><thead><tr><th>Clock</th><th>Due</th><th>State</th><th>Basis</th></tr></thead><tbody>` +
            arr.map((cl) => `<tr><td>${esc(cl.label || cl.clock || "")}</td>
              <td>${fmtDate(cl.due)}</td><td>${badge(cl.state || "")}</td>
              <td class="muted">${esc(cl.basis_note || cl.basis || "")}</td></tr>`).join("") + `</tbody></table>`;
        }).catch(() => {});
      };
      loadDates();
      const loadElig = () => Api.program.eligibilityHistory(id).then((r) => {
        const el = document.getElementById("p-elig-history"); if (!el) return;
        const revs = r.reviews || [];
        el.innerHTML = revs.length ? `<table><thead><tr><th>Result</th><th>Reason</th><th>By</th><th>When</th></tr></thead><tbody>` +
          revs.map((v) => `<tr><td>${badge(v.result)}</td><td>${esc(v.reason || "—")}</td>
            <td>${esc(v.decided_by || "")}</td><td class="muted">${fmtDate(v.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No eligibility review on record yet.</p>`;
      }).catch(() => {});
      loadElig();
      $("#p-date")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.setDate(id, f.key.value, f.value.value); UI.toast("Program date recorded — clocks re-projected"); loadDates(); }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      $("#p-elig-auto")?.addEventListener("click", async (ev) => {
        ev.preventDefault();
        const out = document.getElementById("p-elig-auto-out");
        await UI.run(ev.currentTarget, async () => {
          try {
            const r = await Api.program.eligibilityAuto(id);
            if (r.result === "NEEDS_HUMAN") {
              out.innerHTML = `<p class="warn-box">⚠ Can't auto-decide — missing rule inputs:
                <b>${(r.missing || []).map(esc).join(", ")}</b>. Fill them in the form below and compute manually.</p>`;
            } else {
              out.innerHTML = `<p>${badge(r.result)} ${r.reason ? esc(r.reason) : ""}
                <span class="muted">auto review ${esc(r.review_id)} — inputs derived from case record + analyzed documents</span></p>`;
              loadElig();
            }
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        }, "Evaluating…");
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
          loadElig();
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      $("#p-send")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        const split = (v) => v.split(",").map((x) => x.trim()).filter(Boolean);
        try {
          const r = await Api.program.send(id, { template: f.template.value, body: f.body.value, to: split(f.to.value), cc: split(f.cc.value), auto_share: f.auto_share.checked, auto_download: f.auto_download.checked });
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
      const renderInvoices = (r) => {
        const el = document.getElementById("p-inv-list"); if (!el) return;
        el.innerHTML = (r.invoices || []).length ? `<table><tbody>` + r.invoices.map((v) =>
          `<tr><td class="mono">${esc(v.invoice_no)}</td><td>${esc(v.party)}</td><td>$${(v.amount_cents / 100).toFixed(2)}</td>
           <td>${badge(v.status)}</td>
           <td>${v.status === "OPEN" ? `<a href="javascript:void(0)" onclick="Views.payInvoice('${v.id}')">💳 pay by card</a> ·
             <a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','PAY',this)">mark paid</a> ·
             <a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','REFUND',this)">refund</a>` : ""}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No invoices on this case.</p>`;
      };
      const refreshInv = () => Api.program.invoices(id).then(renderInvoices).catch(() => {});
      refreshInv();
      // Card payments settle via Stripe webhook — poll so status flips live
      // instead of requiring a manual reload.
      const invPoll = setInterval(() => {
        if (!document.getElementById("p-inv-list")) { clearInterval(invPoll); return; }
        refreshInv();
      }, 20000);
      $("#p-inv")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.issueInvoice(id, { party: f.party.value, kind: f.kind.value, amount_cents: money(f.amount.value) });
          UI.toast("Invoice issued (number = case number)"); refreshInv(); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
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
      const recent = r.recent || [];
      return `<div class="view-head"><h1>QA gate</h1>
        <span class="muted">nothing reaches a party without approval on gated templates</span></div>
      <details class="prog-sec" ${q.length ? "" : "open"}><summary>What the QA gate does</summary>
        <p class="muted">Every correspondence template in this program's rules either sends <b>immediately</b>
        or carries a <b>QA role</b> (e.g. attorney, program manager). Drafts on gated templates stop here as
        <b>PENDING</b> — the named role reviews the exact subject, body, recipients, and any secure links, then
        approves (email is delivered and logged) or rejects with a note back to the drafter. Ungated templates
        never appear here. An empty queue simply means no gated draft is waiting right now.</p></details>` +
        (q.length ? `<h2>Awaiting review (${q.length})</h2><table><thead><tr><th>Subject</th><th>Case</th><th>Drafted by</th><th></th></tr></thead><tbody>` +
          q.map((i) => `<tr><td>${esc(i.subject)}</td><td class="mono">${esc((i.case_id || "").slice(0, 8))}…</td>
            <td>${esc(i.drafted_by)}</td>
            <td><button class="mini" onclick="Views.qaReview('${i.id}')">review</button></td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">Queue empty — no drafts awaiting review.</p>`) + `<div id="qa-detail"></div>` +
        (recent.length ? `<h2>Recently decided</h2><table><thead><tr><th>Subject</th><th>Status</th><th>Reviewed by</th><th>When</th></tr></thead><tbody>` +
          recent.map((i) => `<tr><td>${esc(i.subject)}</td><td>${badge(i.status)}</td>
            <td>${esc(i.reviewed_by || "—")}</td><td class="muted">${fmtDate(i.reviewed_at)}</td></tr>`).join("") +
          `</tbody></table>` : "");
    } catch (e) { return err(e); }
  }

  async function qaReview(qaId) {
    const box = $("#qa-detail");
    try {
      const d = await Api.program.qaGet(qaId);
      const to = (d.to_recipients || []).join(", ");
      const isNote = d.channel === "note";
      const isCopilot = (d.artifact || "").startsWith("copilot_");
      box.dataset.channel = d.channel || "email";
      // Copilot drafts are accept/EDIT/reject: the reviewer rewrites in place
      // and the edited text is what gets approved (server records the edit).
      const bodyHtml = isCopilot
        ? `<textarea id="qa-edit" rows="14" style="width:100%">${esc(d.body)}</textarea>
           <p class="muted">Copilot draft — edit freely; the approved text is what gets ${isNote ? "filed" : "sent"}, and the edit is recorded.</p>`
        : `<pre class="qa-body">${esc(d.body)}</pre>`;
      box.innerHTML = `<div class="card"><h3>${esc(d.subject)}</h3>
        <p class="muted">${isNote ? "determination rationale · files to the case timeline on approval" : `to: ${esc(to)} · channel ${esc(d.channel)}`}
          ${isCopilot ? ' · <span class="badge s-review">COPILOT DRAFT</span>' : ""}</p>
        ${bodyHtml}
        <div class="actions">
          <button onclick="Views.qaDecide('${qaId}','APPROVE',this)">${isNote ? "Approve & file" : "Approve & send"}</button>
          <button class="danger" onclick="Views.qaDecide('${qaId}','REJECT',this)">Reject</button></div></div>`;
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function qaDecide(qaId, decision, el) {
    let note = "";
    const isNote = $("#qa-detail")?.dataset.channel === "note";
    const edited = $("#qa-edit") ? $("#qa-edit").value : "";
    if (decision === "REJECT") {
      const v = await UI.modal({ title: "Reject draft", danger: true, submitLabel: "Reject",
        fields: [{ name: "note", label: "Rejection note", type: "textarea", required: true,
          hint: "Returned to the drafter with the draft." }] });
      if (!v) return;
      note = v.note;
    } else if (isNote) {
      if (!(await UI.confirm("Approve and file?", "The rationale is recorded on the case timeline. Nothing is emailed.", "Approve & file"))) return;
    } else if (!(await UI.confirm("Approve and send?", "The email is delivered to all recipients now and logged to correspondence.", "Approve & send"))) return;
    await UI.run(el, async () => {
      try {
        const r = await Api.program.qaDecision(qaId, decision, note, edited);
        if (r && r.email_delivery_error) {
          UI.toast(`Approved, but email delivery failed: ${r.email_delivery_error}`, { kind: "warn", sticky: true });
        } else {
          UI.toast(decision === "APPROVE" ? (isNote ? "Approved — filed to case timeline" : "Approved — sent and logged") : "Rejected");
        }
        App.rerender();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
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
      const r = await Api.program.intake({ limit: 50 });
      const rows = r.intake || [];
      const intakeNext = r.next_offset ?? -1;
      const intakeTotal = r.total ?? rows.length;
      window._intakePager = { next: intakeNext }; // reset on every view render
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
          `</tbody></table>` : `<p class="muted">No intake requests.</p>`) +
        (intakeNext >= 0 ? `<p class="pager" id="intake-pg"><span class="muted">Showing ${rows.length} of ${intakeTotal}</span>
          <button class="mini" onclick="Views.intakeMore(this)">Load more (${Math.min(50, intakeTotal - rows.length)} remaining)</button></p>` : "");
    } catch (e) { return err(e); }
  }

  async function intakeMore(btn) {
    const st = window._intakePager || { next: 50 };
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.intake({ limit: 50, offset: st.next });
        const rows = r.intake || [];
        st.next = r.next_offset ?? -1;
        window._intakePager = st;
        const body = document.querySelector("#intake-pg")?.previousElementSibling?.querySelector("tbody");
        if (body) body.insertAdjacentHTML("beforeend", rows.map((i) => `<tr><td>${esc(i.email)}</td><td>${esc(i.org || "")}</td>
            <td>${i.filing_party_type === "HEALTH_PLAN" ? badge("HEALTH_PLAN") : `<span class="muted">Provider</span>`}</td>
            <td>${badge(i.status)} ${day13Countdown(i)}</td>
            <td class="muted">${fmtDate(i.outreach_at)}</td>
            <td class="muted">${i.packet_complete_at ? fmtDate(i.packet_complete_at) : "—"}</td>
            <td>${!intakeTerminalSet().includes(i.status) ?
              `<select onchange="Views.advanceIntake('${i.id}', this.value, this)">
                ${intakeAdvanceOptions(i.status)}</select>` : ""}</td></tr>`).join(""));
        const pg = document.getElementById("intake-pg");
        if (pg && st.next < 0) pg.outerHTML = "";
        else if (pg) pg.querySelector("button").textContent = "Load more";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Loading…");
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
      const hist = r.history || [];
      const lastDelivered = (name) => hist.filter((h) => h.name === name)
        .sort((a, b) => String(b.delivered_at).localeCompare(String(a.delivered_at)))[0];
      const daysUntil = (due) => Math.ceil((new Date(due + "T00:00:00") - Date.now()) / 864e5);
      const dueChip = (due) => {
        if (!due) return `<span class="muted">—</span>`;
        const n = daysUntil(due);
        if (n < 0) return `<span class="badge s-denied">${Math.abs(n)}d overdue</span>`;
        if (n <= 3) return `<span class="badge warn">due in ${n}d</span>`;
        return `<span class="badge s-paid">due in ${n}d</span>`;
      };
      return `<div class="view-head"><h1>Contract deliverables</h1>
        <span class="muted">what the program owes the agency, when it's due, and proof it shipped</span></div>
      <details class="prog-sec" open><summary>How deliverables work</summary>
        <p class="muted">Each program contract defines scheduled reports (the <b>rule</b> — e.g. monthly, per quarter).
        The platform computes the <b>next due date</b> from the rule and delivery history. When a report ships,
        press <b>mark delivered</b> — that records the delivery timestamp (your proof to the agency) and rolls the
        schedule forward. Ad hoc agency requests get a due date of +10 business days automatically.
        Anything overdue or due within 3 days is highlighted; due soon is amber, on track is green.</p></details>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.requestDeliverable(new FormData(event.target), event.target.querySelector('button'))">
        <input name="name" required placeholder="Ad hoc report name" />
        <input name="ref" placeholder="Contract ref (optional)" size="10" />
        <button class="mini">request ad hoc (due +10 business days)</button></form>` +
        (d.length ? `<div class="deliv-grid">` + d.map((x) => {
          const last = lastDelivered(x.name);
          return `<div class="deliv-card">
            <div class="deliv-head"><b>${esc(x.name)}</b>${dueChip(x.next_due)}</div>
            <div class="muted mono">${esc(x.due_rule)}</div>
            <div class="deliv-meta">next due <b>${esc(x.next_due || "—")}</b><br/>
              last delivered ${last ? fmtDate(last.delivered_at) : `<span class="muted">never</span>`}</div>
            <button class="mini" onclick="Views.submitDeliverable('${esc(x.name)}', this)">✓ mark delivered</button></div>`;
        }).join("") + `</div>` : `<p class="muted">No deliverables configured for this program.</p>`) +
        (hist.length ? `<h2>Delivery history</h2><table><thead><tr><th>Deliverable</th><th>Status</th><th>Delivered</th></tr></thead><tbody>` +
          hist.map((h) => `<tr><td>${esc(h.name)}</td><td>${badge(h.status)}</td>
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

  // finance() pager state — survives only for the rendered page
  let finPager = null;
  let payRow = null;

  async function financeMore(kind, btn) {
    const st = finPager?.[kind];
    if (!st || st.next < 0) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.payments(null, { limit: 25, offset: st.next });
        const rows = r.payments || [];
        st.rows = st.rows.concat(rows);
        st.next = r.next_offset ?? -1;
        st.total = r.total ?? st.total;
        const body = document.getElementById("fin-pay-body");
        if (body && payRow) body.insertAdjacentHTML("beforeend", rows.map(payRow).join(""));
        const pg = document.getElementById("fin-pay-pg");
        if (pg) pg.outerHTML = st.next >= 0
          ? `<p class="pager" id="fin-pay-pg"><span class="muted">Showing ${st.rows.length} of ${st.total}</span>
             <button class="mini" onclick="Views.financeMore('payments', this)">Load more (${Math.min(25, st.total - st.rows.length)} remaining)</button></p>`
          : `<p class="pager"><span class="muted">Showing ${st.rows.length} of ${st.total}</span></p>`;
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Loading…");
  }

  // ---- Financial dashboard (all money movement through the platform) -------

  async function finance() {
    try {
      const FIN_PAGE = 25;
      const [fin, pays] = await Promise.all([
        Api.program.financial(),
        Api.program.payments(null, { limit: FIN_PAGE }).catch(() => ({ payments: [] })),
      ]);
      payRow = (p) => `<tr><td class="mono">${esc((p.case_id || "").slice(0, 8))}…</td>
            <td>${esc(p.payer_email || "—")}</td><td>${usd(p.amount_cents)}</td><td>${badge(p.status)}</td>
            <td class="mono">${esc(p.payment_intent || p.session_id || "")}</td>
            <td class="muted">${fmtDate(p.created_at)}</td></tr>`;
      finPager = { payments: { rows: pays.payments || [], next: pays.next_offset ?? -1,
        total: pays.total ?? (pays.payments || []).length } };
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
      const plist = finPager.payments.rows;
      if (plist.length)
        html += `<h2>Card payments</h2><table><thead><tr><th>Case</th><th>Payer</th><th>Amount</th><th>Status</th><th>Stripe ref</th><th>When</th></tr></thead><tbody id="fin-pay-body">` +
          plist.map(payRow).join("") + `</tbody></table>` +
          (finPager.payments.next >= 0
            ? `<p class="pager" id="fin-pay-pg"><span class="muted">Showing ${plist.length} of ${finPager.payments.total}</span>
               <button class="mini" onclick="Views.financeMore('payments', this)">Load more (${Math.min(FIN_PAGE, finPager.payments.total - plist.length)} remaining)</button></p>` : "");

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

  // ---- Inline SVG charts (dependency-free; match the app palette) ---------
  const CH_COLORS = ["#2E6B52", "#B08D3E", "#1D4E7E", "#5B3E8C", "#9C2B1F", "#1E5B3C", "#8A5E10", "#6E6A5E"];

  function chartDonut(segs, size = 180) {
    const total = segs.reduce((a, s) => a + s.value, 0);
    if (!total) return `<div class="chart-empty muted">No data yet</div>`;
    const R = 70, C = 2 * Math.PI * R;
    let off = 0;
    const arcs = segs.filter((s) => s.value > 0).map((s, i) => {
      const frac = s.value / total, len = frac * C;
      const el = `<circle r="${R}" cx="${size/2}" cy="${size/2}" fill="none"
        stroke="${s.color || CH_COLORS[i % CH_COLORS.length]}" stroke-width="26"
        stroke-dasharray="${len} ${C - len}" stroke-dashoffset="${-off}"
        transform="rotate(-90 ${size/2} ${size/2})"><title>${esc(s.label)}: ${s.value}</title></circle>`;
      off += len;
      return el;
    }).join("");
    const legend = segs.filter((s) => s.value > 0).map((s, i) =>
      `<span class="legend-item"><i style="background:${s.color || CH_COLORS[i % CH_COLORS.length]}"></i>${esc(s.label)} <b>${s.value}</b></span>`).join("");
    return `<div class="donut-wrap"><svg viewBox="0 0 ${size} ${size}" width="${size}" height="${size}" role="img">
      ${arcs}<text x="${size/2}" y="${size/2 - 4}" text-anchor="middle" class="donut-n">${total}</text>
      <text x="${size/2}" y="${size/2 + 16}" text-anchor="middle" class="donut-l">total</text></svg>
      <div class="legend">${legend}</div></div>`;
  }

  function chartHBars(rows, fmt = (v) => v) {
    const max = Math.max(...rows.map((r) => r.value), 1);
    return `<div class="hbars">` + rows.map((r) => `<div class="hbar-row">
      <span class="hbar-label" title="${esc(r.label)}">${esc(r.label)}</span>
      <span class="hbar-track"><span class="hbar-fill${r.warn ? " warn" : ""}" style="width:${Math.max(2, (r.value / max) * 100)}%"></span>
        ${r.warnValue ? `<span class="hbar-fill bad" style="width:${(r.warnValue / max) * 100}%"></span>` : ""}</span>
      <span class="hbar-val">${esc(String(fmt(r.value)))}${r.warnValue ? ` <b class="bad-t">${fmt(r.warnValue)} overdue</b>` : ""}</span>
      </div>`).join("") + `</div>`;
  }

  function chartArea(points, { w = 560, h = 150, color = "#2E6B52", fmt = (v) => v, label = "" } = {}) {
    if (!points.length) return `<div class="chart-empty muted">No data yet</div>`;
    const max = Math.max(...points.map((p) => p.y), 1);
    const px = (i) => (i / Math.max(points.length - 1, 1)) * (w - 44) + 36;
    const py = (v) => h - 24 - (v / max) * (h - 40);
    const line = points.map((p, i) => `${i ? "L" : "M"}${px(i).toFixed(1)},${py(p.y).toFixed(1)}`).join(" ");
    const area = `${line} L${px(points.length - 1).toFixed(1)},${h - 24} L${px(0).toFixed(1)},${h - 24} Z`;
    const gid = "g" + Math.abs(label.split("").reduce((a, c) => a + c.charCodeAt(0), 0));
    const ticks = [0, Math.floor(points.length / 2), points.length - 1].map((i) =>
      `<text x="${px(i)}" y="${h - 8}" text-anchor="middle" class="axis">${esc(points[i].x)}</text>`).join("");
    return `<svg viewBox="0 0 ${w} ${h}" class="area-chart" role="img" aria-label="${esc(label)}">
      <defs><linearGradient id="${gid}" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stop-color="${color}" stop-opacity="0.35"/><stop offset="1" stop-color="${color}" stop-opacity="0.02"/>
      </linearGradient></defs>
      <line x1="36" y1="${h - 24}" x2="${w - 8}" y2="${h - 24}" class="axis-line"/>
      <text x="4" y="${py(max) + 4}" class="axis">${fmt(max)}</text>
      <path d="${area}" fill="url(#${gid})"/>
      <path d="${line}" fill="none" stroke="${color}" stroke-width="2.5" stroke-linejoin="round"/>
      ${points.map((p, i) => `<circle cx="${px(i)}" cy="${py(p.y)}" r="2.4" fill="${color}"><title>${esc(p.x)}: ${fmt(p.y)}</title></circle>`).join("")}
      ${ticks}</svg>`;
  }

  // ---- Operations dashboard (staff roles; presence + workload + KPIs) -----

  async function opsDashboard() {
    try {
      const d = await Api.program.opsDashboard();
      const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
      const tk = (d.task_kpis && d.task_kpis[0]) || {};
      const ck = (d.case_kpis && d.case_kpis[0]) || {};
      const sla = (d.sla && d.sla[0]) || {};
      const fin = (d.financial && d.financial[0]) || {};
      const q = (d.queues && d.queues[0]) || {};

      // Auto-refresh every 30s while the view stays open (presence + queues move).
      afterRender(() => setTimeout(() => { if (location.hash === "#/ops") App.rerender(); }, 30000));

      let html = `<div class="view-head"><h1>Operations dashboard</h1>
        <span class="muted">live workload, presence and stakeholder KPIs · tenant <b>${esc(Api.getTenant()).toUpperCase()}</b> · refreshes every 30s</span></div>
        <div class="kpi-row">
          <div class="kpi"><span class="kpi-n">${tk.completion_pct_30d ?? "—"}%</span><span class="kpi-l">Task completion (30d)</span></div>
          <div class="kpi"><span class="kpi-n">${tk.open ?? 0}</span><span class="kpi-l">Open tasks${tk.overdue ? ` · <b class="bad-t">${tk.overdue} overdue</b>` : ""}</span></div>
          <div class="kpi"><span class="kpi-n">${ck.unassigned_open ?? 0}</span><span class="kpi-l">Unassigned open ${App.t("case_plural").toLowerCase()}</span></div>
          <div class="kpi"><span class="kpi-n">${sla.breaches_7d ?? 0}</span><span class="kpi-l">SLA breaches (7d) · ${sla.breaches_total ?? 0} total</span></div>
          <div class="kpi"><span class="kpi-n">${usd(fin.collected_30d_cents)}</span><span class="kpi-l">Collected (30d)</span></div>
          <div class="kpi"><span class="kpi-n">${(d.online || []).length}</span><span class="kpi-l">Staff online now</span></div>
        </div>`;

      // Queue alerts strip
      const alerts = [
        [q.checks_review, "checks awaiting OCR review", "#/finance"],
        [q.checks_awaiting_clear, "matched checks awaiting clearing", "#/finance"],
        [q.qa_pending, "letters in the QA gate", "#/qa"],
        [q.intake_open, "pre-case intake requests open", "#/intake"],
        [q.onboarding_pending, "onboarding applications pending", "#/onboarding"],
      ].filter(([n]) => Number(n) > 0);
      if (alerts.length)
        html += `<div class="ops-alerts">` + alerts.map(([n, label, href]) =>
          `<a class="ops-alert" href="${href}"><b>${n}</b> ${label}</a>`).join("") + `</div>`;

      // Charts row 1: pipeline donut + intake trend
      const statuses = (d.cases || []).map((c) => ({ label: c.status, value: Number(c.n) }));
      const opened = (d.cases_trend || []).map((x) => ({ x: String(x.day).slice(5), y: Number(x.opened) }));
      html += `<div class="chart-grid">
        <div class="chart-card"><h2>${App.t("case_noun")} pipeline by status</h2>${chartDonut(statuses)}
          <p class="muted chart-foot">${ck.opened_7d ?? 0} opened in 7d · ${ck.opened_30d ?? 0} in 30d${ck.avg_open_age_days ? ` · avg open age ${ck.avg_open_age_days}d` : ""}</p></div>
        <div class="chart-card"><h2>Intake pace — ${App.t("case_plural").toLowerCase()} opened, 30 days</h2>
          ${chartArea(opened, { label: "cases opened", color: "#2E6B52" })}</div></div>`;

      // Charts row 2: workload by assignee + throughput
      const byAssignee = (d.tasks_by_assignee || []).map((a) => ({
        label: a.assignee, value: Number(a.open), warnValue: Number(a.overdue) || 0, warn: Number(a.overdue) > 0 }));
      const done = (d.throughput_trend || []).map((x) => ({ x: String(x.day).slice(5), y: Number(x.done) }));
      html += `<div class="chart-grid">
        <div class="chart-card"><h2>Workload by assignee (open tasks${byAssignee.some((a) => a.warn) ? ", red = overdue" : ""})</h2>
          ${byAssignee.length ? chartHBars(byAssignee) : '<p class="muted">No tasks yet.</p>'}</div>
        <div class="chart-card"><h2>Throughput — tasks completed, 14 days</h2>
          ${chartArea(done, { label: "tasks completed", color: "#B08D3E" })}</div></div>`;

      // Collections trend (full width)
      const coll = (d.collections_trend || []).map((x) => ({ x: String(x.day).slice(5), y: Math.round(Number(x.collected_cents) / 100) }));
      html += `<div class="chart-card"><h2>Collections — payments settled per day, 30 days</h2>
        ${chartArea(coll, { label: "collections", color: "#1D4E7E", fmt: (v) => "$" + v.toLocaleString(), w: 1120 })}</div>`;

      // Presence + outstanding side by side
      const online = d.online || [];
      html += `<div class="chart-grid">
        <div class="chart-card"><h2>Who's online</h2>` +
          (online.length ? `<table><thead><tr><th></th><th>Staff</th><th>Roles</th><th>Last seen</th></tr></thead><tbody>` +
            online.map((u) => `<tr><td><span class="presence-dot"></span></td>
              <td>${esc(u.display_name || u.user_sub)}</td>
              <td class="muted">${esc((Array.isArray(u.roles) ? u.roles : []).filter((r) => !String(r).startsWith("default")).join(", ") || "—")}</td>
              <td class="muted">${fmtDate(u.last_seen)}</td></tr>`).join("") + `</tbody></table>`
          : `<p class="muted">No staff active in the last 3 minutes.</p>`) + `</div>
        <div class="chart-card"><h2>Outstanding receivables</h2>` +
          ((d.outstanding || []).length ? `<table><thead><tr><th>Party</th><th>Open</th><th>Total</th><th>Overdue</th></tr></thead><tbody>` +
            d.outstanding.map((o) => `<tr><td>${badge(o.party)}</td><td>${o.open_invoices}</td>
              <td>${usd(o.open_cents)}</td><td class="${Number(o.overdue_cents) > 0 ? "bad-t" : ""}">${usd(o.overdue_cents)}</td></tr>`).join("") +
            `</tbody></table>` : `<p class="muted">No open invoices.</p>`) + `</div></div>`;

      // Escalation trail
      if ((d.escalations || []).length)
        html += `<h2>Recent escalations</h2><table><thead><tr><th>Case</th><th>Clock</th><th>Level</th><th>To</th><th>When</th></tr></thead><tbody>` +
          d.escalations.map((e) => `<tr><td class="mono">${esc((e.case_id || "").slice(0, 8))}…</td><td>${badge(e.clock)}</td>
            <td>L${e.level}</td><td>${esc(e.escalated_to || "—")}</td><td class="muted">${fmtDate(e.created_at)}</td></tr>`).join("") + `</tbody></table>`;
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

  return { dashboard, cases, caseDetail, newDispute, sortCases, onboarding, onboardingNew, decide, voice, reports, showAnalysis, check, assign, letter, saveCurrentView, escalate, relate, feeTransfer, peek, copilotBrief, copilotDraftQA, copilotPropose, copilotDecideBatch, assistant, assistantChip, askGraph, settleInvoice, qaQueue, qaReview, qaDecide, intake, newIntake, advanceIntake, intakeMore, deliverables, submitDeliverable, requestDeliverable, finance, payInvoice, financeMore, moveDoc, rulesAdmin, ruleEdit, ruleDelete, rulesSave, bindRulesAdmin, manifestEdit, opsDashboard };
})();
