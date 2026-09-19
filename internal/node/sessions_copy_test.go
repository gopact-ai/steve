package node

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

func sessionCopyFixture(commands int) *ownedSession {
	one := &ownedSession{record: sessionRecord{CommandHashes: map[string]string{}, Commands: map[string]nodewire.SessionCommand{}}}
	for i := range commands {
		id := fmt.Sprint(i)
		one.record.Commands[id] = nodewire.SessionCommand{ID: id, Output: "old receipt", Activity: []string{"activity"}}
		one.record.CommandHashes[id] = "hash"
	}
	one.record.CurrentCommand = "0"
	settings := view.Settings{Models: []string{"model"}, Options: []view.Option{{Choices: []view.Choice{{Value: "one"}}}}}
	one.record.State = nodewire.SessionState{Settings: settings, ModelChoices: []view.Choice{{Value: "first"}}, Progress: view.Progress{Settings: settings, Tools: []view.Tool{{Children: []view.Tool{{Name: "child"}}}}, Plan: []view.Step{{Text: "plan"}}, Timeline: []view.Span{{Text: "narration"}}, Usage: view.Usage{Cost: &view.Cost{Amount: 2}}}, Questions: []nodewire.SessionQuestion{{Question: view.Question{Choices: []view.Choice{{Value: "choice"}}}, Answer: &nodewire.SessionAnswer{Text: "answer"}, Permission: &permission.Ask{Options: []acp.PermissionOption{{Name: "allow", Kind: acp.PermissionOptionKindAllowOnce, OptionID: "allow", Meta: acp.Meta{"nested": map[string]any{"key": []any{"value"}}}}}}}}}
	return one
}

func TestSessionStateReadCostDoesNotGrowWithCommandReceipts(t *testing.T) {
	small, large := sessionCopyFixture(1), sessionCopyFixture(512)
	base := testing.AllocsPerRun(5, func() { _ = small.stateLocked("0") })
	grown := testing.AllocsPerRun(5, func() { _ = large.stateLocked("0") })
	if grown > base+5 {
		t.Fatalf("state read copied receipt history: 1=%v allocations, 512=%v", base, grown)
	}
}

func TestSessionSnapshotsDoNotAliasMutableState(t *testing.T) {
	one := sessionCopyFixture(2)
	before, err := json.Marshal(one.record)
	if err != nil {
		t.Fatal(err)
	}
	copied := one.copyLocked()
	raw, err := json.Marshal(copied)
	if err != nil || string(raw) != string(before) {
		t.Fatal("copy lost durable fields")
	}
	copied.CommandHashes["0"] = "changed"
	cmd := copied.Commands["0"]
	cmd.Activity[0] = "changed"
	copied.Commands["0"] = cmd
	copied.State.Settings.Models[0] = "changed"
	copied.State.Settings.Options[0].Choices[0].Value = "changed"
	copied.State.ModelChoices[0].Value = "changed"
	copied.State.Progress.Tools[0].Children[0].Name = "changed"
	copied.State.Progress.Settings.Options[0].Choices[0].Value = "changed"
	copied.State.Progress.Plan[0].Text = "changed"
	copied.State.Progress.Timeline[0].Text = "changed"
	copied.State.Progress.Usage.Cost.Amount = 9
	copied.State.Questions[0].Question.Choices[0].Value = "changed"
	copied.State.Questions[0].Answer.Text = "changed"
	copied.State.Questions[0].Permission.Options[0].Meta["nested"].(map[string]any)["key"].([]any)[0] = "changed"
	state := one.stateLocked("0")
	state.Command.Activity[0] = "changed"
	state.Questions[0].Answer.Text = "changed"
	after, err := json.Marshal(one.record)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("returned snapshot aliases original record")
	}
}

func BenchmarkSessionStateWithReceipts(b *testing.B) {
	for _, n := range []int{1, 512} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			one := sessionCopyFixture(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = one.stateLocked("0")
			}
		})
	}
}
