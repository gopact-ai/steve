package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

type restartFixture struct{ calls, acks int }

func (s *restartFixture) Services(context.Context) (consoleapi.ServicesView, error) {
	return consoleapi.ServicesView{}, nil
}
func (s *restartFixture) Restart(_ context.Context, name string, r consoleapi.RestartRequest) (consoleapi.RestartOperation, error) {
	s.calls++
	return consoleapi.RestartOperation{CommandID: r.CommandID, State: "accepted"}, nil
}
func (s *restartFixture) RestartStatus(context.Context, string, string) (consoleapi.RestartOperation, error) {
	return consoleapi.RestartOperation{}, nil
}
func (s *restartFixture) RestartAccepted(string, string) { s.acks++ }

type failedResponse struct{ head http.Header }

func (w *failedResponse) Header() http.Header       { return w.head }
func (w *failedResponse) WriteHeader(int)           {}
func (w *failedResponse) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }
func TestAcceptedRestartProgressesWhenReplyIsLost(t *testing.T) {
	fixture := &restartFixture{}
	s := &Server{services: fixture}
	r := httptest.NewRequest("POST", "/console/services/hub/restart", strings.NewReader(`{"command_id":"once"}`))
	r.SetPathValue("name", "hub")
	s.consoleRestart(&failedResponse{head: make(http.Header)}, r)
	if fixture.calls != 1 || fixture.acks != 1 {
		t.Fatalf("restart stalled after lost reply: %+v", fixture)
	}
	for _, body := range []string{`{"command_id":"once"}{}`, `{"command_id":"once","shell":"bad"}`} {
		r := httptest.NewRequest("POST", "/console/services/hub/restart", strings.NewReader(body))
		s.consoleRestart(httptest.NewRecorder(), r)
	}
	if fixture.calls != 1 {
		t.Fatal("malformed restart submitted")
	}
}
func TestMaintenanceRejectsNewWritesAndNeverBlocksReads(t *testing.T) {
	s := &Server{}
	release, err := s.SealWrites()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler := s.guard(func(w http.ResponseWriter, r *http.Request) { calls++ })
	write := httptest.NewRecorder()
	handler(write, httptest.NewRequest("PUT", "/console/settings", nil))
	if write.Code != http.StatusConflict || calls != 0 {
		t.Fatal("write crossed seal")
	}
	handler(httptest.NewRecorder(), httptest.NewRequest("GET", "/state", nil))
	if calls != 1 {
		t.Fatal("read blocked")
	}
	release()
	handler(httptest.NewRecorder(), httptest.NewRequest("PUT", "/console/settings", nil))
	if calls != 2 {
		t.Fatal("abandoned restart stranded writes")
	}
}
