package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

// chatReplies is a Feishu chat that takes every text reply and keeps it,
// and refuses cards, so a turn's final answer arrives as text.
type chatReplies struct {
	nopChannel
	mu    sync.Mutex
	texts []string
}

func (c *chatReplies) Reply(_ context.Context, _ string, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, text)
	return nil
}

func (c *chatReplies) ReplyText(_ context.Context, _ string, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, text)
	return fmt.Sprintf("reply-%d", len(c.texts)), nil
}

func (c *chatReplies) sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.texts...)
}

// A scheduled run that finds its task still in an earlier attempt is
// refused in the chat in words the owner can act on, not as a failure that
// explains nothing.
func TestScheduledRunOnAnOpenAttemptTellsTheOwnerWhatToDo(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	f := testFire()
	catalog, err := agent.NewCatalog(map[string]agent.Config{f.Member: {Harness: "mock", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: f.ProjectID, Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	// The turn gets time to reach admission; with no idle timeout it
	// would expire before the task is even looked at.
	c := turntest.New(t, func(o *turntest.Options) {
		o.Ledger, o.Catalog, o.Projects, o.Tasks, o.Timeout = book, catalog, projects, tasks, time.Minute
		o.Owner, o.ChannelOwners, o.DefaultProject = f.Requester, map[string]string{"feishu": f.Requester}, f.ProjectID
	})
	if _, err := projects.Bind(t.Context(), f.ConversationID, f.ProjectID, f.Requester); err != nil {
		t.Fatal(err)
	}
	earlier, err := tasks.Create(task.Task{
		Goal: f.Prompt, Channel: f.ConversationID, Transport: "feishu", Member: f.Member, Requester: f.Requester,
		ProjectID: f.ProjectID, Origin: "schedule:" + f.ScheduleID, ChatID: f.ChatID, ChatType: f.ChatType, AnchorMessage: f.MessageID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.BeginTurn(earlier.ID, f.Member, "", task.TurnInput{Address: channel.Address{Channel: "feishu", Conversation: f.ConversationID, Message: f.MessageID}, ChatID: f.ChatID, ChatType: f.ChatType}); err != nil {
		t.Fatal(err)
	}
	g := New(c)
	chat := &chatReplies{}
	g.BindChannel(chat)
	if _, err := g.FireSchedule(t.Context(), f); err != nil {
		t.Fatalf("fire: %v", err)
	}
	text := i18n.New(i18n.LocaleZH)
	sent := chat.sent()
	if len(sent) < 2 || sent[0] != text.T(i18n.ScheduleNotice, f.ScheduleID) {
		t.Fatalf("the chat got %q, want the notice and then the answer", sent)
	}
	answer := sent[len(sent)-1]
	if answer == text.T(i18n.AgentFailed) {
		t.Fatalf("the chat got the failure that explains nothing: %q", answer)
	}
	for _, want := range []string{"#" + earlier.ID, "/tasks cancel " + earlier.ID} {
		if !strings.Contains(answer, want) {
			t.Errorf("the chat got %q, which does not say %q", answer, want)
		}
	}
	if strings.Contains(answer, text.T(i18n.RecoveryRetry)) {
		t.Errorf("console control in chat: %q", answer)
	}
	if strings.Contains(answer, "admit turn") || strings.Contains(answer, "open attempt") {
		t.Errorf("the chat got the internal error: %q", answer)
	}
}
