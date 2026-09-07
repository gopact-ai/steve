package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

type desktopFixture struct{ enrolled []string }

func (d *desktopFixture) DesktopStatus(context.Context) (consoleapi.DesktopStatus, error) {
	return consoleapi.DesktopStatus{Enabled: true, NodeID: "node-local", SetupRequired: len(d.enrolled) == 0, AgentCount: len(d.enrolled)}, nil
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
	if w.Code != http.StatusOK || len(d.enrolled) != 1 || !strings.Contains(w.Body.String(), `"setup_required":false`) {
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
