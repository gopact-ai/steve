package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) SetMailControl(control consoleapi.MailControl) { s.mail = control }

func (s *Server) mailRoutes(mux *http.ServeMux) {
	for _, route := range []string{
		"GET /console/mail/accounts", "POST /console/mail/accounts",
		"POST /console/mail/accounts/batch",
		"PATCH /console/mail/accounts/{id}", "DELETE /console/mail/accounts/{id}",
		"POST /console/mail/accounts/{id}/login", "POST /console/mail/accounts/{id}/sync",
		"GET /console/mail/logins/{id}", "GET /console/mail/imports/{id}",
		"POST /console/mail/imports/preview", "POST /console/mail/imports/commit",
		"POST /console/mail/exports", "PUT /console/mail/apps", "PUT /console/mail/ai",
	} {
		mux.HandleFunc(route, s.guard(s.consoleMail))
	}
}

func (s *Server) consoleMail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if s.mail == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"code": "mail_unavailable", "error": "Mail provider administration is not configured"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil || (len(raw) != 0 && !json.Valid(raw)) {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "Invalid mailbox request or import exceeds 4 MB"})
		return
	}
	body, status, err := s.mail.Exchange(r.Context(), r.Method, strings.TrimPrefix(r.URL.Path, "/console/mail"), raw)
	if err != nil {
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"code": "mail_unavailable", "error": err.Error()})
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
