package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

const sharedConversationID = "console:shared"

// sharedConversation is a console conversation on project p in which two
// agents, worker and helper, each hold a task, with the production
// completion check. Nothing runs: what the console holds in the
// conversation is written to its records directly.
type sharedConversation struct {
	coordinator    *turn.Coordinator
	book           *ledger.Ledger
	tasks          *task.Store
	sessions       *state.Store
	projects       *project.Store
	worker, helper string
}

func openSharedConversation(t *testing.T) sharedConversation {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "test", Default: true}, "helper": {Harness: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}, {ID: "p2", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	f := sharedConversation{book: book, tasks: tasks, sessions: sessions, projects: projects}
	f.coordinator = turntest.New(t, func(o *turntest.Options) {
		o.Ledger, o.Catalog, o.Store, o.Tasks, o.Node = book, catalog, sessions, tasks, "hub"
		o.Projects, o.DefaultProject = projects, "p"
		o.ConsoleCompletionGuard = console.CheckTaskCompletionTx
	})
	for _, agentID := range []string{"worker", "helper"} {
		if err := sessions.SaveSession(state.Session{ConversationID: sharedConversationID, AgentID: agentID, HarnessID: "test", UpstreamID: agentID + "-session"}); err != nil {
			t.Fatal(err)
		}
		tracked, err := tasks.Create(task.Task{Transport: "console", Channel: sharedConversationID, Member: agentID, Requester: "owner", ProjectID: "p", Goal: agentID + " goal"})
		if err != nil {
			t.Fatal(err)
		}
		if agentID == "worker" {
			f.worker = tracked.ID
		} else {
			f.helper = tracked.ID
		}
	}
	return f
}

// hold writes what the console holds in the conversation.
func (f sharedConversation) hold(t *testing.T, held console.DurableState) {
	t.Helper()
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { return console.StoreStateTx(tx, held) }); err != nil {
		t.Fatal(err)
	}
}

// command runs input as the owner's console command.
func (f sharedConversation) command(t *testing.T, input string) (turn.Result, error) {
	t.Helper()
	return f.coordinator.Handle(t.Context(), turn.Request{
		Channel: "console", ConversationID: sharedConversationID, ChatID: console.ChatID,
		MessageID: console.AnchorMark + "command", ExchangeID: "command", Input: input,
		SenderOpenID: "owner", ChatType: protocol.ChatP2P, Mentioned: true, Locale: "en",
	})
}

// A refused reset or project switch names a task, and says to cancel it,
// only when what holds the close up is that task's own: cancelling it is
// then the way out. What the conversation holds for no task being closed —
// a question or a line of another agent's task, or of none — is not any
// closing task's to answer for, and the refusal names none of them, in a
// close of one task as in a close of several.
func TestCloseRefusalNamesATaskOnlyForWhatIsItsOwn(t *testing.T) {
	en := i18n.New(i18n.LocaleEN)
	for _, tc := range []struct {
		name  string
		input string
		held  func(f sharedConversation) console.DurableState
		// owner is the task the refusal names, with key; "" names none.
		owner func(f sharedConversation) string
		key   i18n.Key
	}{
		{
			name: "a switch waits for the continuation of the task checked last", input: "/project use p2",
			held: func(f sharedConversation) console.DurableState {
				return exchangeHeld(consoleapi.Exchange{ID: "later", Conversation: sharedConversationID, ExpectedTask: f.worker, State: consoleapi.ExchangeQueued})
			},
			owner: func(f sharedConversation) string { return f.worker }, key: i18n.TaskCloseDelivery,
		},
		{
			name: "a reset waits for its own task's question", input: "/new",
			held: func(f sharedConversation) console.DurableState {
				return questionHeld(consoleapi.PendingQuestion{Conversation: sharedConversationID, TaskID: f.worker, State: "pending"})
			},
			owner: func(f sharedConversation) string { return f.worker }, key: i18n.TaskCloseAttention,
		},
		{
			name: "a reset waits for another agent's question", input: "/new",
			held: func(f sharedConversation) console.DurableState {
				return questionHeld(consoleapi.PendingQuestion{Conversation: sharedConversationID, TaskID: f.helper, State: "pending"})
			},
		},
		{
			name: "a reset waits for another agent's running line", input: "/new",
			held: func(f sharedConversation) console.DurableState {
				return exchangeHeld(consoleapi.Exchange{ID: "other", Conversation: sharedConversationID, ExpectedTask: f.helper, State: consoleapi.ExchangeRunning})
			},
		},
		{
			name: "a switch waits for the conversation's question", input: "/project use p2",
			held: func(sharedConversation) console.DurableState {
				return questionHeld(consoleapi.PendingQuestion{Conversation: sharedConversationID, State: "pending"})
			},
		},
		{
			name: "a switch waits for a line recovering in the conversation", input: "/project use p2",
			held: func(sharedConversation) console.DurableState {
				return exchangeHeld(consoleapi.Exchange{ID: "other", Conversation: sharedConversationID, State: consoleapi.ExchangeRecovering})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := openSharedConversation(t)
			f.hold(t, tc.held(f))
			result, err := f.command(t, tc.input)
			var refusal turn.UserError
			if !errors.As(err, &refusal) {
				t.Fatalf("%s = %+v, %v; want it refused", tc.input, result, err)
			}
			if tc.owner != nil {
				owner := tc.owner(f)
				if want := en.T(tc.key, owner, protocol.CommandTasks); refusal.Text != want {
					t.Errorf("refusal = %q, want %q", refusal.Text, want)
				}
			} else {
				for _, id := range []string{f.worker, f.helper} {
					if strings.Contains(refusal.Text, "#"+id) {
						t.Errorf("refusal names task %s, which holds nothing up: %q", id, refusal.Text)
					}
				}
				if strings.Contains(refusal.Text, string(protocol.CommandTasks)+" cancel") {
					t.Errorf("refusal says to cancel a task that holds nothing up: %q", refusal.Text)
				}
			}
			for _, id := range []string{f.worker, f.helper} {
				if tracked, _ := f.tasks.Get(id); tracked.State != task.StateDraft {
					t.Errorf("task %s after the refused %s = %s, want it as it was", id, tc.input, tracked.State)
				}
			}
			if _, kept := f.sessions.Conversation(sharedConversationID).Sessions["worker"]; !kept {
				t.Errorf("refused %s archived the session", tc.input)
			}
			if binding, _, err := f.projects.Binding(t.Context(), sharedConversationID); err != nil || binding.ProjectID == "p2" {
				t.Errorf("refused %s left the conversation bound to %+v, %v", tc.input, binding, err)
			}
		})
	}
}

func exchangeHeld(e consoleapi.Exchange) console.DurableState {
	return console.DurableState{Exchanges: map[string][]console.DurableExchange{e.Conversation: {{Exchange: e}}}}
}

func questionHeld(q consoleapi.PendingQuestion) console.DurableState {
	q.ID = "question"
	return console.DurableState{Questions: map[string]consoleapi.PendingQuestion{q.ID: q}}
}
