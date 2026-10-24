package approvals

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/feed"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

type tasks struct {
	mu      sync.Mutex
	mode    string
	waiting map[string]int
}

func (t *tasks) ModeOf(string) string { return t.mode }
func (t *tasks) Waiting(id string) func() {
	t.mu.Lock()
	t.waiting[id]++
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		t.waiting[id]--
		t.mu.Unlock()
	}
}
func (t *tasks) count(id string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.waiting[id]
}

var options = []acp.PermissionOption{
	{OptionID: "yes", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
	{OptionID: "always", Name: "Always allow", Kind: acp.PermissionOptionKindAllowAlways},
	{OptionID: "no", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
}

func taskCtx(id string) context.Context {
	return execution.WithProbeKey(context.Background(), execution.Key{TaskID: id, AttemptID: "a"})
}

func openOne(t *testing.T, s *Service) Card {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if open := s.Open(); len(open) == 1 {
			return open[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no card opened")
	return Card{}
}

func TestAutoModeAsksThePersonAndUsesTheirChoice(t *testing.T) {
	tk := &tasks{mode: "auto", waiting: map[string]int{}}
	events := feed.New("e", 0)
	s := New(tk, events, time.Minute)
	got := make(chan acp.RequestPermissionOutcome, 1)
	go func() {
		out, _ := s.Ask(taskCtx("7"), permission.Ask{ToolName: "git push origin fix-7", Kind: acp.ToolKindExecute, Reason: "push the branch", Options: options})
		got <- out
	}()
	card := openOne(t, s)
	if card.TaskID != "7" || card.Kind != KindPermission || card.Title != "git push origin fix-7" || len(card.Options) != 3 || !card.Options[0].Allow || card.Options[2].Allow {
		t.Fatalf("card = %+v", card)
	}
	if tk.count("7") != 1 {
		t.Fatal("task not marked waiting while the card is open")
	}
	if _, err := s.Resolve(card.ID, Decision{Approve: true, Choice: "missing"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown option = %v", err)
	}
	if _, err := s.Resolve(card.ID, Decision{Approve: true, Choice: "always"}); err != nil {
		t.Fatal(err)
	}
	if out := <-got; out.Outcome != acp.RequestPermissionOutcomeTypeSelected || out.OptionID != "always" {
		t.Fatalf("outcome = %+v", out)
	}
	if _, err := s.Resolve(card.ID, Decision{Approve: false}); !errors.Is(err, ErrClosed) {
		t.Fatalf("second answer = %v", err)
	}
	if tk.count("7") != 0 || len(s.Open()) != 0 {
		t.Fatal("card still waiting after its answer")
	}
	events.Publish("probe", 0)
	all, _ := events.After(feed.Cursor{Epoch: "e"})
	if len(all) != 3 || all[0].Type != EventOpened || all[1].Type != EventResolved {
		t.Fatalf("events = %+v", all)
	}
}

func TestRejectionAndTimeoutBothRefuse(t *testing.T) {
	tk := &tasks{mode: "auto", waiting: map[string]int{}}
	s := New(tk, nil, time.Minute)
	got := make(chan acp.RequestPermissionOutcome, 1)
	go func() {
		out, _ := s.Ask(taskCtx("1"), permission.Ask{ToolName: "rm -rf build", Options: options})
		got <- out
	}()
	card := openOne(t, s)
	s.Resolve(card.ID, Decision{Approve: false})
	if out := <-got; out.OptionID != "no" {
		t.Fatalf("rejected = %+v", out)
	}
	s.timeout = 20 * time.Millisecond
	out, err := s.Ask(taskCtx("1"), permission.Ask{ToolName: "deploy", Options: options})
	if err != nil || out.OptionID != "no" {
		t.Fatalf("timed out = %+v %v", out, err)
	}
	if card, _ := s.Get("2"); card.State != StateExpired {
		t.Fatalf("expired card = %+v", card)
	}
}

func TestYoloGrantsAtOnceButKeepsTheRecord(t *testing.T) {
	tk := &tasks{mode: ModeYolo, waiting: map[string]int{}}
	s := New(tk, nil, time.Minute)
	out, err := s.Ask(taskCtx("3"), permission.Ask{ToolName: "git push", Options: options})
	if err != nil || out.OptionID != "yes" {
		t.Fatalf("yolo = %+v %v", out, err)
	}
	card, err := s.Get("1")
	if err != nil || card.State != StateApproved || !card.Auto || tk.count("3") != 0 || len(s.Open()) != 0 {
		t.Fatalf("record = %+v %v", card, err)
	}
}

func TestQuestionsReachThePersonEvenInYolo(t *testing.T) {
	tk := &tasks{mode: ModeYolo, waiting: map[string]int{}}
	s := New(tk, nil, time.Minute)
	got := make(chan view.Answer, 1)
	go func() {
		a, _ := s.AskUser(taskCtx("4"), view.Question{Message: "Which branch?", Choices: []view.Choice{{Value: "main", Label: "main"}, {Value: "dev", Label: "dev"}}})
		got <- a
	}()
	card := openOne(t, s)
	if card.Kind != KindQuestion || card.AllowFreeText || len(card.Options) != 2 {
		t.Fatalf("card = %+v", card)
	}
	if _, err := s.Resolve(card.ID, Decision{Text: "whatever"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("free text on a choice question = %v", err)
	}
	s.Resolve(card.ID, Decision{Choice: "dev"})
	if a := <-got; a.Value != "dev" || a.Decision != "accept" {
		t.Fatalf("answer = %+v", a)
	}
}

func TestAnEndedExecutionExpiresItsCard(t *testing.T) {
	tk := &tasks{mode: "auto", waiting: map[string]int{}}
	s := New(tk, nil, time.Minute)
	ctx, cancel := context.WithCancel(taskCtx("5"))
	done := make(chan error, 1)
	go func() {
		_, err := s.Ask(ctx, permission.Ask{ToolName: "x", Options: options})
		done <- err
	}()
	card := openOne(t, s)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := s.Get(card.ID); got.State != StateExpired || tk.count("5") != 0 {
		t.Fatalf("card = %+v", got)
	}
}

func TestDenyRefusesAtOnceWithARecord(t *testing.T) {
	tk := &tasks{mode: ModeDeny, waiting: map[string]int{}}
	s := New(tk, nil, time.Minute)
	out, err := s.Ask(taskCtx("9"), permission.Ask{ToolName: "gh pr merge 3", Options: options})
	if err != nil || out.OptionID != "no" {
		t.Fatalf("deny = %+v %v", out, err)
	}
	a, err := s.AskUser(taskCtx("9"), view.Question{Message: "merge?"})
	if err != nil || a.Decision != "decline" || a.Chosen() {
		t.Fatalf("question = %+v %v", a, err)
	}
	for _, id := range []string{"1", "2"} {
		if card, _ := s.Get(id); card.State != StateRejected || !card.Auto {
			t.Fatalf("record %s = %+v", id, card)
		}
	}
	if len(s.Open()) != 0 || tk.count("9") != 0 {
		t.Fatal("deny waited on the person")
	}
	events := feed.New("e", 0)
	s.feed = events
	s.Ask(taskCtx("9"), permission.Ask{ToolName: "again", Options: options})
	all, _ := events.After(feed.Cursor{Epoch: "e"})
	if len(all) != 1 || all[0].Type != EventResolved {
		t.Fatalf("an automatic decision published %+v", all)
	}
}
