// ui.js — Meridian interaction layer: modal dialogs, toasts with undo, confirm,
// busy-state runner. Replaces every window.prompt / alert / confirm with branded,
// keyboard-complete, screen-reader-announced surfaces.
const UI = (() => {
  // ---- Toasts -------------------------------------------------------------
  // kinds: success (default for completed actions), info, warn, error.
  // Iconed, animated in/out, hover-to-pause, capped stack, aria-live.
  let region;
  const ICONS = { success: "✓", info: "ℹ", warn: "⚠", error: "✕" };

  function toast(message, opts = {}) {
    if (!region) {
      region = document.createElement("div");
      region.className = "toast-region";
      region.setAttribute("aria-live", "polite");
      region.setAttribute("role", "status");
      document.body.appendChild(region);
    }
    // Cap the stack at 4 — drop the oldest so rapid actions stay readable.
    while (region.children.length >= 4) region.firstChild.remove();

    const kind = opts.kind || "success";
    const t = document.createElement("div");
    t.className = `toast t-${kind} toast-in`;
    t.innerHTML = `<span class="toast-icon" aria-hidden="true">${ICONS[kind] || ICONS.info}</span><span class="toast-msg">${message}</span>`;
    if (opts.undo) {
      const b = document.createElement("button");
      b.textContent = "Undo";
      b.onclick = () => { opts.undo(); dismiss(); };
      t.appendChild(b);
    }
    const x = document.createElement("button");
    x.className = "toast-x";
    x.setAttribute("aria-label", "Dismiss notification");
    x.textContent = "✕";
    x.onclick = () => dismiss();
    t.appendChild(x);
    region.appendChild(t);

    // sticky: no auto-dismiss — reserved for findings the user MUST see
    // (e.g. policy violations); dismissal is via the ✕ button only.
    const sticky = !!opts.sticky;
    let remaining = opts.duration || (kind === "error" || kind === "warn" ? 9000 : 4500);
    let timer = sticky ? null : setTimeout(dismiss, remaining);
    let started = Date.now();
    if (!sticky) {
      t.addEventListener("mouseenter", () => { clearTimeout(timer); remaining -= Date.now() - started; });
      t.addEventListener("mouseleave", () => { started = Date.now(); timer = setTimeout(dismiss, Math.max(remaining, 800)); });
    }

    let gone = false;
    function dismiss() {
      if (gone) return;
      gone = true;
      clearTimeout(timer);
      t.classList.remove("toast-in");
      t.classList.add("toast-out");
      setTimeout(() => t.remove(), 220);
    }
    return dismiss;
  }

  // ---- Busy runner ----------------------------------------------------------
  // UI.run(button, async fn) — disables the control, shows an inline spinner,
  // restores it afterwards (label preserved). Prevents double-submit and gives
  // immediate "something is happening" feedback on every async action.
  async function run(ctrl, fn, busyLabel) {
    if (!ctrl || ctrl.disabled) return;
    const isBtn = /^(BUTTON|A)$/.test(ctrl.tagName);
    const prevHTML = isBtn ? ctrl.innerHTML : null;
    ctrl.disabled = true;
    ctrl.classList.add("is-busy");
    if (isBtn) ctrl.innerHTML = `<span class="spin" aria-hidden="true"></span>${busyLabel || prevHTML}`;
    try { return await fn(); }
    finally {
      ctrl.disabled = false;
      ctrl.classList.remove("is-busy");
      if (isBtn) ctrl.innerHTML = prevHTML;
    }
  }

  // Flash an element (row, field) to confirm a change landed.
  function flash(el, cls = "flash-ok") {
    if (!el) return;
    el.classList.remove(cls);
    void el.offsetWidth; // restart animation
    el.classList.add(cls);
    setTimeout(() => el.classList.remove(cls), 1400);
  }

  // ---- Modal (focus-trapped, Esc closes, returns a promise) ----
  // fields: [{name, label, type, value, placeholder, required, options, hint}]
  // onRefresh(vals, setOptions): optional live hook — fires (debounced) as
  // the user types so a modal can offer search-as-you-type selects;
  // setOptions(name, [[value,label],...]) re-renders one select's options.
  function modal({ title, body = "", fields = [], submitLabel = "Save", danger = false, wide = false, onRefresh = null }) {
    return new Promise((resolve) => {
      const prev = document.activeElement;
      const scrim = document.createElement("div");
      scrim.className = "modal-scrim";
      scrim.innerHTML = `
        <div class="modal ${wide ? "modal-wide" : ""}" role="dialog" aria-modal="true" aria-label="${title}">
          <div class="modal-h"><h2>${title}</h2>
            <button class="icon-btn modal-x" aria-label="Close dialog">✕</button></div>
          <form class="modal-b" novalidate>
            ${body ? `<p class="modal-body">${body}</p>` : ""}
            ${fields.map((f) => `
              <label class="mfield">${f.label}${f.required ? ' <span class="req">*</span>' : ""}
                ${f.options
                  ? `<select name="${f.name}" ${f.required ? "required" : ""}>${f.options.map((o) =>
                      `<option value="${o[0]}" ${o[0] === f.value ? "selected" : ""}>${o[1]}</option>`).join("")}</select>`
                  : f.type === "textarea"
                    ? `<textarea name="${f.name}" rows="3" placeholder="${f.placeholder || ""}" ${f.required ? "required" : ""}>${f.value || ""}</textarea>`
                    : `<input name="${f.name}" type="${f.type || "text"}" value="${f.value || ""}"
                        placeholder="${f.placeholder || ""}" ${f.required ? "required" : ""} ${f.step ? `step="${f.step}"` : ""} />`}
                ${f.hint ? `<span class="mhint">${f.hint}</span>` : ""}
              </label>`).join("")}
            <div class="modal-f">
              <button type="button" class="btn-secondary m-cancel">Cancel</button>
              <button type="submit" class="${danger ? "btn-danger" : "btn-primary"}">${submitLabel}</button>
            </div>
          </form>
        </div>`;
      document.body.appendChild(scrim);
      const close = (val) => { scrim.remove(); prev?.focus?.(); resolve(val); };
      scrim.querySelector(".modal-x").onclick = () => close(null);
      scrim.querySelector(".m-cancel").onclick = () => close(null);
      scrim.addEventListener("mousedown", (e) => { if (e.target === scrim) close(null); });
      scrim.addEventListener("keydown", (e) => {
        if (e.key === "Escape") close(null);
        if (e.key === "Tab") { // focus trap
          const els = [...scrim.querySelectorAll("button,input,select,textarea")].filter((x) => !x.disabled);
          const first = els[0], last = els[els.length - 1];
          if (e.shiftKey && document.activeElement === first) { last.focus(); e.preventDefault(); }
          else if (!e.shiftKey && document.activeElement === last) { first.focus(); e.preventDefault(); }
        }
      });
      if (onRefresh) {
        const form = scrim.querySelector("form");
        const setOptions = (name, opts) => {
          const sel = form.querySelector(`select[name="${name}"]`);
          if (sel) sel.innerHTML = opts.map((o) => `<option value="${o[0]}">${o[1]}</option>`).join("");
        };
        let tmr;
        form.addEventListener("input", () => {
          clearTimeout(tmr);
          tmr = setTimeout(() => onRefresh(Object.fromEntries(new FormData(form)), setOptions), 300);
        });
      }
      scrim.querySelector("form").onsubmit = (e) => {
        e.preventDefault();
        const data = Object.fromEntries(new FormData(e.target));
        for (const f of fields) {
          if (f.required && !String(data[f.name] || "").trim()) {
            e.target.querySelector(`[name="${f.name}"]`).focus();
            toast(`Enter ${f.label.toLowerCase()} to continue.`, { kind: "warn" });
            return;
          }
        }
        close(data);
      };
      scrim.querySelector("input,select,textarea,button")?.focus();
    });
  }

  const confirm = (title, body, submitLabel = "Confirm", danger = false) =>
    modal({ title, body, submitLabel, danger }).then((v) => v !== null);

  return { toast, modal, confirm, run, flash };
})();
