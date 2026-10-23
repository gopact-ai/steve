package node

import (
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/pihome"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

// A node-owned pi session recorded under a write policy is not resumed under
// read: pi would restore the tools its transcript recorded. The node opens a
// new native session and tells the hub why.
func TestNodePiResumeUnderNarrowerPolicyStartsFresh(t *testing.T) {
	cfg, req, old := resumedFixture(t, buildMockAgent(t))
	t.Setenv("HOME", t.TempDir())
	cfg.Harnesses = map[string]HarnessSpec{harness.Pi: cfg.Harnesses["mock"]}
	req.Harness, old.State.Harness = harness.Pi, harness.Pi
	req.Permission = "read"
	old.ConfigHash = sessionConfigHash(req)
	saveResumeFixture(t, cfg, old)
	env := []string{pihome.EnvHomes + "=" + steveruntime.PiHome(cfg.StateDir)}
	if err := pihome.Remember(env, old.UpstreamID, "write"); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if state.ContextReset != harness.ContextResetPolicy || state.ContextID != state.ID {
		t.Fatalf("resume across policy classes not reported: %+v", state)
	}
	current, _, err := s.sessions.readRecord(state.ID)
	if err != nil || current.UpstreamID == "" || current.UpstreamID == old.UpstreamID {
		t.Fatalf("read-only open resumed the write session: %+v %v", current, err)
	}
	if pihome.Resumable(env, current.UpstreamID, "write") {
		t.Fatal("new session not recorded under the read-only class")
	}
}
