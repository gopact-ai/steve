package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/view"
)

func TestAttemptUsagePreservesExplicitZeroReport(t *testing.T) {
	for _, reported := range []bool{false, true} {
		spent := &turnSpend{usage: view.Usage{Reported: reported, ContextTokens: 500}}
		u := spent.attemptUsage()
		if u.Reported != reported || u.Context != 500 || u.Input != 0 || u.Output != 0 {
			t.Fatalf("usage=%+v reported=%v", u, reported)
		}
	}
}
