package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/sameorigin"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

type SSHService interface {
	SSHDiscover(context.Context) (sshconnect.Discovery, error)
	SSHCheck(context.Context, string) (sshconnect.CheckResult, error)
	SSHPlan(context.Context, sshconnect.InstallRequest) (sshconnect.InstallPlan, error)
	SSHCommit(context.Context, string) (sshconnect.InstallResult, error)
	SSHStatus(context.Context, string) (sshconnect.InstallResult, error)
	SSHAbandon(context.Context, string) error
	SSHBrowse(context.Context, sshconnect.BrowseRequest) (sshconnect.Listing, error)
	// SSHUpgrade brings an enrolled machine to this build and returns when
	// that has settled; SSHUpgradeStatus reads how far it has come.
	SSHUpgrade(context.Context, string) (sshconnect.InstallResult, error)
	SSHUpgradeStatus(context.Context, string) (sshconnect.InstallResult, error)
}

func (s *Server) SetSSH(service SSHService) { s.ssh = service }

// SSHHandler provides the same authenticated local-management API to a stable
// desktop gateway that can outlive its current coordination application. It
// answers loopback names only, whoever mounts it.
func SSHHandler(service SSHService, token, origin string) (http.Handler, error) {
	if service == nil || len(token) < 32 || origin == "" {
		return nil, errors.New("private SSH management requires a service, token and origin")
	}
	server := &Server{ssh: service, token: token, sshOrigin: origin}
	mux := http.NewServeMux()
	server.sshRoutes(mux)
	return sameorigin.Guard(mux, sameorigin.Loopback), nil
}

func (s *Server) sshRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /console/ssh/candidates", s.guard(s.sshCandidates))
	mux.HandleFunc("POST /console/ssh/check", s.guard(s.sshCheck))
	mux.HandleFunc("POST /console/ssh/plans", s.guard(s.sshPlan))
	mux.HandleFunc("POST /console/ssh/plans/{id}/install", s.guard(s.sshInstall))
	mux.HandleFunc("GET /console/ssh/plans/{id}", s.guard(s.sshStatus))
	mux.HandleFunc("DELETE /console/ssh/plans/{id}", s.guard(s.sshAbandon))
	mux.HandleFunc("POST /console/ssh/browse", s.guard(s.sshBrowse))
	mux.HandleFunc("POST /console/ssh/upgrades/{node}", s.guard(s.sshUpgrade))
	mux.HandleFunc("GET /console/ssh/upgrades/{node}", s.guard(s.sshUpgradeStatus))
}

func (s *Server) sshCandidates(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w, r) {
		return
	}
	result, err := s.ssh.SSHDiscover(r.Context())
	if err != nil {
		s.sshError(w, err)
		return
	}
	writeJSON(w, result)
}

func (s *Server) sshCheck(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w, r) {
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
	writeJSON(w, result)
}

func (s *Server) sshPlan(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w, r) {
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
	writeJSON(w, result)
}

func (s *Server) sshInstall(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w, r) {
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
	writeJSON(w, result)
}

// sshStatus reads how an installation is going: the phase it is in and
// what the remote has said. It never starts, resumes or repeats anything.
func (s *Server) sshStatus(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w, r) {
		return
	}
	result, err := s.ssh.SSHStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		s.sshError(w, err)
		return
	}
	writeJSON(w, result)
}

// sshAbandon gives up a plan or operation that will not finish, so the
// machine can be enrolled again. The remote machine is left as it is.
func (s *Server) sshAbandon(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w, r) {
		return
	}
	if err := s.ssh.SSHAbandon(r.Context(), r.PathValue("id")); err != nil {
		s.sshError(w, err)
		return
	}
	writeJSON(w, map[string]any{"plan_id": r.PathValue("id"), "abandoned": true})
}

// sshBrowse lists the directories under one remote path so the workspace
// can be picked from what the machine actually has. It reads only.
func (s *Server) sshBrowse(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w, r) {
		return
	}
	var request sshconnect.BrowseRequest
	if !decodeSSH(w, r, &request) {
		return
	}
	result, err := s.ssh.SSHBrowse(r.Context(), request)
	if err != nil {
		s.sshError(w, err)
		return
	}
	writeJSON(w, result)
}

func (s *Server) sshAvailable(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if s.ssh != nil {
		return true
	}
	w.WriteHeader(http.StatusNotImplemented)
	writeJSON(w, map[string]string{"error": i18n.FromContext(r.Context()).T(i18n.HTTPSSHOff)})
	return false
}

func (s *Server) sshError(w http.ResponseWriter, err error) {
	var step *sshconnect.StepError
	if errors.As(err, &step) {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": step.Error(), "step": step})
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
	writeJSON(w, map[string]string{"error": err.Error()})
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

func (s *Server) sshUpgrade(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w, r) {
		return
	}
	result, err := s.ssh.SSHUpgrade(r.Context(), r.PathValue("node"))
	if err != nil && result.Status == "" {
		s.sshError(w, err)
		return
	}
	writeJSON(w, result)
}

func (s *Server) sshUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	if !s.sshAvailable(w, r) {
		return
	}
	result, err := s.ssh.SSHUpgradeStatus(r.Context(), r.PathValue("node"))
	if err != nil {
		s.sshError(w, err)
		return
	}
	writeJSON(w, result)
}
