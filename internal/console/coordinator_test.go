package console

import (
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

// A coordinator that knows nothing leaves the console with nothing to say
// about a conversation: no project or agents, no setup, no suggestions or
// verbs, a line parsed by its syntax alone and no conversation started.
func TestConsoleOverACoordinatorThatKnowsNothing(t *testing.T) {
	s := New(turntest.IdleCoordinator{}, "owner", nil)
	ctx := t.Context()
	want := consoleapi.Context{Conversation: "console:c", Agents: []consoleapi.AgentChoice{}}
	if got, err := s.Context(ctx, "c"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("context = %#v, %v; want %#v", got, err, want)
	}
	if got, err := s.Setup(ctx, "c", "builder"); err == nil {
		t.Fatalf("setup = %#v without a coordinator that knows it", got)
	}
	if got := s.Suggest(ctx, "c", "/"); len(got) != 0 {
		t.Fatalf("suggestions = %#v", got)
	}
	if got := s.VerbsFor(ctx); got != nil {
		t.Fatalf("verbs = %#v, want nil", got)
	}
	for _, line := range []string{"@builder fix it", "/use builder fix it", "!stop now"} {
		target, parsed := s.parseInput(line)
		wantTarget, wantParsed := turn.ParseAddressedInput(line)
		if target != wantTarget || !reflect.DeepEqual(parsed, wantParsed) {
			t.Fatalf("parse %q = %q %#v; want %q %#v", line, target, parsed, wantTarget, wantParsed)
		}
	}
	if err := s.InitializeConversation(ctx, "new", "p"); err == nil || len(s.Conversations()) != 0 {
		t.Fatalf("initialization = %v, conversations %v", err, s.Conversations())
	}
}

// A console without a coordinator has nothing to run its turns, so it is
// refused when it is made rather than on its first line.
func TestNewRefusesANilCoordinator(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a nil coordinator")
		}
	}()
	New(nil, "owner", nil)
}

// catalogCoordinator parses a line as a coordinator whose catalog knows
// codex does: "@codex/cancel" is a stop addressed to codex, though the
// syntax alone, which wants a space after the address, reads a prompt.
type catalogCoordinator struct{ queueHandler }

func (c *catalogCoordinator) ParseInput(line string) (string, turn.ParsedInput) {
	if line == "@codex/cancel" {
		return "@codex", turn.ParseInput("/cancel")
	}
	return c.queueHandler.ParseInput(line)
}

// The console sorts a line by how its coordinator parses it. A stop that
// only the coordinator's catalog recognizes runs beside the turn in
// progress instead of queueing behind it, and like any stop it is
// recorded but not drawn.
func TestConsoleClassifiesALineAsItsCoordinatorParsesIt(t *testing.T) {
	const stop = "@codex/cancel"
	if _, parsed := turn.ParseAddressedInput(stop); parsed.Interrupt || parsed.Control() {
		t.Fatalf("the syntax alone already reads %q as a stop", stop)
	}
	c := &catalogCoordinator{queueHandler{started: make(chan *queueCall, 2)}}
	s := New(c, "owner", nil)
	work := enqueueForTest(t, s, "main", "block")
	running := nextCall(t, &c.queueHandler)
	control := enqueueForTest(t, s, "main", stop)
	select {
	case call := <-c.started:
		if call.req.Input != stop {
			t.Fatalf("started %q, want %q", call.req.Input, stop)
		}
		call.finish <- nil
	case <-time.After(2 * time.Second):
		t.Fatalf("%q waited behind the running turn", stop)
	}
	awaitExchange(t, s, control.ID)
	running.finish <- nil
	awaitExchange(t, s, work.ID)
	hidden := 0
	for _, r := range s.Replies("main") {
		if r.ExchangeID != control.ID {
			continue
		}
		if !r.Silent {
			t.Fatalf("drew a line of the stop: %+v", r)
		}
		hidden++
	}
	if hidden != 2 {
		t.Fatalf("recorded %d lines of the stop, want it and its receipt", hidden)
	}
}
