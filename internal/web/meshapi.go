package web

import (
	"encoding/json"
	"net/http"

	"doge/internal/mesh"
)

// handleMeshForwardsList lists the declarative forward rules (mesh.json
// entries; the running node hot-applies every change made through these
// endpoints).
func (s *Server) handleMeshForwardsList(w http.ResponseWriter, r *http.Request) {
	if s.meshView == nil {
		writeError(w, http.StatusNotFound, "mesh not enabled (build with -tags mesh)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"forwards": s.meshView.ForwardList()})
}

// handleMeshForwardsAdd validates, persists and hot-applies a new forward
// rule. The body is one mesh.json forwards entry: mode "expose" publishes a
// local service to the mesh, mode "listen" bridges a local listener to a
// remote peer's exposed service (see mesh.ForwardRule).
func (s *Server) handleMeshForwardsAdd(w http.ResponseWriter, r *http.Request) {
	if s.meshView == nil {
		writeError(w, http.StatusNotFound, "mesh not enabled (build with -tags mesh)")
		return
	}
	var rule mesh.ForwardRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.meshView.ForwardAdd(rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMeshForwardsRemove stops and deletes a forward rule by name.
func (s *Server) handleMeshForwardsRemove(w http.ResponseWriter, r *http.Request) {
	if s.meshView == nil {
		writeError(w, http.StatusNotFound, "mesh not enabled (build with -tags mesh)")
		return
	}
	if err := s.meshView.ForwardRemove(r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMeshForwardsSetEnabled hot-enables/disables one rule without touching
// the others.
func (s *Server) handleMeshForwardsSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.meshView == nil {
			writeError(w, http.StatusNotFound, "mesh not enabled (build with -tags mesh)")
			return
		}
		if err := s.meshView.ForwardSetEnabled(r.PathValue("name"), enabled); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
