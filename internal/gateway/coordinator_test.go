package gateway

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

// IdleCoordinator is a gateway Coordinator, so a fake that embeds it and
// overrides Handle is one too.
var _ Coordinator = turntest.IdleCoordinator{}

// idleCoordinator is turntest.IdleCoordinator with turns that answer nothing.
type idleCoordinator struct{ turntest.IdleCoordinator }

func (idleCoordinator) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{}, nil
}

// A gateway over a coordinator that knows nothing classifies a line by its
// syntax alone and fires a schedule without refusing it.
func TestGatewayOverACoordinatorThatKnowsNothing(t *testing.T) {
	g := New(idleCoordinator{})
	for _, line := range []string{"hello", "!now", "/cancel", "@builder /cancel", "/use builder /cancel", "@builder/cancel", "@builder /every 30m inspect", "/schedules"} {
		_, parsed := turn.ParseAddressedInput(line)
		if got, want := g.immediateInput(line), parsed.Interrupt || parsed.Control(); got != want {
			t.Errorf("%q immediate = %v; want %v", line, got, want)
		}
		if got, want := g.scheduleControl(line), parsed.ScheduleControl(); got != want {
			t.Errorf("%q schedule control = %v; want %v", line, got, want)
		}
	}
	g.BindChannel(&scheduledNotice{id: "notice"})
	if receipt, err := g.FireSchedule(t.Context(), testFire()); err != nil || receipt.MessageID != "notice" {
		t.Fatalf("fire = %+v, %v; want it announced and run", receipt, err)
	}
}

// A gateway without a coordinator has nothing to run its turns, so it is
// refused when it is made rather than on its first line.
func TestNewRefusesANilCoordinator(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a nil coordinator")
		}
	}()
	New(nil)
}

// catalogCoordinator parses a line as a coordinator whose catalog knows
// codex does: "@codex/" followed by a command is that command addressed
// to codex, so "@codex/cancel" and "@codex/schedules" are controls,
// though the syntax alone, which wants a space after the address, reads
// them as prompts. A control is answered at once; any other line is a
// turn that holds until release.
type catalogCoordinator struct {
	turntest.IdleCoordinator
	entered chan turn.Request
	release chan struct{}
}

func (p *catalogCoordinator) ParseInput(line string) (string, turn.ParsedInput) {
	if control, ok := strings.CutPrefix(line, "@codex/"); ok {
		return "@codex", turn.ParseInput("/" + control)
	}
	return turn.ParseAddressedInput(line)
}

func (p *catalogCoordinator) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	p.entered <- req
	if _, parsed := p.ParseInput(req.Input); parsed.Control() {
		return turn.Result{Text: "ok"}, nil
	}
	if req.OnTurnReady != nil {
		req.OnTurnReady("task-"+req.MessageID, "attempt-"+req.MessageID)
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return turn.Result{}, ctx.Err()
	}
	return turn.Result{Text: "complete original result", Attempt: "attempt-" + req.MessageID}, nil
}

// A stop that only the coordinator's catalog recognizes joins the turn its
// conversation is running instead of queueing behind it.
func TestGatewayClassifiesALineAsItsCoordinatorParsesIt(t *testing.T) {
	const stop = "@codex/cancel"
	if _, parsed := turn.ParseAddressedInput(stop); parsed.Interrupt || parsed.Control() {
		t.Fatalf("the syntax alone already reads %q as a stop", stop)
	}
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &catalogCoordinator{entered: make(chan turn.Request, 2), release: make(chan struct{})}
	g := New(p)
	g.BindChannel(&recoveryChannel{})
	g.SetRecoveryLedger(book)
	ctx, cancel := context.WithCancel(t.Context())
	var workers recoveryTestWorkers
	defer func() { cancel(); workers.Wait() }()
	g.SetIngressLifetime(ctx, &workers, nil)
	msg := inboundFixture()
	if err := g.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.entered:
	case <-time.After(waitDeadline):
		t.Fatal("the first turn never started")
	}
	msg.MessageID, msg.Text = "stop", stop
	if err := g.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-p.entered:
		if req.Input != stop {
			t.Fatalf("started %q, want %q", req.Input, stop)
		}
	case <-time.After(waitDeadline):
		t.Fatalf("%q waited behind the running turn", stop)
	}
	close(p.release)
}

// A schedule control that only the coordinator's catalog recognizes leaves
// the agent gate's anchor where it was, in the durable and the in-memory
// path alike.
func TestScheduleControlsTheCoordinatorParsesPreserveChannelAnchor(t *testing.T) {
	const control = "@codex/schedules"
	if _, parsed := turn.ParseAddressedInput(control); parsed.ScheduleControl() {
		t.Fatalf("the syntax alone already reads %q as a schedule control", control)
	}
	for _, path := range []string{"in-memory", "durable"} {
		t.Run(path, func(t *testing.T) {
			p := &catalogCoordinator{entered: make(chan turn.Request, 1)}
			g := New(p)
			g.BindChannel(&reply{text: make(chan string, 4)})
			gate := &recordingGate{calls: make(chan string, 4)}
			g.SetAgentGate(gate)
			msg := feishu.InboundMessage{ConversationID: "chat", ChatID: "chat", MessageID: "control", Text: control, SenderOpenID: "owner"}
			if path == "durable" {
				book, err := ledger.Open(t.TempDir(), ledger.Options{})
				if err != nil {
					t.Fatal(err)
				}
				_, ui, err := g.dispatchInput(t.Context(), book, "schedule-control", gatewayInput{Message: msg}, msg, "", nil)
				if ui != nil {
					ui.closeProgress()
				}
				book.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else if err := g.process(msg); err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.entered:
			default:
				t.Fatalf("%s never reached the coordinator", control)
			}
			select {
			case changed := <-gate.calls:
				t.Fatalf("%s replaced active anchor: %s", control, changed)
			default:
			}
		})
	}
}

// errStaleSchedule is what staleScheduleCoordinator refuses a scheduled run
// with.
var errStaleSchedule = errors.New("scheduled conversation no longer has a project binding")

// staleScheduleCoordinator refuses every scheduled run, as a coordinator
// does once a schedule's conversation, project or requester no longer
// holds, and records what it was asked to check.
type staleScheduleCoordinator struct {
	countingProcessor
	checked []string
}

func (p *staleScheduleCoordinator) ValidateScheduled(_ context.Context, conversation, project, requester string) error {
	p.checked = append(p.checked, conversation, project, requester)
	return errStaleSchedule
}

// A scheduled run its coordinator refuses is neither announced nor run.
func TestFireScheduleRefusesARunItsCoordinatorRefuses(t *testing.T) {
	p := &staleScheduleCoordinator{}
	g := New(p)
	notice := &scheduledNotice{id: "notice"}
	g.BindChannel(notice)
	f := testFire()
	if receipt, err := g.FireSchedule(t.Context(), f); !errors.Is(err, errStaleSchedule) {
		t.Fatalf("fire = %+v, %v; want the coordinator's refusal", receipt, err)
	}
	if want := []string{f.ConversationID, f.ProjectID, f.Requester}; !slices.Equal(p.checked, want) {
		t.Fatalf("checked %q, want %q", p.checked, want)
	}
	if notice.calls != 0 || p.calls.Load() != 0 {
		t.Fatalf("refused run posted %d notices and ran %d turns", notice.calls, p.calls.Load())
	}
}
