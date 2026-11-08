package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"resourcereservation/internal/faults"
)

// The fault-injection controls (D13). Every one needs BOTH switches: the server was started with
// FAULTS_ENABLED=true, and the caller presents the admin token. With either missing nothing can be armed.

func (h *Handler) registerAdmin() {
	h.mux.Handle("GET /admin/faults", h.guard(http.HandlerFunc(h.listFaults)))
	h.mux.Handle("PUT /admin/faults/{point}", h.guard(http.HandlerFunc(h.setFault)))
	h.mux.Handle("DELETE /admin/faults/{point}", h.guard(http.HandlerFunc(h.deleteFault)))
	h.mux.Handle("DELETE /admin/faults", h.guard(http.HandlerFunc(h.clearFaults)))
}

// guard refuses unless fault injection is enabled and the bearer token matches (constant-time compare).
func (h *Handler) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !h.d.Faults.Enabled():
			fail(w, http.StatusForbidden, "fault injection is disabled (start the server with FAULTS_ENABLED=true)")
		case h.d.AdminToken == "":
			fail(w, http.StatusForbidden, "no admin token is configured (set ADMIN_TOKEN)")
		case subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(h.d.AdminToken)) != 1:
			fail(w, http.StatusUnauthorized, "missing or invalid admin token")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (h *Handler) listFaults(w http.ResponseWriter, r *http.Request) {
	list, err := h.d.Faults.List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"faults": list, "known": faults.Known})
}

func (h *Handler) setFault(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action      faults.Action  `json:"action"`
		Params      map[string]any `json:"params"`
		Remaining   *int           `json:"remaining"`
		Probability float64        `json:"probability"`
	}
	if !decode(w, r, &in) {
		return
	}
	f := faults.Fault{Point: r.PathValue("point"), Action: in.Action, Params: in.Params, Remaining: in.Remaining, Probability: in.Probability}
	if err := h.d.Faults.Set(r.Context(), f); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// Refresh this process at once so it reacts without waiting for the next poll.
	_ = h.d.Faults.Refresh(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteFault(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Faults.Delete(r.Context(), r.PathValue("point")); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = h.d.Faults.Refresh(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) clearFaults(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Faults.Clear(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = h.d.Faults.Refresh(r.Context())
	w.WriteHeader(http.StatusNoContent)
}
