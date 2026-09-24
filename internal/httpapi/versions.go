package httpapi

import "net/http"

func (s *Server) consoleVersions(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		http.Error(w, "version discovery is unavailable", http.StatusNotImplemented)
		return
	}
	v, err := s.admin.Versions(r.Context())
	queueResponse(w, v, err)
}
