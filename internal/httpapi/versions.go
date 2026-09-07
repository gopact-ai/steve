package httpapi

import (
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) consoleVersions(w http.ResponseWriter, r *http.Request) {
	service, ok := s.admin.(consoleapi.VersionService)
	if !ok {
		http.Error(w, "version discovery is unavailable", http.StatusNotImplemented)
		return
	}
	v, err := service.Versions(r.Context())
	queueResponse(w, v, err)
}
