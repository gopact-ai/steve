package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type enrollServer struct {
	agents   int
	enrolled []map[string]any
}

func (s *enrollServer) handler(t *testing.T, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/console/desktop":
			json.NewEncoder(w).Encode(map[string]int{"agent_count": s.agents})
		case r.Method == http.MethodGet && r.URL.Path == "/console/desktop/agents":
			json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]any{
				{"id": "codex", "name": "Codex", "installed": true},
				{"id": "claude", "name": "Claude Code", "installed": true},
				{"id": "gemini", "name": "Gemini CLI", "installed": true, "requires": []string{"node"}},
				{"id": "opencode", "name": "OpenCode", "installed": false},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/console/desktop/agents":
			var body struct {
				Agents []map[string]any `json:"agents"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			s.enrolled = body.Agents
			s.agents += len(body.Agents)
			json.NewEncoder(w).Encode(map[string]int{"agent_count": s.agents})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestEnrollRunningUsesTheServiceEnrollment(t *testing.T) {
	installed := &Installation{Token: "owner-token"}
	state := &enrollServer{}
	server := httptest.NewServer(state.handler(t, installed.Token))
	defer server.Close()
	got, err := EnrollRunning(context.Background(), installed, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Added, []string{"Codex", "Claude Code"}) {
		t.Fatalf("added %v", got.Added)
	}
	if len(got.Pending) != 1 {
		t.Fatalf("pending %v, want the tool missing node", got.Pending)
	}
	want := []map[string]any{{"candidate_id": "codex", "default": true}, {"candidate_id": "claude"}}
	if !reflect.DeepEqual(state.enrolled, want) {
		t.Fatalf("enrolled %v, want %v", state.enrolled, want)
	}
	again, err := EnrollRunning(context.Background(), installed, server.URL)
	if err != nil || len(again.Added) != 0 {
		t.Fatalf("second run added %v (%v); an owner's agents must be left alone", again.Added, err)
	}
}

func TestEnrollRunningRejectsNonLocalAddresses(t *testing.T) {
	if _, err := EnrollRunning(context.Background(), &Installation{Token: "x"}, "http://example.com:80"); err == nil {
		t.Fatal("enrolled against a remote address")
	}
}
