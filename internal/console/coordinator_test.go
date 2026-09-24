package console

import (
	"reflect"
	"testing"

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
