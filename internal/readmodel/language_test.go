package readmodel

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
)

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// What the snapshot itself says about an unconfirmed writer or effect is
// in the language of the reader who asked for it.
func TestSnapshotSaysUnconfirmedWorkInTheReaderLanguage(t *testing.T) {
	s := newSourceFixture()
	s.disclosures = nil
	s.live = []attempt.Record{{
		Spec:  attempt.Spec{ID: "quarantined", Agent: "local", TaskID: "2", Project: "p", Workspace: project.Workspace{Path: "/work/p"}},
		State: attempt.Running, Unsettled: true, StartedAt: time.Now().Add(-time.Minute),
	}}
	m := fixture(t)
	m.src.Ledger = s.adapter()
	snap := m.Snapshot(i18n.WithLocale(t.Context(), i18n.LocaleEN))
	said := []string{}
	for _, request := range snap.Inbox {
		said = append(said, request.Summary)
	}
	for _, effect := range snap.Facts.Effects {
		said = append(said, effect.Error)
	}
	for _, agent := range snap.Agents {
		for _, activity := range agent.Activities {
			said = append(said, activity.Detail)
		}
	}
	if len(said) < 4 {
		t.Fatalf("snapshot said too little: %q", said)
	}
	for _, text := range said {
		if text == "" || containsHan(text) {
			t.Fatalf("snapshot said %q to an English reader; all: %q", text, said)
		}
	}
}
