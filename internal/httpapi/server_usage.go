package httpapi

import "net/http"

// usage is an explicit historical read, independent of the live fleet state.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, s.model.UsageSummary(r.Context()))
}
