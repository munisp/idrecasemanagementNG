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
      eligibilityHistory: (caseId) => req("GET", `${t()}/cases/${caseId}/eligibility`),
      optOut: (caseId, eligible, rationale) => req("POST", `${t()}/cases/${caseId}/opt-out`, { eligible, rationale }),
      send: (caseId, p) => req("POST", `${t()}/cases/${caseId}/correspondence`, p),
      correspondence: (caseId) => req("GET", `${t()}/cases/${caseId}/correspondence`),
      shareLink: (caseId, kind, days) => req("POST", `${t()}/cases/${caseId}/share-links`, { kind, days_ttl: days }),
      invoices: (caseId) => req("GET", `${t()}/cases/${caseId}/invoices`),
      issueInvoice: (caseId, p) => req("POST", `${t()}/cases/${caseId}/invoices`, p),
      settleInvoice: (invId, action, ref) => req("POST", `${t()}/invoices/${invId}/settle`, { action, remittance_ref: ref }),
      receivables: () => req("GET", `${t()}/reports/receivables`),
      financial: () => req("GET", `${t()}/reports/financial`),
      payments: (caseId, opts) => req("GET", (caseId ? `${t()}/cases/${caseId}/payments` : `${t()}/payments`) + qs(opts || {})),
      checkout: (invId) => req("POST", `${t()}/invoices/${invId}/checkout`),
      claims: (caseId) => req("GET", `${t()}/cases/${caseId}/claims`),
      importClaims: (caseId, claims) => req("POST", `${t()}/cases/${caseId}/claims`, { claims }),
      qaQueue: () => req("GET", `${t()}/qa`),
      qaGet: (id) => req("GET", `${t()}/qa/${id}`),
      qaDecision: (id, decision, note, editedBody) => req("POST", `${t()}/qa/${id}/decision`, { decision, note, edited_body: editedBody || undefined }),
      intake: (opts) => req("GET", `${t()}/intake${qs(opts || {})}`),
      createIntake: (p) => req("POST", `${t()}/intake`, p),
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
