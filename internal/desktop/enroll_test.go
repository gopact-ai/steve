package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

type enrollServer struct {
	mu       sync.Mutex
	ready    int // /state answers 503 this many times first
	agents   map[string]bool
	refuse   map[string]bool
	defaults []string
}

func (s *enrollServer) handler(t *testing.T, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/state":
			if s.ready > 0 {
				s.ready--
				http.Error(w, "coordination is starting", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte("{}"))
		case r.Method == http.MethodGet && r.URL.Path == "/console/desktop":
			json.NewEncoder(w).Encode(map[string]int{"agent_count": len(s.agents)})
		case r.Method == http.MethodGet && r.URL.Path == "/console/desktop/agents":
			list := []map[string]any{}
			for _, c := range []struct{ id, name string }{{"codex", "Codex"}, {"claude", "Claude Code"}, {"pi", "pi"}} {
				list = append(list, map[string]any{"id": c.id, "name": c.name, "installed": true, "registered": s.agents[c.id]})
			}
			list = append(list, map[string]any{"id": "gemini", "name": "Gemini CLI", "installed": true, "requires": []string{"node"}})
			json.NewEncoder(w).Encode(map[string]any{"agents": list})
		case r.Method == http.MethodPost && r.URL.Path == "/console/desktop/agents":
			var body struct {
				Agents []struct {
					CandidateID string `json:"candidate_id"`
					Default     bool   `json:"default"`
				} `json:"agents"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Agents) != 1 {
				t.Errorf("enrollment sent %v (%v), want one agent per request", body, err)
			}
			id := body.Agents[0].CandidateID
			if s.refuse[id] {
				http.Error(w, "adapter install failed", http.StatusBadGateway)
				return
			}
			s.agents[id] = true
			if body.Agents[0].Default {
				s.defaults = append(s.defaults, id)
			}
			json.NewEncoder(w).Encode(map[string]int{"agent_count": len(s.agents)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func enrollFixture(t *testing.T, state *enrollServer) (*Installation, string) {
	t.Helper()
	installed := &Installation{Token: "owner-token", Paths: Paths{Root: t.TempDir()}}
	server := httptest.NewServer(state.handler(t, installed.Token))
	t.Cleanup(server.Close)
	return installed, server.URL
}

func TestEnrollRunningWaitsForTheServiceAndRegistersEachAgent(t *testing.T) {
	state := &enrollServer{ready: 3, agents: map[string]bool{}}
	installed, address := enrollFixture(t, state)
	got, err := enrollRunning(context.Background(), installed, address, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Added, []string{"Codex", "Claude Code", "pi"}) || len(got.Pending) != 1 || len(got.Failed) != 0 {
		t.Fatalf("got %+v", got)
	}
	if !reflect.DeepEqual(state.defaults, []string{"codex"}) {
		t.Fatalf("defaults %v, want only the first agent", state.defaults)
	}
	again, err := enrollRunning(context.Background(), installed, address, time.Millisecond)
	if err != nil || len(again.Added)+len(again.Failed) != 0 {
		t.Fatalf("second run %+v (%v); a finished enrollment must not run again", again, err)
	}
}

func TestEnrollRunningResumesAPartialBatch(t *testing.T) {
	state := &enrollServer{agents: map[string]bool{}, refuse: map[string]bool{"claude": true}}
	installed, address := enrollFixture(t, state)
	first, err := enrollRunning(context.Background(), installed, address, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Added, []string{"Codex", "pi"}) || len(first.Failed) != 1 {
		t.Fatalf("first run %+v, want Claude reported and the others registered", first)
	}
	state.mu.Lock()
	state.refuse = nil
	state.mu.Unlock()
	second, err := enrollRunning(context.Background(), installed, address, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second.Added, []string{"Claude Code"}) {
		t.Fatalf("resume %+v, want only the failed agent", second)
	}
	if !reflect.DeepEqual(state.defaults, []string{"codex"}) {
		t.Fatalf("defaults %v; resuming must not add a second default", state.defaults)
	}
}

func TestEnrollRunningLeavesOwnerAgentsAlone(t *testing.T) {
	state := &enrollServer{agents: map[string]bool{"codex": true}}
	installed, address := enrollFixture(t, state)
	got, err := enrollRunning(context.Background(), installed, address, time.Millisecond)
	if err != nil || len(got.Added) != 0 {
		t.Fatalf("got %+v (%v); an installation with agents and no init marker belongs to its owner", got, err)
	}
}

func TestEnrollRunningStopsWaitingAtTheDeadline(t *testing.T) {
	state := &enrollServer{ready: 1 << 30, agents: map[string]bool{}}
	installed, address := enrollFixture(t, state)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := enrollRunning(ctx, installed, address, time.Millisecond); err == nil {
		t.Fatal("enrollment proceeded against a service that never became ready")
	}
}

func TestEnrollRunningRejectsNonLocalAddresses(t *testing.T) {
	if _, err := EnrollRunning(context.Background(), &Installation{Token: "x"}, "http://example.com:80"); err == nil {
		t.Fatal("enrolled against a remote address")
	}
}
