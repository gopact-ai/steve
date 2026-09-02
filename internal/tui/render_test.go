package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/readmodel"
)

// The ledger view renders one of everything and says "none" for the rest;
// a person watching steve top must see what the ledger holds, not a blank.
func TestLedgerViewShowsEveryKindOfFact(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)
	snap := readmodel.Snapshot{Facts: readmodel.Facts{
		Reservations: []readmodel.Reservation{{ID: "resv-1-build", Endpoint: "endpoint:node-a/codex", For: "1/build", ExpiresAt: now}},
		Attestations: []readmodel.Attestation{{Artifact: "0123456789abcdef0123", Step: "build", Kind: "command", Verifier: "go test ./...", Verdict: "pass", Attempt: "att-1", At: now}},
		Replicas:     []readmodel.Replica{{Artifact: "0123456789abcdef0123", Node: "node-b", Generation: 2, State: "verified", Note: "direct from node-a", At: now}},
		Disclosures:  []readmodel.Disclosure{{ID: "disc-7", Project: "vault", TaskID: "9", Requester: "ou_guest", Bytes: 512, At: now}},
		Effects:      []readmodel.Effect{{ID: "int-abc", Tool: "feishu_send", TaskID: "9", Attempt: "att-2", Error: "timeout", At: now}},
		Grants:       []readmodel.Grant{{Project: "vault", Principal: "ou_guest", Role: "write", By: "ou_owner"}},
	}}
	out := renderLedger(snap, 120)
	for _, want := range []string{"disc-7", "int-abc", "resv-1-build", "pass", "direct from node-a", "ou_guest", "write"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ledger view lacks %q:\n%s", want, out)
		}
	}
	empty := renderLedger(readmodel.Snapshot{}, 80)
	for _, want := range []string{"nothing waiting", "no capacity reserved", "no verdicts yet", "no copies on nodes", "no grants"} {
		if !strings.Contains(empty, want) {
			t.Fatalf("empty ledger view lacks %q:\n%s", want, empty)
		}
	}
}
