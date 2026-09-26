package attempt

import (
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
)

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// How a stopped execution ended is recorded in the language of whoever
// confirmed the stop.
func TestStopConfirmationIsRecordedInTheCallerLanguage(t *testing.T) {
	for _, action := range []string{"task-stop", "process-stop"} {
		t.Run(action, func(t *testing.T) {
			s, _, old, proof, tasks := retainedFixture(t)
			ctx := i18n.WithLocale(t.Context(), i18n.LocaleEN)
			var got Record
			var err error
			if action == "task-stop" {
				if _, err := tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
				proof.Session.Command.State, proof.Session.Command.Settled = "cancelled", true
				proof.Session.State = "idle"
				got, err = s.ConfirmTaskStopped(ctx, old.ID, "test", proof)
			} else {
				proof.Session.Command = nil
				proof.Session.State, proof.Session.ProcessStopped = "closed", true
				got, err = s.ConfirmProcessStopped(ctx, old.ID, "test", proof)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Error == "" || containsHan(got.Error) {
				t.Fatalf("recorded ending = %q, want English", got.Error)
			}
		})
	}
}
