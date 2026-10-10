package turn

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func retirementCoordinator(t *testing.T) (*Coordinator, *nodeSessions, Request, string) {
	t.Helper()
	sessions := &nodeSessions{fakeManager: &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}}
	c, _, _ := owedCloseCoordinator(t, sessions)
	req := ownerChannels[0].req
	_, first := startOwedSession(t, c, req, "ns_owed")
	r, err := c.attempts.Get(t.Context(), first.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	return c, sessions, req, r.TaskID
}

func TestResetConversationSessionsPersistsAnUndispatchedClose(t *testing.T) {
	c, sessions, req, taskID := retirementCoordinator(t)
	before, ok := c.tasks.Get(taskID)
	if !ok {
		t.Fatal("missing task")
	}
	session := c.store.Conversation(req.ConversationID).Sessions["codex"]
	sessions.err = &nodewire.SessionNotDispatched{Cause: errors.New("test node offline")}
	if err := c.ResetConversationSessions(t.Context(), req.ConversationID); err != nil {
		t.Fatal(err)
	}
	owed := c.store.OwedCloses()
	if len(owed) != 1 || owed[0].TaskID != taskID || owed[0].UpstreamID != session.UpstreamID || owed[0].NodeID != session.NodeID || owed[0].HarnessID != session.HarnessID {
		t.Fatalf("lost exact close obligation: %+v", owed)
	}
	if _, ok := c.store.Conversation(req.ConversationID).Sessions["codex"]; ok {
		t.Fatal("old live slot survived retirement")
	}
	if got := c.store.ArchivedSessions(req.ConversationID, "codex"); len(got) != 1 || got[0].UpstreamID != session.UpstreamID || got[0].ArchivedAt != owed[0].OwedAt {
		t.Fatalf("archive differs from debt: %+v", got)
	}
	after, _ := c.tasks.Get(taskID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reset changed task authority/accounting")
	}
	reopened, err := state.OpenLedger(ledgerOf(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.OwedCloses(), owed) {
		t.Fatal("reopen lost close debt")
	}
}

func TestDiscardConversationKeepsAuthorityForANewOwedClose(t *testing.T) {
	c, sessions, req, taskID := retirementCoordinator(t)
	if err := c.tasks.ChannelIdle(t.Context(), req.ConversationID, checkConversationRetirement); err != nil {
		t.Fatalf("invalid idle prerequisite: %v", err)
	}
	before, _ := c.tasks.Get(taskID)
	job, err := c.schedules.Create(schedule.Job{ConversationID: req.ConversationID, Member: "codex", Prompt: "later", Spec: schedule.Spec{Kind: schedule.KindEvery, Every: time.Hour, Text: "every hour"}})
	if err != nil {
		t.Fatal(err)
	}
	sessions.err = &nodewire.SessionNotDispatched{Cause: errors.New("test node offline")}
	err = c.DiscardConversation(t.Context(), req.ConversationID)
	if !errors.Is(err, task.ErrRetirementPending) || !errors.Is(err, state.ErrCloseOwed) {
		t.Fatalf("discard = %v; want durable close pending", err)
	}
	after, exists := c.tasks.Get(taskID)
	if !exists || !reflect.DeepEqual(before, after) {
		t.Fatal("pending discard deleted or changed original authority")
	}
	jobs := c.schedules.List(req.ConversationID)
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatal("pending discard dropped schedule")
	}
	owed := c.store.OwedCloses()
	if len(owed) != 1 || owed[0].TaskID != taskID {
		t.Fatalf("missing exact obligation: %+v", owed)
	}
	// This fixture's receiving node now acknowledges exactly the original close.
	// Production reconciliation owns the same operation; no new task is created.
	sessions.err = nil
	if _, err := c.commands().closeSession(t.Context(), c.store.ArchivedSessions(req.ConversationID, "codex")[0].Session); err != nil {
		t.Fatal(err)
	}
	if err := c.store.SettleOwedClose(owed[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.DiscardConversation(t.Context(), req.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, exists := c.tasks.Get(taskID); exists {
		t.Fatal("settled discard kept task")
	}
	if len(c.schedules.List(req.ConversationID)) != 0 {
		t.Fatal("settled discard kept schedule")
	}
}

func TestConversationRetirementRetainsUnresolvedClose(t *testing.T) {
	for _, route := range []string{"reset", "discard"} {
		for _, failure := range []error{errors.New("test response lost"), context.DeadlineExceeded, &node.SessionError{Code: "uncertain", Message: "test original stop unconfirmed"}} {
			t.Run(route+"/"+failure.Error(), func(t *testing.T) {
				c, sessions, req, taskID := retirementCoordinator(t)
				before := c.store.Conversation(req.ConversationID)
				beforeTask, _ := c.tasks.Get(taskID)
				sessions.err = failure
				var err error
				if route == "reset" {
					err = c.ResetConversationSessions(t.Context(), req.ConversationID)
				} else {
					err = c.DiscardConversation(t.Context(), req.ConversationID)
				}
				if !errors.Is(err, failure) {
					t.Fatalf("retirement swallowed close failure: %v", err)
				}
				if !reflect.DeepEqual(before, c.store.Conversation(req.ConversationID)) {
					t.Fatal("failed close lost session/history")
				}
				after, exists := c.tasks.Get(taskID)
				if !exists || !reflect.DeepEqual(beforeTask, after) {
					t.Fatal("failed close changed original task")
				}
				if len(c.store.OwedCloses()) != 0 {
					t.Fatal("response uncertainty was invented as undispatched")
				}
			})
		}
	}
}

type retirementGateRuntime struct {
	*nodeSessions
	entered chan struct{}
	release chan struct{}
}

func (r *retirementGateRuntime) CloseSession(ctx context.Context, at harness.Placement, id string) error {
	close(r.entered)
	select {
	case <-r.release:
		return r.nodeSessions.CloseSession(ctx, at, id)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestConversationRetirementFencesNewTurnsOnlyInItsConversation(t *testing.T) {
	c, sessions, req, _ := retirementCoordinator(t)
	gate := &retirementGateRuntime{nodeSessions: sessions, entered: make(chan struct{}), release: make(chan struct{})}
	c.runtime = gate
	done := make(chan error, 1)
	go func() { done <- c.ResetConversationSessions(t.Context(), req.ConversationID) }()
	select {
	case <-gate.entered:
	case <-time.After(waitDeadline):
		t.Fatal("close did not reach gate")
	}
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	if c.beginTurn(req.ConversationID, "new-agent", cancel) {
		c.clearActive(req.ConversationID, "new-agent")
		t.Error("new agent entered retiring conversation")
	}
	if err := c.DiscardConversation(t.Context(), req.ConversationID); !errors.Is(err, ErrConversationBusy) {
		t.Errorf("concurrent retirement = %v", err)
	}
	if !c.beginTurn("console:unrelated", "other", cancel) {
		t.Error("retirement globally blocked unrelated conversation")
	} else {
		c.clearActive("console:unrelated", "other")
	}
	close(gate.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("reset did not finish")
	}
	if !c.beginTurn(req.ConversationID, "new-agent", cancel) {
		t.Fatal("completed retirement kept turn fence")
	}
	c.clearActive(req.ConversationID, "new-agent")
}

func TestConversationRetirementDoesNotInventAnUnidentifiedDebt(t *testing.T) {
	for _, route := range []string{"reset", "discard"} {
		t.Run(route, func(t *testing.T) {
			c, sessions, req, taskID := retirementCoordinator(t)
			current := c.store.Conversation(req.ConversationID).Sessions["codex"]
			current.UpstreamID = "ns_no_attempt"
			if err := c.store.SaveSession(current); err != nil {
				t.Fatal(err)
			}
			sessions.err = &nodewire.SessionNotDispatched{Cause: errors.New("test node offline")}
			var err error
			if route == "reset" {
				err = c.ResetConversationSessions(t.Context(), req.ConversationID)
			} else {
				err = c.DiscardConversation(t.Context(), req.ConversationID)
			}
			if !errors.Is(err, sessions.err) {
				t.Fatalf("unidentified close = %v", err)
			}
			if got := c.store.Conversation(req.ConversationID).Sessions["codex"]; !reflect.DeepEqual(got, current) {
				t.Fatal("unidentified close lost its slot")
			}
			if len(c.store.OwedCloses()) != 0 {
				t.Fatal("made up an execution for close")
			}
			if _, exists := c.tasks.Get(taskID); !exists {
				t.Fatal("unidentified close dropped its task")
			}
		})
	}
}

func TestConversationRetirementKeepsItsSlotAfterPersistenceRefusal(t *testing.T) {
	c, sessions, req, taskID := retirementCoordinator(t)
	before := c.store.Conversation(req.ConversationID)
	book := ledgerOf(t, c)
	if _, err := book.DB().Exec(`CREATE TRIGGER refuse_retirement BEFORE INSERT ON bindings WHEN NEW.kind='document' AND NEW.id='state' BEGIN SELECT RAISE(FAIL,'test session retirement write refused'); END`); err != nil {
		t.Fatal(err)
	}
	sessions.err = &nodewire.SessionNotDispatched{Cause: errors.New("test node offline")}
	if err := c.ResetConversationSessions(t.Context(), req.ConversationID); err == nil {
		t.Fatal("refused retirement reported success")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER refuse_retirement`); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []*state.Store{c.store, reopened} {
		if !reflect.DeepEqual(store.Conversation(req.ConversationID), before) || len(store.OwedCloses()) != 0 {
			t.Fatal("refused write lost live slot or added half an obligation")
		}
	}
	if _, exists := c.tasks.Get(taskID); !exists {
		t.Fatal("refused write changed task authority")
	}
	if !c.beginTurn(req.ConversationID, "other", func() {}) {
		t.Fatal("failed retirement left its admission fence")
	}
	c.clearActive(req.ConversationID, "other")
}
