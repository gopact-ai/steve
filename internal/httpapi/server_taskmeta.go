package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) consoleTaskMeta(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.adminOr(w) {
		return
	}
	var patch *consoleapi.TaskMetaPatch
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil || patch == nil {
		http.Error(w, "invalid task metadata", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "expected one task metadata object", http.StatusBadRequest)
		return
	}
	if err := s.admin.SetTaskMeta(r.Context(), r.PathValue("task"), *patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, updated := range s.model.Snapshot(r.Context()).Tasks {
		if updated.ID == r.PathValue("task") {
			writeJSON(w, updated)
			return
		}
	}
	http.Error(w, "task "+r.PathValue("task")+" not found", http.StatusBadRequest)
}
