package contentreplica_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
)

// A node that answers that the caller is no longer the writer ends the
// caller's attempt: no other node is asked, since each would answer the
// same, and the error says the caller was superseded, not that copies are
// missing.
func TestACallerThatIsNoLongerTheWriterStopsAskingOtherNodes(t *testing.T) {
	data := []byte("content of a superseded caller")
	ref := checkpoint.Reference(data)
	superseded := fmt.Errorf("answered stale: %w", contentreplica.ErrSuperseded)
	stopped := func(t *testing.T, err error, asked, want []string) {
		t.Helper()
		if !errors.Is(err, contentreplica.ErrSuperseded) || errors.Is(err, contentreplica.ErrIncomplete) || !slices.Equal(asked, want) {
			t.Fatalf("superseded caller: %v after asking %q; want superseded, not incomplete, after asking %q", err, asked, want)
		}
	}
	t.Run("storing", func(t *testing.T) {
		_, r, client := newCluster(t, openBook(t), "internal", "a", "b", "c")
		r.refused = map[string]error{"b": superseded, "c": superseded}
		_, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
		stopped(t, err, r.asked, []string{"b"})
	})
	t.Run("reading", func(t *testing.T) {
		_, r, client := newCluster(t, openBook(t), "internal", "a", "b", "c")
		m, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		first := m.Receipts[0].NodeID
		r.asked = nil
		r.refused = map[string]error{"a": superseded, "b": superseded}
		_, err = client("c").Read(t.Context(), m, &bytes.Buffer{})
		stopped(t, err, r.asked, []string{first})
	})
}
