package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/sshconnect"
)

type SSHService interface {
	SSHDiscover(context.Context) (sshconnect.Discovery, error)
	SSHCheck(context.Context, string) (sshconnect.CheckResult, error)
	SSHPlan(context.Context, sshconnect.InstallRequest) (sshconnect.InstallPlan, error)
	SSHCommit(context.Context, string) (sshconnect.InstallResult, error)
}

func (s *Server) SetSSH(service SSHService) { s.ssh = service }

// SSHHandler provides the same authenticated local-management API to a stable
// desktop gateway that can outlive its current coordination application.
func SSHHandler(service SSHService, token, origin string) (http.Handler, error) {
	if service == nil || len(token) < 32 || origin == "" {
		return nil, errors.New("private SSH management requires a service, token and origin")
	}
	server := &Server{ssh: service, token: token, sshOrigin: origin}
	mux := http.NewServeMux()
	server.sshRoutes(mux)
	return mux, nil
}

func (s *Server) sshRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /console/ssh/candidates", s.guard(s.sshCandidates))
	mux.HandleFunc("POST /console/ssh/check", s.guard(s.sshCheck))
	mux.HandleFunc("POST /console/ssh/plans", s.guard(s.sshPlan))
	mux.HandleFunc("POST /console/ssh/plans/{id}/install", s.guard(s.sshInstall))
}

func (s *Server) sshCandidates(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w) {
		return
	}
	result, err := s.ssh.SSHDiscover(r.Context())
	if err != nil {
		s.sshError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) sshCheck(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w) {
		return
	}
	var request struct {
		Alias string `json:"alias"`
	}
	if !decodeSSH(w, r, &request) {
		return
	}
	result, err := s.ssh.SSHCheck(r.Context(), request.Alias)
	if err != nil {
		s.sshError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) sshPlan(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w) {
		return
	}
	var request sshconnect.InstallRequest
	if !decodeSSH(w, r, &request) {
		return
	}
	request.HubURL = s.sshOrigin
	if request.HubURL == "" {
		request.HubURL = s.URL()
	}
	result, err := s.ssh.SSHPlan(r.Context(), request)
	if err != nil {
		s.sshError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) sshInstall(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w) {
		return
	}
	result, err := s.ssh.SSHCommit(r.Context(), r.PathValue("id"))
	if err != nil && (result.PlanID != r.PathValue("id") || result.Status == "") {
		s.sshError(w, err)
		return
	}
	if err != nil && result.Status == "" {
		result.Status = "needs_attention"
	}
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) sshAvailable(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if s.ssh != nil {
		return true
	}
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "SSH 接入尚未启用"})
	return false
}

func (s *Server) sshError(w http.ResponseWriter, err error) {
	var step *sshconnect.StepError
	if errors.As(err, &step) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": step.Error(), "step": step})
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func decodeSSH(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeDesktopError(w, err, http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeDesktopError(w, errors.New("expected one request object"), http.StatusBadRequest)
		return false
	}
	return true
}
