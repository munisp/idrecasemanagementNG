// demo.js — optional disconnected demo mode.
// When window.IDRE_CONFIG.demoMode is true, the REAL portal code (views, router, api wrapper)
// runs against built-in fixtures: only `fetch` and the OIDC token are stubbed. Every screen,
// error path, and render function executes exactly as in production. Set demoMode:false for live.
(function () {
  if (!window.IDRE_CONFIG || !window.IDRE_CONFIG.demoMode) return;
  window.IDRE_DEMO = true;

  // --- Fake OIDC session (PKCE flow bypassed; claims shape identical to Keycloak) ---
  const b64 = (o) => btoa(JSON.stringify(o)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  const tok = `${b64({ alg: "none", typ: "JWT" })}.${b64({
    sub: "demo-maria", preferred_username: "maria.chen", exp: 1999999999,
    realm_access: { roles: ["CASE_MANAGER", "ARBITRATOR", "FINANCE", "FEDERAL_ADMIN", "PARTY"] },
    groups: ["/tenant/tx"],
  })}.demo`;
  sessionStorage.setItem("idre.token", tok);
  Auth.login = () => location.reload();
  Auth.logout = () => location.reload();

  // --- Fixtures (shapes mirror the Go case-api responses) ---
  const now = Date.now(), d = (h) => new Date(now - h * 36e5).toISOString();
  const CASES = [
    { id: "c1", case_number: "CMS-TX-2026-01482", status: "IN_REVIEW", service_line: "ER", qpa_cents: 1842000, opened_at: d(96) },
    { id: "c2", case_number: "CMS-TX-2026-01479", status: "OFFER_WINDOW_OPEN", service_line: "RADIOLOGY", qpa_cents: 693000, opened_at: d(120) },
    { id: "c3", case_number: "CMS-TX-2026-01471", status: "DETERMINED", service_line: "ANESTHESIA", qpa_cents: 4125000, opened_at: d(300) },
    { id: "c4", case_number: "CMS-TX-2026-01466", status: "NEGOTIATION_TRACKED", service_line: "ER", qpa_cents: 987500, opened_at: d(50) },
    { id: "c5", case_number: "CMS-TX-2026-01455", status: "PAYMENT_PENDING", service_line: "AIR_AMBULANCE", qpa_cents: 5240000, opened_at: d(720) },
    { id: "c6", case_number: "CMS-TX-2026-01448", status: "CLOSED_PAID", service_line: "LAB", qpa_cents: 1276000, opened_at: d(1100) },
  ];
  const DOCS = [
    { doc_id: "doc1", content_type: "application/pdf", size_bytes: 248000, sealed: false, analysis_status: "ANALYZED", doc_type: "ITEMIZED_BILL",
      filename: "itemized-bill.pdf", folder: "EVIDENCE", scan_status: "CLEAN", uploaded_by: "maria.chen", uploaded_at: d(20) },
    { doc_id: "doc2", content_type: "application/pdf", size_bytes: 96000, sealed: true, analysis_status: "SEALED", doc_type: "OFFER_JUSTIFICATION",
      filename: "offer-justification.pdf", folder: "OFFERS", scan_status: "CLEAN", uploaded_by: "provider-portal", uploaded_at: d(21) },
    { doc_id: "doc3", content_type: "image/png", size_bytes: 412000, sealed: false, analysis_status: "QUEUED", doc_type: null,
      filename: "eob-scan.png", folder: "INTAKE", scan_status: "CLEAN", uploaded_by: "sharebox:7f3a", uploaded_at: d(6) },
    { doc_id: "doc4", content_type: "application/pdf", size_bytes: 8420000, sealed: false, analysis_status: "ANALYZED", doc_type: "MEDICAL_RECORD",
      filename: "medical-records-batch1.pdf", folder: "PARTY_UPLOADS", scan_status: "CLEAN", uploaded_by: "sharebox:7f3a", uploaded_at: d(4) },
  ];
  const ACTS = [
    { type: "STATUS_CHANGE", body: "Status changed OFFERS_SEALED → IN_REVIEW", at: d(3) },
    { type: "ANALYSIS_COMPLETE", body: "Analysis complete on itemized-bill.pdf — 14 fields extracted, 2 seals detected", at: d(6) },
    { type: "OFFER_SEALED", body: "Payer offer received and sealed (vault, HKDF tenant key)", at: d(20) },
    { type: "OFFER_SEALED", body: "Provider offer received and sealed", at: d(21) },
    { type: "NOTE", body: "QPA documentation verified against remit — R. Osei", at: d(26) },
    { type: "TIMER", body: "Dispute auto-opened by statutory timer (negotiation window expired)", at: d(70) },
  ];
  const CHECKLIST = [
    { id: "k1", stage: "DETERMINATION", item: "Verify QPA methodology against remittance", done: true, done_by: "r.osei", required: true },
    { id: "k2", stage: "DETERMINATION", item: "Confirm both offers received within 10-bd window", done: true, done_by: "system", required: true },
    { id: "k3", stage: "DETERMINATION", item: "Review itemized bill (doc-intel extraction confirmed)", done: true, done_by: "m.chen", required: true },
    { id: "k4", stage: "DETERMINATION", item: "Select prevailing offer", done: false, required: true },
    { id: "k5", stage: "DETERMINATION", item: "Draft determination rationale", done: false, required: true },
    { id: "k6", stage: "DETERMINATION", item: "Issue determination letter (tenant-branded PDF)", done: false, required: true },
  ];
  const RELS = [{ rel_type: "BATCH", related_case_id: "c2" }];
  const SUMMARY = [
    { status: "NEGOTIATION_TRACKED", count: 12, avg_qpa_usd: 9800 }, { status: "INITIATED", count: 8, avg_qpa_usd: 15200 },
    { status: "OFFER_WINDOW_OPEN", count: 9, avg_qpa_usd: 11400 }, { status: "IN_REVIEW", count: 7, avg_qpa_usd: 19800 },
    { status: "DETERMINED", count: 6, avg_qpa_usd: 22600 }, { status: "CLOSED_PAID", count: 21, avg_qpa_usd: 16900 },
  ];
  const SLAS = [
    { case_id: "c5", clock: "PAYMENT_30CD", detail: "Payment overdue by 2 calendar days", at: d(40) },
    { case_id: "c1", clock: "DETERMINATION_30BD", detail: "2 business days remaining", at: d(3) },
  ];
  const ACCOUNTS = [
    { id: "a1", legal_name: "Riverbend Surgical Center", type: "PROVIDER", npi: "1928304756", phone: "+1 512 555 0182" },
    { id: "a2", legal_name: "Aetna Better Health of Texas", type: "PAYER", npi: null, phone: "+1 800 555 0143" },
    { id: "a3", legal_name: "Alamo Imaging Partners", type: "PROVIDER", npi: "8475620193", phone: "+1 210 555 0166" },
  ];
  const A360 = {
    account: ACCOUNTS[0],
    contacts: [{ name: "Dana Whitfield", role_title: "Billing lead", email: "dana@riverbend.example", phone: "+1 512 555 0183" }],
    cases: CASES.slice(0, 3),
    notes: [{ body: "Prefers portal messaging over phone for determination questions.", created_at: d(200) }],
  };
  const LEADS = [
    { id: "l1", name: "Front Desk", organization: "Gulf Coast Radiology", source: "VOICE", summary: "Asked about joining the IDR portal as a provider org.", status: "NEW" },
    { id: "l2", name: "J. Park", organization: "Humana", source: "WEB", summary: "Payer onboarding inquiry — batch disputes.", status: "CONVERTED" },
  ];
  const TASKS = [
    { id: "t1", task_ref: "TASK-2026-00041", subject: "Call Riverbend re: missing remit page", case_id: "c1", due_date: "2026-10-01", status: "OPEN" },
    { id: "t2", task_ref: "TASK-2026-00042", subject: "Review batch eligibility for CMS-TX-2026-01490", case_id: "", due_date: "2026-10-02", status: "OPEN" },
    { id: "t3", task_ref: "TASK-2026-00039", subject: "Verify escrow posting for admin fee", case_id: "c3", due_date: "2026-09-29", status: "DONE" },
  ];
  const CAL = [
    { type: "OFFER_WINDOW_CLOSE", title: "Offer window closes (10bd)", case_id: "c2", case_number: "CMS-TX-2026-01479", due_date: "2026-10-07" },
    { type: "TASK", title: "Call Riverbend re: missing remit page", case_id: "c1", case_number: "CMS-TX-2026-01482", due_date: "2026-10-01" },
    { type: "DETERMINATION_DUE", title: "Determination due (30bd)", case_id: "c1", case_number: "CMS-TX-2026-01482", due_date: "2026-10-03" },
  ];
  const NOTIFS = [
    { id: "n1", type: "SLA_RISK", message: "CMS-TX-2026-01482: 2 business days left on the determination clock", read_at: null, created_at: d(2) },
    { id: "n2", type: "ONBOARDING_DECISION", message: "Alamo Imaging Partners approved — account created", read_at: null, created_at: d(9) },
    { id: "n3", type: "ASSIGNMENT", message: "You were assigned CMS-TX-2026-01479", read_at: d(30), created_at: d(30) },
  ];
  const VIEWS = [
    { id: "v1", name: "In review (arbiter queue)", filters: { status: "IN_REVIEW" }, pinned: true },
    { id: "v2", name: "Payment pending", filters: { status: "PAYMENT_PENDING" }, pinned: false },
  ];
  // Statutory-clock fixtures (mirror GET /cases/clocks projection shape)
  const CLOCKS = {
    c1: [{ clock: "DETERMINATION_30BD", label: "Determination due (30bd)", basis: "business", total_days: 30, remaining: 2, due: "2026-10-03", state: "risk", cite: "45 CFR 149.510(c)(4)(ii)(B)", basis_note: "offer-window close" }],
    c2: [{ clock: "OFFER_WINDOW_10BD", label: "Offer window (10bd)", basis: "business", total_days: 10, remaining: 6, due: "2026-10-07", state: "ok", cite: "45 CFR 149.510(b)(2)(ii)(B)", basis_note: "set when the window opened" }],
    c3: [{ clock: "PAYMENT_30CD", label: "Payment due (30cd)", basis: "calendar", total_days: 30, remaining: 9, due: "2026-10-10", state: "watch", cite: "45 CFR 149.510(c)(4)(vii)", basis_note: "determination timestamp (status change)" }],
    c4: [{ clock: "NEGOTIATION_30BD", label: "Open negotiation (30bd)", basis: "business", total_days: 30, remaining: 14, due: "2026-10-15", state: "ok", cite: "45 CFR 149.510(b)(1)", basis_note: "negotiation end date supplied at initiation" }],
    c5: [{ clock: "PAYMENT_30CD", label: "Payment due (30cd)", basis: "calendar", total_days: 30, remaining: -2, due: "2026-09-29", state: "breach", cite: "45 CFR 149.510(c)(4)(vii)", basis_note: "determination timestamp (status change)" }],
  };
  A360.health = { score: 62, band: "watch", open_disputes: 2, sla_breaches: 1, formula: "100 − 8×open disputes − 20×SLA breaches" };
  const APPS = [
    { id: "ob1", legal_name: "Pecos Valley ER Group", type: "PROVIDER_ORG", status: "PENDING_APPROVAL", submitted_at: d(15) },
    { id: "ob2", legal_name: "Lone Star Audit Partners", type: "STATE_AUDITOR_ORG", status: "APPROVED", submitted_at: d(200) },
  ];
  const INTAKE = [{ caller_name: "S. Delgado", caller_phone: "+1 956 555 0114", organization: "Rio Grande Cardiology", summary: "Wants status of dispute and how to upload a remit.", status: "NEW", created_at: d(5) }];
  const LOGS = [
    { direction: "OUTBOUND", tool: "window_closing", case_number: "CMS-TX-2026-01479", status: "COMPLETED", created_at: d(8) },
    { direction: "INBOUND", tool: "intake", case_number: "", status: "LOGGED", created_at: d(5) },
  ];
  const HITS = (q) => [
    { kind: "case", id: "c1", label: "CMS-TX-2026-01482", detail: `ER · IN_REVIEW — matches “${q}”` },
    { kind: "account", id: "a1", label: "Riverbend Surgical Center", detail: "PROVIDER · NPI 1928304756" },
  ];

  // Pad the dispute set to 126 rows so pagination is real in demo (pages of 50).
  for (let i = 7; i <= 126; i++) {
    CASES.push({ id: "cx" + i, case_number: "CMS-TX-2026-" + String(10000 + i), status: ["INITIATED", "IN_REVIEW", "OFFER_WINDOW_OPEN", "DETERMINED", "PAYMENT_PENDING"][i % 5],
      service_line: ["ER", "RADIOLOGY", "LAB", "ANESTHESIA"][i % 4], qpa_cents: 500000 + i * 1377, opened_at: d(i) });
  }

  // --- fetch shim: the real Api wrapper runs; only transport is simulated ---
  const realFetch = window.fetch.bind(window);
  window.fetch = async (url, opts = {}) => {
    const u = String(url);
    if (!u.includes("/v1/tenants/")) return realFetch(url, opts);
    const method = (opts.method || "GET").toUpperCase();
    const json = (data, status = 200) =>
      new Response(JSON.stringify(data), { status, headers: { "content-type": "application/json" } });
    const p = u.replace(/^.*\/v1\/tenants\/[^/]+/, "");
    await new Promise((r) => setTimeout(r, 120)); // realistic latency

    if (method === "GET") {
      if (/\/prefs$/.test(p)) return json({});
      if (/\/cases\/[\w-]+\/documents\/[\w-]+\/analysis$/.test(p))
        return json({ status: "ANALYZED", doc_type: "ITEMIZED_BILL", result: { seal_detected: true, table_count: 3, extracted: { cpt: "99285", billed: 18420.0, qpa: 11240.0, dos: "2026-08-14" }, findings: [] } });
      if (/\/cases\/clocks$/.test(p)) return json(Object.entries(CLOCKS).map(([case_id, clocks]) => ({ case_id, clocks })));
      if (/\/cases\/[\w-]+\/clocks$/.test(p)) { const id = p.split("/")[2]; return CLOCKS[id] ? json(CLOCKS[id]) : json({ error: "not found" }, 404); }
      if (/\/cases\/[\w-]+\/documents$/.test(p)) return json(DOCS);
      if (/\/cases\/[\w-]+\/documents\/[\w-]+$/.test(p) && opts.method === "PATCH") return json({ folder: JSON.parse(opts.body || "{}").folder });
      if (/\/cases\/[\w-]+\/activities$/.test(p)) return json(ACTS);
      if (/\/cases\/[\w-]+\/checklist$/.test(p)) return json(CHECKLIST);
      if (/\/cases\/[\w-]+\/relationships$/.test(p)) return json(RELS);
          if (/\/cases\/[\w-]+$/.test(p)) { const id = p.split("/")[2]; const cc = CASES.find((c) => c.id === id) || CASES[0];
      return json({ ...cc, internal_status: "In Review", agency_status: "Submitted",
        program_dates: { received_at: "2026-09-20", plan_notified_at: "2026-09-25" } }); }
      if (/\/cases(\?|$)/.test(p)) {
        // Mirror the backend's keyset pagination over the fixture set.
        const qs = new URLSearchParams(p.split("?")[1] || "");
        const limit = Math.min(parseInt(qs.get("limit") || "50", 10), 200);
        const status = qs.get("status") || "";
        let rows = CASES.slice().sort((a, b) => (a.opened_at < b.opened_at ? 1 : -1));
        if (status) rows = rows.filter((c) => c.status === status);
        const total = rows.length;
        const sort = qs.get("sort") || "";
        if (sort) {
          const col = sort.replace(/^-/, ""), dir = sort.startsWith("-") ? -1 : 1;
          rows.sort((a, b) => (a[col] > b[col] ? 1 : a[col] < b[col] ? -1 : 0) * dir);
          const off = parseInt(qs.get("offset") || "0", 10);
          const pageRows = rows.slice(off, off + limit);
          const next = off + limit < rows.length ? "offset:" + (off + limit) : "";
          return json({ cases: pageRows, next_cursor: next, total });
        }
        const cur = qs.get("cursor");
        if (cur) { const cid = cur.split("|")[1]; const i = rows.findIndex((c) => c.id === cid); if (i >= 0) rows = rows.slice(i + 1); }
        const pageRows = rows.slice(0, limit);
        const next = rows.length > limit && pageRows.length
          ? pageRows[pageRows.length - 1].opened_at + "|" + pageRows[pageRows.length - 1].id : "";
        return json({ cases: pageRows, next_cursor: next, total });
      }
      if (/\/reports\/sla$/.test(p)) return json(SLAS);
      if (/\/reports\/summary$/.test(p)) return json(SUMMARY);
      if (/\/accounts\/[\w-]+\/360$/.test(p)) return json(A360);
      if (/\/accounts(\?|$)/.test(p)) {
        const qs = new URLSearchParams(p.split("?")[1] || "");
        const off = parseInt(qs.get("offset") || "0", 10), lim = parseInt(qs.get("limit") || "100", 10);
        return json({ accounts: ACCOUNTS.slice(off, off + lim), total: ACCOUNTS.length, next_offset: off + lim < ACCOUNTS.length ? off + lim : -1 });
      }
      if (/\/leads(\?|$)/.test(p)) {
        const qs = new URLSearchParams(p.split("?")[1] || "");
        const off = parseInt(qs.get("offset") || "0", 10), lim = parseInt(qs.get("limit") || "100", 10);
        return json({ leads: LEADS.slice(off, off + lim), total: LEADS.length, next_offset: off + lim < LEADS.length ? off + lim : -1 });
      }
      if (/\/tasks(\?|$)/.test(p)) {
        const qs = new URLSearchParams(p.split("?")[1] || "");
        const off = parseInt(qs.get("offset") || "0", 10), lim = parseInt(qs.get("limit") || "100", 10);
        return json({ tasks: TASKS.slice(off, off + lim), total: TASKS.length, next_offset: off + lim < TASKS.length ? off + lim : -1 });
      }
      if (/\/search\?/.test(p)) return json(HITS(new URLSearchParams(p.split("?")[1]).get("q") || ""));
      if (/\/calendar$/.test(p)) return json(CAL);
      if (/\/notifications$/.test(p)) return json(NOTIFS);
      if (/\/views$/.test(p)) return json(VIEWS);
      if (/\/onboarding\/applications$/.test(p)) return json(APPS);
      if (/\/voice\/intake$/.test(p)) return json(INTAKE);
      if (/\/voice\/logs$/.test(p)) return json(LOGS);
      // No early 404 here: fixtures below are method-agnostic (POST variants
      // are guarded with opts.method === "POST" ahead of their GET twins).
    }
    // Shared fixtures + POSTs: plausible server answers (mutations are acknowledged, views then reload fixtures)
    if (/\/cases\/initiate$/.test(p)) return json({ case_id: "c1" });
    if (/\/checklists\/[\w-]+\/check$/.test(p)) return json({ ok: true });
    if (/\/cases\/bulk$/.test(p)) {
      const ids = JSON.parse(opts.body || "{}").case_ids || [];
      return json({ results: ids.map((id) => ({ case_id: id, ok: true })) });
    }
    if (/\/queues\/grab-next$/.test(p)) return json({ claimed: true, case_id: "c2", case_number: "CMS-TX-2026-01479", status: "OFFER_WINDOW_OPEN" });
    // --- graph intelligence fixtures (mirror graph-intel response shapes) ---
    if (/\/graph\/ask$/.test(p)) {
      const q = JSON.parse(opts.body || "{}").question || "";
      const num = (q.match(/IDR-2026-\d+|CMS-TX-2026-\d+/i) || ["CMS-TX-2026-01479"])[0];
      return json({
        log_id: "demo" + Math.random().toString(16).slice(2, 10), tenant: "tx", question: q,
        answer: `Case ${num} sits in a cluster of 3 disputes between Lone Star Imaging and BlueShield of Texas, all on the same service line. The strongest connection runs through the shared payer: ${num} -[AGAINST]-> BlueShield of Texas <-[AGAINST]- CMS-TX-2026-01480. GraphSAGE link prediction ranks CMS-TX-2026-01480 (91%) and CMS-TX-2026-01502 (74%) as likely related — consistent with batching criteria in 45 CFR 149.510(c)(3).`,
        entities: [{ kind: "Case", id: "c2", label: num, detail: "OFFER_WINDOW_OPEN" }, { kind: "Party", id: "BlueShield of Texas", label: "BlueShield of Texas", detail: "payer" }],
        citations: [
          { path: `${num} -[AGAINST]-> BlueShield of Texas <-[AGAINST]- CMS-TX-2026-01480` },
          { path: `${num} -[FILED_BY]-> Lone Star Imaging <-[FILED_BY]- CMS-TX-2026-01502` },
        ],
        gnn_ranked: [{ case_id: "c3", score: 0.91 }, { case_id: "c5", score: 0.74 }],
        generator: "ollama:qwen2.5:3b", latency_ms: 812,
      });
    }
    if (/\/graph\/feedback$/.test(p)) return json({ log_id: JSON.parse(opts.body || "{}").log_id, rating: 1, edges_reinforced: 2 });
    if (/\/cases\/[\w-]+\/related$/.test(p)) return json({ case_id: "c2", model_version: "graphsage-np-1.0",
      predictions: [{ case_id: "c3", score: 0.91, model_version: "graphsage-np-1.0" }, { case_id: "c5", score: 0.74, model_version: "graphsage-np-1.0" }, { case_id: "c1", score: 0.58, model_version: "graphsage-np-1.0" }] });
    if (/\/cases\/[\w-]+\/graph-neighbors$/.test(p)) return json({ case_id: "c2", hops: 2, nodes: [
      { kind: "Party", id: "BlueShield of Texas", label: "BlueShield of Texas", detail: "payer" },
      { kind: "Party", id: "Lone Star Imaging", label: "Lone Star Imaging", detail: "provider" },
      { kind: "Case", id: "c3", label: "CMS-TX-2026-01480", detail: "OFFERS_SEALED" }] });
    if (/\/graph\/sync$/.test(p)) return json({ tenant: "tx", cases: 6, bronze: "lakehouse/bronze/cases-tx-demo.jsonl", silver: "lakehouse/silver/cases_tx.parquet" });
    if (/\/graph\/train$/.test(p)) return json({ tenant: "tx", trained: true, model_version: "graphsage-np-1.0", cases: 6, positive_edges: 11, epochs: 120, final_loss: 0.214, train_seconds: 0.4 });

    // --- program-rules fixtures (FL AHCA shape on the demo tenant) ---
    if (/\/program$/.test(p)) return json({ program: "custom", config: {
      case_number: { pattern: "FL{yy}-{seq}", seq_pad: 3 },
      statuses: { internal: ["Pre-Case", "Initial Review", "Hold", "Full Review", "Determination", "Plan Notification Packet Issued", "Provider Closure Letter Issued", "Final Order Issued", "Decided - Invoice Paid", "Plan Opt-Out", "Ineligible", "Dismissed", "Withdrawn"],
        agency: ["Submitted", "Under Review", "Eligible", "Ineligible", "Determination Issued", "Other"] },
      clocks: [
        { name: "AGENCY_RECOMMENDATION", label: "Agency recommendation", basis: "received_at", days: 60, day_type: "calendar", cite: "AHCA CDR contract §2.3.3", breach: "escalate_pm" },
        { name: "PLAN_RESPONSE", label: "Plan response window", basis: "plan_notified_at", days: 15, day_type: "calendar", cite: "AHCA CDR contract", breach: "follow_up" }],
      eligibility: { thresholds: [{ provider_type: "hospital_inpatient", contracted: true, min_cents: 2500000 }],
        filing_window_months: 12, ineligibility_reasons: ["below_threshold", "over_12_months"] },
      correspondence: { templates: [
        { key: "estimate_cost", subject: "Full Review Estimate {case_number}: FL AHCA", to: ["filing_party"], cc: ["agency"] },
        { key: "acceptance", subject: "Results of Preliminary Review {case_number}: FL AHCA", to: ["provider"], cc: ["agency", "health_plan"] },
        { key: "dismissal", subject: "Dismissal {case_number}: FL AHCA", to: ["provider"], cc: ["agency"], qa_role: "ATTORNEY" }] },
      deliverables: [
        { name: "Weekly report", due_rule: "weekly:MONDAY" }, { name: "Monthly report", due_rule: "monthly:10" },
        { name: "Agency recommendation letter", due_rule: "case:AGENCY_RECOMMENDATION" }],
    } });
    if (/\/cases\/[\w-]+\/program-date$/.test(p)) return json({ status: "recorded" });
    if (/\/cases\/[\w-]+\/status$/.test(p)) return json({ status: "updated" });
        if (/\/cases\/[\w-]+\/eligibility$/.test(p) && (!opts.method || opts.method === "GET")) return json({ reviews: [
      { id: "er1", result: "ELIGIBLE", reason: "", evidence: { threshold_min_cents: 2500000 }, decided_by: "maria.chen", created_at: d(30) }] });
if (/\/cases\/[\w-]+\/eligibility$/.test(p) && opts.method === "POST") return json({ review_id: "er1", result: "ELIGIBLE", reason: "", evidence: { threshold_min_cents: 2500000 } });
    if (/\/cases\/[\w-]+\/correspondence$/.test(p) && opts.method === "POST") {
      const b = JSON.parse(opts.body || "{}");
      return json(b.template === "dismissal"
        ? { qa_id: "qa1", status: "PENDING", subject: "Dismissal CMS-TX-2026-01482: FL AHCA" }
        : { qa_id: "qa2", status: "SENT", subject: "Estimate CMS-TX-2026-01482: FL AHCA" });
    }
    if (/\/cases\/[\w-]+\/correspondence$/.test(p)) return json({ correspondence: [
      { id: "m1", direction: "OUT", template: "estimate_cost", subject: "Full Review Estimate CMS-TX-2026-01482: FL AHCA", recipients: { to: ["billing@provider.example"] }, sent_by: "maria.chen", created_at: d(30) },
      { id: "m2", direction: "IN", template: null, subject: "RE: Full Review Estimate", recipients: {}, sent_by: "billing@provider.example", created_at: d(24) }] });
    if (/\/cases\/[\w-]+\/share-links$/.test(p)) return json({ token: "demo-share-7f3a", path: "/s/demo-share-7f3a", kind: JSON.parse(opts.body || "{}").kind });
    if (/\/cases\/[\w-]+\/invoices$/.test(p) && opts.method === "POST") return json({ invoice_id: "inv2", invoice_no: "CMS-TX-2026-01482" });
    if (/\/cases\/[\w-]+\/invoices$/.test(p)) return json({ invoices: [
      { id: "inv1", invoice_no: "CMS-TX-2026-01482", party: "PROVIDER", kind: "INITIAL_FEE", amount_cents: 12359, status: "OPEN", due_date: d(-20).slice(0, 10) },
      { id: "inv3", invoice_no: "CMS-TX-2026-01482", party: "HEALTH_PLAN", kind: "FULL_REVIEW", amount_cents: 41200, status: "PAID", due_date: d(10).slice(0, 10) }] });
    if (/\/invoices\/[\w-]+\/settle$/.test(p)) return json({ status: "PAID" });
    if (/\/reports\/receivables$/.test(p)) return json({ receivables: [
      { party: "PROVIDER", kind: "INITIAL_FEE", status: "OPEN", n: 14, total_cents: 1730260, overdue_cents: 247180 }] });
    if (/\/cases\/[\w-]+\/claims$/.test(p) && opts.method === "POST") {
      const n = (JSON.parse(opts.body || "{}").claims || []).length;
      // Demo: imports of >=100 claims exercise the large-volume policy path.
      return json(n >= 100
        ? { imported: n, volume_check: { large_volume: true, claims: n, review_class: "no_medical_review",
            violations: n > 500 ? [`${n} claims exceeds the 500-claim per-dispute cap (no_medical_review)`] : [],
            disposition: "INELIGIBLE (resubmission permitted once cured)" } }
        : { imported: n });
    }
    if (/\/cases\/[\w-]+\/claims$/.test(p)) return json({ claims: [
      { claim_number: "CLM-1042", cpt: "99285", billed_cents: 184200, paid_cents: 91200, created_at: d(40) },
      { claim_number: "CLM-1043", cpt: "99291", billed_cents: 226000, paid_cents: 118400, created_at: d(40) }] });
    if (/\/qa\/[\w-]+\/decision$/.test(p)) return json({ status: JSON.parse(opts.body || "{}").decision === "APPROVE" ? "APPROVED" : "REJECTED" });
    if (/\/qa\/[\w-]+$/.test(p)) return json({ id: "qa1", case_id: "c1", channel: "email", subject: "Dismissal CMS-TX-2026-01482: FL AHCA",
      body: "Dear provider,\n\nFollowing preliminary review, case CMS-TX-2026-01482 does not meet the program eligibility threshold…",
      to_recipients: ["billing@provider.example"], cc_recipients: ["cdr@ahca.example"], status: "PENDING", drafted_by: "maria.chen", created_at: d(2) });
    if (/\/qa$/.test(p)) return json({ queue: [
      { id: "qa1", case_id: "c1", artifact: "dismissal", channel: "email", subject: "Dismissal CMS-TX-2026-01482: FL AHCA", status: "PENDING", drafted_by: "maria.chen", created_at: d(2) }] ,
      recent: [
        { id: "qa0", case_id: "c3", subject: "Determination letter FL26-014", status: "SENT", drafted_by: "maria.chen", reviewed_by: "atty.rogers", reviewed_at: d(26) },
        { id: "qa9", case_id: "c5", subject: "Payment chase FL26-009", status: "REJECTED", drafted_by: "sam.ortiz", reviewed_by: "atty.rogers", reviewed_at: d(50) }] });
    if (/\/intake$/.test(p) && opts.method === "POST") return json({ intake_id: "in2", status: "INSTRUCTED" });
    if (/\/intake\/[\w-]+\/advance$/.test(p)) return json({ status: JSON.parse(opts.body || "{}").status });
    if (/\/intake$/.test(p)) return json({ intake: [
      { id: "in1", email: "revcycle@memorial.example", org: "Memorial Regional", status: "DOCS_RECEIVED", outreach_at: d(48), created_at: d(50), filing_party_type: "PROVIDER", packet_complete_at: null },
      { id: "in4", email: "disputes@bayfront.example", org: "Bayfront Medical", status: "PACKET_COMPLETE", outreach_at: d(200), created_at: d(202), filing_party_type: "PROVIDER", packet_complete_at: d(100) },
      { id: "in3", email: "claims@sunhealth.example", org: "Sun Health Plan", status: "PAID", outreach_at: d(120), created_at: d(122), filing_party_type: "HEALTH_PLAN", packet_complete_at: null }] });
    if (/\/manifest$/.test(p)) return json({
      program: "fl-ahca-claims-dispute", version: "1.0.2026", sector: "healthcare",
      terminology: { case_noun: "Dispute", case_plural: "Disputes", party_a: "Provider", party_b: "Health Plan", neutral: "Reviewer", intake_noun: "Intake request" },
      lifecycle: {
        intake_statuses: [
          { name: "INSTRUCTED", label: "Packet requested" }, { name: "DOCS_RECEIVED", label: "Docs received" },
          { name: "PACKET_COMPLETE", label: "Packet complete", anchors_clock: "packet_complete_at" },
          { name: "PAID", label: "Fee paid" },
          { name: "CONVERTED", label: "Converted to dispute", terminal: true },
          { name: "INELIGIBLE", label: "Ineligible", terminal: true },
          { name: "CLOSED_REFUNDED", label: "Closed (refunded)", terminal: true }],
        case_statuses: ["ACCEPTED","PLAN_NOTIFICATION","IN_REVIEW","DECIDED","PLAN_OPT_OUT","INELIGIBLE","DISMISSED","WITHDRAWN"]
          .map((n) => ({ name: n, label: n.replace(/_/g, " ") }))
      },
      clocks: [
        { name: "initial_review", days: 10, day_type: "calendar", basis: "packet_complete_at" },
        { name: "agency_determination", days: 60, day_type: "calendar", basis: "initiation_at" },
        { name: "completeness_gate", days: 13, day_type: "calendar", basis: "outreach_at" }]
    });
    if (/\/rules\/audit$/.test(p)) return json({ changes: [
      { id: 2, changed_by: "f3a1c9e2-admin-4b7d", note: "AHCA memo: day-13 completeness gate", before: [], after: [{ name: "intake-day13-incomplete" }], changed_at: d(72) },
      { id: 1, changed_by: "f3a1c9e2-admin-4b7d", note: "Initial rule set", before: [], after: [], changed_at: d(200) }] });
    if (/\/rules$/.test(p) && opts.method === "PUT") {
      const body = JSON.parse(opts.body || "{}");
      return json({ rules: body.rules || [], saved: (body.rules || []).length });
    }
    if (/\/rules$/.test(p)) return json({ rules: [
      { name: "intake-day13-incomplete", event: "sweep.intake", enabled: true,
        _basis: "AHCA 2026: documentation not received by the 13th day => incomplete, ineligibility letter issues",
        conditions: [{ field: "days_since_outreach", op: "gte", value: 13 }, { field: "status", op: "in", value: ["INSTRUCTED", "DOCS_RECEIVED"] }],
        actions: [{ type: "set_status", params: { status: "INELIGIBLE" } }, { type: "notify", params: { kind: "SLA_BREACH", body: "Intake {{id}} ({{email}}) incomplete at day {{days}} — issue ineligibility letter" } }] },
      { name: "doc-unverified-fields-review", event: "doc.analyzed", enabled: true,
        _basis: "Extracted fields that cannot be traced to source text must be human-verified before feeding a determination",
        conditions: [{ field: "ungrounded_count", op: "gt", value: 0 }],
        actions: [{ type: "flag_review", params: { reason: "{{ungrounded_count}} unverified field(s) — requires human confirmation" } }, { type: "notify", params: { kind: "MILESTONE", body: "Document {{doc_id}} flagged for review" } }] },
      { name: "doc-poor-scan-review", event: "doc.analyzed", enabled: false,
        _basis: "Barely-legible scans are valid evidence but extraction confidence is degraded",
        conditions: [{ field: "scan_quality_poor", op: "eq", value: true }],
        actions: [{ type: "flag_review", params: { reason: "Poor scan quality — verify against the original" } }] }] });
    if (/\/deliverables$/.test(p) && opts.method === "POST") return json({ status: "DELIVERED" });
    if (/\/deliverables$/.test(p)) return json({ deliverables: [
      { name: "Weekly report", due_rule: "weekly:MONDAY", next_due: d(-96).slice(0, 10) },
      { name: "Monthly report", due_rule: "monthly:10", next_due: d(-240).slice(0, 10) },
      { name: "Agency recommendation letter", due_rule: "case:AGENCY_RECOMMENDATION", next_due: "event-driven" }],
      history: [{ name: "Weekly report", status: "DELIVERED", delivered_at: d(24) }] });
    if (/\/cases\/[\w-]+\/opt-out$/.test(p)) return json({ status: "recorded" });
    if (/\/invoices\/[\w-]+\/checkout$/.test(p)) return json({ payment_id: "pay2", checkout_url: "", session_id: "cs_test_demo" });
    if (/\/reports\/financial$/.test(p)) return json({
      kpi: { collected_cents: 4812300, refunded_cents: 41200, collected_30d_cents: 918400, payments_count: 37 },
      receivables: [
        { status: "OPEN", party: "PROVIDER", n: 14, total_cents: 1730260 },
        { status: "OPEN", party: "HEALTH_PLAN", n: 9, total_cents: 1240800 },
        { status: "PAID", party: "PROVIDER", n: 22, total_cents: 2718980 },
        { status: "PAID", party: "HEALTH_PLAN", n: 15, total_cents: 2093320 },
        { status: "REFUNDED", party: "PROVIDER", n: 1, total_cents: 41200 }],
      aging: [
        { bucket: "current", n: 12, total_cents: 1890400 },
        { bucket: "1-30", n: 7, total_cents: 743500 },
        { bucket: "31-60", n: 3, total_cents: 261160 },
        { bucket: "60+", n: 1, total_cents: 76000 }],
      by_method: [{ provider: "stripe", status: "PAID", n: 31, total_cents: 4421100 }, { provider: "stripe", status: "PENDING", n: 3, total_cents: 247180 }],
      events: [
        { id: 9, case_id: "c1", kind: "PAYMENT_PAID", direction: "IN", amount_cents: 41200, party: "HEALTH_PLAN", ref: "pi_3Qf2demo1", actor: "stripe-webhook", created_at: d(4) },
        { id: 8, case_id: "c1", kind: "PAYMENT_INITIATED", direction: "NONE", amount_cents: 41200, party: "HEALTH_PLAN", ref: "cs_test_demo1", actor: "maria.chen", created_at: d(5) },
        { id: 7, case_id: "c2", kind: "INVOICE_ISSUED", direction: "NONE", amount_cents: 12359, party: "PROVIDER", ref: "CMS-TX-2026-01479", actor: "maria.chen", created_at: d(9) },
        { id: 6, case_id: "c3", kind: "REFUND_ISSUED", direction: "OUT", amount_cents: 41200, party: "PROVIDER", ref: "pi_3Qe9demo7", actor: "stripe-webhook", created_at: d(12) },
        { id: 5, case_id: "c3", kind: "PAYMENT_PAID", direction: "IN", amount_cents: 41200, party: "PROVIDER", ref: "pi_3Qe9demo7", actor: "stripe-webhook", created_at: d(30) }],
      stripe_enabled: true });
    if (/\/presence\/ping$/.test(p)) return json({ status: "seen" });
    if (/\/ops\/dashboard$/.test(p)) return json({
      tenant: "tx",
      online: [
        { user_sub: "demo-maria", display_name: "maria.chen", roles: ["CASE_MANAGER"], last_seen: new Date().toISOString() },
        { user_sub: "demo-james", display_name: "j.osei", roles: ["ARBITRATOR"], last_seen: new Date(Date.now() - 60e3).toISOString() },
        { user_sub: "demo-priya", display_name: "priya.nair", roles: ["FINANCE"], last_seen: new Date(Date.now() - 110e3).toISOString() }],
      tasks_by_assignee: [
        { assignee: "maria.chen", open: 9, overdue: 2, done_30d: 21 },
        { assignee: "j.osei", open: 6, overdue: 0, done_30d: 14 },
        { assignee: "priya.nair", open: 4, overdue: 1, done_30d: 11 },
        { assignee: "(unassigned)", open: 5, overdue: 3, done_30d: 0 }],
      task_kpis: [{ open: 24, overdue: 6, done_30d: 46, created_30d: 61, completion_pct_30d: 75.4 }],
      cases: [
        { status: "IN_REVIEW", n: 18 }, { status: "ACCEPTED", n: 11 },
        { status: "DECIDED", n: 27 }, { status: "PLAN_NOTIFICATION", n: 6 },
        { status: "DISMISSED", n: 4 }, { status: "WITHDRAWN", n: 2 }],
      case_kpis: [{ unassigned_open: 7, opened_7d: 9, opened_30d: 34, avg_open_age_days: 16.2 }],
      sla: [{ breaches_7d: 1, breaches_total: 5, determination_breaches: 3, payment_breaches: 2 }],
      outstanding: [
        { party: "HEALTH_PLAN", open_invoices: 9, open_cents: 1240800, overdue_cents: 261160 },
        { party: "PROVIDER", open_invoices: 14, open_cents: 1730260, overdue_cents: 76000 }],
      financial: [{ collected_cents: 4812300, collected_30d_cents: 918400, refunded_cents: 41200 }],
      queues: [{ checks_review: 1, checks_awaiting_clear: 1, qa_pending: 3, intake_open: 4, onboarding_pending: 2 }],
      escalations: [
        { case_id: "c2-demo-8871", clock: "DETERMINATION_30BD", level: 1, escalated_to: "SUPERVISOR", created_at: d(20) },
        { case_id: "c7-demo-1120", clock: "PAYMENT_30CD", level: 2, escalated_to: "FEDERAL_ADMIN", created_at: d(70) }],
      cases_trend: Array.from({ length: 30 }, (_, i) => ({ day: d((29 - i) * 24).slice(0, 10), opened: [2,3,1,0,4,2,5,3,2,1,3,4,2,0,1,3,2,4,5,3,2,1,4,3,2,6,4,3,5,4][i] })),
      collections_trend: Array.from({ length: 30 }, (_, i) => ({ day: d((29 - i) * 24).slice(0, 10),
        collected_cents: [41200,0,12359,88750,41200,0,0,152300,41200,66000,0,23410,98000,41200,0,12359,41200,88750,0,41200,152300,66000,41200,0,98000,41200,23410,12359,88750,152300][i], refunded_cents: 0 })),
      throughput_trend: Array.from({ length: 14 }, (_, i) => ({ day: d((13 - i) * 24).slice(0, 10), done: [3,5,2,6,4,1,0,4,6,3,5,7,4,5][i] })) });
    if (/\/payments$/.test(p) || /\/cases\/[\w-]+\/payments$/.test(p)) return json({ payments: [
      { id: "pay1", case_id: "c1", invoice_id: "inv3", provider: "stripe", session_id: "cs_test_demo1", payment_intent: "pi_3Qf2demo1", amount_cents: 41200, currency: "usd", payer_email: "ap@sunhealth.example", status: "PAID", created_at: d(4) },
      { id: "pay3", case_id: "c2", invoice_id: "inv1", provider: "stripe", session_id: "cs_test_demo2", payment_intent: null, amount_cents: 12359, currency: "usd", payer_email: null, status: "PENDING", created_at: d(1) }] });
    if (/\/cases\/[\w-]+\/assign$/.test(p)) return json({ assigned_to: "m.chen" });
    if (/\/fees\/transfer$/.test(p)) return json({ transfer_id: "tb-demo-1842", posted: true });
    if (/\/voice\/outbound$/.test(p)) return json({ status: "QUEUED" });
    return method === "GET" ? json({ error: "not found" }, 404) : json({ ok: true });
  };
})();
