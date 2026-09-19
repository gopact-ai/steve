package gateway

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
)

type recoveryProbe struct {
	calls, resumes atomic.Int32
	retained       []turn.RetainedChat
	fail           error
}

func (p *recoveryProbe) Handle(_ context.Context, r turn.Request) (turn.Result, error) {
	p.calls.Add(1)
	if r.OnTurnReady != nil {
		r.OnTurnReady(r.ExpectedTask, "new-attempt")
	}
	return turn.Result{Text: "complete original result", Attempt: "new-attempt"}, p.fail
}
func (p *recoveryProbe) RetainedChats(context.Context) ([]turn.RetainedChat, error) {
	return p.retained, nil
}
func (p *recoveryProbe) ResumeRetainedChat(_ context.Context, id string, r turn.Request) (turn.Result, error) {
	p.resumes.Add(1)
	return turn.Result{Text: "complete original result", Attempt: id}, p.fail
}

type recoveryChannel struct {
	notices, results atomic.Int32
	noticeErr        error
}

func (ch *recoveryChannel) Reply(context.Context, string, string) error { return nil }
func (ch *recoveryChannel) ReplyText(_ context.Context, _ string, text string) (string, error) {
	if strings.Contains(text, "complete original result") {
		ch.results.Add(1)
		return "reply-receipt", nil
	}
	ch.notices.Add(1)
	return "notice-receipt", ch.noticeErr
}
func revivalFixture() Revival {
	return Revival{TaskID: "parent", Goal: "original goal", Member: "worker", ConversationID: "conversation", ChatID: "chat", MessageID: "original-message", Requester: "owner", ChatType: "p2p"}
}

func TestGatewayRecoveryInputSurvivesBeforeDispatchAndDoesNotReplayCompletedCommand(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := &recoveryProbe{}
	ch := &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	if err := g.QueueRecovery(t.Context(), book, "restart:parent", revivalFixture(), ""); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 0 || ch.notices.Load() != 0 {
		t.Fatal("acceptance dispatched work before accounting")
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	g = New(p)
	g.BindChannel(ch)
	for range 2 {
		if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls.Load() != 1 || ch.notices.Load() != 1 || ch.results.Load() != 1 {
		t.Fatalf("replayed durable command: calls=%d notices=%d results=%d", p.calls.Load(), ch.notices.Load(), ch.results.Load())
	}
	wrong := revivalFixture()
	wrong.TaskID = "other"
	if err := g.QueueRecovery(t.Context(), book, "restart:parent", wrong, ""); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("key accepted another identity: %v", err)
	}
}

func TestGatewayRecoveryBoundResultNeverSubmitsPrompt(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &recoveryProbe{}
	ch := &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	if err := g.QueueRecovery(t.Context(), book, "result:original", revivalFixture(), "original-attempt"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls.Load() != 0 || ch.notices.Load() != 0 || ch.results.Load() != 1 || p.resumes.Load() != 1 {
		t.Fatalf("replayed native input or result: calls=%d notices=%d replies=%d resumes=%d", p.calls.Load(), ch.notices.Load(), ch.results.Load(), p.resumes.Load())
	}
}

func TestGatewayCompletedProcessingErrorDeliversOnceWithoutReenteringNative(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &recoveryProbe{fail: errors.New("original execution failed")}
	ch := &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	if err := g.QueueRecovery(t.Context(), book, "failed-original", revivalFixture(), ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
			t.Fatalf("completed processing error stranded its result: %v", err)
		}
	}
	if p.calls.Load() != 1 || p.resumes.Load() != 0 || ch.results.Load() != 1 || ch.notices.Load() != 1 {
		t.Fatalf("failed result reran work or lost delivery: calls=%d resumes=%d replies=%d notices=%d", p.calls.Load(), p.resumes.Load(), ch.results.Load(), ch.notices.Load())
	}
}

func TestGatewayUnknownNoticeRemainsVisibleAndNeverReposts(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &recoveryProbe{}
	ch := &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	if err := g.QueueRecovery(t.Context(), book, "restart:parent", revivalFixture(), ""); err != nil {
		t.Fatal(err)
	}
	_, err = book.DB().Exec(`CREATE TRIGGER fail_notice_receipt BEFORE UPDATE ON commands WHEN NEW.kind='gateway-recovery-notice' BEGIN SELECT RAISE(ABORT,'notice receipt unavailable'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err == nil {
		t.Fatal("receipt failure hidden")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER fail_notice_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); !errors.Is(err, channel.ErrOutcomeUnknown) {
		t.Fatalf("unknown notice not visible: %v", err)
	}
	rows, err := book.Commands(t.Context(), "gateway-recovery-notice")
	if err != nil || len(rows) != 1 || rows[0].FinishedAt != nil {
		t.Fatalf("unknown receipt lost: %+v %v", rows, err)
	}
	if ch.notices.Load() != 1 || p.calls.Load() != 0 {
		t.Fatal("unknown notice retried or dispatched native work")
	}
}

func TestGatewayUnknownDispatchUsesPersistedAttemptNotHandle(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &recoveryProbe{retained: []turn.RetainedChat{{AttemptID: "new-attempt", TaskID: "parent", Conversation: "conversation", MessageID: "notice-receipt", Completed: true}}}
	ch := &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	if err := g.QueueRecovery(t.Context(), book, "restart:parent", revivalFixture(), ""); err != nil {
		t.Fatal(err)
	}
	_, err = book.DB().Exec(`CREATE TRIGGER fail_dispatch_receipt BEFORE UPDATE ON commands WHEN NEW.kind='gateway-recovery-dispatch' BEGIN SELECT RAISE(ABORT,'dispatch receipt unavailable'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err == nil {
		t.Fatal("dispatch receipt failure hidden")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER fail_dispatch_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 || p.resumes.Load() != 1 || ch.results.Load() != 1 {
		t.Fatalf("dispatch was replayed: calls=%d resumes=%d replies=%d", p.calls.Load(), p.resumes.Load(), ch.results.Load())
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 || p.resumes.Load() != 1 || ch.results.Load() != 1 {
		t.Fatal("completed result was not idempotent")
	}
}

func TestGatewayRecoveryAckFailureRestartsWithoutRedelivering(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, ch := &recoveryProbe{}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	key := "result:original"
	if err := g.QueueRecovery(t.Context(), book, key, revivalFixture(), "original-attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_recovery_ack BEFORE UPDATE OF acknowledged_by ON commands BEGIN SELECT RAISE(ABORT,'ack unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err == nil || !strings.Contains(err.Error(), "ack unavailable") {
		t.Fatalf("ack persistence failure hidden: %v", err)
	}
	pending, err := book.PendingCommands(t.Context(), recoveryInputKind)
	if err != nil || len(pending) != 1 {
		t.Fatalf("failed acknowledgement disappeared: %+v %v", pending, err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_recovery_ack`); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	g = New(p)
	g.BindChannel(ch)
	for range 2 {
		if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	pending, err = book.PendingCommands(t.Context(), recoveryInputKind)
	if err != nil || len(pending) != 0 {
		t.Fatalf("ack retry did not converge: %+v %v", pending, err)
	}
	if ch.results.Load() != 1 || p.resumes.Load() != 1 || p.calls.Load() != 0 || ch.notices.Load() != 0 {
		t.Fatalf("ack-only retry repeated work: replies=%d resumes=%d calls=%d notices=%d", ch.results.Load(), p.resumes.Load(), p.calls.Load(), ch.notices.Load())
	}
}

func TestGatewayRecoveryReadsPendingNotCompletedHistory(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if _, err := book.DB().Exec(`WITH RECURSIVE n(seq) AS (VALUES(1) UNION ALL SELECT seq+1 FROM n WHERE seq<10000)
		INSERT INTO commands(id,kind,actor,received_at,finished_at,result,error,acknowledged_by)
		SELECT 'old-'||seq,'gateway-recovery-input','owner','invalid-date','invalid-date','invalid-json','','proof-'||seq FROM n`); err != nil {
		t.Fatal(err)
	}
	p, ch := &recoveryProbe{}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	if err := g.QueueRecovery(t.Context(), book, "pending", revivalFixture(), "original-attempt"); err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
		t.Fatalf("runtime decoded completed history: %v", err)
	}
	pending, err := book.PendingCommands(t.Context(), recoveryInputKind)
	if err != nil || len(pending) != 0 || ch.results.Load() != 1 {
		t.Fatalf("pending=%+v replies=%d err=%v", pending, ch.results.Load(), err)
	}
}
