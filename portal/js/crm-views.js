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
        : h.kind === "account" ? `#/crm/accounts/${h.id}` : h.kind === "lead" ? "#/crm/leads" : "#/crm/accounts";
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

  return { pipeline, accounts, account360, accountNew, leads, convert, tasks, done, search, calendar };
})();
