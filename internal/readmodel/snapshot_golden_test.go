package readmodel

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/view"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files this package compares against")

type fakeSchedules []schedule.Job

func (f fakeSchedules) List(string) []schedule.Job { return f }

// timestamps matches every RFC 3339 value the snapshot carries, which is
// the only part of it a wall clock decides.
var timestamps = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// TestSnapshotMatchesGolden wires every source, including a ledger with a
// live attempt, a fresh progress report, a pending question, a schedule
// and a workspace repository, and compares the whole snapshot with the
// checked-in copy. The order of every list and the value of every field
// is what the console renders; a refactoring of Snapshot must leave both
// untouched. Usage is left out: its period keys follow today's date.
func TestSnapshotMatchesGolden(t *testing.T) {
	m := fixture(t)
	s := newSourceFixture()
	s.live = []attempt.Record{
		{Spec: attempt.Spec{ID: "live", Agent: "local", TaskID: "2", Workspace: project.Workspace{Path: "/work/p"}}, State: attempt.Running},
		{Spec: attempt.Spec{ID: "stuck", Agent: "builder", TaskID: "1", Node: "node-a", Workspace: project.Workspace{Path: "/work/q"}}, State: attempt.Leased, Unsettled: true, Error: "lease lost"},
	}
	m.src.Ledger = s.adapter()
	m.src.HomeProject, m.src.DefaultProject = "p", "q"
	m.src.Repos = func(workspaceID string) []nodewire.Repo {
		return []nodewire.Repo{{Path: "repo-of-" + workspaceID}}
	}
	m.src.Schedules = fakeSchedules{{ID: "nightly", ConversationID: "chat:1", Member: "local", Prompt: "sweep", Spec: schedule.Spec{Text: "0 3 * * *"}, Runs: 2, State: "armed"}}
	m.SetInteractions(questionSource{{ID: "ask", State: "pending", Conversation: "console:ask", TaskID: "1", Project: "p", Message: "Which branch?"}})
	m.Publish(Event{At: time.Now(), Kind: "console.progress", TaskID: "2", Conversation: "chat:1", Progress: &consoleapi.Progress{Agent: "local", Tools: []consoleapi.ToolCall{{Kind: "bash", Name: "go test ./...", Status: view.ToolRunning}}}})

	snap := m.Snapshot(t.Context())
	snap.Usage = Usage{}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got := timestamps.ReplaceAll(raw, []byte("<time>"))
	path := filepath.Join("testdata", "snapshot.json")
	if *updateGolden {
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with -update)", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), got) {
		t.Fatalf("snapshot differs from testdata/snapshot.json; diff the file against this output or regenerate with -update:\n%s", got)
	}
}
