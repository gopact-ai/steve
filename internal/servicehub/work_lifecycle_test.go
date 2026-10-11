package servicehub_test

import (
	"errors"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/work"
	"testing"
	"time"
)

func TestCompletedWorkFreesCapacityWithoutChangingItsRetryIdentity(t *testing.T) {
	f := setup(t, "", nil)
	_, p := enroll(t, f, identity.Grant{Key: "capacity-provider", Name: "Provider", Kind: "provider", Work: map[string]string{"console:main": "scratch"}})
	f.server.Work.Limits = durable.Limits{Total: 1, PerSource: 1}
	first := work.Request{Key: "message-1", Conversation: "console:main", Project: "scratch", Instruction: "Process message one"}
	original, err := f.server.Work.Submit(t.Context(), p, first)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Key = "message-2"
	second.Instruction = "Process message two"
	if _, err := f.server.Work.Submit(t.Context(), p, second); !errors.Is(err, durable.ErrCapacity) {
		t.Fatalf("unfinished work did not impose backpressure: %v", err)
	}
	if again, err := f.server.Work.Submit(t.Context(), p, first); err != nil || again.ID != original.ID {
		t.Fatalf("full queue rejected an already accepted retry: %v", err)
	}
	if err := f.server.Work.Dispatch(t.Context(), original.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, result, err := f.server.Work.Status(t.Context(), p, original.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.State == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("original exchange did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	next, err := f.server.Work.Submit(t.Context(), p, second)
	if err != nil {
		t.Fatalf("completed work permanently consumed capacity: %v", err)
	}
	again, result, err := f.server.Work.Status(t.Context(), p, original.ID)
	if err != nil || again.Outcome == nil || result.Exchange == "" || result.State != "done" {
		t.Fatalf("original result was not retained: %+v %v", again, err)
	}
	replay, err := f.server.Work.Submit(t.Context(), p, first)
	if err != nil || replay.ID != original.ID || next.ID == original.ID || f.runner.calls.Load() != 1 {
		t.Fatalf("retry repeated or changed original work: %+v %v", replay, err)
	}
}
