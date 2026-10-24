// Author: Patrick Munis at newwave
package main

// User preferences — server-side source of truth for the portal's device-local
// settings (theme, density, last tenant, palette recents). Closes the
// in-memory/local-only gap from the repo sweep: prefs previously lived only in
// localStorage, so they didn't follow a user across PWA, desktop, and the
// Capacitor native shell. Table: public.user_prefs (casemgmt-core.sql).

import (
	"encoding/json"
	"net/http"
)

const prefMaxValueBytes = 8192 // prefs are small; reject junk payloads

var prefAllowedKeys = map[string]bool{
	"theme": true, "density": true, "last_tenant": true, "recents": true,
}

func (s *server) getPrefs(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	rows, err := s.db.Query(r.Context(),
		`SELECT key, value FROM public.user_prefs WHERE tenant=$1 AND user_sub=$2`,
		tenant, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v json.RawMessage
		if err := rows.Scan(&k, &v); err == nil {
			out[k] = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) putPref(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		!prefAllowedKeys[in.Key] || len(in.Value) == 0 || len(in.Value) > prefMaxValueBytes {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.user_prefs (tenant, user_sub, key, value)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant, user_sub, key)
		DO UPDATE SET value=$4, updated_at=now()`, tenant, p.Subject, in.Key, in.Value); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}
