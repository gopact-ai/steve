package httpapi

import (
	"context"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nativehistory"
)

type nativeHistoryService interface {
	NativeHistory(context.Context, string, nativehistory.Source) ([]nativehistory.Entry, error)
	ImportNativeHistory(context.Context, string, consoleapi.NativeImportRequest) (consoleapi.ImportedSession, error)
}

func (s *Server) consoleNativeHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	service, ok := s.admin.(nativeHistoryService)
	if !ok {
		writeDesktopError(w, nativehistory.ErrUnsupported, http.StatusNotImplemented)
		return
	}
	if r.Method == http.MethodGet {
		source := nativehistory.Source{Harness: r.URL.Query().Get("harness"), Home: r.URL.Query().Get("home")}
		entries, err := service.NativeHistory(r.Context(), r.PathValue("name"), source)
		if err != nil {
			writeDesktopError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"entries": entries})
		return
	}
	var request consoleapi.NativeImportRequest
	if !decodeSSH(w, r, &request) {
		return
	}
	result, err := service.ImportNativeHistory(r.Context(), r.PathValue("name"), request)
	if err != nil {
		writeDesktopError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}
