// crm-views.js — CRM screens: pipeline kanban, accounts 360, leads, tasks, search.
const CrmViews = (() => {
  // See views.js: bind after router innerHTML injection (macrotask, not microtask).
  const afterRender = (fn) => setTimeout(fn, 0);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const fmtDate = (d) => (d ? new Date(d).toLocaleDateString("en-US", { dateStyle: "medium" }) : "—");
  const badge = (s) => `<span class="badge s-${esc(s).toLowerCase().replace(/_/g, "-")}">${esc(s)}</span>`;
  const err = (e) => `<p class="error">${esc(e.message)}</p>`;
  const CRM_PAGE = 100;
  // Offset pager shared by accounts/leads/tasks: footer markup + a binder that
  // appends the next page's rows in place (busy state on the button).
  const pagerHtml = (id, shown, total, nextOffset) =>
    `<p class="pager" id="${id}-pg"><span class="muted">Showing ${shown} of ${total}</span>
      ${nextOffset >= 0 ? `<button class="mini" id="${id}-more">Load more (${Math.min(CRM_PAGE, total - shown)} remaining)</button>` : ""}</p>`;
  function bindPager(id, tbodySel, state, fetchPage, rowsHtml) {
    document.getElementById(id + "-more")?.addEventListener("click", async (ev) => {
      await UI.run(ev.currentTarget, async () => {
        try {
          const r = await fetchPage(state.next);
          state.loaded = state.loaded.concat(r.rows);
          state.next = r.next;
          document.querySelector(tbodySel).insertAdjacentHTML("beforeend", rowsHtml(r.rows));
          document.getElementById(id + "-pg").outerHTML = pagerHtml(id, state.loaded.length, state.total, state.next);
          bindPager(id, tbodySel, state, fetchPage, rowsHtml);
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      }, "Loading…");
    });
  }

  // ---- Pipeline (kanban over case statuses) ----------------------------------
  // Covers EVERY status a case can hold — a status missing here makes those
  // disputes silently vanish from the board.
  const PIPELINE = [
    ["INITIATED", "Initiated"], ["NEGOTIATION_TRACKED", "Negotiation"],
    ["OFFER_WINDOW_OPEN", "Offer window"], ["OFFERS_REVEALED", "Revealed"],
    ["IN_REVIEW", "In review"], ["DETERMINED", "Determined"],
    ["PAYMENT_PENDING", "Payment pending"],
    ["CLOSED_PAID", "Closed (paid)"], ["CLOSED_DISMISSED", "Closed (dismissed)"],
    // FL AHCA vocabulary (2026): completed cases REST at Decided - Invoice
    // Paid; only these four states are true closures.
    ["Decided - Invoice Paid", "Decided — invoice paid"],
    ["Plan Opt-Out", "Plan opt-out"], ["Ineligible", "Ineligible"],
    ["Dismissed", "Dismissed"], ["Withdrawn", "Withdrawn"],
    ["Plan Notification Packet Issued", "Plan notified"],
  ];
  async function pipeline() {
    try {
      const { cases } = await Api.cases.list({ limit: 200 });
      // Sector-agnostic: a configured Program Manifest supplies the board's
      // stages + labels; legacy tenants fall back to the built-in pipeline.
      const stages = (window.App && App.pipeline && App.pipeline()) || PIPELINE;
      const known = new Set(stages.map(([s]) => s));
      // Safety net: a status the board doesn't know yet still gets a column
      // instead of its cases disappearing silently.
      const extra = [...new Set(cases.filter((c) => !known.has(c.status)).map((c) => c.status))];
      const layout = [...stages, ...extra.map((s) => [s, s.replace(/_/g, " ").toLowerCase()])];
      const cols = layout.map(([status, label]) => {
        const items = cases.filter((c) => c.status === status);
        return `<div class="kanban-col"><h3>${label} <span class="muted">${items.length}</span></h3>` +
          items.map((c) => `<div class="kanban-card" onclick="location.hash='#/cases/${c.id}'">
            <b>${esc(c.case_number)}</b><br/><span class="muted">${esc(c.service_line)} · $${(c.qpa_cents / 100).toLocaleString()}</span></div>`).join("") +
          `</div>`;
      });
      return `<h1>Pipeline</h1><div class="kanban">${cols.join("")}</div>`;
    } catch (e) { return `<h1>Pipeline</h1>` + err(e); }
  }

  // ---- Accounts ---------------------------------------------------------------
  async function accounts() {
    try {
      const page = await Api.crm.accounts(null, { limit: CRM_PAGE });
      const state = { loaded: page.accounts.slice(), next: page.next_offset, total: page.total };
      const rowsHtml = (list) => list.map((a) => `<tr class="click" onclick="location.hash='#/crm/accounts/${a.id}'">
            <td>${esc(a.legal_name)}</td><td>${badge(a.type)}</td><td>${esc(a.npi)}</td><td>${esc(a.phone)}</td></tr>`).join("");
      if (!state.loaded.length)
        return `<h1>Accounts</h1><p><a class="button" href="#/crm/accounts/new">New account</a></p>
          <p class="muted">No accounts yet — convert leads or create one.</p>`;
      afterRender(() => bindPager("acc", "#acc-tb tbody", state,
        (off) => Api.crm.accounts(null, { limit: CRM_PAGE, offset: off }).then((r) => ({ rows: r.accounts, next: r.next_offset })),
        rowsHtml));
      return `<h1>Accounts</h1><p><a class="button" href="#/crm/accounts/new">New account</a></p>
        <table id="acc-tb"><thead><tr><th>Legal name</th><th>Type</th><th>NPI</th><th>Phone</th></tr></thead>
        <tbody>${rowsHtml(state.loaded)}</tbody></table>` + pagerHtml("acc", state.loaded.length, state.total, state.next);
    } catch (e) { return `<h1>Accounts</h1>` + err(e); }
  }

  async function account360(id) {
    try {
      const d = await Api.crm.account360(id);
      const a = d.account;
      Palette.remember("account", a.id, a.legal_name);
      let html = `<h1>${esc(a.legal_name)}</h1><p>${badge(a.type)} · NPI ${esc(a.npi) || "—"} · ${esc(a.phone) || "—"}</p>`;
      if (App.feature("entity360"))
        html += `<div class="actions"><button class="mini" onclick="CrmViews.openEntity360('accounts','${esc(a.id)}')">Entity 360 — every rail this organization appears on</button></div>`;
      if (d.health)
        html += `<div class="cards"><div class="card health-${esc(d.health.band)}">
          <div class="num">${d.health.score}</div><div class="lbl">Relationship health — ${esc(d.health.band)}</div>
          <div class="muted">${d.health.open_disputes} open disputes · ${d.health.sla_breaches} SLA breaches · ${esc(d.health.formula)}</div></div></div>`;
      html += `<h2>Contacts (${d.contacts.length})</h2>` +
        (d.contacts.length ? `<table><tbody>` + d.contacts.map((c) =>
          `<tr><td>${esc(c.name)}</td><td>${esc(c.role_title)}</td><td>${esc(c.email)}</td><td>${esc(c.phone)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No contacts.</p>`) +
        `<form id="nc" class="form"><b>Add contact</b>
           <input name="name" placeholder="Name" required />
           <input name="role_title" placeholder="Role (e.g. Billing lead)" />
           <input name="email" type="email" placeholder="Email" />
           <input name="phone" placeholder="Phone" /><button>Add</button></form>`;
      html += `<h2>Disputes (${d.cases.length})</h2>` +
        (d.cases.length ? `<table><tbody>` + d.cases.map((c) =>
          `<tr class="click" onclick="location.hash='#/cases/${c.id}'"><td>${esc(c.case_number)}</td>
           <td>${badge(c.status)}</td><td>${esc(c.service_line)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No disputes for this account.</p>`);
      html += `<h2>Notes</h2>` +
        (d.notes.length ? `<table><tbody>` + d.notes.map((n) =>
          `<tr><td>${esc(n.body)}</td><td class="muted">${fmtDate(n.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No notes.</p>`) +
        `<form id="nn" class="form"><b>Add note</b><input name="body" required /><button>Add note</button></form>`;
      afterRender(() => {
        document.querySelector("#nc")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = Object.fromEntries(new FormData(ev.target));
          try { await Api.crm.createContact({ account_id: id, ...f }); UI.toast("Contact added"); App.rerender(); }
          catch (e) { UI.toast(e.message, { kind: "warn" }); }
        });
        document.querySelector("#nn")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = Object.fromEntries(new FormData(ev.target));
          try { await Api.crm.addNote({ record_type: "ACCOUNT", record_id: id, body: f.body }); UI.toast("Note added"); App.rerender(); }
          catch (e) { UI.toast(e.message, { kind: "warn" }); }
        });
      });
      return html;
    } catch (e) { return err(e); }
  }

  function accountNew() {
    afterRender(() => document.querySelector("#na").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = Object.fromEntries(new FormData(ev.target));
      try { await Api.crm.createAccount(f); UI.toast("Account created"); location.hash = "#/crm/accounts"; }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }));
    return `<h1>New account</h1><form id="na" class="form">
      <label>Type <select name="type"><option>PROVIDER</option><option>PAYER</option><option>IDRE</option><option>AUDITOR</option><option>OTHER</option></select></label>
      <label>Legal name <input name="legal_name" required /></label>
      <label>NPI <input name="npi" /></label>
      <label>Phone <input name="phone" /></label>
      <button>Create account</button></form>`;
  }

  // ---- Leads ---------------------------------------------------------------------
  async function leads() {
    try {
      const page = await Api.crm.leads({ limit: CRM_PAGE });
      const state = { loaded: page.leads.slice(), next: page.next_offset, total: page.total };
      const rowsHtml = (list) => list.map((l) => `<tr><td>${esc(l.name)}</td><td>${esc(l.organization)}</td><td>${badge(l.source)}</td>
            <td>${esc((l.summary || "").slice(0, 80))}</td><td>${badge(l.status)}</td>
            <td>${l.status !== "CONVERTED" ? `<button onclick="CrmViews.convert('${l.id}')">Convert</button>` : ""}</td></tr>`).join("");
      if (!state.loaded.length)
        return `<h1>Leads</h1><p class="muted">Voice intake becomes a lead automatically; convert qualified leads into accounts.</p><p class="muted">No leads.</p>`;
      afterRender(() => bindPager("lead", "#lead-tb tbody", state,
        (off) => Api.crm.leads({ limit: CRM_PAGE, offset: off }).then((r) => ({ rows: r.leads, next: r.next_offset })),
        rowsHtml));
      return `<h1>Leads</h1><p class="muted">Voice intake becomes a lead automatically; convert qualified leads into accounts.</p>
        <table id="lead-tb"><thead><tr><th>Name</th><th>Organization</th><th>Source</th><th>Summary</th><th>Status</th><th></th></tr></thead>
        <tbody>${rowsHtml(state.loaded)}</tbody></table>` + pagerHtml("lead", state.loaded.length, state.total, state.next);
    } catch (e) { return `<h1>Leads</h1>` + err(e); }
  }

  async function convert(id) {
    const v = await UI.modal({ title: "Convert lead to account", submitLabel: "Convert",
      body: "Creates the account, links the lead's history, and marks the lead converted.",
      fields: [{ name: "type", label: "Account type", options: [["PROVIDER", "Provider"], ["PAYER", "Payer"], ["IDRE", "IDRE entity"], ["OTHER", "Other"]], required: true }] });
    if (!v) return;
    try { await Api.crm.convertLead(id, v.type); UI.toast("Lead converted to account"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- Tasks ------------------------------------------------------------------------
  async function tasks() {
    try {
      const page = await Api.crm.tasks(true, { limit: CRM_PAGE });
      const state = { loaded: page.tasks.slice(), next: page.next_offset, total: page.total };
      let html = `<h1>My tasks</h1>
        <form id="nt" class="form"><b>New task</b>
          <input name="subject" placeholder="Subject" required />
          <input name="case_id" placeholder="Case ID (optional)" />
          <input name="due_date" type="date" /><button>Create</button></form>`;
      const rowsHtml = (list) => list.map((t) => `<tr><td class="mono">${esc(t.task_ref || "—")}</td>
          <td>${esc(t.subject)}</td>
          <td>${t.case_id ? `<a href="#/cases/${esc(t.case_id)}" class="mono">${esc(t.case_id.slice(0, 8))}…</a>` : "—"}</td>
          <td>${esc(t.due_date)}</td>
          <td>${badge(t.status)}</td>
          <td>${t.status === "OPEN" ? `<button onclick="CrmViews.done('${t.id}')">Done</button>` : ""}</td></tr>`).join("");
      html += state.loaded.length ? `<table id="task-tb"><thead><tr><th>Ref</th><th>Subject</th><th>Case</th><th>Due</th><th>Status</th><th></th></tr></thead>
        <tbody>${rowsHtml(state.loaded)}</tbody></table>` + pagerHtml("task", state.loaded.length, state.total, state.next)
        : `<p class="muted">No open tasks.</p>`;
      afterRender(() => bindPager("task", "#task-tb tbody", state,
        (off) => Api.crm.tasks(true, { limit: CRM_PAGE, offset: off }).then((r) => ({ rows: r.tasks, next: r.next_offset })),
        rowsHtml));
      afterRender(() => document.querySelector("#nt")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = Object.fromEntries(new FormData(ev.target));
        try { const r = await Api.crm.createTask(f); UI.toast(`Task ${r.task_ref || ""} created`.trim()); App.rerender(); }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      }));
      return html;
    } catch (e) { return `<h1>My tasks</h1>` + err(e); }
  }

  async function done(id) {
    try { await Api.crm.completeTask(id); UI.toast("Task completed"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- Global search -------------------------------------------------------------------
  async function search(q) {
    if (!q) return `<h1>Search</h1><p class="muted">Type in the search box above.</p>`;
    try {
      const hits = await Api.crm.search(q);
      const link = (h) => h.kind === "case" ? `#/cases/${h.id}`
        : h.kind === "document" ? `#/cases/${h.case_id || ""}`
        : h.kind === "account" ? `#/crm/accounts/${h.id}` : h.kind === "entity" ? `#/crm/360/${h.id}`
        : h.kind === "lead" ? "#/crm/leads" : "#/crm/accounts";
      return `<h1>Search: “${esc(q)}”</h1>` +
        (hits.length ? `<table><tbody>` + hits.map((h) =>
          `<tr class="click" onclick="location.hash='${link(h)}'"><td>${badge(h.kind)}</td>
           <td>${esc(h.label)}</td><td class="muted">${esc(h.detail)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No matches.</p>`);
    } catch (e) { return err(e); }
  }

  // ---- Calendar (month grid + agenda) ----------------------------------------------------
  // Statutory deadlines deserve a real calendar, not a list: month grid with
  // color-coded chips (statutory clocks = brass, tasks = green, offers = blue),
  // today highlighted, month navigation, and an agenda under it.
  function calMonthGrid(items, y, m) {
    const first = new Date(y, m, 1);
    const daysInMonth = new Date(y, m + 1, 0).getDate();
    const todayISO = new Date().toISOString().slice(0, 10);
    const byDay = {};
    items.forEach((i) => {
      const d = String(i.due_date || "").slice(0, 10);
      if (d) (byDay[d] ||= []).push(i);
    });
    const chipCls = (t) => t === "OFFER_WINDOW_CLOSE" ? "cal-chip-stat"
      : /TASK/i.test(t || "") ? "cal-chip-task" : "cal-chip-clock";
    let cells = "";
    for (let i = 0; i < first.getDay(); i++) cells += `<div class="cal-cell empty"></div>`;
    for (let d = 1; d <= daysInMonth; d++) {
      const iso = `${y}-${String(m + 1).padStart(2, "0")}-${String(d).padStart(2, "0")}`;
      const dayItems = byDay[iso] || [];
      cells += `<div class="cal-cell ${iso === todayISO ? "today" : ""} ${dayItems.length ? "has-items" : ""}">
        <span class="cal-num">${d}</span>
        ${dayItems.slice(0, 3).map((i) => `<div class="cal-chip ${chipCls(i.type)}"
            title="${esc(i.type)} — ${esc(i.title)}">${i.case_id
              ? `<a href="#/cases/${esc(i.case_id)}">${esc(i.case_number || i.title)}</a>`
              : esc(i.title)}</div>`).join("")}
        ${dayItems.length > 3 ? `<span class="muted cal-more">+${dayItems.length - 3} more</span>` : ""}</div>`;
    }
    return `<div class="cal-grid">
      ${["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"].map((d) => `<div class="cal-dow">${d}</div>`).join("")}
      ${cells}</div>`;
  }

  async function calendar() {
    try {
      const items = await Api.cm.calendar();
      items.sort((a, b) => String(a.due_date).localeCompare(String(b.due_date)));
      const now = new Date();
      const state = { y: now.getFullYear(), m: now.getMonth() };
      const render = () => {
        const label = new Date(state.y, state.m, 1)
          .toLocaleDateString("en-US", { month: "long", year: "numeric" });
        const grid = document.getElementById("cal-grid-box");
        if (grid) {
          grid.innerHTML = calMonthGrid(items, state.y, state.m);
          document.getElementById("cal-month-label").textContent = label;
        }
      };
      afterRender(() => {
        render();
        document.getElementById("cal-prev")?.addEventListener("click", () => {
          state.m--; if (state.m < 0) { state.m = 11; state.y--; } render();
        });
        document.getElementById("cal-next")?.addEventListener("click", () => {
          state.m++; if (state.m > 11) { state.m = 0; state.y++; } render();
        });
      });
      const groups = {};
      items.forEach((i) => (groups[String(i.due_date).slice(0, 10)] ||= []).push(i));
      const day = (d) => new Date(d + "T00:00:00").toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric" });
      const agenda = Object.keys(groups).map((dd) =>
        `<div class="cal-day"><h3>${day(dd)}</h3>` + groups[dd].map((i) =>
          `<div class="cal-item ${i.type === "OFFER_WINDOW_CLOSE" ? "cal-stat" : "cal-task"}">
             ${badge(i.type)} ${i.case_number
               ? `<a href="#/cases/${i.case_id}">${esc(i.case_number)}</a> — ` : ""}${esc(i.title)}</div>`).join("") +
        `</div>`).join("");
      return `<div class="view-head"><h1>Calendar</h1>
        <span class="muted">statutory clocks, offer windows, and task due dates</span></div>
        <div class="cal-nav">
          <button class="mini" id="cal-prev">‹ prev</button>
          <b id="cal-month-label"></b>
          <button class="mini" id="cal-next">next ›</button>
          <span class="cal-legend"><span class="cal-chip cal-chip-stat">statutory</span>
            <span class="cal-chip cal-chip-clock">clock</span>
            <span class="cal-chip cal-chip-task">task</span></span></div>
        <div id="cal-grid-box"></div>
        <h2>Agenda</h2>` + (agenda || `<p class="muted">No upcoming deadlines or tasks.</p>`);
    } catch (e) { return `<h1>Calendar</h1>` + err(e); }
  }

  // ---- Entity 360 ------------------------------------------------------------
  // One organization across every rail that collects data on it: CRM account,
  // TPA registry/clients, health plan registry/parties, cases, invoices,
  // payments, correspondence. Identity keys (NPI/TIN/NAIC/FEIN) link
  // automatically; name-only matches need a person's confirmation.
  const $ = (sel) => document.querySelector(sel);
  const money = (c) => c == null ? "—" : "$" + (Number(c) / 100).toLocaleString("en-US", { minimumFractionDigits: 2 });

  // From any screen that knows a source row: resolve to the entity, offer
  // same-name entities for a human-confirmed merge when there are no
  // identity keys to settle it, then open the 360.
  async function openEntity360(sourceTable, sourceId) {
    try {
      const r = await Api.crm.entityLookup(sourceTable, sourceId);
      if (r.suggestions && r.suggestions.length) {
        const v = await UI.modal({
          title: "Same-name organizations found",
          body: "No identity key (NPI / TIN / NAIC) settles which organization this is. Link it to an existing entity only if you know they are the same organization — otherwise it keeps its own.",
          submitLabel: "Link",
          fields: [{ name: "entity_id", label: "Link to", options: [["", "Keep separate — none of these"]] .concat(
            r.suggestions.map((s) => [s.id, `${s.canonical_name} — ${s.entity_type}${s.npi ? " · NPI " + s.npi : ""}${s.naic ? " · NAIC " + s.naic : ""}${s.score != null ? ` · match ${(s.score * 100).toFixed(0)}%` : ""}${s.registry_confirmed ? " · ✓ NPPES" : ""}`])) }],
        });
        if (v && v.entity_id) {
          await Api.crm.entityLink(v.entity_id, { source_table: sourceTable, source_id: sourceId });
          UI.toast("Linked — opening that entity's 360");
          location.hash = `#/crm/360/${v.entity_id}`;
          return;
        }
      }
      location.hash = `#/crm/360/${r.entity_id}`;
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  function entitySearchPage() {
    afterRender(() => {
      $("#ent-q")?.addEventListener("input", async (e) => {
        const q = e.target.value.trim();
        const box = $("#ent-results");
        if (q.length < 2) { box.innerHTML = ""; return; }
        try {
          const list = (await Api.crm.entitySearch(q)).entities || [];
          box.innerHTML = list.length ? `<table><thead><tr><th>Name</th><th>Type</th><th>NPI</th><th>NAIC</th><th>Linked records</th><th></th></tr></thead><tbody>` +
            list.map((x) => `<tr class="click" onclick="location.hash='#/crm/360/${x.id}'"><td>${esc(x.canonical_name)}</td>
              <td>${badge(x.entity_type)}</td><td class="mono">${esc(x.npi) || "—"}</td><td class="mono">${esc(x.naic) || "—"}</td>
              <td class="num">${x.links}</td><td>›</td></tr>`).join("") + `</tbody></table>`
            : `<p class="muted">No entities match. Entities are created automatically as organizations register, onboard, or get CRM accounts.</p>`;
        } catch (e2) { box.innerHTML = err(e2); }
      });
      $("#ent-q")?.focus();
      // CRM outreach list (#14): established entities that have gone quiet —
      // enough disputes to be a relationship, nothing heard in a while.
      (async () => {
        const box = $("#ent-outreach");
        if (!box) return;
        try {
          const r = await Api.crm.entityOutreach(3, 90);
          const rows = r.outreach || [];
          box.innerHTML = rows.length ? `<h2>Outreach — established but quiet (${rows.length})</h2>
            <p class="muted">${r.min_cases}+ disputes, no note or correspondence in ${r.quiet_days} days.</p>
            <table><thead><tr><th>Organization</th><th>Type</th><th>Disputes</th><th>Last engagement</th><th></th></tr></thead><tbody>` +
            rows.map((x) => `<tr class="click" onclick="location.hash='#/crm/360/${x.entity_id}'"><td>${esc(x.canonical_name)}</td>
              <td>${badge(x.entity_type)}</td><td class="num">${x.cases}</td>
              <td class="mono">${x.last_engagement ? fmtDate(x.last_engagement) : "never"}</td>
              <td><button class="mini" onclick="event.stopPropagation();CrmViews.entityOutreachLead('${esc(x.canonical_name)}',${x.cases},'${x.last_engagement || ""}')">Add to leads</button></td></tr>`).join("") + `</tbody></table>`
            : "";
        } catch (e2) { box.innerHTML = ""; }
      })();
    });
    return `<div class="view-head"><h1>Entity 360</h1></div>
      <p class="muted">One organization across every rail — TPA filings, health plan submissions, CRM accounts, disputes, invoices. Search by name, NPI, TIN or NAIC.</p>
      <div class="kgqa-bar"><input id="ent-q" placeholder="Search organizations…" autocomplete="off" /></div>
      <div id="ent-results"></div>
      <div id="ent-outreach"></div>`;
  }

  // entityOutreachLead (#14): one click from the outreach table to a CRM lead.
  async function entityOutreachLead(name, cases, last) {
    try {
      await Api.crm.createLead({ organization: name,
        summary: `Outreach: ${cases} disputes on file${last ? ", last engagement " + last.slice(0, 10) : ", no engagement recorded"}.` });
      UI.toast("Lead added — see CRM → Leads");
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function entity360(id) {
    try {
      const d = await Api.crm.entity360(id);
      if (d.merged_into) {
        UI.toast(`“${d.merged_name || "This entity"}” was merged — opening the surviving record`);
        location.hash = `#/crm/360/${d.merged_into}`;
        return `<h1>Entity 360</h1><p class="muted">Redirecting…</p>`;
      }
      const e = d.entity;
      Palette.remember("entity", e.id, e.canonical_name);
      const srcLink = (l) => l.source_table === "accounts" ? `#/crm/accounts/${l.source_id}`
        : (l.source_table === "tpas" || l.source_table === "tpa_clients") ? "#/tpa" : "#/healthplan";
      let html = `<div class="view-head"><h1>${esc(e.canonical_name)}</h1>${badge(e.entity_type)}
          <a class="button mini" href="#/crm/360">Search</a>
          <button class="mini" onclick="CrmViews.entityLinkPicker('${id}')">Link a record…</button>
          <button class="mini danger" onclick="CrmViews.entityMergeModal('${id}')">Merge with another entity…</button></div>
        <p class="muted">${[e.npi && "NPI " + esc(e.npi), e.naic && "NAIC " + esc(e.naic),
          e.tin && "TIN " + esc(e.tin), e.fein && "FEIN " + esc(e.fein)].filter(Boolean).join(" · ") || "No identity keys on file"}
          · since ${fmtDate(e.created_at)}</p>`;

      // Composite risk score (#13) — first thing a reviewer sees.
      const rk = d.risk || {};
      if (rk.score != null) {
        const bandClr = rk.band === "review" ? "bad" : rk.band === "watch" ? "warn" : "ok";
        html += `<h2>Risk</h2><div class="cards">
          <div class="card"><div class="num ${bandClr}">${rk.score}</div><div class="lbl">Composite risk (${esc(rk.band)})</div></div>` +
          (rk.factors || []).map((x) => `<div class="card"><div class="num">${x.points}</div><div class="lbl">${esc((x.factor || "").replace(/_/g, " "))}${x.count != null ? " (" + x.count + ")" : ""}${x.days != null ? " (" + x.days + "d)" : ""}</div></div>`).join("") + `</div>`;
      }

      // Relationship graph (#10): entities this one has faced in disputes —
      // ego network, edges weighted by shared cases. SVG radial, no library.
      let g = { nodes: [], edges: [] };
      try { g = await Api.crm.entityGraph(id); } catch (e2) { /* graph panel optional */ }
      // Normalize to seed-relative edges (a REPRESENTS edge may point either
      // way) and keep only edges touching this entity for the radial.
      g.edges = (g.edges || []).filter((x) => x.from === id || x.to === id)
        .map((x) => ({ ...x, other: x.from === id ? x.to : x.from }));
      if ((g.nodes || []).length > 1) {
        const cx = 340, cy = 190, R = 140;
        const maxW = Math.max(...g.edges.map((x) => x.cases || 0), 1);
        const seed = g.nodes.find((n) => n.seed);
        const ring = g.nodes.filter((n) => !n.seed && g.edges.some((x) => x.other === n.id));
        const pos = {};
        ring.forEach((n, i) => {
          const a = (2 * Math.PI * i) / ring.length - Math.PI / 2;
          pos[n.id] = [cx + R * Math.cos(a), cy + R * Math.sin(a)];
        });
        html += `<h2>Relationship graph — who it faces in disputes (${ring.length})</h2>
          <svg viewBox="0 0 680 380" style="width:100%;max-width:680px;display:block" role="img" aria-label="Entity relationship graph">` +
          g.edges.map((x) => { const [x1, y1] = pos[x.other] || [cx, cy];
            const rep = x.kind === "REPRESENTS";
            return `<line x1="${cx}" y1="${cy}" x2="${x1}" y2="${y1}" stroke="var(--line,#888)"
              stroke-width="${rep ? 1.5 : 1 + 3 * (x.cases || 0) / maxW}" opacity="0.55"${rep ? ` stroke-dasharray="5 4"` : ""}/>`; }).join("") +
          ring.map((n) => { const [nx, ny] = pos[n.id];
            const lbl = String(n.name || "").length > 22 ? String(n.name).slice(0, 21) + "…" : String(n.name || "");
            return `<g style="cursor:pointer" onclick="location.hash='#/crm/360/${n.id}'">
              <circle cx="${nx}" cy="${ny}" r="10" fill="var(--accent,#4a7)"/><title>${esc(n.name)} — ${esc(n.type)}</title>
              <text x="${nx}" y="${ny + 22}" text-anchor="middle" font-size="10" fill="currentColor">${esc(lbl)}</text></g>`; }).join("") +
          `<circle cx="${cx}" cy="${cy}" r="16" fill="var(--brass,#a83)"/><title>${esc(seed?.name || "")}</title>
           <text x="${cx}" y="${cy + 32}" text-anchor="middle" font-size="11" font-weight="600" fill="currentColor">${esc(String(seed?.name || "").slice(0, 26))}</text></svg>` +
          `<table><thead><tr><th>Related organization</th><th>Relationship</th><th>Type</th><th>Shared disputes</th></tr></thead><tbody>` +
          g.edges.slice(0, 15).map((x) => { const n = g.nodes.find((y) => y.id === x.other);
            return `<tr class="click" onclick="location.hash='#/crm/360/${x.other}'"><td>${esc(n?.name || "")}</td>
              <td>${x.kind === "REPRESENTS" ? "org chart" : "opposes in disputes"}</td>
              <td>${badge(n?.type || "")}</td><td class="num">${x.cases ?? "—"}</td></tr>`; }).join("") + `</tbody></table>`;
      }

      // Panel 1: where the platform knows this organization (#3 deep links).
      html += `<h2>Identity &amp; onboarding — where this organization appears (${d.links.length})</h2>` +
        (d.links.length ? `<table><thead><tr><th>Record</th><th>Name</th><th>Kind</th><th>Status</th><th>Link</th><th></th></tr></thead><tbody>` +
          d.links.map((l) => `<tr class="click" onclick="location.hash='${srcLink(l)}'"><td>${esc(l.source_label)}</td>
            <td>${esc(l.detail?.name || l.source_id)}</td><td>${esc(l.detail?.kind || "")}</td>
            <td>${l.detail?.status ? badge(l.detail.status) : "—"}${l.detail?.npi_status && l.detail.npi_status !== "VERIFIED" ? " " + badge(l.detail.npi_status) : ""}</td>
            <td>${l.link_kind === "CONFIRMED" ? `confirmed by ${esc(l.linked_by)}` : "identity-key match"}</td>
            <td><button class="mini danger" onclick="event.stopPropagation();CrmViews.entityUnlinkFn('${id}',${l.link_id})" title="Remove this link">Unlink</button></td></tr>`).join("") +
          `</tbody></table>`
          : `<p class="muted">No linked records yet. Entities resolve automatically when a record carries an identity key (NPI / TIN / NAIC / FEIN) —
             for anything else, link it yourself:</p>
             <div class="actions"><button onclick="CrmViews.entityLinkPicker('${id}')">Link a record…</button></div>`);

      // Panel 2: pipeline — initiator vs respondent split (#11) + ordered funnel (#7).
      const pl = d.pipeline || {};
      html += `<h2>Pipeline</h2><div class="cards">
        <div class="card"><div class="num">${pl.total ?? 0}</div><div class="lbl">Disputes</div></div>
        <div class="card"><div class="num">${pl.initiating ?? 0}</div><div class="lbl">Filed by this entity</div></div>
        <div class="card"><div class="num">${pl.responding ?? 0}</div><div class="lbl">Answered as respondent</div></div>
        <div class="card"><div class="num">${pl.open ?? 0}</div><div class="lbl">Open</div></div>
        <div class="card"><div class="num">${pl.closed ?? 0}</div><div class="lbl">Closed</div></div></div>` +
        ((pl.funnel || []).length ? `<h3>Funnel</h3><table><thead><tr><th>Stage</th><th>Now at stage</th><th>Reached stage</th><th>Conversion</th></tr></thead><tbody>` +
          pl.funnel.map((s) => `<tr><td>${badge(s.stage)}</td><td class="num">${s.count}</td><td class="num">${s.reached}</td>
            <td class="num">${s.conversion_pct != null ? Number(s.conversion_pct).toFixed(1) + "%" : "—"}</td></tr>`).join("") + `</tbody></table>` : "") +
        ((pl.by_status || []).length ? `<table><thead><tr><th>Status</th><th>Disputes</th></tr></thead><tbody>` +
          pl.by_status.map((s) => `<tr><td>${badge(s.status)}</td><td class="num">${s.count}</td></tr>`).join("") + `</tbody></table>` : "") +
        ((pl.recent || []).length ? `<h3>Most recent</h3><table><thead><tr><th>Case</th><th>Status</th><th>Service line</th><th>Opened</th></tr></thead><tbody>` +
          pl.recent.map((c) => `<tr class="click" onclick="location.hash='#/cases/${c.id}'"><td class="mono">${esc(c.case_number)}</td>
            <td>${badge(c.status)}</td><td>${esc(c.service_line)}</td><td>${fmtDate(c.opened_at)}</td></tr>`).join("") + `</tbody></table>` : "");

      // Panel 3: financials + aging buckets (#8).
      const f = d.financials || {};
      const ag = f.aging || {};
      html += `<h2>Financials</h2><div class="cards">
        <div class="card"><div class="num">${money(f.fee_invoiced_cents)}</div><div class="lbl">Dispute fees invoiced (${f.fee_invoices ?? 0})</div></div>
        <div class="card"><div class="num">${money(f.fee_collected_cents)}</div><div class="lbl">Fees collected</div></div>
        <div class="card"><div class="num">${money(f.fee_open_cents)}</div><div class="lbl">Fees open</div></div>
        <div class="card"><div class="num">${money(f.paid_cents)}</div><div class="lbl">Card/ACH payments (${f.payments ?? 0})</div></div>
        ${f.billing_invoices != null ? `<div class="card"><div class="num">${money(f.billing_collected_cents)}</div><div class="lbl">Service billing collected (${f.billing_invoices})</div></div>
        <div class="card"><div class="num">${money(f.billing_outstanding_cents)}</div><div class="lbl">Service billing outstanding</div></div>` : ""}
        ${f.checkout_checkouts != null ? `<div class="card"><div class="num">${money(f.checkout_paid_cents)}</div><div class="lbl">Fee checkouts paid (${f.checkout_checkouts})</div></div>` : ""}</div>` +
        (f.aging ? `<h3>Open fee aging${ag.oldest_open_days ? ` — oldest ${ag.oldest_open_days} days` : ""}</h3><div class="cards">
          <div class="card"><div class="num">${money(ag.bucket_0_30)}</div><div class="lbl">0–30 days</div></div>
          <div class="card"><div class="num">${money(ag.bucket_31_60)}</div><div class="lbl">31–60 days</div></div>
          <div class="card"><div class="num">${money(ag.bucket_61_90)}</div><div class="lbl">61–90 days</div></div>
          <div class="card"><div class="num">${money(ag.bucket_90_plus)}</div><div class="lbl">90+ days</div></div></div>` : "");

      // Panel 4: performance + tenant-wide benchmark (#9).
      const pf = d.performance || {};
      const bm = pf.benchmark || {};
      html += `<h2>Performance</h2><div class="cards">
        <div class="card"><div class="num">${pf.closed ?? 0}</div><div class="lbl">Disputes closed</div></div>
        <div class="card"><div class="num">${pf.avg_days_to_close ?? "—"}</div><div class="lbl">Avg days to close</div></div>
        <div class="card"><div class="num">${pf.determinations ?? 0}</div><div class="lbl">Determinations</div></div>
        <div class="card"><div class="num">${money(pf.avg_award_cents)}</div><div class="lbl">Avg award</div></div>
        <div class="card"><div class="num">${pf.above_qpa ?? 0}</div><div class="lbl">Awards above QPA</div></div>
        <div class="card"><div class="num">${pf.sla_breaches ?? 0}</div><div class="lbl">SLA breaches</div></div></div>` +
        (Object.keys(bm).length ? `<h3>Against the whole program</h3><div class="cards">
          ${bm.tenant_avg_days_to_close != null ? `<div class="card"><div class="num">${bm.tenant_avg_days_to_close}</div><div class="lbl">Program avg days to close</div></div>` : ""}
          ${bm.tenant_median_days_to_close != null ? `<div class="card"><div class="num">${bm.tenant_median_days_to_close}</div><div class="lbl">Program median days to close</div></div>` : ""}
          ${bm.faster_than_pct != null ? `<div class="card"><div class="num">${bm.faster_than_pct}%</div><div class="lbl">Of program cases closed slower than this entity's average</div></div>` : ""}
          ${bm.entity_above_qpa_pct != null ? `<div class="card"><div class="num">${Number(bm.entity_above_qpa_pct).toFixed(1)}%</div><div class="lbl">Entity awards above QPA</div></div>` : ""}
          ${bm.tenant_above_qpa_pct != null ? `<div class="card"><div class="num">${bm.tenant_above_qpa_pct}%</div><div class="lbl">Program awards above QPA</div></div>` : ""}</div>` : "");

      // Panel 5: compliance & risk.
      const cp = d.compliance || {};
      const dv = cp.doc_verdicts || {};
      html += `<h2>Compliance</h2>` +
        (Object.keys(dv).length ? `<div class="cards">` + Object.entries(dv).map(([st, n]) =>
          `<div class="card"><div class="num">${n}</div><div class="lbl">Package documents ${esc(st.toLowerCase().replace(/_/g, " "))}</div></div>`).join("") + `</div>`
          : `<p class="muted">No onboarding package documents on the linked records.</p>`) +
        ((cp.npi_flags || []).length ? `<h3>NPI registry flags</h3><table><thead><tr><th>Record</th><th>Name</th><th>Status</th><th>Registry name</th></tr></thead><tbody>` +
          cp.npi_flags.map((x) => `<tr><td>${esc(x.source)}</td><td>${esc(x.name)}</td><td>${badge(x.npi_status)}</td><td>${esc(x.npi_registry_name || "—")}</td></tr>`).join("") + `</tbody></table>` : "") +
        ((cp.breach_clocks || []).length ? `<h3>SLA breaches by clock</h3><table><thead><tr><th>Clock</th><th>Breaches</th></tr></thead><tbody>` +
          cp.breach_clocks.map((x) => `<tr><td class="mono">${esc(x.clock)}</td><td class="num">${x.count}</td></tr>`).join("") + `</tbody></table>` : "");

      // Panel 6: unified engagement timeline (#12), legacy sections retained below it.
      const en = d.engagement || {};
      html += `<h2>Engagement</h2>` +
        ((en.timeline || []).length ? `<table><thead><tr><th>When</th><th>Kind</th><th>What</th><th>Case</th></tr></thead><tbody>` +
          en.timeline.map((x) => `<tr${x.case_id ? ` class="click" onclick="location.hash='#/cases/${x.case_id}'"` : ""}>
            <td class="mono">${fmtDate(x.at)}</td><td>${badge(x.kind)}</td><td>${esc(x.summary || "")}</td>
            <td class="mono">${x.case_id ? esc(String(x.case_id).slice(0, 8)) : "—"}</td></tr>`).join("") + `</tbody></table>`
          : `<p class="muted">No engagement recorded yet.</p>`) +
        ((en.notes || []).length ? `<h3>Notes</h3><table><tbody>` + en.notes.map((n) =>
          `<tr><td>${badge(n.stream)}</td><td>${esc((n.body || "").slice(0, 160))}</td><td>${esc(n.author)}</td><td class="mono">${fmtDate(n.created_at)}</td></tr>`).join("") + `</tbody></table>` : "");
      return html;
    } catch (e) { return `<h1>Entity 360</h1>` + err(e); }
  }

  // entityLinkPicker (#4): staff-driven linking — pick any source record not
  // yet on an entity and attach it here (CONFIRMED by the caller).
  async function entityLinkPicker(entityId) {
    try {
      const v = await UI.modal({
        title: "Link a record to this entity",
        body: "Search every rail — CRM accounts, TPAs, TPA clients, health plans, health plan parties. Records already linked elsewhere are marked.",
        submitLabel: "Link",
        fields: [{ name: "q", label: "Search", placeholder: "Name, NPI, TIN or NAIC…", required: true },
                 { name: "pick", label: "Record", type: "select", options: [["", "— search first —"]] }],
        onRefresh: async (vals, setOptions) => {
          if (!vals.q || vals.q.length < 2) return;
          const r = await Api.crm.entitySourceSearch(vals.q);
          setOptions("pick", [["", "— pick a record —"]].concat((r.sources || []).map((x) =>
            [`${x.source_table}:${x.source_id}`, `${x.name} — ${x.source_label}${x.linked_to ? " (already linked)" : ""}`])));
        },
      });
      if (!v || !v.pick) return;
      const [source_table, source_id] = v.pick.split(":");
      await Api.crm.entityLink(entityId, { source_table, source_id });
      UI.toast("Linked");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // entityMergeModal (#1): fold a duplicate entity into this one — its links,
  // CRM account, and identity keys move over; the duplicate is tombstoned.
  async function entityMergeModal(entityId) {
    try {
      const v = await UI.modal({
        title: "Merge another entity into this one",
        body: "The chosen entity's linked records and identity keys move to this entity, and the chosen entity is retired. This cannot be undone from the portal.",
        submitLabel: "Merge",
        danger: true,
        fields: [{ name: "q", label: "Find the duplicate", placeholder: "Name, NPI or NAIC…", required: true },
                 { name: "into", label: "Duplicate to retire", type: "select", options: [["", "— search first —"]] }],
        onRefresh: async (vals, setOptions) => {
          if (!vals.q || vals.q.length < 2) return;
          const r = await Api.crm.entitySearch(vals.q);
          setOptions("into", [["", "— pick the duplicate —"]].concat((r.entities || []).filter((x) => x.id !== entityId).map((x) =>
            [x.id, `${x.canonical_name} — ${x.entity_type}${x.npi ? " · NPI " + x.npi : ""} (${x.links} linked)`])));
        },
      });
      if (!v || !v.into) return;
      await Api.crm.entityMerge(v.into, entityId);
      UI.toast("Merged — the duplicate is retired");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function entityUnlinkFn(entityId, linkId) {
    if (!(await UI.confirm("Unlink this record?", "The source record keeps its data; it just stops counting toward this entity's 360.", "Unlink", true))) return;
    try { await Api.crm.entityUnlink(entityId, linkId); UI.toast("Unlinked"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  return { pipeline, accounts, account360, accountNew, leads, convert, tasks, done, search, calendar,
    entity360, entitySearchPage, openEntity360, entityUnlinkFn, entityLinkPicker, entityMergeModal, entityOutreachLead };
})();
