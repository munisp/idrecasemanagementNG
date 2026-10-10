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
      const html = (r.batches || []).length
        ? `<h3>Action batches</h3>` + r.batches.map((b) => copilotBatchCard(caseId, b)).join("")
        : "";
      // Render into whichever surface is open — the case page panel and the
      // assistant thread show the same batches off the same endpoints.
      for (const elId of ["copilot-batches", "asst-batches"]) {
        const box = document.getElementById(elId);
        if (box) box.innerHTML = html;
      }
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
      // Morning briefing (conversation-first step 2): the Assistant home opens
      // with the worker-scoped digest, narrated — the case picker sits below
      // it. The briefing is read-only and never fails: when the model is down
      // the server narrates the digest itself (fallback badge).
      afterRender(async () => {
        const box = document.getElementById("asst-briefing");
        if (!box) return;
        try {
          const r = await Api.program.briefing();
          const d = r.digest || {};
          const stat = (n, label) => n ? `<span class="chip-stat"><b>${n}</b> ${label}</span>` : "";
          const risk = (d.at_risk_sla || []).map((c) =>
            // Deep-link both ways: the case number opens the case SCREEN
            // (docket/documents), the row opens its conversation thread.
            `<tr class="click" onclick="location.hash='#/assistant/${c.case_id}'"><td class="mono"><a href="#/cases/${c.case_id}" onclick="event.stopPropagation()">${esc(c.case_number)}</a></td>
             <td>${badge(c.status)}</td><td>${esc(c.lane)}</td><td class="sla-hot">${c.sla_days_remaining}d left</td></tr>`).join("");
          box.innerHTML = `<div class="asst-turn asst-ai">
            <div class="asst-who">briefing${r.model ? ` · ${esc(r.model)}` : " · structured digest"}</div>
            <div class="asst-body">${esc(r.narration || "")}</div></div>
            <div class="asst-chips" style="margin:8px 0 0 0">
              ${stat(d.my_open_cases, "open assigned")}${stat((d.at_risk_sla || []).length, "SLA risk")}
              ${stat(d.pending_qa, "at QA gate")}${d.checks_in_review ? `<a href="#/finance"><span class="chip-stat"><b>${d.checks_in_review}</b> checks in review ↗</span></a>` : ""}
              ${stat(d.new_docs_24h, "docs analyzed 24h")}${stat((d.tasks_due || []).length, "tasks due")}
            </div>
            ${risk ? `<h3 style="margin:10px 0 4px">SLA risk — open a thread to act</h3>
              <table><thead><tr><th>Case</th><th>Status</th><th>Lane</th><th>SLA</th></tr></thead><tbody>${risk}</tbody></table>` : ""}`;
        } catch (e) {
          box.innerHTML = `<p class="muted">Briefing unavailable: ${esc(e.message)}</p>`;
        }
      });
      try {
        const r = await Api.cases.list({ limit: 50 });
        const rows = (r.cases || []).map((c) =>
          `<tr class="click" onclick="location.hash='#/assistant/${c.id}'"><td class="mono"><a href="#/cases/${c.id}" onclick="event.stopPropagation()">${esc(c.case_number)}</a></td>
           <td>${esc(c.service_line || "")}</td><td>${badge(c.status)}</td></tr>`).join("");
        return `<div class="view-head"><h1>Assistant</h1>
          <span class="muted">grounded on platform-verified case facts · advisory only · every turn is on the record</span></div>
          <div id="asst-briefing"><p class="muted">Preparing your briefing…</p></div>
          <h2 style="margin-top:14px">Case threads</h2>
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
      asstQaLoad(caseId);
      copilotLoadBatches(caseId); // pending action batches render inline — decide without leaving the thread
    });
    return `<div class="view-head"><h1>Assistant</h1>
      <span class="muted" id="asst-case">loading case…</span></div>
      <div id="asst-thread" class="asst-thread"></div>
      <div id="asst-qa"></div>
      <div id="asst-batches"></div>
      <div id="asst-tools"></div>
      <div class="asst-chips">
        <button class="mini" onclick="Views.assistantTool('${caseId}','docs')">📄 Documents</button>
        <button class="mini" onclick="Views.assistantTool('${caseId}','checklist')">☑ Stage checklist</button>
        <button class="mini" onclick="Views.assistantTool('${caseId}','mail')">✉ Send mail</button>
        <button class="mini" onclick="Views.assistantTool('${caseId}','checks')">🧾 Manual checks</button>
        <button class="mini" onclick="Views.assistantTool('${caseId}','time')">⏱ Log time</button>
      </div>
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
            `Proposed ${(r.actions || []).length} action(s) — review and approve in the batch card below, without leaving this thread:\n${r.rationale || ""}\n${acts}`, r.model || "");
          copilotLoadBatches(caseId);
        } else {
          asstAppend(caseId, "user", kind === "correspondence" ? "Draft correspondence." : "Draft a determination rationale.");
          const r = await Api.program.copilotDraft(caseId, kind);
          asstAppend(caseId, "assistant",
            `Draft queued in the QA gate (${r.subject}). Approve, edit, or reject it there — nothing is sent or filed automatically.`, "");
        }
      } catch (e) { asstAppend(caseId, "assistant", `⚠ ${e.message}`, ""); }
    }, "Working…");
  }

  // ---- Conversational QA gate (conversation-first step 3) -----------------
  // Pending gate items for THIS case render inline in the thread as cards;
  // approve / edit & approve / reject call the SAME qaDecision endpoint the
  // QA screen uses — the gate moves into the conversation, its semantics
  // (audit note, [human-edited] marker, send-on-approve) are untouched.
  async function asstQaLoad(caseId) {
    const box = document.getElementById("asst-qa");
    if (!box) return;
    try {
      const r = await Api.program.qaQueue(caseId);
      const q = r.queue || [];
      if (!q.length) { box.innerHTML = ""; return; }
      const cards = await Promise.all(q.map(async (i) => {
        const d = await Api.program.qaGet(i.id);
        const isNote = d.channel === "note";
        const isCopilot = (d.artifact || "").startsWith("copilot_");
        const bodyHtml = isCopilot
          ? `<textarea id="asst-qa-edit-${d.id}" rows="10" style="width:100%">${esc(d.body)}</textarea>
             <p class="muted">Copilot draft — edit freely; the approved text is what gets ${isNote ? "filed" : "sent"}, and the edit is recorded.</p>`
          : `<pre class="qa-body">${esc(d.body)}</pre>`;
        return `<div class="card asst-qa-card" data-qa="${d.id}" data-channel="${esc(d.channel || "email")}">
          <h3>⛨ Gate: ${esc(d.subject)}</h3>
          <p class="muted">${esc(d.drafted_by)} · ${isNote ? "determination rationale · files to timeline" : `to: ${esc((d.to_recipients || []).join(", "))}`}
            ${isCopilot ? ' · <span class="badge s-review">COPILOT DRAFT</span>' : ""}</p>
          ${bodyHtml}
          <div class="actions">
            <button onclick="Views.asstQaDecide('${caseId}','${d.id}','APPROVE',this)">${isNote ? "Approve & file" : "Approve & send"}</button>
            <button class="danger" onclick="Views.asstQaDecide('${caseId}','${d.id}','REJECT',this)">Reject</button></div></div>`;
      }));
      box.innerHTML = `<h3 style="margin:10px 0 6px">${q.length} item(s) at the QA gate for this case</h3>` + cards.join("");
    } catch (e) { box.innerHTML = ""; }
  }

  async function asstQaDecide(caseId, qaId, decision, btn) {
    const card = document.querySelector(`.asst-qa-card[data-qa="${qaId}"]`);
    const isNote = card?.dataset.channel === "note";
    const edited = document.getElementById(`asst-qa-edit-${qaId}`)?.value || "";
    let note = "";
    if (decision === "REJECT") {
      const v = await UI.modal({ title: "Reject draft", danger: true, submitLabel: "Reject",
        fields: [{ name: "note", label: "Rejection note", type: "textarea", required: true,
          hint: "Returned to the drafter with the draft." }] });
      if (!v) return;
      note = v.note;
    } else if (isNote) {
      if (!(await UI.confirm("Approve and file?", "The rationale is recorded on the case timeline. Nothing is emailed.", "Approve & file"))) return;
    } else if (!(await UI.confirm("Approve and send?", "The email is delivered to all recipients now and logged to correspondence.", "Approve & send"))) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.qaDecision(qaId, decision, note, edited);
        card?.remove();
        asstAppend(caseId, "assistant",
          decision === "APPROVE"
            ? (r.email_delivery_error ? `⚠ Approved but email failed: ${r.email_delivery_error}`
              : isNote ? "Rationale approved and filed to the case timeline." : "Draft approved and sent — logged to correspondence.")
            : "Draft rejected and returned to the drafter.", "");
        const box = document.getElementById("asst-qa");
        if (box && !box.querySelector(".asst-qa-card")) box.innerHTML = "";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, decision === "APPROVE" ? "Approving…" : "Rejecting…");
  }

  // ---- Thread tools: the conversation reaches the case screens ------------
  // Documents, stage checklist, mail, and manual checks render inline in the
  // thread and call the SAME endpoints the screens use — same roles, same
  // audit, same timeline. The conversation is a second surface over the case
  // record, never a parallel state store.
  async function assistantTool(caseId, tool) {
    const box = document.getElementById("asst-tools");
    if (!box) return;
    if (box.dataset.open === tool) { box.innerHTML = ""; box.dataset.open = ""; return; }
    box.dataset.open = tool;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      if (tool === "docs") box.innerHTML = await asstDocsHtml(caseId);
      else if (tool === "checklist") box.innerHTML = await asstChecklistHtml(caseId);
      else if (tool === "mail") box.innerHTML = await asstMailHtml(caseId);
      else if (tool === "checks") box.innerHTML = await asstChecksHtml();
      else if (tool === "time") box.innerHTML = await asstTimeHtml(caseId);
    } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
  }

  async function asstDocsHtml(caseId) {
    const docs = await Api.cases.documents(caseId);
    const byFolder = {};
    (docs || []).forEach((d) => { byFolder[d.folder || "GENERAL"] = (byFolder[d.folder || "GENERAL"] || 0) + 1; });
    const pending = (docs || []).filter((d) => d.analysis_status && !["COMPLETE", "FAILED", "SKIPPED", ""].includes(d.analysis_status));
    const rows = (docs || []).map((d) => `<tr>
      <td class="mono">${esc(d.filename || d.doc_id.slice(0, 8) + "…")}</td>
      <td>${esc(d.folder || "GENERAL")}</td>
      <td>${d.sealed ? '<span class="badge s-sealed">🔒 sealed</span>' : badge(d.analysis_status || "—")}</td>
      <td><a href="${Api.cases.downloadUrl(caseId, d.doc_id)}" target="_blank">download</a></td></tr>`).join("");
    return `<div class="card"><h3>📄 Documents — ${(docs || []).length} on the docket</h3>
      <p class="muted">${Object.entries(byFolder).map(([f, n]) => `${esc(f)} ${n}`).join(" · ") || "none yet"}${pending.length ? ` · ⚠ ${pending.length} awaiting analysis` : ""}</p>
      ${rows ? `<table><thead><tr><th>File</th><th>Folder</th><th>Analysis</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : ""}
      <div class="actions">
        <button class="mini" onclick="Views.asstRequestUpload('${caseId}',this)">🔗 Request party upload link</button>
        <a class="button mini" href="#/cases/${caseId}">Open full docket ↗</a>
      </div></div>`;
  }

  async function asstRequestUpload(caseId, btn) {
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.shareLink(caseId, "upload", 7);
        const full = location.origin + r.url;
        asstAppend(caseId, "user", "Request a party upload link.");
        asstAppend(caseId, "assistant", `Secure upload link minted (expires in 7 days):\n${full}\n\nSend it via Send mail — the {share_link} placeholder mints one automatically on send.`, "");
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Minting link…");
  }

  async function asstChecklistHtml(caseId) {
    const items = await Api.cm.checklist(caseId);
    const stages = {};
    (items || []).forEach((i) => { (stages[i.stage] = stages[i.stage] || []).push(i); });
    const total = (items || []).length, done = (items || []).filter((i) => i.done).length;
    const canCheck = can("CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN");
    return `<div class="card"><h3>☑ Stage checklists — ${done} of ${total} complete</h3>` +
      Object.entries(stages).map(([stage, its]) =>
        `<h4 style="margin:10px 0 4px">${esc(stage)}</h4><ul class="checklist">` + its.map((i) =>
          `<li>${i.done ? "✅" : canCheck
            ? `<button class="mini" onclick="Views.asstCheck('${caseId}','${i.id}',this)">check</button>` : "⬜"}
            ${esc(i.item)}${i.required ? " *" : ""}${i.done_by ? ` <span class="muted">(${esc(i.done_by)})</span>` : ""}</li>`).join("") +
        `</ul>`).join("") + `</div>`;
  }

  async function asstCheck(caseId, itemId, btn) {
    await UI.run(btn, async () => {
      try {
        await Api.cm.checkItem(itemId);
        const box = document.getElementById("asst-tools");
        if (box) box.innerHTML = await asstChecklistHtml(caseId);
        UI.toast("Checklist item completed — recorded on the case");
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Recording…");
  }

  async function asstMailHtml(caseId) {
    const prog = await Api.program.get().catch(() => null);
    const tpls = prog?.config?.correspondence?.templates || [];
    if (!tpls.length)
      return `<div class="card"><h3>✉ Send mail</h3><p class="muted">This program defines no correspondence templates — use the case screen's correspondence tools.</p></div>`;
    corrTplCache = tpls;
    return `<div class="card"><h3>✉ Send mail</h3>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.asstSendMail('${caseId}', this)">
        <select name="template" onchange="Views.corrTemplateBody(this)">${tpls.map((tp) =>
          `<option value="${esc(tp.key)}">${esc(tp.key)}${tp.qa_role ? " (QA: " + esc(tp.qa_role) + ")" : ""}</option>`).join("")}</select>
        <select name="rfi_to" title="only used for the rfi template"><option value="provider">RFI to: provider</option><option value="plan">RFI to: health plan</option></select>
        <input name="to" placeholder="to emails (comma-separated)" required />
        <input name="cc" placeholder="cc emails" />
        <textarea name="body" rows="3" placeholder="message body — {share_link} mints a secure upload link on send, {download_link} a link to the latest generated document" required></textarea>
        <label><input type="checkbox" name="auto_share" checked /> Attach secure upload link</label>
        <label><input type="checkbox" name="auto_download" /> Attach latest-document link</label>
        <button>Send / submit for QA</button></form>
      <p class="muted">Same endpoint as the case screen — QA-flagged templates wait in the gate above until a human approves.</p></div>`;
  }

  async function asstSendMail(caseId, form) {
    const split = (s) => (s || "").split(",").map((x) => x.trim()).filter(Boolean);
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.send(caseId, {
          template: form.template.value, body: form.body.value,
          to: split(form.to.value), cc: split(form.cc.value),
          rfi_to: form.rfi_to.value, auto_share: form.auto_share.checked, auto_download: form.auto_download.checked,
        });
        asstAppend(caseId, "user", `Send mail: ${form.template.value} to ${form.to.value}.`);
        asstAppend(caseId, "assistant",
          r.queued_for_qa ? `Submitted — the draft is waiting in the QA gate above. Approve it there and it sends; nothing goes out automatically.`
            : `Sent and logged to correspondence.`, "");
        form.body.value = "";
        asstQaLoad(caseId); // a QA-gated template appears inline immediately
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Sending…");
  }

  async function asstChecksHtml() {
    const r = await Api.program.checks("REVIEW", { limit: 50 });
    const ck = r.checks || [];
    if (!ck.length)
      return `<div class="card"><h3>🧾 Manual checks</h3><p class="muted">No checks awaiting review — the OCR queue is clear.</p></div>`;
    const rows = ck.map((c) => `<tr>
      <td class="mono">${esc((c.check_id || c.id || "").slice(0, 8))}…</td>
      <td>${esc(c.payee || "—")}</td>
      <td>${c.courtesy_amount_cents != null ? "$" + (c.courtesy_amount_cents / 100).toLocaleString() : "—"}</td>
      <td>${esc(c.memo || "")}</td>
      <td><input id="asst-ck-ref-${c.check_id || c.id}" placeholder="remittance ref" style="width:130px" />
          <button class="mini" onclick="Views.asstClearCheck('${c.check_id || c.id}',this)">clear</button></td></tr>`).join("");
    return `<div class="card"><h3>🧾 Manual checks — ${ck.length} awaiting review</h3>
      <p class="muted">OCR could not match these to an open invoice. Match each to its remittance and clear — or <a href="#/finance">open finance ↗</a> for the full queue.</p>
      <table><thead><tr><th>Check</th><th>Payee</th><th>Courtesy</th><th>Memo</th><th>Clear</th></tr></thead><tbody>${rows}</tbody></table></div>`;
  }

  async function asstClearCheck(checkId, btn) {
    const ref = document.getElementById(`asst-ck-ref-${checkId}`)?.value.trim() || "";
    if (!ref) { UI.toast("Enter the remittance reference first", { kind: "warn" }); return; }
    if (!(await UI.confirm("Clear this check?", `Matched to remittance ${ref} — funds settle against the open invoice.`, "Clear check"))) return;
    await UI.run(btn, async () => {
      try {
        await Api.program.clearCheck(checkId, ref);
        UI.toast("Check cleared");
        const box = document.getElementById("asst-tools");
        if (box) box.innerHTML = await asstChecksHtml();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Clearing…");
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
      { name: "role", label: "Assign as", options: [["CASE_MANAGER", "Case manager"], ["ARBITRATOR", "Arbitrator"], ["DOCTOR", "Doctor"], ["NURSE", "Nurse"]], required: true },
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

      // Time — per-role effort ledger on this dispute. Append-only; the
      // role stamped is the logger's real role. Eligibility decides whether
      // the work is billable; this records that it happened.
      if (can("CASE_MANAGER", "DOCTOR", "NURSE", "ARBITRATOR", "ATTORNEY", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        html += `<h2>Time</h2><div id="time-box"><p class="muted">Loading…</p></div>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.timeAdd('${id}', this)">
            <input name="hours" type="number" step="0.25" min="0.25" max="24" placeholder="hours" required aria-label="Hours worked" />
            ${timeRoleSelect()}
            <input name="entry_date" type="date" aria-label="Date worked" />
            <input name="note" placeholder="what was done" maxlength="500" />
            <label><input type="checkbox" name="billable" checked /> billable</label>
            <button class="mini">Log time</button>
            <span class="muted">recorded under your name and role — corrections are compensating entries, never edits</span></form>`;
        timeCaseBox(id).then((h) => { const b = document.getElementById("time-box"); if (b) b.innerHTML = h; });
      }

      // Financials — every dollar tied to this dispute.
      if (can("CASE_MANAGER", "FINANCE", "STATE_AUDITOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        html += `<h2>Financials</h2><div id="fin-box"><p class="muted">Loading…</p></div>`;
        caseFinBox(id).then((h) => { const b = document.getElementById("fin-box"); if (b) b.innerHTML = h; });
      }

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
              return { label: b === "current" ? "Current" : b + "d", value: row ? row.total_cents / 100 : 0 , billingInvoices, billingGen, billingActFn, billingDetail, billingExportFn, billingPayForm, billingPayRun, billingFilter: (f) => billingList(f.status.value), arapView, arapRecord, arapSettle, arapVoid, reconView, reconImportRun, reconFetchRun, reconMatchRun, reconOpen, reconResolveFn, tpaView, tpaClaimFn, tpaAddClientFn, tpaClientStatusFn, tpaFileFn, tpaDashBox, tpaAdminStatusFn, tpaBulkFile, tpaBulkSubmit, tpaBatchesBox, tpaBatchDetail };
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
        ${corrTplCache = cfg.correspondence?.templates || [], ""}
        <select name="template" onchange="Views.corrTemplateBody(this)">${(cfg.correspondence?.templates || []).map((tp) =>
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
        try { const sr = await Api.program.setStatus(id, { internal_status: f.internal.value, agency_status: f.agency.value });
          (sr.warnings || []).forEach((w) => UI.toast("⚠ " + w, { kind: "warn", sticky: true }));
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
      const eligBadge = (v) => `${badge(v.result)}${v.borderline ? ` <span class="badge warn" title="Borderline call — PM second review required">borderline</span>` : ""}${v.override ? ` <span class="badge" title="Human override of the computed result">override</span>` : ""}`;
      const loadElig = () => Api.program.eligibilityHistory(id).then((r) => {
        const el = document.getElementById("p-elig-history"); if (!el) return;
        const revs = r.reviews || [];
        el.innerHTML = revs.length ? `<table><thead><tr><th>Result</th><th>Reason</th><th>Rules</th><th>By</th><th>When</th></tr></thead><tbody>` +
          revs.map((v) => `<tr><td>${eligBadge(v)}</td><td>${esc(v.reason || "—")}${v.override_reason ? `<div class="muted">override: ${esc(v.override_reason)}</div>` : ""}</td>
            <td class="mono muted" title="rule version that produced this decision">${esc(v.rule_version || "—")}</td>
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
              showEligResult(r, out, null);
              loadElig();
            }
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        }, "Evaluating…");
      });
      $("#p-elig")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        const payload = () => ({
          provider_type: f.provider_type.value, contracted: f.contracted.checked,
          disputed_amount_cents: money(f.amount.value),
          final_determination_at: f.fd.value || "",
          flags: f.flags.value ? f.flags.value.split(",").map((x) => x.trim()).filter(Boolean) : [],
          aor_valid: f.aor_valid.checked,
        });
        const out = document.getElementById("p-elig-out");
        const resubmit = async (reason) => {
          try {
            const r = await Api.program.eligibility(id, { ...payload(), override: true, override_reason: reason });
            UI.toast("Override recorded — case ELIGIBLE, PM notified");
            showEligResult(r, out, null);
            loadElig();
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        };
        try {
          const r = await Api.program.eligibility(id, payload());
          showEligResult(r, out, resubmit);
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
          `<tr><td>${badge(m.direction)}${m.delivery_status === "FAILED" ? ` <span class="badge warn" title="${esc(m.delivery_error || "")}">FAILED</span>` : ""}</td><td>${esc(m.subject || "")}</td>
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
      // Conversational intake (step 5): the chat EXTRACTS into the form
      // (including manifest sector fields); the human reviews and files with
      // the same button as always. The model never files.
      afterRender(() => {
        window._intakeChat = { fields: {}, history: [] };
        // Human keystrokes win over extraction: once a field is touched, the
        // assistant never overwrites it.
        const f0 = document.getElementById("intake-form");
        f0 && Array.from(f0.elements).forEach((el) =>
          el.name && el.addEventListener("input", () => { el.dataset.touched = "1"; }));
        document.getElementById("intake-chat-form")?.addEventListener("submit", (ev) => {
          ev.preventDefault();
          const msg = ev.target.message.value.trim();
          if (msg) { ev.target.message.value = ""; intakeChatTurn(msg); }
        });
        // Bulk intake (CSV): parse on file pick, submit posts the idempotent
        // batch and renders the per-row receipt.
        window._intakeBulk = { items: [] };
        document.getElementById("intake-bulk-file")?.addEventListener("change", (ev) => bulkIntakeFile(ev.target));
        document.getElementById("intake-bulk-btn")?.addEventListener("click", (ev) => { ev.preventDefault(); bulkIntakeSubmit(ev.target); });
      });
      return `<div class="view-head"><h1>Pre-case intake</h1>
        <span class="muted">instruction requests awaiting documents and fees — the 10-day initial review starts at PACKET_COMPLETE</span></div>
        <details open class="card" style="margin-bottom:12px"><summary><b>✦ Describe it, I'll fill the form</b> — conversational intake (extraction only; you review and file)</summary>
          <div id="intake-chat-thread" class="asst-thread" style="min-height:80px;max-height:30vh;margin:10px 0">
            <div class="asst-turn asst-ai"><div class="asst-who">intake assistant</div>
            <div class="asst-body">Describe the request in your own words — who called, who files, amounts, reference numbers, anything else. I'll fill the form below as we go.</div></div>
          </div>
          <form id="intake-chat-form" class="asst-form">
            <input name="message" autocomplete="off" placeholder="Describe the intake in one or two sentences…" aria-label="Describe the intake" />
            <button>Send</button></form>
          <p class="muted" id="intake-chat-missing" style="margin:6px 0 0"></p></details>
        <details class="card" style="margin-bottom:12px"><summary><b>Bulk intake (CSV)</b> — third-party batch filing; idempotent by batch reference, up to 500 rows</summary>
          <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin:10px 0">
            <input type="file" id="intake-bulk-file" accept=".csv,text/csv" />
            <input id="intake-bulk-ref" placeholder="batch reference — the idempotency key" style="flex:1;min-width:220px" />
            <button class="mini" id="intake-bulk-btn" disabled>Submit batch</button></div>
          <p class="muted">Header row required: <code>email, contact_name, org, filing_party_type, external_ref, notes</code>
            plus one column per manifest intake field (matched by field name, e.g. <code>disputed_amount, claim_ref</code>).
            Resubmitting the same batch reference replays the receipt — nothing files twice.</p>
          <div id="intake-bulk-preview"></div>
          <div id="intake-bulk-result"></div></details>
        <form id="intake-form" class="inline-form" onsubmit="return Views.newIntake(this)">
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

  // One turn of conversational intake: send the worker's words plus the
  // fields extracted so far (client-carried state, endpoint is stateless),
  // render the follow-up, and prefill the REAL form — filing stays manual.
  async function intakeChatTurn(msg) {
    const thread = document.getElementById("intake-chat-thread");
    const add = (role, body) => thread?.insertAdjacentHTML("beforeend",
      `<div class="asst-turn ${role === "user" ? "asst-user" : "asst-ai"}">
         <div class="asst-who">${role === "user" ? "you" : "intake assistant"}</div>
         <div class="asst-body">${esc(body)}</div></div>`);
    const st = window._intakeChat || (window._intakeChat = { fields: {}, history: [] });
    add("user", msg); add("assistant", "…");
    try {
      const r = await Api.program.intakeConverse(msg, st.fields, st.history);
      thread.lastElementChild.remove();
      add("assistant", r.reply || "");
      st.history.push(msg);
      st.fields = r.fields || {};
      // Prefill the form; never overwrite text the human has typed.
      const f = document.getElementById("intake-form");
      if (f) {
        // Chat-filled fields are tagged "from chat" so the reviewer can see
        // exactly which values the model supplied; human-typed text is never
        // overwritten.
        const tag = (el) => { el.classList.add("from-chat"); el.title = "Prefilled from the conversation — review before filing"; };
        if (st.fields.email && !f.email.dataset.touched) { f.email.value = st.fields.email; tag(f.email); }
        if (st.fields.contact_name && !f.contact_name.dataset.touched) { f.contact_name.value = st.fields.contact_name; tag(f.contact_name); }
        if (st.fields.org && !f.org.dataset.touched) { f.org.value = st.fields.org; tag(f.org); }
        if (st.fields.filing_party_type) { f.filing_party_type.value = st.fields.filing_party_type; tag(f.filing_party_type); }
        Object.entries(st.fields.extra || {}).forEach(([name, v]) => {
          const el = f.elements["fld_" + name];
          if (el && !el.dataset.touched) { el.value = v; tag(el); }
        });
      }
      const miss = document.getElementById("intake-chat-missing");
      if (miss) {
        if (r.ready) {
          // The handoff: conversation is done, the screen takes over — scroll
          // the reviewer to the prefilled form; filing stays a human click.
          miss.innerHTML = `✓ Ready — review the prefilled form and file when you're satisfied.
            <button class="mini" type="button" onclick="document.getElementById('intake-form').scrollIntoView({behavior:'smooth',block:'center'});document.getElementById('intake-form').classList.add('chat-handoff')">Review the prefilled form ↓</button>`;
        } else {
          miss.textContent = (r.missing || []).length ? "Still needed: " + r.missing.join(", ") : "";
        }
      }
    } catch (e) {
      thread?.lastElementChild?.remove();
      add("assistant", `⚠ ${e.message}`);
    }
    thread && (thread.scrollTop = thread.scrollHeight);
  }

  // Minimal CSV parser: quotes, escaped quotes, CRLF — Excel's default
  // export is exactly this shape.
  function bulkCsvRows(text) {
    const rows = []; let row = [], cur = "", inQ = false;
    for (let i = 0; i < text.length; i++) {
      const c = text[i];
      if (inQ) {
        if (c === '"' && text[i + 1] === '"') { cur += '"'; i++; }
        else if (c === '"') inQ = false;
        else cur += c;
      } else if (c === '"') inQ = true;
      else if (c === ",") { row.push(cur); cur = ""; }
      else if (c === "\n" || c === "\r") {
        if (c === "\r" && text[i + 1] === "\n") i++;
        row.push(cur); cur = "";
        if (row.some((v) => v.trim() !== "")) rows.push(row);
        row = [];
      } else cur += c;
    }
    row.push(cur);
    if (row.some((v) => v.trim() !== "")) rows.push(row);
    return rows;
  }

  // Base columns are the createIntake payload keys; every OTHER column is a
  // manifest intake field value, matched by field name (the server drops
  // anything the manifest doesn't declare — same trust boundary as the form).
  function bulkIntakeFile(input) {
    const BASE = ["email", "contact_name", "org", "filing_party_type", "external_ref", "notes"];
    const file = input.files && input.files[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      const rows = bulkCsvRows(String(reader.result || ""));
      const head = (rows.shift() || []).map((h) => h.trim().toLowerCase().replace(/^\uFEFF/, ""));
      if (!head.includes("email")) {
        document.getElementById("intake-bulk-preview").innerHTML = `<p class="badge warn">header row must include at least: email</p>`;
        return;
      }
      const items = rows.map((r) => {
        const cell = (name) => { const i = head.indexOf(name); return i >= 0 ? (r[i] || "").trim() : ""; };
        const fields = {};
        head.forEach((h, i) => { if (h && !BASE.includes(h) && (r[i] || "").trim() !== "") fields[h] = r[i].trim(); });
        return {
          external_ref: cell("external_ref"), email: cell("email"),
          contact_name: cell("contact_name"), org: cell("org"), notes: cell("notes"),
          filing_party_type: cell("filing_party_type").toUpperCase(), fields,
        };
      }).filter((it) => it.email);
      window._intakeBulk = { items };
      const bad = rows.length - items.length;
      document.getElementById("intake-bulk-preview").innerHTML =
        `<p class="muted">${items.length} row(s) ready${bad ? ` — ${bad} row(s) skipped (no email)` : ""}${items.length > 500 ? " — <b>over the 500-row limit, split the file</b>" : ""}.</p>`;
      document.getElementById("intake-bulk-btn").disabled = !items.length || items.length > 500;
    };
    reader.readAsText(file);
  }

  async function bulkIntakeSubmit(btn) {
    const ref = (document.getElementById("intake-bulk-ref")?.value || "").trim();
    if (!ref) { UI.toast("batch reference required — it is the idempotency key", { kind: "warn" }); return; }
    const { items } = window._intakeBulk || { items: [] };
    if (!items.length) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.intakeBulk({ batch_ref: ref, items });
        const rows = (r.results || []).map((x) => `<tr>
          <td class="muted">${esc(x.external_ref || "")}</td>
          <td>${x.status === "CREATED" ? badge("CREATED") : `<span class="badge warn">ERROR</span>`}</td>
          <td class="muted">${esc(x.intake_id || "")}</td>
          <td class="muted">${esc(x.error || "")}</td></tr>`).join("");
        document.getElementById("intake-bulk-result").innerHTML = `<div class="card" style="margin-top:10px">
          <b>Batch ${esc(r.batch_ref || ref)}</b> — ${r.created} filed, ${r.errors} error(s)${r.idempotent_replay ? " — <b>idempotent replay</b>: this reference was already submitted; showing the recorded receipt, nothing re-filed" : ""}
          <table style="margin-top:8px"><thead><tr><th>Row</th><th>Outcome</th><th>Intake</th><th>Error</th></tr></thead><tbody>${rows}</tbody></table></div>`;
        UI.toast(r.idempotent_replay ? "Batch already filed — receipt replayed" : `Batch filed: ${r.created} created, ${r.errors} errors`,
          { kind: r.errors ? "warn" : "ok" });
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Filing batch…");
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


  // Log time from the thread — same POST /cases/{id}/time endpoint the case
  // screen's Time section uses; role stamping is server-side either way.
  async function asstTimeHtml(caseId) {
    const r = await Api.cases.timeList(caseId);
    const recent = (r.entries || []).slice(0, 10).map((e) => `<tr>
      <td>${esc(e.entry_date)}</td><td>${esc(e.subject)}</td><td>${badge(e.role)}</td>
      <td>${fmtMins(e.minutes)}</td><td class="muted">${esc(e.note || "")}</td></tr>`).join("");
    return `<div class="card"><h3>⏱ Time on this dispute — ${fmtMins(r.total_minutes)} logged</h3>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.asstTimeAdd('${caseId}', this)">
        <input name="hours" type="number" step="0.25" min="0.25" max="24" placeholder="hours" required />
        ${timeRoleSelect()}
        <input name="entry_date" type="date" />
        <input name="note" placeholder="what was done" maxlength="500" />
        <label><input type="checkbox" name="billable" checked /> billable</label>
        <button class="mini">Log</button></form>
      ${recent ? `<table><thead><tr><th>Date</th><th>Who</th><th>Role</th><th>Time</th><th>Note</th></tr></thead><tbody>${recent}</tbody></table>` : `<p class="muted">No entries yet.</p>`}
      <p class="muted">Append-only, recorded under your name and role — <a href="#/time">team weekly/monthly rollups ↗</a></p></div>`;
  }

  async function asstTimeAdd(caseId, form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.cases.timeAdd(caseId, {
          hours: parseFloat(form.hours.value),
          entry_date: form.entry_date.value || "",
          note: form.note.value.trim(),
          billable: form.billable.checked,
          role: form.role ? form.role.value : "",
        });
        asstAppend(caseId, "user", `Log ${form.hours.value}h${form.note.value.trim() ? " — " + form.note.value.trim() : ""}.`);
        asstAppend(caseId, "assistant", "Time logged against this dispute under your name and role. The entry is append-only — a correction is a compensating entry, never an edit.", "");
        const box = document.getElementById("asst-tools");
        if (box) box.innerHTML = await asstTimeHtml(caseId);
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Logging…");
  }

  // Correspondence compose: pre-fill the body with the program template's
  // standard text. Fills only when the body is empty or still holds another
  // template's untouched text — staff edits are never clobbered.
  let corrTplCache = [];
  function corrTemplateBody(sel) {
    const form = sel.form;
    const tp = corrTplCache.find((t) => t.key === sel.value);
    if (!tp || !tp.body || !form || !form.body) return;
    const ta = form.body;
    const untouched = corrTplCache.some((t) => t.body && t.body === ta.value);
    if (!ta.value.trim() || untouched) ta.value = tp.body;
  }

  // ---- Time entries (per-role effort, per dispute) ------------------------
  const fmtMins = (m) => `${(m / 60).toFixed(m % 60 ? 2 : 0)}h`;

  // The role is chosen PER ENTRY from the roles the user actually holds —
  // one person can code one entry and do QA the next. Blank = primary role.
  const timeRoleSelect = () => {
    const roles = (Auth.claims()?.roles || []);
    if (roles.length < 2) return "";
    return `<select name="role" aria-label="Role performed" title="Role being performed for this entry">
      <option value="">role: primary</option>
      ${roles.map((r) => `<option value="${esc(r)}">${esc(r)}</option>`).join("")}</select>`;
  };

  async function timeCaseBox(caseId) {
    try {
      const r = await Api.cases.timeList(caseId);
      const rows = (r.entries || []).map((e) => `<tr>
        <td>${esc(e.entry_date)}</td><td>${esc(e.subject)}</td><td>${badge(e.role)}</td>
        <td>${fmtMins(e.minutes)}</td><td>${e.billable ? "✓" : "—"}</td><td class="muted">${esc(e.note || "")}</td></tr>`).join("");
      return rows
        ? `<p><b>${fmtMins(r.total_minutes)}</b> logged on this dispute</p>
           <table><thead><tr><th>Date</th><th>Who</th><th>Role</th><th>Time</th><th>Billable</th><th>Note</th></tr></thead><tbody>${rows}</tbody></table>`
        : `<p class="muted">No time logged yet.</p>`;
    } catch (e) { return `<p class="muted">${esc(e.message)}</p>`; }
  }

  async function timeAdd(caseId, form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.cases.timeAdd(caseId, {
          hours: parseFloat(form.hours.value),
          entry_date: form.entry_date.value || "",
          note: form.note.value.trim(),
          billable: form.billable.checked,
          role: form.role ? form.role.value : "",
        });
        UI.toast("Time logged");
        form.hours.value = ""; form.note.value = "";
        const h = await timeCaseBox(caseId);
        const b = document.getElementById("time-box");
        if (b) b.innerHTML = h;
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Logging…");
  }

  // ---- My timesheet: the daily time-entry screen ------------------------
  // Entries across all disputes the user works on, with daily/weekly/monthly
  // totals. Entries are fully editable by their owner; the server keeps the
  // complete before/after history (audit trail / revisions).
  let tsCache = {}; // entry id -> entry (for inline editing)

  async function myTimesheet() {
    afterRender(() => myTimesheetBoxes());
    const today = new Date().toISOString().slice(0, 10);
    return `<div class="view-head"><h1>My timesheet</h1>
      <span class="muted">log the hours you work each day — totals roll up daily, weekly, and monthly</span></div>
      <div id="ts-summary" class="cards"></div>
      <div class="panel"><h2>Log time</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.myTimeAdd(this)">
          <select name="case_id" id="ts-case" required><option value="">dispute…</option></select>
          <input name="entry_date" type="date" value="${today}" required />
          <input name="hours" type="number" step="0.25" min="0.25" max="24" placeholder="hours" required style="width:6em" />
          ${timeRoleSelect()}
          <input name="note" placeholder="what you worked on" style="min-width:16em" />
          <label class="chk"><input name="billable" type="checkbox" checked /> billable</label>
          <button>Log</button></form></div>
      <div class="panel"><h2>My entries</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.myTimeRange(this)">
          <label>From <input name="from" type="date" /></label>
          <label>To <input name="to" type="date" /></label>
          <button class="mini">Apply</button>
          <button class="mini ghost" type="button" onclick="Views.myTimeRange(this.form, true)">Last 14 days</button></form>
        <div id="ts-list"><p class="muted">Loading…</p></div></div>`;
  }

  let tsFrom = "", tsTo = "";

  async function myTimesheetBoxes() {
    // Summary cards
    const sum = document.getElementById("ts-summary");
    if (sum) {
      try {
        const r = await Api.time.summary();
        sum.innerHTML = [["Today", r.today_minutes], ["This week", r.week_minutes], ["This month", r.month_minutes]]
          .map(([l, m]) => `<div class="card"><div class="card-n">${fmtMins(m || 0)}</div><div class="card-l">${l}</div></div>`).join("");
      } catch (e) { sum.innerHTML = ""; }
    }
    // Case picker for the log form
    const sel = document.getElementById("ts-case");
    if (sel && sel.options.length <= 1) {
      try {
        const r = await Api.cases.list({ limit: 200 });
        const cs = r.cases || [];
        sel.innerHTML = `<option value="">dispute…</option>` + cs.map((c) =>
          `<option value="${esc(c.id)}">${esc(c.case_number || c.id.slice(0, 8))}${c.title ? " — " + esc(c.title.slice(0, 40)) : ""}</option>`).join("");
      } catch (e) { /* leave placeholder */ }
    }
    await myTimeListBox();
  }

  function myTimeRange(form, reset) {
    if (reset) { tsFrom = ""; tsTo = ""; form.from.value = ""; form.to.value = ""; }
    else { tsFrom = form.from.value || ""; tsTo = form.to.value || ""; }
    myTimeListBox();
  }

  async function myTimeListBox() {
    const box = document.getElementById("ts-list");
    if (!box) return;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      const r = await Api.time.mine(tsFrom, tsTo);
      const entries = r.entries || [];
      tsCache = {};
      entries.forEach((e) => { tsCache[e.id] = e; });
      const totals = {};
      (r.day_totals || []).forEach((d) => { totals[String(d.entry_date).slice(0, 10)] = d.minutes; });
      let lastDay = null;
      const rows = entries.map((e) => {
        const day = String(e.entry_date).slice(0, 10);
        let head = "";
        if (day !== lastDay) {
          lastDay = day;
          head = `<tr class="day-head"><td colspan="7"><b>${esc(day)}</b> <span class="muted">— ${fmtMins(totals[day] || 0)} total</span></td></tr>`;
        }
        return head + `<tr id="ts-row-${e.id}">
          <td class="mono"><a href="#/cases/${e.case_id}">${esc(e.case_number || String(e.case_id).slice(0, 8))}</a></td>
          <td>${badge(e.role)}</td><td>${fmtMins(e.minutes)}</td>
          <td>${String(e.billable) === "true" || e.billable === true ? "✓" : "—"}</td>
          <td class="muted">${esc(e.note || "")}</td>
          <td><button class="mini ghost" onclick="Views.myTimeEditStart(${e.id})">Edit</button>
              <button class="mini ghost bad" onclick="Views.myTimeDel(${e.id}, this)">Delete</button></td></tr>`;
      }).join("");
      box.innerHTML = entries.length
        ? `<table><thead><tr><th>Dispute</th><th>Role</th><th>Time</th><th>Billable</th><th>Note</th><th></th></tr></thead><tbody>${rows}</tbody></table>
           <p class="muted">Showing ${esc(r.from)} → ${esc(r.to)} · ${entries.length} entr${entries.length === 1 ? "y" : "ies"}</p>`
        : `<p class="muted">No entries in this range yet — log your first day above.</p>`;
    } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
  }

  async function myTimeAdd(form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.cases.timeAdd(form.case_id.value, {
          hours: parseFloat(form.hours.value),
          entry_date: form.entry_date.value || "",
          note: form.note.value.trim(),
          billable: form.billable.checked,
          role: form.role ? form.role.value : "",
        });
        UI.toast("Time logged");
        form.hours.value = ""; form.note.value = "";
        myTimesheetBoxes();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Logging…");
  }

  function myTimeEditStart(id) {
    const e = tsCache[id];
    const row = document.getElementById(`ts-row-${id}`);
    if (!e || !row) return;
    row.innerHTML = `<td colspan="6"><form class="inline-form" onsubmit="event.preventDefault(); Views.myTimeSave(${id}, this)">
      <input name="entry_date" type="date" value="${esc(String(e.entry_date).slice(0, 10))}" required />
      <input name="hours" type="number" step="0.25" min="0.25" max="24" value="${(e.minutes / 60).toFixed(2)}" required style="width:6em" />
      ${timeRoleSelect()}
      <input name="note" value="${esc(e.note || "")}" style="min-width:14em" />
      <label class="chk"><input name="billable" type="checkbox" ${(e.billable === true || String(e.billable) === "true") ? "checked" : ""} /> billable</label>
      <button class="mini">Save</button>
      <button class="mini ghost" type="button" onclick="Views.myTimeListBox()">Cancel</button></form></td>`;
    const rs = row.querySelector('select[name="role"]');
    if (rs) rs.value = e.role || "";
  }

  async function myTimeSave(id, form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.time.update(id, {
          hours: parseFloat(form.hours.value),
          entry_date: form.entry_date.value || "",
          note: form.note.value.trim(),
          billable: form.billable.checked,
          role: form.role ? form.role.value : "",
        });
        UI.toast("Entry updated");
        myTimesheetBoxes();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Saving…");
  }

  async function myTimeDel(id, btn) {
    const e = tsCache[id];
    if (!confirm(`Delete this time entry (${e ? fmtMins(e.minutes) + " on " + String(e.entry_date).slice(0, 10) : id})? The change is kept in the audit history.`)) return;
    await UI.run(btn, async () => {
      try { await Api.time.remove(id); UI.toast("Entry deleted"); myTimesheetBoxes(); }
      catch (err) { UI.toast(err.message, { kind: "warn" }); }
    }, "Deleting…");
  }

  async function timeReport() {
    if (!can("CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Team time</h1></div><p class="muted">Requires CASE_MANAGER or FINANCE.</p>`;
    const now = new Date();
    const defMonth = now.toISOString().slice(0, 7);
    if (can("CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      afterRender(() => timeRatesBox());
    return `<div class="view-head"><h1>Team time</h1>
      <span class="muted">hours worked per dispute — weekly and monthly rollups for billing review</span></div>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.timeReportRun(this)">
        <label>Week containing <input name="week" type="date" /></label>
        <label>or month <input name="month" type="month" value="${defMonth}" /></label>
        <label>or range <input name="start" type="date" /> → <input name="end" type="date" /></label>
        <button>Run report</button></form>
      <div id="time-rpt"></div>
      ${can("CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `
      <div class="card"><h2>Send this report out</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.timeReportSend(this)">
          <input name="emails" placeholder="recipients (comma-separated emails)" required style="min-width:20em" />
          <input name="message" placeholder="optional message" style="min-width:14em" />
          <button class="mini">Email report</button>
          <button class="mini ghost" type="button" onclick="Views.timeReportCsv()">Download CSV</button></form>
        <p class="muted">Sends the period currently shown above — run the report first. Body carries the summary; the CSV attachment carries per-person and per-dispute detail with billable amounts.</p></div>` : ""}
      <div id="time-rates"></div>`;
  }

  // Email / export the team-hours report currently on screen.
  async function timeReportSend(form) {
    if (!window._timeRptOpts) { UI.toast("Run the report for a period first", { kind: "warn" }); return; }
    const emails = form.emails.value.split(",").map((x) => x.trim()).filter(Boolean);
    if (!emails.length) { UI.toast("At least one recipient", { kind: "warn" }); return; }
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.timeReportSend({ ...window._timeRptOpts, emails, message: form.message.value.trim() });
        UI.toast(`Report emailed to ${r.sent} recipient(s) — ${esc(r.period)}`);
        form.emails.value = ""; form.message.value = "";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Sending…");
  }

  function timeReportCsv() {
    const r = window._timeRpt;
    if (!r) { UI.toast("Run the report for a period first", { kind: "warn" }); return; }
    const money = r.total_amount_cents !== undefined;
    const hrs = (m) => (m / 60).toFixed(2);
    const cell = (v) => /[",\n]/.test(String(v)) ? `"${String(v).replace(/"/g, '""')}"` : String(v);
    const lines = [["section", "who_or_case", "hours", "billable_hours"].concat(money ? ["amount"] : []).join(",")];
    (r.by_person || []).forEach((p) => lines.push(["person", cell(p.case_number), hrs(p.minutes), hrs(p.billable_minutes)].concat(money ? [p.amount_cents != null ? (p.amount_cents / 100).toFixed(2) : ""] : []).join(",")));
    (r.by_case || []).forEach((c) => lines.push(["case", cell(c.case_number || c.case_id), hrs(c.minutes), hrs(c.billable_minutes)].concat(money ? [c.amount_cents != null ? (c.amount_cents / 100).toFixed(2) : ""] : []).join(",")));
    const blob = new Blob([lines.join("\n")], { type: "text/csv" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `timesheet-${(r.period || "report").replace(/\s+/g, "_")}.csv`;
    a.click();
    URL.revokeObjectURL(a.href);
  }

  async function timeRatesBox() {
    const box = document.getElementById("time-rates");
    if (!box) return;
    const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
    try {
      const r = await Api.program.timeRates();
      const rates = r.rates || [];
      const rows = rates.map((x) => `<tr>
        <td>${badge(x.role)}</td><td>${usd(x.rate_cents_per_hour)}/h</td>
        <td class="muted">${esc(x.updated_by)} · ${fmtDate(x.updated_at)}</td></tr>`).join("");
      const manage = r.can_manage
        ? `<form class="inline-form" onsubmit="event.preventDefault(); Views.timeRateSet(this)">
            <select name="role">${["DOCTOR","NURSE","CASE_MANAGER","ARBITRATOR","ATTORNEY","FINANCE","STATE_AUDITOR"].map((x) => `<option>${x}</option>`).join("")}</select>
            <input name="rate" type="number" step="0.01" min="0" placeholder="$/hour" required />
            <button class="mini">Set rate</button></form>`
        : "";
      box.innerHTML = `<h2>Billable rates by role</h2>
        ${rows ? `<table><thead><tr><th>Role</th><th>Rate</th><th>Last set</th></tr></thead><tbody>${rows}</tbody></table>`
               : `<p class="muted">No rates configured — reports show hours only until rates are set.</p>`}
        ${manage || `<p class="muted">Rates are set by CASE_MANAGER / administrators.</p>`}`;
    } catch (e) { box.innerHTML = ""; }
  }

  async function timeRateSet(form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.program.timeRateSet(form.role.value, Math.round(parseFloat(form.rate.value) * 100));
        UI.toast(`Rate set: ${form.role.value}`);
        form.rate.value = "";
        timeRatesBox();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Saving…");
  }

  async function timeReportRun(form) {
    const box = document.getElementById("time-rpt");
    const btn = form.querySelector("button");
    box.innerHTML = `<p class="muted">Computing…</p>`;
    await UI.run(btn, async () => {
      try {
        const opts = form.start.value && form.end.value
          ? { start: form.start.value, end: form.end.value }
          : form.week.value ? { week: form.week.value } : { month: form.month.value };
        window._timeRptOpts = opts;
        const r = await Api.program.timeReport(opts);
        window._timeRpt = r;
        const usd2 = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
        const money = r.total_amount_cents !== undefined;
        const caseRows = (r.by_case || []).map((c) => `<tr>
          <td class="mono"><a href="#/cases/${c.case_id}">${esc(c.case_number || c.case_id.slice(0, 8) + "…")}</a></td>
          <td>${fmtMins(c.minutes)}</td><td>${fmtMins(c.billable_minutes)}</td><td>${c.people}</td>
          ${money ? `<td>${c.amount_cents != null ? usd2(c.amount_cents) : "—"}${c.unrated_billable_minutes ? ` <span class="muted" title="billable minutes with no configured role rate">+${fmtMins(c.unrated_billable_minutes)} unrated</span>` : ""}</td>` : ""}</tr>`).join("");
        const personRows = (r.by_person || []).map((p) => `<tr>
          <td>${esc(p.case_number)}</td><td>${fmtMins(p.minutes)}</td><td>${fmtMins(p.billable_minutes)}</td>
          ${money ? `<td>${p.amount_cents != null ? usd2(p.amount_cents) : "—"}</td>` : ""}</tr>`).join("");
        box.innerHTML = `<h2>${esc(r.period)}</h2>
          <p><b>${fmtMins(r.total_minutes)}</b> total · <b>${fmtMins(r.billable_minutes)}</b> billable${money ? ` · <b>${usd2(r.total_amount_cents)}</b> billable amount` : ""}${money && r.unrated_billable_minutes ? ` · <span class="muted">${fmtMins(r.unrated_billable_minutes)} unrated</span>` : ""}</p>
          <h3>By dispute</h3>
          ${caseRows ? `<table><thead><tr><th>Case</th><th>Hours</th><th>Billable</th><th>People</th>${money ? "<th>Amount</th>" : ""}</tr></thead><tbody>${caseRows}</tbody></table>` : `<p class="muted">No time logged in this period.</p>`}
          <h3>By team member</h3>
          ${personRows ? `<table><thead><tr><th>Who</th><th>Hours</th><th>Billable</th>${money ? "<th>Amount</th>" : ""}</tr></thead><tbody>${personRows}</tbody></table>` : ""}`;
      } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
    }, "Computing…");
  }

  // ---------- Service-fee invoicing, AR/AP, reconciliation ----------
  const usdC = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
  const finRoles = ["CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"];

  async function caseFinBox(caseId) {
    try {
      const r = await Api.program.caseFinancials(caseId);
      const t = r.totals || {};
      const feeRows = (r.fee_invoices || []).map((i) => `<tr><td class="mono">${esc(i.invoice_no)}</td>
        <td>${esc(i.party)}</td><td>${esc(i.kind)}</td><td>${usdC(i.amount_cents)}</td>
        <td>${usdC(i.paid_cents)}</td><td>${badge(i.status)}</td></tr>`).join("");
      const svcRows = (r.service_lines || []).map((l) => `<tr><td class="mono">${esc(l.invoice_no)}</td>
        <td>${badge(l.role)}</td><td>${fmtMins(l.minutes)}</td><td>${usdC(l.amount_cents)}</td>
        <td>${badge(l.invoice_status)}</td></tr>`).join("");
      const svcPays = (r.service_payments || []).map((p) => `<tr><td class="mono">${esc(p.invoice_no)}</td>
        <td>${usdC(p.amount_cents)}</td><td>${esc(p.method)}</td><td class="mono">${esc(p.ref || "")}</td>
        <td>${esc((p.received_at || "").slice(0, 10))}</td></tr>`).join("");
      const apRows = (r.payables || []).map((p) => `<tr><td>${esc(p.payee)}</td><td>${badge(p.source)}</td>
        <td>${usdC(p.amount_cents)}</td><td>${badge(p.status)}</td><td class="mono">${esc(p.settle_ref || "")}</td></tr>`).join("");
      return `<div class="stat-grid">
          <div class="stat"><div class="stat-num">${usdC(t.ar_due_cents)}</div><div class="muted">AR due (fees)</div></div>
          <div class="stat"><div class="stat-num">${usdC(t.service_billed_cents)}</div><div class="muted">Service billed</div></div>
          <div class="stat"><div class="stat-num">${usdC(t.collected_total_cents)}</div><div class="muted">Collected total</div></div>
          <div class="stat"><div class="stat-num">${usdC(t.ap_open_cents)}</div><div class="muted">AP open (awards…)</div></div></div>
        <h3>Fee invoices (payer/provider)</h3>
        ${feeRows ? `<table class="tbl"><thead><tr><th>Invoice</th><th>Party</th><th>Kind</th><th>Amount</th><th>Paid</th><th>Status</th></tr></thead><tbody>${feeRows}</tbody></table>` : '<p class="muted">No fee invoices on this dispute.</p>'}
        <h3>Service-fee lines</h3>
        ${svcRows ? `<table class="tbl"><thead><tr><th>Invoice</th><th>Role</th><th>Time</th><th>Amount</th><th>Status</th></tr></thead><tbody>${svcRows}</tbody></table>` : '<p class="muted">No service billing on this dispute.</p>'}
        ${svcPays ? `<h3>Service payments allocated to this dispute</h3><table class="tbl"><thead><tr><th>Invoice</th><th>Allocated</th><th>Method</th><th>Ref</th><th>Received</th></tr></thead><tbody>${svcPays}</tbody></table>` : ""}
        <h3>Payables</h3>
        ${apRows ? `<table class="tbl"><thead><tr><th>Payee</th><th>Source</th><th>Amount</th><th>Status</th><th>Settle ref</th></tr></thead><tbody>${apRows}</tbody></table>` : '<p class="muted">No obligations recorded.</p>'}`;
    } catch (e) { return `<p class="error">${esc(e.message)}</p>`; }
  }

  // ===== Invoices view (#/billing) =====
  function billingInvoices() {
    if (!can(...finRoles))
      return `<div class="view-head"><h1>Billing</h1></div><p class="muted">Requires CASE_MANAGER or FINANCE.</p>`;
    afterRender(() => billingList());
    const now = new Date().toISOString();
    return `<div class="view-head"><h1>Billing — service-fee invoices</h1>
      <span class="muted">generated from the time ledger × role rates; every dollar ties back to disputes</span></div>
      <div class="card"><h2>Generate</h2>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.billingGen(this)">
        <label>Week <input name="week" type="date" /></label>
        <label>Month <input name="month" type="month" value="${now.slice(0, 7)}" /></label>
        <label>Range <input name="start" type="date" /> → <input name="end" type="date" /></label>
        <label><input type="checkbox" name="percase" /> per-case invoices</label>
        <button>Generate DRAFT</button></form>
      <p class="muted">Idempotent per period — an existing active invoice for the same period is reported, never duplicated. Billable time without a role rate blocks generation (configurable).</p>
      <div id="billing-gen-out"></div></div>
      <div id="billing-list"><p class="muted">Loading…</p></div>
      <div id="billing-detail"></div>`;
  }

  async function billingGen(form) {
    const out = document.getElementById("billing-gen-out");
    const params = form.start.value && form.end.value ? { start: form.start.value, end: form.end.value }
      : form.week.value ? { week: form.week.value } : { month: form.month.value };
    out.innerHTML = `<p class="muted">Generating…</p>`;
    try {
      const r = await Api.program.billingGenerate(params, { consolidate: !form.percase.checked });
      const made = (r.invoices || []).map((i) => `<div>✅ <b class="mono">${esc(i.invoice_no)}</b> — ${usdC(i.total_cents)} · ${i.lines} line(s) ${i.case_id ? `· case ${esc(i.case_id.slice(0, 8))}…` : "· consolidated"}</div>`).join("");
      const skipped = (r.skipped || []).map((x) => `<div class="muted">⏭ ${esc(x.case_id || "consolidated")}: ${esc(x.reason)} ${x.existing_invoice_no ? `(existing <b class="mono">${esc(x.existing_invoice_no)}</b>)` : ""}</div>`).join("");
      const unrated = (r.excluded_unrated || []).map((u) => `<div class="muted">⚠ unrated: ${esc(u.role)} ${fmtMins(u.billable_minutes)} on ${esc((u.case_id || "").slice(0, 8))}…</div>`).join("");
      out.innerHTML = `<p><b>${esc(r.period)}</b></p>${made}${skipped}${unrated}` || `<p class="muted">No billable time in period.</p>`;
      billingList();
    } catch (e) {
      const unr = e.data && e.data.unrated ? e.data.unrated.map((u) => `<div>⚠ ${esc(u.role)} — ${fmtMins(u.billable_minutes)} (case ${esc((u.case_id || "").slice(0, 8))}…)</div>`).join("") : "";
      out.innerHTML = `<p class="error">${esc(e.message)}</p>${unr}`;
    }
  }

  async function billingList(status) {
    const box = document.getElementById("billing-list");
    if (!box) return;
    try {
      const r = await Api.program.billingList(status ? { status } : {});
      const rows = (r.invoices || []).map((i) => {
        const due = Number(i.total_cents) - Number(i.paid_cents || 0);
        let actions = "";
        if (i.status === "DRAFT" && can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
          actions += `<button class="mini" onclick="Views.billingActFn('${i.id}','approve')">Approve</button> `;
        if (i.status === "APPROVED" && can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
          actions += `<button class="mini" onclick="Views.billingActFn('${i.id}','issue')">Issue</button> `;
        if (["DRAFT", "APPROVED", "ISSUED"].includes(i.status) && can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
          actions += `<button class="mini danger" onclick="Views.billingActFn('${i.id}','void')">Void</button> `;
        if (i.status === "ISSUED" && can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
          actions += `<button class="mini" onclick="Views.billingPayForm('${i.id}')">Record payment</button> `;
        actions += `<button class="mini" onclick="Views.billingDetail('${i.id}')">Detail</button>
          <button class="mini" onclick="Views.billingExportFn('${i.id}')">CSV</button>`;
        return `<tr><td class="mono">${esc(i.invoice_no)}</td>
          <td>${esc(i.period_start)} → ${esc(i.period_end)}</td>
          <td>${esc(i.bill_to_name || "")}</td><td>${usdC(i.total_cents)}</td>
          <td>${i.status === "ISSUED" ? usdC(due) : "—"}</td>
          <td>${badge(i.status)}</td><td>${actions}</td></tr>`;
      }).join("");
      box.innerHTML = `<h2>Invoices</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.billingFilter(this)">
          <select name="status"><option value="">all statuses</option>${["DRAFT", "APPROVED", "ISSUED", "PAID", "VOID"].map((x) => `<option>${x}</option>`).join("")}</select>
          <button class="mini">Filter</button></form>
        ${rows ? `<table class="tbl"><thead><tr><th>Invoice</th><th>Period</th><th>Bill to</th><th>Total</th><th>Due</th><th>Status</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : '<p class="muted">No invoices yet — generate one above.</p>'}`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function billingActFn(id, action) {
    if (action === "void" && !confirm("Void this invoice?")) return;
    try { await Api.program.billingAct(id, action); billingList(); }
    catch (e) { alert(e.message); }
  }

  async function billingDetail(id) {
    const box = document.getElementById("billing-detail");
    try {
      const r = await Api.program.billingGet(id);
      const inv = r.invoice || {};
      const lines = (r.lines || []).map((l) => `<tr><td class="mono">${esc(l.case_number || "")}</td>
        <td>${badge(l.role)}</td><td>${fmtMins(l.minutes)}</td><td>${usdC(l.rate_cents_per_hour)}/h</td>
        <td>${usdC(l.amount_cents)}</td><td>${esc(l.description)}</td></tr>`).join("");
      const pays = (r.payments || []).map((p) => `<tr><td>${usdC(p.amount_cents)}</td><td>${esc(p.method)}</td>
        <td class="mono">${esc(p.ref || "")}</td><td>${esc((p.received_at || "").slice(0, 10))}</td><td>${esc(p.recorded_by)}</td></tr>`).join("");
      const evs = (r.events || []).map((e) => `<div class="muted">${esc(e.event)} · ${esc(e.actor)} · ${fmtDate(e.created_at)}</div>`).join("");
      box.innerHTML = `<div class="card"><h2 class="mono">${esc(inv.invoice_no)}</h2>
        <p>${badge(inv.status)} ${esc(inv.bill_to_name || "")} · due ${esc(inv.due_date || "—")} · ${esc(inv.memo || "")}</p>
        <table class="tbl"><thead><tr><th>Case</th><th>Role</th><th>Time</th><th>Rate</th><th>Amount</th><th>Description</th></tr></thead><tbody>${lines}</tbody></table>
        <p><b>Subtotal ${usdC(inv.subtotal_cents)} · Tax ${usdC(inv.tax_cents)} · Total ${usdC(inv.total_cents)}</b></p>
        ${pays ? `<h3>Payments</h3><table class="tbl"><thead><tr><th>Amount</th><th>Method</th><th>Ref</th><th>Received</th><th>By</th></tr></thead><tbody>${pays}</tbody></table>` : ""}
        <div id="pay-form-${id}"></div><h3>Trail</h3>${evs}</div>`;
      box.scrollIntoView({ behavior: "smooth" });
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function billingExportFn(id) {
    try { await Api.program.billingExport(id); } catch (e) { alert(e.message); }
  }

  function billingPayForm(id) {
    const box = document.getElementById(`pay-form-${id}`) || document.getElementById("billing-detail");
    box.insertAdjacentHTML("beforeend", `<form class="inline-form" onsubmit="event.preventDefault(); Views.billingPayRun('${id}', this)">
      <input name="amount" type="number" step="0.01" min="0.01" placeholder="amount $" required />
      <select name="method"><option>ach</option><option>wire</option><option>check</option><option>card</option></select>
      <input name="ref" placeholder="remittance / bank ref" />
      <input name="received_at" type="date" />
      <button class="mini">Record payment</button></form>
      <p class="muted">Split across the invoice's disputes automatically (proportional), so collections roll up per case.</p>`);
  }

  async function billingPayRun(id, form) {
    try {
      await Api.program.billingPay(id, {
        amount_cents: Math.round(Number(form.amount.value) * 100),
        method: form.method.value, ref: form.ref.value,
        received_at: form.received_at.value || undefined,
      });
      billingList(); billingDetail(id);
    } catch (e) { alert(e.message); }
  }

  // ===== AR/AP view (#/arap) =====
  function arapView() {
    if (!can(...finRoles))
      return `<div class="view-head"><h1>AR / AP</h1></div><p class="muted">Requires CASE_MANAGER or FINANCE.</p>`;
    afterRender(() => { arapSummaryBox(); arapArBox(); arapApBox(); });
    return `<div class="view-head"><h1>Receivables & payables</h1>
      <span class="muted">AR derived live from invoice stores; AP obligations recorded and settled here</span></div>
      <div id="arap-summary"></div>
      <div class="card"><h2>Accounts receivable — aging</h2><div id="arap-ar"><p class="muted">Loading…</p></div></div>
      <div class="card"><h2>Accounts payable</h2>
        ${can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<form class="inline-form" onsubmit="event.preventDefault(); Views.arapRecord(this)">
          <input name="payee" placeholder="payee" required />
          <input name="amount" type="number" step="0.01" min="0.01" placeholder="amount $" required />
          <select name="source"><option>award</option><option>refund</option><option>vendor</option><option>tax</option><option>other</option></select>
          <input name="case_id" placeholder="case id (optional)" />
          <input name="due_date" type="date" />
          <button class="mini">Record obligation</button></form>` : ""}
        ${can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<p><button class="mini" onclick="Views.arapNacha()">Generate ACH (NACHA) payout file</button>
          <span class="muted"> from open payables that carry bank destinations</span></p>` : ""}
        <div id="arap-ap"><p class="muted">Loading…</p></div>
        <div id="arap-nacha"></div></div>`;
  }

  async function arapSummaryBox() {
    const box = document.getElementById("arap-summary");
    try {
      const r = await Api.program.arapSummary();
      box.innerHTML = `<div class="stat-grid">
        <div class="stat"><div class="stat-num">${usdC(r.ar && r.ar.total_cents)}</div><div class="muted">Open receivables</div></div>
        <div class="stat"><div class="stat-num">${usdC(r.ap && r.ap.open_cents)}</div><div class="muted">Open payables</div></div>
        <div class="stat"><div class="stat-num">${usdC(r.net_position_cents)}</div><div class="muted">Net position</div></div></div>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  function agingTable(rows, partyLabel) {
    if (!rows || !rows.length) return '<p class="muted">Nothing open.</p>';
    return `<table class="tbl"><thead><tr><th>${partyLabel}</th><th>Ref</th><th>Case</th><th>Amount</th><th>Due</th><th>Basis</th><th>Days</th><th>Bucket</th></tr></thead><tbody>
      ${rows.map((x) => `<tr class="${x.bucket === "90+" ? "row-breach" : ""}">
        <td>${esc(x.party)}</td><td class="mono">${esc(x.number || x.kind || "")}</td>
        <td>${x.case_id ? `<a href="#/cases/${x.case_id}">${esc(x.case_id.slice(0, 8))}…</a>` : "—"}</td>
        <td>${usdC(x.amount_cents)}</td><td><b>${usdC(x.due_cents)}</b></td>
        <td>${esc(x.basis_date)}</td><td>${x.days_outstanding}</td><td>${badge(x.bucket)}</td></tr>`).join("")}</tbody></table>`;
  }

  async function arapArBox() {
    const box = document.getElementById("arap-ar");
    try {
      const r = await Api.program.receivablesAging();
      const ag = r.aging || {};
      box.innerHTML = `<p>${["current", "1-30", "31-60", "61-90", "90+"].map((b) => `<span class="chip">${b}: <b>${usdC(ag[b] || 0)}</b></span> `).join("")}</p>
        ${agingTable(r.receivables, "Owes")}`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function arapApBox() {
    const box = document.getElementById("arap-ap");
    try {
      const r = await Api.program.payablesList();
      const rows = (r.payables || []).map((x) => {
        const canWrite = can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
        const acts = canWrite ? `<button class="mini" onclick="Views.arapSettle('${x.id}')">Settle</button>
          <button class="mini danger" onclick="Views.arapVoid('${x.id}')">Void</button>` : "";
        return `<tr><td>${esc(x.party)}</td><td>${badge(x.kind)}</td><td>${usdC(x.amount_cents)}</td>
          <td>${x.case_id ? `<a href="#/cases/${x.case_id}">${esc(x.case_id.slice(0, 8))}…</a>` : "—"}</td>
          <td>${esc(x.basis_date)}</td><td>${x.days_outstanding}</td><td>${badge(x.bucket)}</td><td>${acts}</td></tr>`;
      }).join("");
      const ag = r.aging || {};
      box.innerHTML = `<p>${["current", "1-30", "31-60", "61-90", "90+"].map((b) => `<span class="chip">${b}: <b>${usdC(ag[b] || 0)}</b></span> `).join("")}</p>
        ${rows ? `<table class="tbl"><thead><tr><th>Payee</th><th>Source</th><th>Amount</th><th>Case</th><th>Basis</th><th>Days</th><th>Bucket</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : '<p class="muted">No open obligations.</p>'}`;
      nachaBatchesBox();
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  function saveTextFile(name, text) {
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
    a.download = name; a.click(); URL.revokeObjectURL(a.href);
  }

  async function arapNacha() {
    const box = document.getElementById("arap-nacha");
    try {
      const r = await Api.program.nachaPayout({});
      saveTextFile(r.filename, r.file);
      box.innerHTML = `<p class="ok">Batch ${esc(r.batch_id.slice(0, 8))}… — ${r.entry_count} entries, ${usdC(r.total_cents)} — file <b>${esc(r.filename)}</b> downloaded. Upload it to your bank portal.</p>`;
      arapApBox();
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function nachaBatchesBox() {
    const box = document.getElementById("arap-nacha");
    if (!box || box.dataset.busy) return;
    try {
      const r = await Api.program.payoutBatches();
      const bs = r.batches || [];
      if (!bs.length) return;
      box.innerHTML = `<h3>ACH payout batches</h3>` + bs.map((b) => `<span class="chip">${esc(b.file_reference)} · ${b.entry_count} entries · ${usdC(b.total_cents)} · ${badge(b.status)}
        ${can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<a href="#" onclick="event.preventDefault(); Views.arapNachaFile('${b.id}')">download</a>` : ""}</span> `).join(" ");
    } catch { /* batches are supplementary */ }
  }

  async function arapNachaFile(id) {
    try {
      const r = await Api.program.payoutBatchFile(id);
      saveTextFile(r.filename, r.file);
    } catch (e) { alert(e.message); }
  }

  async function arapRecord(form) {
    try {
      await Api.program.payableCreate({
        payee: form.payee.value, amount_cents: Math.round(Number(form.amount.value) * 100),
        source: form.source.value, case_id: form.case_id.value || undefined,
        due_date: form.due_date.value || undefined,
      });
      form.reset(); arapApBox(); arapSummaryBox();
    } catch (e) { alert(e.message); }
  }

  async function arapSettle(id) {
    const method = prompt("Settle via (ach/wire/check):", "ach");
    if (!method) return;
    const ref = prompt("Remittance / bank reference (recon matches on this):", "");
    try { await Api.program.payableAct(id, "settle", { method, ref: ref || "" }); arapApBox(); arapSummaryBox(); }
    catch (e) { alert(e.message); }
  }

  async function arapVoid(id) {
    if (!confirm("Void this obligation?")) return;
    try { await Api.program.payableAct(id, "void", {}); arapApBox(); arapSummaryBox(); }
    catch (e) { alert(e.message); }
  }

  // ===== Reconciliation view (#/recon) =====
  function reconView() {
    if (!can(...finRoles))
      return `<div class="view-head"><h1>Reconciliation</h1></div><p class="muted">Requires CASE_MANAGER or FINANCE.</p>`;
    afterRender(() => reconBatchList());
    const canWrite = can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    return `<div class="view-head"><h1>Reconciliation</h1>
      <span class="muted">match accounting/bank feeds against platform money records — exact ref, then amount within tolerance and date window; ambiguity always goes to a human</span></div>
      ${canWrite ? `<div class="card"><h2>Import external feed</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.reconImportRun(this)">
          <select name="mapping"><option value="quickbooks">QuickBooks CSV</option><option value="xero">Xero CSV</option>
            <option value="sage">Sage CSV</option><option value="bank_generic">Bank (generic CSV)</option></select>
          <input name="file" type="file" accept=".csv" required />
          <button class="mini">Import</button></form>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.reconFetchRun(this)">
          <input name="feed" placeholder="configured http_json feed name" />
          <button class="mini">Fetch from accounting platform</button>
          <span class="muted">feeds are configured per tenant (reconciliation.http_feeds)</span></form>
        <div id="recon-import-out"></div></div>` : ""}
      <div id="recon-batches"><p class="muted">Loading…</p></div>
      <div id="recon-detail"></div>`;
  }

  async function reconImportRun(form) {
    const out = document.getElementById("recon-import-out");
    out.innerHTML = `<p class="muted">Importing…</p>`;
    try {
      const r = await Api.program.reconImportCsv(form.mapping.value, form.file.files[0]);
      out.innerHTML = `<p>✅ Batch #${r.batch_id} — ${r.items} items (${esc(r.period)}). Running auto-match…</p>`;
      const m = await Api.program.reconMatch(r.batch_id);
      out.innerHTML += `<p>Matched <b>${m.matched}</b> · exceptions <b>${m.exceptions}</b> · unmatched <b>${m.unmatched}</b></p>`;
      reconBatchList();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function reconFetchRun(form) {
    const out = document.getElementById("recon-import-out");
    try {
      const r = await Api.program.reconFetch(form.feed.value);
      out.innerHTML = `<p>✅ Batch #${r.batch_id} — ${r.items} items. Running auto-match…</p>`;
      const m = await Api.program.reconMatch(r.batch_id);
      out.innerHTML += `<p>Matched <b>${m.matched}</b> · exceptions <b>${m.exceptions}</b> · unmatched <b>${m.unmatched}</b></p>`;
      reconBatchList();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function reconBatchList() {
    const box = document.getElementById("recon-batches");
    if (!box) return;
    try {
      const r = await Api.program.reconBatches();
      const rows = (r.batches || []).map((b) => `<tr><td>#${b.id}</td><td>${esc(b.source)}</td>
        <td>${esc(b.period_start)} → ${esc(b.period_end)}</td><td>${b.total_items}</td>
        <td>${b.matched}</td><td>${b.exceptions}</td><td>${b.unmatched}</td><td>${badge(b.status)}</td>
        <td><button class="mini" onclick="Views.reconOpen(${b.id})">Open</button>
        ${can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<button class="mini" onclick="Views.reconMatchRun(${b.id})">Re-run match</button>` : ""}</td></tr>`).join("");
      box.innerHTML = `<h2>Batches</h2>${rows ? `<table class="tbl"><thead><tr><th></th><th>Source</th><th>Period</th><th>Items</th><th>Matched</th><th>Exceptions</th><th>Unmatched</th><th>Status</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : '<p class="muted">No imports yet.</p>'}`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function reconMatchRun(id) {
    try { const m = await Api.program.reconMatch(id);
      alert(`matched ${m.matched} · exceptions ${m.exceptions} · unmatched ${m.unmatched}`);
      reconBatchList(); reconOpen(id);
    } catch (e) { alert(e.message); }
  }

  async function reconOpen(id) {
    const box = document.getElementById("recon-detail");
    try {
      const r = await Api.program.reconBatch(id);
      const rows = (r.items || []).map((it) => {
        let act = "";
        if (can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") && it.status !== "MATCHED" && it.status !== "IGNORED")
          act = `<button class="mini" onclick="Views.reconResolveFn(${it.id}, 'match')">Match…</button>
            <button class="mini" onclick="Views.reconResolveFn(${it.id}, 'exception')">Exception</button>
            <button class="mini" onclick="Views.reconResolveFn(${it.id}, 'ignore')">Ignore</button>`;
        return `<tr class="${it.status === "EXCEPTION" ? "row-breach" : ""}">
          <td>${esc(it.txn_date)}</td><td>${usdC(it.amount_cents)}</td><td class="mono">${esc(it.reference || "")}</td>
          <td>${esc(it.description || "")}</td><td>${badge(it.status)}</td>
          <td>${it.platform_kind ? `${esc(it.platform_kind)} ${usdC(it.platform_amount_cents)}` : ""} <span class="muted">${esc(it.match_kind || "")}</span></td>
          <td class="muted">${esc(it.note || "")}</td><td>${act}</td></tr>`;
      }).join("");
      box.innerHTML = `<div class="card"><h2>Batch #${id} — ${esc(r.batch.source)}</h2>
        <table class="tbl"><thead><tr><th>Date</th><th>Amount</th><th>Reference</th><th>Description</th><th>Status</th><th>Platform record</th><th>Note</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`;
      box.scrollIntoView({ behavior: "smooth" });
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function reconResolveFn(itemId, action) {
    try {
      if (action === "match") {
        const ev = prompt("Platform financial_event id to match (amounts must tie):");
        if (!ev) return;
        await Api.program.reconResolve(itemId, { action: "match", event_id: Number(ev) });
      } else if (action === "exception") {
        const note = prompt("Exception note (required):");
        if (!note) return;
        await Api.program.reconResolve(itemId, { action: "exception", note });
      } else {
        await Api.program.reconResolve(itemId, { action });
      }
      const det = document.querySelector("#recon-detail h2");
      if (det) { const m = det.textContent.match(/#(\d+)/); if (m) reconOpen(Number(m[1])); }
      reconBatchList();
    } catch (e) { alert(e.message); }
  }

  // ── Third-party administrators ─────────────────────────────────────
  // ---- Mail log: every inbound + outbound mail, referable ---------------
  function mailLog() {
    if (!can("CASE_MANAGER", "FINANCE", "ARBITRATOR", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"))
      return `<div class="view-head"><h1>Mail log</h1></div><p class="muted">Requires a staff role.</p>`;
    afterRender(mailLogBox);
    return `<div class="view-head"><h1>Mail log</h1>
      <span class="muted">every inbound and outbound mail across the platform — including failed deliveries and mail that matched no case</span></div>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.mailLogFilter(this)">
        <select name="direction"><option value="">IN + OUT</option><option value="IN">Inbound</option><option value="OUT">Outbound</option></select>
        <input name="q" placeholder="search subject / body / case number" style="min-width:18em" />
        <button class="mini">Search</button></form>
      <div id="mail-log"><p class="muted">Loading…</p></div>`;
  }

  let mailLogFilterState = {};

  function mailLogFilter(form) {
    mailLogFilterState = { direction: form.direction.value || "", q: form.q.value.trim() || "" };
    mailLogBox();
  }

  async function mailLogBox() {
    const box = document.getElementById("mail-log");
    if (!box) return;
    try {
      const r = await Api.program.mailJournal(mailLogFilterState);
      const ms = r.mail || [];
      const rows = ms.map((m) => {
        const rc = m.recipients || {};
        const who = m.direction === "IN" ? (rc.from || m.sent_by || "") : (rc.to || []).join(", ");
        return `<tr>
          <td>${badge(m.direction)}</td>
          <td>${m.case_id ? `<a href="#/cases/${m.case_id}">${esc(m.case_number || "case")}</a>` : `<span class="muted" title="pre-case or unmatched mail">—</span>`}</td>
          <td>${esc(m.subject || "")}${m.template && m.template !== "inbound" ? ` <span class="muted">(${esc(m.template)})</span>` : ""}</td>
          <td class="muted">${esc(who)}</td>
          <td>${m.delivery_status === "FAILED" ? `<span class="badge warn" title="${esc(m.delivery_error || "")}">FAILED</span>` : ""}</td>
          <td class="muted">${fmtDate(m.created_at)}</td>
          <td><button class="mini ghost" onclick="Views.mailLogBody(${m.id})">View</button></td></tr>
          <tr id="mail-body-${m.id}" style="display:none"><td colspan="7"><pre style="white-space:pre-wrap;max-height:16em;overflow:auto">${esc(m.body || "")}</pre></td></tr>`;
      }).join("");
      box.innerHTML = ms.length
        ? `<table><thead><tr><th>Dir</th><th>Case</th><th>Subject</th><th>From / To</th><th>Delivery</th><th>When</th><th></th></tr></thead><tbody>${rows}</tbody></table>`
        : `<p class="muted">No mail recorded for this filter.</p>`;
    } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
  }

  function mailLogBody(id) {
    const row = document.getElementById(`mail-body-${id}`);
    if (row) row.style.display = row.style.display === "none" ? "" : "none";
  }

  function tpaView() {
    if (!can("TPA", "CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Third-party administrators</h1></div><p class="muted">Requires a TPA, CASE_MANAGER, or FINANCE account.</p>`;
    afterRender(tpaBoot);
    return `<div class="view-head"><h1>Third-party administrators</h1>
      <span class="muted">File and track disputes on behalf of your initiating parties</span></div>
      <div id="tpa-org"></div>
      <div id="tpa-body" style="display:none">
        <div class="card"><h2>Initiating parties (clients)</h2>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.tpaAddClientFn(this)">
            <input name="party_name" placeholder="organization name" required />
            <input name="party_type" placeholder="party type code (manifest)" style="text-transform:uppercase" />
            <input name="contact_email" type="email" placeholder="contact email" />
            <button class="mini">Add</button></form>
          <p class="muted">Party type codes come from the tenant manifest (e.g. PROVIDER / HEALTH_PLAN).</p>
          <div id="tpa-clients"><p class="muted">Loading…</p></div></div>
        <div class="card"><h2>File a dispute on behalf of a client</h2>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.tpaFileFn(this)">
            <select name="client_id" id="tpa-file-client" required></select>
            <input name="notes" placeholder="notes" />
            <button class="mini">Open intake</button></form>
          <p class="muted">The intake opens with your TPA as the contact — payment and document requests come to you on behalf of the initiating party; staff advance it like any other intake.</p>
          <div id="tpa-file-result"></div></div>
        <div class="card"><h2>Bulk upload — file many disputes at once</h2>
          <p class="muted">CSV with header row: <code>email</code> (required), <code>contact_name</code>, <code>org</code>, <code>amount</code>, <code>filing_party_type</code> (PROVIDER/HEALTH_PLAN), <code>external_ref</code>, <code>notes</code>. Up to 500 rows per batch.</p>
          <form class="inline-form" onsubmit="event.preventDefault()">
            <input id="tpa-bulk-ref" placeholder="batch reference (your idempotency key)" required style="min-width:16em" />
            <input id="tpa-bulk-file" type="file" accept=".csv,text/csv" />
            <button class="mini" id="tpa-bulk-btn" disabled>File batch</button></form>
          <div id="tpa-bulk-preview"></div><div id="tpa-bulk-result"></div></div>
        <div class="card"><h2>My batch filings — status</h2>
          <div id="tpa-batches"><p class="muted">Loading…</p></div></div>
        <div class="card"><h2>Tracking — all your parties' filings</h2>
          <div id="tpa-rollup"></div><div id="tpa-cases"><p class="muted">Loading…</p></div></div>
      </div>
      ${can("CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `
      <div class="card"><h2>State oversight — all TPAs</h2><div id="tpa-admin"><p class="muted">Loading…</p></div></div>` : ""}`;
  }

  async function tpaBoot() {
    const orgBox = document.getElementById("tpa-org");
    try {
      const r = await Api.program.tpaMe();
      orgBox.innerHTML = `<div class="stat-grid"><div class="stat"><div class="stat-num">${esc(r.tpa.name)}</div>
        <div class="muted">${esc(r.tpa.contact_email)} · ${badge(r.tpa.status)}</div></div></div>`;
      document.getElementById("tpa-body").style.display = "";
      tpaClientsBox(); tpaDashBox(); tpaBatchesBox();
      window._tpaBulk = { items: [] };
      const bref = document.getElementById("tpa-bulk-ref");
      if (bref && !bref.value) bref.value = "TPA-" + new Date().toISOString().slice(0, 16).replace(/[-:T]/g, "");
      $("#tpa-bulk-file")?.addEventListener("change", (ev) => tpaBulkFile(ev.target));
      $("#tpa-bulk-btn")?.addEventListener("click", (ev) => { ev.preventDefault(); tpaBulkSubmit(ev.target); });
    } catch (e) {
      orgBox.innerHTML = `<div class="card"><h2>Link your TPA organization</h2>
        <p class="muted">Enter the one-time claim code from your TPA registration.</p>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.tpaClaimFn(this)">
          <input name="code" placeholder="TPA-XXXXXX" required /><button class="mini">Claim</button></form></div>`;
    }
    if (can("CASE_MANAGER", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) tpaAdminBox();
  }

  async function tpaClaimFn(form) {
    try { await Api.program.tpaClaim(form.code.value.trim()); tpaBoot(); }
    catch (e) { alert(e.message); }
  }

  async function tpaClientsBox() {
    const box = document.getElementById("tpa-clients");
    try {
      const r = await Api.program.tpaClients();
      const cs = r.clients || [];
      box.innerHTML = cs.length ? `<table class="tbl"><thead><tr><th>Party</th><th>Type</th><th>Email</th><th>Filings</th><th>Status</th><th></th></tr></thead><tbody>
        ${cs.map((c) => `<tr><td>${esc(c.party_name)}</td><td>${badge(c.party_type)}</td><td>${esc(c.contact_email || "—")}</td>
          <td>${c.intake_count}</td><td>${badge(c.status)}</td>
          <td><button class="mini" onclick="Views.tpaClientStatusFn('${c.id}','${c.status === "ACTIVE" ? "SUSPENDED" : "ACTIVE"}')">${c.status === "ACTIVE" ? "Suspend" : "Reactivate"}</button></td></tr>`).join("")}</tbody></table>` : '<p class="muted">No initiating parties yet — add one above.</p>';
      const sel = document.getElementById("tpa-file-client");
      if (sel) sel.innerHTML = cs.filter((c) => c.status === "ACTIVE").map((c) => `<option value="${c.id}">${esc(c.party_name)} (${esc(c.party_type)})</option>`).join("");
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaAddClientFn(form) {
    try {
      await Api.program.tpaAddClient({ party_name: form.party_name.value, party_type: (form.party_type.value || "").toUpperCase(), contact_email: form.contact_email.value });
      form.reset(); tpaClientsBox();
    } catch (e) { alert(e.message); }
  }

  async function tpaClientStatusFn(id, status) {
    try { await Api.program.tpaClientStatus(id, status); tpaClientsBox(); }
    catch (e) { alert(e.message); }
  }

  async function tpaFileFn(form) {
    const out = document.getElementById("tpa-file-result");
    try {
      const r = await Api.program.tpaIntake({ client_id: form.client_id.value, notes: form.notes.value, fields: {} });
      out.innerHTML = `<p class="ok">Intake <span class="mono">${esc(r.intake_id.slice(0, 8))}…</span> opened (${esc(r.status)}) for <b>${esc(r.initiating_party)}</b> — filed by ${esc(r.filed_by)}.</p>`;
      form.reset(); tpaDashBox(); tpaClientsBox();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaDashBox(tpaId) {
    const roll = document.getElementById("tpa-rollup"), box = document.getElementById("tpa-cases");
    try {
      const r = await Api.program.tpaDashboard(tpaId);
      roll.innerHTML = (r.by_client || []).map((x) => `<span class="chip">${esc(x.initiating_party)}: <b>${x.filings}</b> filings (${x.converted} converted) · paid ${usdC(x.paid_cents)}</span> `).join("");
      const rows = (r.filings || []).map((x) => `<tr><td>${esc(x.client_name)}</td><td>${badge(x.initiating_party_type)}</td>
        <td>${x.case_id ? `<a href="#/cases/${x.case_id}">${esc(x.case_number || String(x.case_id).slice(0, 8))}</a>` : "—"}</td>
        <td>${badge(x.case_status || x.intake_status)}</td><td>${usdC(x.disputed_amount_cents)}</td><td>${usdC(x.paid_cents)}</td><td>${esc(x.filed)}</td></tr>`).join("");
      box.innerHTML = rows ? `<table class="tbl"><thead><tr><th>Initiating party</th><th>Type</th><th>Case</th><th>Status</th><th>Disputed</th><th>Paid</th><th>Filed</th></tr></thead><tbody>${rows}</tbody></table>` : '<p class="muted">No filings yet.</p>';
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  // TPA bulk upload: same parser and idempotent batch semantics as the
  // staff intake screen, but scoped here so TPA-role users never need staff
  // screens. Batches are recorded under the filer's own subject — "My batch
  // filings" below is the status view.
  function tpaBulkFile(input) {
    const file = input.files && input.files[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      const rows = bulkCsvRows(String(reader.result || ""));
      const head = (rows.shift() || []).map((h) => h.trim().toLowerCase().replace(/^\uFEFF/, ""));
      if (!head.includes("email")) {
        $("#tpa-bulk-preview").innerHTML = `<p class="badge warn">header row must include at least: email</p>`;
        return;
      }
      const items = rows.map((r) => {
        const cell = (name) => { const i = head.indexOf(name); return i >= 0 ? (r[i] || "").trim() : ""; };
        const cents = cell("amount") ? Math.round(parseFloat(cell("amount").replace(/[$,]/g, "")) * 100) : 0;
        return {
          external_ref: cell("external_ref"), email: cell("email"),
          contact_name: cell("contact_name"), org: cell("org"), notes: cell("notes"),
          filing_party_type: cell("filing_party_type").toUpperCase(),
          disputed_amount_cents: Number.isFinite(cents) ? cents : 0,
          qpa_cents: Number.isFinite(cents) ? cents : 0,
        };
      }).filter((it) => it.email);
      window._tpaBulk = { items };
      const bad = rows.length - items.length;
      $("#tpa-bulk-preview").innerHTML =
        `<p class="muted">${items.length} row(s) ready${bad ? ` — ${bad} row(s) skipped (no email)` : ""}${items.length > 500 ? " — <b>over the 500-row limit, split the file</b>" : ""}.</p>`;
      $("#tpa-bulk-btn").disabled = !items.length || items.length > 500;
    };
    reader.readAsText(file);
  }

  async function tpaBulkSubmit(btn) {
    const ref = ($("#tpa-bulk-ref")?.value || "").trim();
    if (!ref) { UI.toast("batch reference required — it is the idempotency key", { kind: "warn" }); return; }
    const { items } = window._tpaBulk || { items: [] };
    if (!items.length) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.intakeBulk({ batch_ref: ref, items });
        const rows = (r.results || []).map((x) => `<tr>
          <td class="muted">${esc(x.external_ref || "")}</td>
          <td>${x.status === "CREATED" ? badge("CREATED") : `<span class="badge warn">ERROR</span>`}${x.payment_required ? ` <span class="badge warn">fee due</span>` : ""}</td>
          <td>${x.case_number ? `<a href="#/cases/${x.case_id}">${esc(x.case_number)}</a>` : esc(x.intake_id || "")}</td>
          <td class="muted">${esc(x.error || "")}</td></tr>`).join("");
        $("#tpa-bulk-result").innerHTML = `<p><b>Batch ${esc(r.batch_ref || ref)}</b> — ${r.created} filed, ${r.errors} error(s)${r.idempotent_replay ? " — <b>idempotent replay</b>: already submitted; nothing re-filed" : ""}</p>
          <table><thead><tr><th>Row</th><th>Outcome</th><th>Intake / case</th><th>Error</th></tr></thead><tbody>${rows}</tbody></table>`;
        UI.toast(r.idempotent_replay ? "Batch already filed — receipt replayed" : `Batch filed: ${r.created} created, ${r.errors} errors`, { kind: r.errors ? "warn" : "ok" });
        tpaBatchesBox();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Filing batch…");
  }

  async function tpaBatchesBox() {
    const box = document.getElementById("tpa-batches");
    if (!box) return;
    try {
      const r = await Api.program.intakeBatches();
      const bs = r.batches || [];
      box.innerHTML = bs.length ? `<table><thead><tr><th>Batch ref</th><th>Submitted</th><th>Rows</th><th>Filed</th><th>Errors</th><th></th></tr></thead><tbody>
        ${bs.map((b) => `<tr><td class="mono">${esc(b.batch_ref)}</td><td>${esc(String(b.created_at || "").slice(0, 16).replace("T", " "))}</td>
          <td>${b.item_count}</td><td>${b.created_count}</td><td>${b.error_count ? `<span class="badge warn">${b.error_count}</span>` : "0"}</td>
          <td><button class="mini ghost" onclick="Views.tpaBatchDetail(${b.id})">Receipt</button></td></tr>
          <tr id="tpa-batch-${b.id}" style="display:none"><td colspan="6"></td></tr>`).join("")}</tbody></table>`
        : '<p class="muted">No batches yet — upload a CSV above.</p>';
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaBatchDetail(id) {
    const row = document.getElementById(`tpa-batch-${id}`);
    if (!row) return;
    if (row.style.display !== "none") { row.style.display = "none"; return; }
    row.style.display = "";
    row.firstElementChild.innerHTML = `<p class="muted">Loading receipt…</p>`;
    try {
      const r = await Api.program.intakeBatch(id);
      const rows = (r.results || []).map((x) => `<tr>
        <td class="muted">${esc(x.external_ref || "")}</td>
        <td>${x.status === "CREATED" ? badge("CREATED") : `<span class="badge warn">ERROR</span>`}</td>
        <td>${x.case_number ? `<a href="#/cases/${x.case_id}">${esc(x.case_number)}</a>` : esc(x.intake_id || "—")}</td>
        <td class="muted">${esc(x.error || "")}</td></tr>`).join("");
      row.firstElementChild.innerHTML = `<b>${esc(r.batch_ref)}</b> · submitted ${esc(String(r.submitted_at || "").slice(0, 16).replace("T", " "))} · ${r.created} filed / ${r.errors} errors
        <table><thead><tr><th>Row</th><th>Outcome</th><th>Case</th><th>Error</th></tr></thead><tbody>${rows}</tbody></table>`;
    } catch (e) { row.firstElementChild.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaAdminBox() {
    const box = document.getElementById("tpa-admin");
    if (!box) return;
    try {
      const r = await Api.program.adminTpas();
      const rows = (r.tpas || []).map((t) => `<tr><td>${esc(t.name)}</td><td>${esc(t.contact_email)}</td><td>${t.clients}</td><td>${t.filings}</td>
        <td>${badge(t.status)}</td>
        <td>${can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<button class="mini ${t.status === "ACTIVE" ? "danger" : ""}" onclick="Views.tpaAdminStatusFn('${t.id}','${t.status === "ACTIVE" ? "SUSPENDED" : "ACTIVE"}')">${t.status === "ACTIVE" ? "Suspend" : "Reactivate"}</button>` : ""}</td></tr>`).join("");
      box.innerHTML = rows ? `<table class="tbl"><thead><tr><th>TPA</th><th>Contact</th><th>Clients</th><th>Filings</th><th>Status</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : '<p class="muted">No TPAs registered.</p>';
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaAdminStatusFn(id, status) {
    try { await Api.program.adminTpaStatus(id, status); tpaAdminBox(); }
    catch (e) { alert(e.message); }
  }

  return { dashboard, cases, caseDetail, newDispute, sortCases, onboarding, onboardingNew, decide, voice, reports, showAnalysis, check, assign, letter, saveCurrentView, escalate, relate, feeTransfer, peek, copilotBrief, copilotDraftQA, copilotPropose, copilotDecideBatch, assistant, assistantChip, asstQaDecide, assistantTool, asstRequestUpload, asstCheck, asstSendMail, asstClearCheck, corrTemplateBody, timeAdd, myTimesheet, myTimeRange, myTimeListBox, myTimeAdd, myTimeEditStart, myTimeSave, myTimeDel, timeReport, timeReportRun, timeReportSend, timeReportCsv, timeRateSet, asstTimeAdd, bulkIntakeFile, bulkIntakeSubmit, askGraph, settleInvoice, qaQueue, qaReview, qaDecide, intake, newIntake, advanceIntake, intakeMore, intakeChatTurn, deliverables, submitDeliverable, requestDeliverable, finance, payInvoice, financeMore, moveDoc, rulesAdmin, ruleEdit, ruleDelete, rulesSave, bindRulesAdmin, manifestEdit, opsDashboard, billingInvoices, billingGen, billingActFn, billingDetail, billingExportFn, billingPayForm, billingPayRun, billingFilter: (f) => billingList(f.status.value), arapView, arapRecord, arapSettle, arapVoid, arapNacha, arapNachaFile, reconView, reconImportRun, reconFetchRun, reconMatchRun, reconOpen, reconResolveFn, tpaView, tpaClaimFn, tpaAddClientFn, tpaClientStatusFn, tpaFileFn, tpaDashBox, tpaAdminStatusFn, mailLog, mailLogFilter, mailLogBody };
})();
