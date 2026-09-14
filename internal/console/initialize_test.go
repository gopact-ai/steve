package console

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
)

type initializingHandler struct {
	echo
	calls   [][3]string
	failure error
}

func (h *initializingHandler) InitializeConversation(_ context.Context, conversation, project, requester string) error {
	h.calls = append(h.calls, [3]string{conversation, project, requester})
	return h.failure
}

func TestInitializeConversationIsEmptyDurableAndTitledByFirstPrompt(t *testing.T) {
	h := &initializingHandler{}
	model := readmodel.New(readmodel.Sources{})
	events, stop := model.Subscribe(t.Context())
	defer stop()
	s := New(h, "owner", model)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeConversation(t.Context(), "new", "p"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.calls, [][3]string{{"console:new", "p", "owner"}}) || len(h.seen) != 0 {
		t.Fatalf("wrong handler calls: %+v, %+v", h.calls, h.seen)
	}
	if len(s.Replies("new")) != 0 || len(s.Queue("new")) != 0 {
		t.Fatal("initialization created transcript or work")
	}
	items := s.Summaries(t.Context())
	if len(items) != 1 || items[0].Title != "" || items[0].Count != 0 || items[0].LastAt.IsZero() {
		t.Fatalf("empty summary = %+v", items)
	}
	select {
	case event := <-events:
		if event.Kind != "console.meta" {
			t.Fatalf("event = %+v", event)
		}
	default:
		t.Fatal("missing metadata event")
	}
	if err := s.InitializeConversation(t.Context(), "console:new", "p"); err != nil {
		t.Fatal(err)
	}
	if got := s.Summaries(t.Context()); !reflect.DeepEqual(got, items) {
		t.Fatalf("retry changed summary: %+v", got)
	}
	restarted := New(h, "owner", nil)
	if err := restarted.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if got := restarted.Summaries(t.Context()); !reflect.DeepEqual(got, items) {
		t.Fatalf("restart lost empty conversation: %+v", got)
	}
	if err := restarted.InitializeConversation(t.Context(), "new", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Send(t.Context(), "new", "Build a useful dashboard"); err != nil {
		t.Fatal(err)
	}
	if got := restarted.Summaries(t.Context()); len(got) != 1 || got[0].Title != "Build a useful dashboard" || got[0].Count != 2 {
		t.Fatalf("first actual prompt = %+v", got)
	}
}

func TestInitializeConversationFailureLeavesNoPhantomAndCanRetry(t *testing.T) {
	h := &initializingHandler{failure: errors.New("project denied")}
	s := New(h, "owner", nil)
	doc := &brokenQueueDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeConversation(t.Context(), "new", "p"); err == nil {
		t.Fatal("binding failure accepted")
	}
	if len(s.Conversations()) != 0 || len(s.meta) != 0 {
		t.Fatal("binding failure created phantom")
	}
	h.failure = nil
	doc.err = errors.New("disk unavailable")
	if err := s.InitializeConversation(t.Context(), "new", "p"); err == nil {
		t.Fatal("save failure accepted")
	}
	if len(s.Conversations()) != 0 || len(s.meta) != 0 {
		t.Fatal("save failure created phantom")
	}
	doc.err = nil
	if err := s.InitializeConversation(t.Context(), "new", "p"); err != nil {
		t.Fatal(err)
	}
	if len(s.Conversations()) != 1 {
		t.Fatal("retry did not restore conversation")
	}
}

func TestInitializeConversationRetriesCommittedBindingAfterTranscriptSaveFailure(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}, {ID: "other", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	coordinator := turn.New(nil, nil, nil, nil, time.Minute)
	coordinator.SetIdentity("owner", nil)
	coordinator.SetProjects(projects, "p", "")
	s := New(coordinator, "owner", nil)
	doc := &brokenQueueDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	doc.err = errors.New("disk unavailable")
	if err := s.InitializeConversation(t.Context(), "new", "p"); err == nil {
		t.Fatal("save failure accepted")
	}
	binding, ok, err := projects.Binding(t.Context(), "console:new")
	if err != nil || !ok || binding.Version != 1 {
		t.Fatalf("binding did not commit: %+v, %v", binding, err)
	}
	if len(s.Conversations()) != 0 {
		t.Fatal("failed persistence created phantom")
	}
	doc.err = nil
	restarted := New(coordinator, "owner", nil)
	if err := restarted.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if err := restarted.InitializeConversation(t.Context(), "new", "other"); err == nil {
		t.Fatal("retry switched already-committed project")
	}
	if len(restarted.Conversations()) != 0 {
		t.Fatal("conflicting retry created phantom")
	}
	if err := restarted.InitializeConversation(t.Context(), "new", "p"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := projects.Binding(t.Context(), "console:new"); got != binding {
		t.Fatalf("retry advanced binding: %+v", got)
	}
	if len(restarted.Conversations()) != 1 || len(restarted.Replies("new")) != 0 {
		t.Fatal("retry did not persist empty conversation")
	}
}

func TestInitializeConversationRejectsInvalidOrStoppedRequests(t *testing.T) {
	for _, reason := range []string{"conversation", "prefix", "project", "owner", "handler", "closing", "maintenance", "recovery", "canceled"} {
		t.Run(reason, func(t *testing.T) {
			h := &initializingHandler{}
			s := New(h, "owner", nil)
			conversation, project := "new", "p"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch reason {
			case "conversation":
				conversation = " "
			case "prefix":
				conversation = "console:"
			case "project":
				project = " "
			case "owner":
				s.owner = ""
			case "handler":
				s.handler = &echo{}
			case "closing":
				s.closing = true
			case "maintenance":
				s.maintenance = true
			case "recovery":
				s.EnableRetainedRecovery(ctx)
				cancel()
			case "canceled":
				cancel()
			}
			if err := s.InitializeConversation(ctx, conversation, project); err == nil {
				t.Fatal("invalid initialization accepted")
			}
			if len(h.calls) != 0 || len(s.Conversations()) != 0 {
				t.Fatal("invalid initialization changed state")
			}
		})
	}
}

func TestSummariesLeaveCommandOnlyTitlesEmptyAndPreserveUserTitles(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	when := time.Now().UTC()
	s.replies["console:opaque-id"] = []consoleapi.Reply{{Kind: "sent", Input: "/project use p", At: when}}
	s.replies["console:named"] = []consoleapi.Reply{{Kind: "sent", Input: "/project use p", At: when}}
	s.meta["console:named"] = Meta{Title: "/project use p", TitleBy: "user"}
	before := append([]consoleapi.Reply{}, s.replies["console:opaque-id"]...)
	for _, got := range s.Summaries(t.Context()) {
		if got.ID == "console:opaque-id" && (got.Title != "" || !got.LastAt.Equal(when)) {
			t.Fatalf("command-only summary = %+v", got)
		}
		if got.ID == "console:named" && (got.Title != "/project use p" || got.TitleBy != "user") {
			t.Fatalf("user title changed = %+v", got)
		}
	}
	if !reflect.DeepEqual(s.replies["console:opaque-id"], before) {
		t.Fatal("summary rewrote transcript")
	}
}
