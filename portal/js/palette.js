// Author: Patrick Munis at newwave
// palette.js — ⌘K / Ctrl+K command palette: navigation, actions, record search,
// recent items. Keyboard-first, fuzzy-matched, sectioned by result type.
const Palette = (() => {
  let open = false, recents = JSON.parse(localStorage.getItem("idre.recents") || "[]");

  function remember(kind, id, label) {
    recents = [{ kind, id, label }, ...recents.filter((r) => r.id !== id)].slice(0, 8);
    if (window.Prefs) Prefs.push("recents", recents);
    else localStorage.setItem("idre.recents", JSON.stringify(recents));
  }

  const ACTIONS = () => ([
    { s: "Go", icon: "▤", label: "Home", run: () => go("#/dashboard") },
    { s: "Go", icon: "▦", label: "Disputes", run: () => go("#/cases") },
    { s: "Go", icon: "▥", label: "Pipeline", run: () => go("#/pipeline") },
    { s: "Go", icon: "◈", label: "Accounts", run: () => go("#/crm/accounts") },
    { s: "Go", icon: "◎", label: "Leads", run: () => go("#/crm/leads") },
    { s: "Go", icon: "☑", label: "My tasks", run: () => go("#/crm/tasks") },
    { s: "Go", icon: "▨", label: "Calendar", run: () => go("#/calendar") },
    { s: "Go", icon: "◫", label: "Reports", run: () => go("#/reports") },
    { s: "Action", icon: "＋", label: "New dispute", kbd: "C", run: () => go("#/new") },
    { s: "Action", icon: "⇪", label: "Grab next from queue", run: grabNext },
    { s: "Action", icon: "✦", label: "Ask the dispute graph (KGQA)", run: () => go("#/ask") },
    { s: "Action", icon: "⟳", label: "Sync dispute graph (Postgres → FalkorDB → lakehouse)", run: graphSync },
    { s: "Action", icon: "⌘", label: "Train GNN link predictor", run: graphTrain },
    { s: "Action", icon: "◐", label: "Toggle dark mode", run: () => document.getElementById("theme-toggle").click() },
  ]);
  async function graphSync() {
    try { const r = await Api.graph.sync(); UI.toast(`Graph synced — ${r.cases} cases mirrored to FalkorDB + lakehouse`); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function graphTrain() {
    try { const r = await Api.graph.train(); UI.toast(r.trained ? `GNN trained: loss ${r.final_loss} over ${r.cases} cases, ${r.positive_edges} edges` : `Not trained: ${r.reason}`, { kind: r.trained ? "ok" : "warn" }); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  const go = (h) => { location.hash = h; };
  async function grabNext() {
    try {
      const r = await Api.cm.grabNext();
      if (r.claimed) { UI.toast(`Claimed ${r.case_number} from the queue`); go("#/cases/" + r.case_id); }
      else UI.toast(r.message || "Queue is empty", { kind: "warn" });
    } catch (e) { UI.toast("Could not claim: " + e.message, { kind: "warn" }); }
  }

  const fuzzy = (q, s) => s.toLowerCase().includes(q.toLowerCase());

  function show() {
    if (open) return;
    open = true;
    const prev = document.activeElement;
    const w = document.createElement("div");
    w.className = "palette-scrim";
    w.innerHTML = `
      <div class="palette" role="dialog" aria-modal="true" aria-label="Command palette">
        <input class="palette-in" placeholder="Search records, go anywhere, do anything…" aria-label="Command palette input" />
        <div class="palette-list" role="listbox"></div>
      </div>`;
    document.body.appendChild(w);
    const input = w.querySelector(".palette-in"), list = w.querySelector(".palette-list");
    const close = () => { open = false; w.remove(); prev?.focus?.(); };
    w.addEventListener("mousedown", (e) => { if (e.target === w) close(); });

    let items = [], sel = 0;
    async function refresh() {
      const q = input.value.trim();
      const acts = ACTIONS().filter((a) => !q || fuzzy(q, a.label));
      let hits = [], rec = [];
      if (q.length >= 2) {
        try { hits = (await Api.crm.search(q)).slice(0, 6); } catch { hits = []; }
      } else if (!q) {
        rec = recents.slice(0, 5).map((r) => ({ kind: r.kind, id: r.id, label: r.label, detail: "Recent" }));
      }
      items = [
        ...hits.map((h) => ({ s: "Records", icon: h.kind === "case" ? "▦" : "◈", label: h.label, detail: h.detail,
          run: () => go(h.kind === "case" ? `#/cases/${h.id}` : h.kind === "account" ? `#/crm/accounts/${h.id}` : "#/crm/leads") })),
        ...rec.map((h) => ({ s: "Recent", icon: "◷", label: h.label, detail: h.detail,
          run: () => go(h.kind === "case" ? `#/cases/${h.id}` : `#/crm/accounts/${h.id}`) })),
        ...acts,
      ];
      sel = 0;
      let lastS = "";
      list.innerHTML = items.map((it, i) => {
        const head = it.s !== lastS ? `<div class="palette-sec">${it.s}</div>` : "";
        lastS = it.s;
        return head + `<div class="palette-item ${i === sel ? "sel" : ""}" role="option" data-i="${i}" aria-selected="${i === sel}">
          <span class="ri">${it.icon}</span><span class="pl">${it.label}</span>
          ${it.detail ? `<span class="pd">${it.detail}</span>` : ""}
          ${it.kbd ? `<span class="kbd">${it.kbd}</span>` : ""}</div>`;
      }).join("") || `<div class="palette-empty">No matches — press Enter to search all records for “${q}”.</div>`;
      list.querySelectorAll(".palette-item").forEach((el) =>
        el.addEventListener("mousedown", () => { items[+el.dataset.i].run(); close(); }));
    }
    input.addEventListener("input", refresh);
    input.addEventListener("keydown", (e) => {
      if (e.key === "Escape") close();
      else if (e.key === "ArrowDown") { sel = Math.min(sel + 1, items.length - 1); paint(); }
      else if (e.key === "ArrowUp") { sel = Math.max(sel - 1, 0); paint(); }
      else if (e.key === "Enter") {
        if (items[sel]) { items[sel].run(); close(); }
        else if (input.value.trim()) { go(`#/search/${encodeURIComponent(input.value.trim())}`); close(); }
      }
      function paint() {
        list.querySelectorAll(".palette-item").forEach((el, i) => {
          el.classList.toggle("sel", i === sel); el.setAttribute("aria-selected", i === sel);
        });
        list.querySelector(".palette-item.sel")?.scrollIntoView({ block: "nearest" });
      }
    });
    refresh();
    input.focus();
  }

  document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); show(); }
    if (e.key.toLowerCase() === "c" && !/INPUT|TEXTAREA|SELECT/.test(document.activeElement.tagName) && !open) go("#/new");
  });
  return { show, remember };
})();
