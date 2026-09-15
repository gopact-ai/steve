package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
)

type desktopFixture struct {
	enrolled  []string
	step      string
	done      bool
	workspace string
}

func (d *desktopFixture) DesktopStatus(context.Context) (consoleapi.DesktopStatus, error) {
	return consoleapi.DesktopStatus{Enabled: true, NodeID: "node-local", SetupRequired: !d.done, AgentCount: len(d.enrolled), Setup: &consoleapi.DesktopSetup{Step: d.step, Done: d.done}}, nil
}
func (d *desktopFixture) DesktopDiscover(context.Context) (consoleapi.DesktopDiscovery, error) {
	return consoleapi.DesktopDiscovery{Agents: []consoleapi.DesktopAgentCandidate{{ID: "codex", Name: "Codex", Installed: true}}}, nil
}
func (d *desktopFixture) DesktopEnroll(ctx context.Context, r consoleapi.DesktopEnrollRequest) (consoleapi.DesktopStatus, error) {
	d.enrolled = append(d.enrolled, r.AgentIDs...)
	return d.DesktopStatus(ctx)
}

func TestDesktopEnrollmentAcceptsOnlyOneKnownShape(t *testing.T) {
	d := &desktopFixture{}
	s := &Server{desktop: d}
	for _, body := range []string{`{"agent_ids":["codex"],"command":"untrusted"}`, `{"agent_ids":["codex"]} {}`} {
		w := httptest.NewRecorder()
		s.consoleDesktopAgents(w, httptest.NewRequest(http.MethodPost, "/console/desktop/agents", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest || len(d.enrolled) != 0 {
			t.Fatalf("malformed enrollment changed state: %d %v", w.Code, d.enrolled)
		}
	}
	w := httptest.NewRecorder()
	s.consoleDesktopAgents(w, httptest.NewRequest(http.MethodPost, "/console/desktop/agents", strings.NewReader(`{"agent_ids":["codex"]}`)))
	if w.Code != http.StatusOK || len(d.enrolled) != 1 || !strings.Contains(w.Body.String(), `"setup_required":true`) {
		t.Fatalf("enrollment = %d %s", w.Code, w.Body.String())
	}
}

func TestDesktopDiscoveryDoesNotEnroll(t *testing.T) {
	d := &desktopFixture{}
	s := &Server{desktop: d}
	w := httptest.NewRecorder()
	s.consoleDesktopAgents(w, httptest.NewRequest(http.MethodGet, "/console/desktop/agents", nil))
	if w.Code != http.StatusOK || len(d.enrolled) != 0 || !strings.Contains(w.Body.String(), "Codex") {
		t.Fatalf("discovery = %d %s", w.Code, w.Body.String())
	}
}

func TestDesktopStatusDisabledWithoutDesktopService(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).consoleDesktop(w, httptest.NewRequest(http.MethodGet, "/console/desktop", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
}

func (d *desktopFixture) DesktopSetup(ctx context.Context, r consoleapi.DesktopSetupRequest) (consoleapi.DesktopStatus, error) {
	d.step, d.done = r.Step, r.Done
	return d.DesktopStatus(ctx)
}
func (d *desktopFixture) DesktopWorkspace(ctx context.Context, r consoleapi.DesktopWorkspaceRequest) (consoleapi.DesktopStatus, error) {
	if r.Path == "/etc" {
		return consoleapi.DesktopStatus{}, &desktop.InputError{Message: "/etc 属于系统目录"}
	}
	if r.Path == "/broken" {
		return consoleapi.DesktopStatus{}, errors.New("创建工作目录失败：disk full")
	}
	d.workspace = r.Path
	return d.DesktopStatus(ctx)
}

func TestDesktopSetupProgressAndWorkspaceAcceptOnlyTheirShapes(t *testing.T) {
	d := &desktopFixture{}
	s := &Server{desktop: d}
	for _, body := range []string{`{"step":"agents","extra":1}`, `{"step":"agents"} {}`, `nope`} {
		w := httptest.NewRecorder()
		s.consoleDesktopSetup(w, httptest.NewRequest(http.MethodPut, "/console/desktop/setup", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest || d.step != "" {
			t.Fatalf("malformed progress changed state: %d %q", w.Code, d.step)
		}
	}
	w := httptest.NewRecorder()
	s.consoleDesktopSetup(w, httptest.NewRequest(http.MethodPut, "/console/desktop/setup", strings.NewReader(`{"step":"finished","done":true}`)))
	if w.Code != http.StatusOK || d.step != "finished" || !d.done || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("progress = %d %s (%q %v)", w.Code, w.Body.String(), d.step, d.done)
	}
	w = httptest.NewRecorder()
	s.consoleDesktopWorkspace(w, httptest.NewRequest(http.MethodPut, "/console/desktop/workspace", strings.NewReader(`{"path":"/etc"}`)))
	if w.Code != http.StatusBadRequest || d.workspace != "" || !strings.Contains(w.Body.String(), "系统目录") {
		t.Fatalf("refused workspace = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.consoleDesktopWorkspace(w, httptest.NewRequest(http.MethodPut, "/console/desktop/workspace", strings.NewReader(`{"path":"/broken"}`)))
	if w.Code != http.StatusInternalServerError || d.workspace != "" {
		t.Fatalf("a failure of this machine is not the owner's mistake: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.consoleDesktopWorkspace(w, httptest.NewRequest(http.MethodPut, "/console/desktop/workspace", strings.NewReader(`{"path":"~/Steve"}`)))
	if w.Code != http.StatusOK || d.workspace != "~/Steve" {
		t.Fatalf("workspace = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	(&Server{}).consoleDesktopWorkspace(w, httptest.NewRequest(http.MethodPut, "/console/desktop/workspace", strings.NewReader(`{"path":"~/Steve"}`)))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("without a desktop service = %d", w.Code)
	}
}
