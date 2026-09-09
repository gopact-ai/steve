package delegate

import (
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/ctxpack"
)

type preparationGate struct {
	token   string
	revoked []string
}

func (g *preparationGate) Delegated(_, _, _, _, token, _ string) []capability.Extra {
	g.token = token
	return nil
}
func (g *preparationGate) Revoke(token string) { g.revoked = append(g.revoked, token) }

// Capability/context preparation owns the token even when no attempt ever
// opens; prompt failure and success must both release it after settlement.
func TestRunPreparationAndTokenLifetime(t *testing.T) {
	for _, tc := range []struct {
		name, goal, promptError, wantError string
		opens                              int
	}{
		{name: "context-refused", goal: strings.Repeat("x", ctxpack.MaxBytes+1), wantError: "over the 24576 limit", opens: 0},
		{name: "prompt-failed", goal: "work", promptError: "prompt failed", wantError: "prompt failed", opens: 1},
		{name: "completed", goal: "work", opens: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.running(t, "codex")
			gate := &preparationGate{}
			w.service.SetGate(gate)
			w.sessions.reply = func(prompt string) (string, error) {
				if gate.token == "" || len(gate.revoked) != 0 {
					t.Error("token not live during prompt")
				}
				if !strings.Contains(prompt, "work") || !strings.Contains(prompt, worktreeContract) {
					t.Errorf("missing context/contract: %s", prompt)
				}
				if tc.promptError != "" {
					return "", errors.New(tc.promptError)
				}
				return "done", nil
			}
			result, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Agent: "builder", Goal: tc.goal})
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error: %v", err)
			}
			if len(w.sessions.opened) != tc.opens {
				t.Fatalf("opened %d sessions", len(w.sessions.opened))
			}
			if gate.token == "" || len(gate.revoked) != 1 || gate.revoked[0] != gate.token {
				t.Fatalf("token lifetime: %+v", gate)
			}
			records, err := w.attempts.ForTask(t.Context(), result.TaskID)
			if err != nil || len(records) != tc.opens {
				t.Fatalf("attempts: %+v %v", records, err)
			}
		})
	}
}
