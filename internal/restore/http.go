package restore

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Guard is the subset of the auth package the handlers need.
type Guard interface {
	RequireLogin(http.Handler) http.Handler
	UserName(*http.Request) (string, bool)
}

type triggerRequest struct {
	Namespace string `json:"namespace"`
	Cluster   string `json:"cluster"`
	Backup    string `json:"backup"`
}

// Register mounts the restore-test API. Listing is public (the dashboard is
// read-only for anonymous users); starting a test requires login.
func (m *Manager) Register(mux *http.ServeMux, g Guard) {
	mux.HandleFunc("GET /api/restore-tests", func(w http.ResponseWriter, r *http.Request) {
		runs, err := m.List(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to read restore test results"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "namespaces": m.cfg.Namespaces, "runs": runs})
	})
	mux.HandleFunc("GET /api/restore-tests/{id}", func(w http.ResponseWriter, r *http.Request) {
		run, err := m.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		} else if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to read run"})
			return
		}
		writeJSON(w, http.StatusOK, run)
	})
	mux.Handle("POST /api/restore-tests", g.RequireLogin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req triggerRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		user, _ := g.UserName(r)
		run, err := m.Trigger(r.Context(), user, strings.TrimSpace(req.Namespace), strings.TrimSpace(req.Cluster), strings.TrimSpace(req.Backup))
		switch {
		case errors.Is(err, ErrInvalid):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrForbidden):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrBusy):
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to start restore test"})
		default:
			writeJSON(w, http.StatusAccepted, run)
		}
	})))
}

// RegisterDisabled reports that the feature is off.
func RegisterDisabled(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/restore-tests", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "namespaces": []string{}, "runs": []Run{}})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
