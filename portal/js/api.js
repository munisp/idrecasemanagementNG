// api.js — typed fetch wrapper: JWT + tenant scoping + offline awareness.
const Api = (() => {
  let tenant = localStorage.getItem("idre.tenant") || null;

  function setTenant(t) { tenant = t; localStorage.setItem("idre.tenant", t); }
  function getTenant() { return tenant; }

  async function req(method, path, body, isForm) {
    const tok = await Auth.token();
    if (!tok) throw new Error("unauthenticated");
    const headers = { Authorization: `Bearer ${tok}` };
    if (body && !isForm) headers["Content-Type"] = "application/json";
    const resp = await fetch(window.IDRE_CONFIG.apiBase + path, {
      method, headers,
      body: isForm ? body : body ? JSON.stringify(body) : undefined,
    });
    if (resp.status === 401) { Auth.login(); throw new Error("redirecting"); }
    const data = resp.headers.get("content-type")?.includes("json") ? await resp.json() : await resp.text();
    if (!resp.ok) {
      const msg = typeof data === "object" && data.error ? data.error : `HTTP ${resp.status}`;
      const err = new Error(msg); err.status = resp.status; err.data = data; throw err;
    }
    return data;
  }

  async function download(path, fallbackName) {
    const tok = await Auth.token();
    if (!tok) throw new Error("unauthenticated");
    const resp = await fetch(window.IDRE_CONFIG.apiBase + path, { headers: { Authorization: `Bearer ${tok}` } });
    if (resp.status === 401) { Auth.login(); throw new Error("redirecting"); }
    if (!resp.ok) throw new Error(`download failed (HTTP ${resp.status})`);
    const cd = resp.headers.get("content-disposition") || "";
    const name = (cd.match(/filename="?([^"]+)"?/) || [])[1] || fallbackName || "download";
    const blobUrl = URL.createObjectURL(await resp.blob());
    const a = document.createElement("a");
    a.href = blobUrl; a.download = name;
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(blobUrl), 10000);
  }

  const t = () => `/v1/tenants/${tenant}`;
  // query-string builder: drops empty values, returns "" or "?a=1&b=2"
  const qs = (params) => {
    const s = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== "" && v != null)).toString();
    return s ? `?${s}` : "";
  };
  return {
    setTenant, getTenant,
    cases: {
      // Keyset-paginated: {cases, next_cursor, total}. Legacy bare-array
      // responses are normalized so older backends still work.
      list: (params = {}) => {
        const qs = new URLSearchParams(Object.entries(params).filter(([, v]) => v)).toString();
        return req("GET", `${t()}/cases${qs ? "?" + qs : ""}`)
          .then((r) => (Array.isArray(r) ? { cases: r, next_cursor: "", total: r.length } : r));
      },
      get: (id) => req("GET", `${t()}/cases/${id}`),
      initiate: (payload) => req("POST", `${t()}/cases/initiate`, payload),
      signal: (id, signal, data) => req("POST", `${t()}/cases/${id}/signal`, { signal, data }),
      upload: (id, file, sealed, folder) => {
        const fd = new FormData();
        fd.append("file", file);
        if (sealed) fd.append("sealed", "true");
        if (folder) fd.append("folder", folder);
        return req("POST", `${t()}/cases/${id}/documents`, fd, true);
      },
      moveDoc: (id, docId, folder) => req("PATCH", `${t()}/cases/${id}/documents/${docId}`, { folder }),
      zipUrl: (id) => `${t()}/cases/${id}/documents.zip`,
      setDetails: (id, details) => req("PATCH", `${t()}/cases/${id}/details`, details),
      lettergen: (id, templateKey) => req("POST", `${t()}/cases/${id}/lettergen/${templateKey}`),
      documents: (id) => req("GET", `${t()}/cases/${id}/documents`),
      timeAdd: (id, p) => req("POST", `${t()}/cases/${id}/time`, p),
      timeList: (id) => req("GET", `${t()}/cases/${id}/time`),
    // Personal timesheet: daily entry screen with day/week/month totals.
    time: {
      mine: (from, to) => req("GET", `${t()}/time/mine${from||to ? `?from=${from||""}&to=${to||""}` : ""}`),
      summary: () => req("GET", `${t()}/time/summary`),
      update: (id, p) => req("PUT", `${t()}/time/${id}`, p),
      remove: (id) => req("DELETE", `${t()}/time/${id}`),
    },
      analysis: (id, docId) => req("GET", `${t()}/cases/${id}/documents/${docId}/analysis`),
      downloadUrl: (id, docId) => `${t()}/cases/${id}/documents/${docId}/download`,
      activities: (id) => req("GET", `${t()}/cases/${id}/activities`),
    },
    onboarding: {
      submit: (payload) => req("POST", `${t()}/onboarding/applications`, payload),
      list: () => req("GET", `${t()}/onboarding/applications`),
      decide: (id, decision, reason) => req("POST", `${t()}/onboarding/applications/${id}/decision`, { decision, reason }),
    },
    voice: {
      intake: () => req("GET", `${t()}/voice/intake`),
      logs: () => req("GET", `${t()}/voice/logs`),
      outbound: (to, caseNumber, script) =>
        req("POST", `${t()}/voice/outbound`, { to, case_number: caseNumber, script }),
    },
    reports: {
      sla: () => req("GET", `${t()}/reports/sla`),
      summary: () => req("GET", `${t()}/reports/summary`),
    },
    crm: {
      // Offset-paginated: {accounts, total, next_offset} (next_offset = -1 at end).
      accounts: (type, params = {}) => {
        const qs = new URLSearchParams({ ...(type ? { type } : {}), ...params }).toString();
        return req("GET", `${t()}/accounts${qs ? "?" + qs : ""}`)
          .then((r) => (Array.isArray(r) ? { accounts: r, total: r.length, next_offset: -1 } : r));
      },
      createAccount: (p) => req("POST", `${t()}/accounts`, p),
      account360: (id) => req("GET", `${t()}/accounts/${id}/360`),
      createContact: (p) => req("POST", `${t()}/contacts`, p),
      leads: (params = {}) => {
        const qs = new URLSearchParams(params).toString();
        return req("GET", `${t()}/leads${qs ? "?" + qs : ""}`)
          .then((r) => (Array.isArray(r) ? { leads: r, total: r.length, next_offset: -1 } : r));
      },
      convertLead: (id, type) => req("POST", `${t()}/leads/${id}/convert`, { type }),
      tasks: (mine, params = {}) => {
        const qs = new URLSearchParams({ ...(mine ? { mine: "true" } : {}), ...params }).toString();
        return req("GET", `${t()}/tasks${qs ? "?" + qs : ""}`)
          .then((r) => (Array.isArray(r) ? { tasks: r, total: r.length, next_offset: -1 } : r));
      },
      createTask: (p) => req("POST", `${t()}/tasks`, p),
      completeTask: (id) => req("POST", `${t()}/tasks/${id}/complete`),
      addNote: (p) => req("POST", `${t()}/notes`, p),
      search: (q) => req("GET", `${t()}/search?q=${encodeURIComponent(q)}`),
    },
    cm: {
      assign: (caseId, role, assignee) => req("POST", `${t()}/cases/${caseId}/assign`, { role, assignee }),
      escalate: (caseId, clock, detail) => req("POST", `${t()}/cases/${caseId}/escalate`, { clock, detail }),
      relate: (p) => req("POST", `${t()}/cases/relate`, p),
      relationships: (caseId) => req("GET", `${t()}/cases/${caseId}/relationships`),
      checklist: (caseId) => req("GET", `${t()}/cases/${caseId}/checklist`),
      checkItem: (itemId) => req("POST", `${t()}/checklists/${itemId}/check`),
      calendar: () => req("GET", `${t()}/calendar`),
      notifications: () => req("GET", `${t()}/notifications`),
      readNotif: (id) => req("POST", `${t()}/notifications/${id}/read`),
      views: () => req("GET", `${t()}/views`),
      saveView: (p) => req("POST", `${t()}/views`, p),
      clocks: (id) => req("GET", `${t()}/cases/${id}/clocks`),
      allClocks: () => req("GET", `${t()}/cases/clocks`),
      bulk: (p) => req("POST", `${t()}/cases/bulk`, p),
      grabNext: () => req("POST", `${t()}/queues/grab-next`),
      letter: (caseId, template, qs) => req("POST", `${t()}/cases/${caseId}/letters/${template}${qs || ""}`),
    },
    graph: {
      ask: (question, k) => req("POST", `${t()}/graph/ask`, { question, k }),
      feedback: (logId, rating) => req("POST", `${t()}/graph/feedback`, { log_id: logId, rating }),
      related: (caseId) => req("GET", `${t()}/cases/${caseId}/related`),
      neighbors: (caseId) => req("GET", `${t()}/cases/${caseId}/graph-neighbors`),
      sync: () => req("POST", `${t()}/graph/sync`),
      train: (epochs) => req("POST", `${t()}/graph/train`, epochs ? { epochs } : {}),
    },
    prefs: {
      all: () => req("GET", `${t()}/prefs`),
      put: (key, value) => req("PUT", `${t()}/prefs`, { key, value }),
    },
    program: {
      get: () => req("GET", `${t()}/program`),
      manifest: () => req("GET", `${t()}/manifest`),
      opsDashboard: () => req("GET", `${t()}/ops/dashboard`),
      caseFinancials: (caseId) => req("GET", `${t()}/cases/${caseId}/financials`),
      // Service-fee invoicing engine
      billingGenerate: (params, body) => req("POST", `${t()}/billing/invoices${qs(params || {})}`, body || {}),
      billingList: (opts) => req("GET", `${t()}/billing/invoices${qs(opts || {})}`),
      billingGet: (id) => req("GET", `${t()}/billing/invoices/${id}`),
      billingAct: (id, action) => req("POST", `${t()}/billing/invoices/${id}/${action}`, {}),
      billingPay: (id, body) => req("POST", `${t()}/billing/invoices/${id}/payments`, body),
      billingExport: async (id) => download(`${t()}/billing/invoices/${id}/export`, "invoice.csv"),
      // Reconciliation engine + adapters
      reconImportCsv: (mapping, file) => {
        const fd = new FormData();
        fd.append("source", "csv_generic"); fd.append("mapping", mapping); fd.append("file", file);
        return req("POST", `${t()}/recon/import`, fd, true);
      },
      reconFetch: (feed) => req("POST", `${t()}/recon/import`, { source: "http_json", feed }),
      reconMatch: (id) => req("POST", `${t()}/recon/batches/${id}/match`, {}),
      reconBatches: () => req("GET", `${t()}/recon/batches`),
      reconBatch: (id) => req("GET", `${t()}/recon/batches/${id}`),
      reconResolve: (itemId, body) => req("POST", `${t()}/recon/items/${itemId}/resolve`, body),
      reconSummary: (opts) => req("GET", `${t()}/recon/summary${qs(opts || {})}`),
      // AR/AP subledger
      receivablesAging: (asOf) => req("GET", `${t()}/arap/receivables${qs(asOf ? { as_of: asOf } : {})}`),
      payablesList: (status, asOf) => req("GET", `${t()}/arap/payables${qs({ status: status || "OPEN", ...(asOf ? { as_of: asOf } : {}) })}`),
      payableCreate: (body) => req("POST", `${t()}/arap/payables`, body),
      payableAct: (id, action, body) => req("POST", `${t()}/arap/payables/${id}/${action}`, body || {}),
      arapSummary: (asOf) => req("GET", `${t()}/arap/summary${qs(asOf ? { as_of: asOf } : {})}`),
      nachaPayout: (body) => req("POST", `${t()}/arap/payouts/nacha`, body || {}),
      payoutBatches: () => req("GET", `${t()}/arap/payouts`),
      payoutBatchFile: (id) => req("GET", `${t()}/arap/payouts/${id}/file`),
      // Third-party administrators (file/track on behalf of initiating parties)
      tpaRegister: (body) => req("POST", "/api/public/tpa/register", body),
      tpaClaim: (code) => req("POST", `${t()}/tpa/claim`, { code }),
      tpaMe: () => req("GET", `${t()}/tpa/me`),
      tpaClients: () => req("GET", `${t()}/tpa/clients`),
      tpaAddClient: (body) => req("POST", `${t()}/tpa/clients`, body),
      tpaClientStatus: (id, status) => req("POST", `${t()}/tpa/clients/${id}/status`, { status }),
      tpaIntake: (body) => req("POST", `${t()}/tpa/intake`, body),
      tpaDashboard: (tpaId) => req("GET", `${t()}/tpa/dashboard${qs(tpaId ? { tpa_id: tpaId } : {})}`),
      adminTpas: () => req("GET", `${t()}/admin/tpas`),
      adminTpaStatus: (id, status) => req("POST", `${t()}/admin/tpas/${id}/status`, { status }),

      pingPresence: (name) => req("POST", `${t()}/presence/ping`, { name }),
      saveManifest: (manifest, note) => req("PUT", `${t()}/manifest`, { manifest, note }),
      rules: () => req("GET", `${t()}/rules`),
      saveRules: (rules, note) => req("PUT", `${t()}/rules`, { rules, note }),
      rulesAudit: () => req("GET", `${t()}/rules/audit`),
      setDate: (caseId, key, value) => req("POST", `${t()}/cases/${caseId}/program-date`, { key, value }),
      setStatus: (caseId, p) => req("POST", `${t()}/cases/${caseId}/status`, p),
      eligibility: (caseId, p) => req("POST", `${t()}/cases/${caseId}/eligibility`, p),
      eligibilityAuto: (caseId) => req("POST", `${t()}/cases/${caseId}/eligibility/auto`),
      copilotBrief: (caseId) => req("POST", `${t()}/cases/${caseId}/copilot/brief`),
      copilotBriefLatest: (caseId) => req("GET", `${t()}/cases/${caseId}/copilot/brief`),
      copilotDraft: (caseId, kind, instructions) => req("POST", `${t()}/cases/${caseId}/copilot/draft`, { kind, instructions }),
      copilotProposeActions: (caseId) => req("POST", `${t()}/cases/${caseId}/copilot/actions`),
      copilotListActions: (caseId) => req("GET", `${t()}/cases/${caseId}/copilot/actions`),
      copilotDecideActions: (caseId, batchId, decision) => req("POST", `${t()}/cases/${caseId}/copilot/actions/${batchId}/decision`, { decision }),
      copilotChat: (caseId, message) => req("POST", `${t()}/cases/${caseId}/copilot/chat`, { message }),
      copilotChatHistory: (caseId) => req("GET", `${t()}/cases/${caseId}/copilot/chat`),
      briefing: () => req("GET", `${t()}/assistant/briefing`),
      eligibilityHistory: (caseId) => req("GET", `${t()}/cases/${caseId}/eligibility`),
      optOut: (caseId, eligible, rationale) => req("POST", `${t()}/cases/${caseId}/opt-out`, { eligible, rationale }),
      send: (caseId, p) => req("POST", `${t()}/cases/${caseId}/correspondence`, p),
      correspondence: (caseId) => req("GET", `${t()}/cases/${caseId}/correspondence`),
      shareLink: (caseId, kind, days) => req("POST", `${t()}/cases/${caseId}/share-links`, { kind, days_ttl: days }),
      checks: (status, opts) => req("GET", `${t()}/checks${qs({ status: status || "", ...(opts || {}) })}`),
      clearCheck: (checkId, remittanceRef) => req("POST", `${t()}/checks/${checkId}/clear`, { remittance_ref: remittanceRef }),
      invoices: (caseId) => req("GET", `${t()}/cases/${caseId}/invoices`),
      issueInvoice: (caseId, p) => req("POST", `${t()}/cases/${caseId}/invoices`, p),
      settleInvoice: (invId, action, ref) => req("POST", `${t()}/invoices/${invId}/settle`, { action, remittance_ref: ref }),
      receivables: () => req("GET", `${t()}/reports/receivables`),
      financial: () => req("GET", `${t()}/reports/financial`),
      timeReport: (opts) => req("GET", `${t()}/reports/time${qs(opts || {})}`),
      timeReportSend: (p) => req("POST", `${t()}/time/report/send`, p),
      timeRates: () => req("GET", `${t()}/reports/time/rates`),
      timeRateSet: (role, rate_cents_per_hour) => req("PUT", `${t()}/reports/time/rates`, { role, rate_cents_per_hour }),
      payments: (caseId, opts) => req("GET", (caseId ? `${t()}/cases/${caseId}/payments` : `${t()}/payments`) + qs(opts || {})),
      checkout: (invId) => req("POST", `${t()}/invoices/${invId}/checkout`),
      claims: (caseId) => req("GET", `${t()}/cases/${caseId}/claims`),
      importClaims: (caseId, claims) => req("POST", `${t()}/cases/${caseId}/claims`, { claims }),
      qaQueue: (caseId) => req("GET", `${t()}/qa${caseId ? `?case_id=${encodeURIComponent(caseId)}` : ""}`),
      qaGet: (id) => req("GET", `${t()}/qa/${id}`),
      qaDecision: (id, decision, note, editedBody) => req("POST", `${t()}/qa/${id}/decision`, { decision, note, edited_body: editedBody || undefined }),
      intake: (opts) => req("GET", `${t()}/intake${qs(opts || {})}`),
      createIntake: (p) => req("POST", `${t()}/intake`, p),
      intakeBulk: (p) => req("POST", `${t()}/intake/bulk`, p),
      intakeBatches: (submitter) => req("GET", `${t()}/intake/bulk${submitter ? `?submitter=${encodeURIComponent(submitter)}` : ""}`),
      intakeBatch: (id) => req("GET", `${t()}/intake/bulk/${id}`),
      intakeConverse: (message, fields, history) => req("POST", `${t()}/intake/converse`, { message, fields, history }),
      advanceIntake: (id, status, caseId) => req("POST", `${t()}/intake/${id}/advance`, { status, case_id: caseId }),
      deliverables: () => req("GET", `${t()}/deliverables`),
      submitDeliverable: (p) => req("POST", `${t()}/deliverables`, p),
      requestDeliverable: (name, contract_ref) => req("POST", `${t()}/deliverables/request`, { name, contract_ref }),
    },
    fees: {
      transfer: (p) => req("POST", `${t()}/fees/transfer`, p),
    },
    geo: {
      mapUrl: () => `${config.caseApiBase}/geo/map`,
    },
  };
})();
