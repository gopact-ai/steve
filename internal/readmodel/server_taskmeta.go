package readmodel

import (
	"encoding/json"
	"io"
	"net/http"
)

type TaskMetaPatch struct {
	Title    *string   `json:"title,omitempty"`
	Priority *string   `json:"priority,omitempty"`
	Labels   *[]string `json:"labels,omitempty"`
	Archived *bool     `json:"archived,omitempty"`
}

func (s *Server) consoleTaskMeta(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.adminOr(w) {
		return
	}
	var patch *TaskMetaPatch
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
	updated, err := s.admin.SetTaskMeta(r.Context(), r.PathValue("task"), *patch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(updated)
}
