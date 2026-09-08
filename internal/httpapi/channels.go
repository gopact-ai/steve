package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) SetChannels(channels consoleapi.ChannelsService) { s.channels = channels }
func (s *Server) consoleChannels(w http.ResponseWriter, r *http.Request) {
	if s.channels == nil {
		http.Error(w, "Channel settings are unavailable", http.StatusNotImplemented)
		return
	}
	var view consoleapi.ChannelsView
	var err error
	if r.Method == http.MethodGet {
		view, err = s.channels.Channels(r.Context())
	} else {
		var req consoleapi.ChannelsUpdate
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&req); err == nil {
			if decoder.Decode(&struct{}{}) != io.EOF {
				err = errors.New("Expected one channel settings object")
			}
		}
		if err == nil {
			view, err = s.channels.UpdateChannels(r.Context(), req)
		}
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, consoleapi.ErrSettingsConflict) {
			status = http.StatusConflict
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, view)
}
