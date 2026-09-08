package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

type runner struct {
	id       string
	turns    int
	prompts  int
	resumes  int
	stopped  bool
	block    bool
	err      error
	progress []view.Progress
	// during runs while the prompt is in flight, before it ends.
	during func()
}

func (r *runner) ID() string { return r.id }
func (r *runner) Prompt(ctx context.Context, _ string, observe func(view.Progress)) (string, []string, error) {
	r.prompts++
	for _, p := range r.progress {
		observe(p)
	}
	if r.during != nil {
		r.during()
	}
	if r.block {
		<-ctx.Done()
		return "", nil, ctx.Err()
	}
	return "plain", nil, r.err
}
func (r *runner) PromptTurn(ctx context.Context, _ string, _ []harness.Media, _ permission.AskFunc, _ acphost.AskUserFunc, observe func(view.Progress)) (string, []string, error) {
	r.turns++
	for _, p := range r.progress {
		observe(p)
	}
	if r.during != nil {
		r.during()
	}
	if r.block {
		<-ctx.Done()
		return "", nil, ctx.Err()
	}
	return "turn", []string{"did"}, r.err
}
func (r *runner) ResumeTurn(ctx context.Context, _ permission.AskFunc, _ acphost.AskUserFunc, observe func(view.Progress)) (string, []string, error) {
	r.resumes++
	for _, p := range r.progress {
		observe(p)
	}
	if r.during != nil {
		r.during()
	}
	if r.block {
		<-ctx.Done()
		return "", nil, ctx.Err()
	}
	return "resumed", nil, r.err
}
func (r *runner) Cancel(context.Context) error { return nil }
func (r *runner) Abort()                       {}
func (r *runner) Stopped() bool                { return r.stopped }

type leaser struct{ lost chan struct{} }

func (l *leaser) Heartbeat(context.Context, string) <-chan struct{} { return l.lost }

type sessions struct{ err error }

func (s sessions) CloseSession(context.Context, harness.Placement, string) error { return s.err }

func TestDriveUsesTheTurnEntryOnlyWhenAsked(t *testing.T) {
	r := &runner{id: "s1", progress: []view.Progress{{Settings: view.Settings{Model: "m"}, Usage: view.Usage{InputTokens: 3, OutputTokens: 4}}}}
	plain := Drive{Session: r, Prompt: "hi"}.Run(t.Context())
	if r.prompts != 1 || r.turns != 0 || plain.Answer != "plain" || !plain.PromptSettled || plain.Stopped {
		t.Fatalf("plain drive = %+v prompts=%d turns=%d", plain, r.prompts, r.turns)
	}
	if u := Usage(plain.Last); u.Model != "m" || u.Input != 3 || u.Output != 4 || !u.Reported {
		t.Fatalf("usage = %+v", u)
	}
	turn := Drive{Session: r, Prompt: "hi", Turn: true}.Run(t.Context())
	if r.turns != 1 || turn.Answer != "turn" || len(turn.Activity) != 1 {
		t.Fatalf("turn drive = %+v turns=%d", turn, r.turns)
	}
}

func TestDriveTellsSettledFromUnconfirmed(t *testing.T) {
	unconfirmed := &runner{id: "s2", err: errors.New("connection lost")}
	out := Drive{Session: unconfirmed}.Run(t.Context())
	if out.PromptSettled || out.Settled() {
		t.Fatalf("a lost connection settled the prompt: %+v", out)
	}
	unconfirmed.stopped = true
	out = Drive{Session: unconfirmed}.Run(t.Context())
	if out.PromptSettled || !out.Stopped || !out.Settled() {
		t.Fatalf("a stopped process did not settle the prompt: %+v", out)
	}
	cancelled := &runner{id: "s3", err: harness.ErrTurnCanceled}
	if out := (Drive{Session: cancelled}).Run(t.Context()); !out.PromptSettled {
		t.Fatalf("the agent ending its own turn is settled: %+v", out)
	}
}

func TestKeepCallsLostOnceAndNotAfterStop(t *testing.T) {
	l := &leaser{lost: make(chan struct{})}
	lost := make(chan struct{}, 2)
	stop := Keep(t.Context(), l, "a1", func() { lost <- struct{}{} })
	close(l.lost)
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("a lost lease did not reach the caller")
	}
	stop()
	quiet := &leaser{lost: make(chan struct{})}
	stopQuiet := Keep(t.Context(), quiet, "a2", func() { t.Error("lost was called after stop") })
	stopQuiet()
	close(quiet.lost)
	time.Sleep(50 * time.Millisecond)
}

func TestCloseIsUnconfirmedUnlessTheProcessStopped(t *testing.T) {
	r := &runner{id: "s4"}
	err := Close(t.Context(), sessions{err: errors.New("node away")}, harness.Placement{}, r)
	if !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("a failed close on a live process = %v", err)
	}
	r.stopped = true
	if err := Close(t.Context(), sessions{err: errors.New("node away")}, harness.Placement{}, r); err != nil {
		t.Fatalf("a failed close on a stopped process = %v", err)
	}
	if err := Close(t.Context(), sessions{}, harness.Placement{}, &runner{id: "s5"}); err != nil {
		t.Fatalf("a clean close = %v", err)
	}
}

func TestCleanupOutlivesTheCaller(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	ctx, stop := Cleanup(parent)
	defer stop()
	if ctx.Err() != nil {
		t.Fatal("cleanup context inherited the caller's cancellation")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > CleanupTimeout {
		t.Fatalf("cleanup deadline = %v %v", deadline, ok)
	}
	if !Managed(&runner{id: "ns_abc"}) || Managed(&runner{id: "s"}) || Managed(nil) {
		t.Fatal("managed sessions are the ones the node owns")
	}
}
