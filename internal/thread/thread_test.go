package thread

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/feed"
	"github.com/gopact-ai/steve/internal/ledger"
)

func open(t *testing.T) (*Service, *feed.Feed, *ledger.Ledger) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	f := feed.New("e", 0)
	return New(book, f), f, book
}

func TestANonceIsAcceptedOnce(t *testing.T) {
	s, f, _ := open(t)
	var heard []Message
	s.OnPosted(func(m Message) { heard = append(heard, m) })
	for range 3 {
		m, _, err := s.Post(t.Context(), Message{Role: RoleOwner, Text: " fix issue 7 ", ClientNonce: "n1"})
		if err != nil || m.ID == "" || m.Text != "fix issue 7" || m.Thread != Main {
			t.Fatalf("post = %+v %v", m, err)
		}
	}
	if len(heard) != 1 {
		t.Fatalf("listeners told %d times", len(heard))
	}
	events, _ := f.After(feed.Cursor{Epoch: "e"})
	if len(events) != 1 || events[0].Type != EventMessage {
		t.Fatalf("events = %+v", events)
	}
	if m, found, err := s.ByNonce(t.Context(), "", "n1"); err != nil || !found || m.ID != heard[0].ID {
		t.Fatalf("by nonce = %+v %v %v", m, found, err)
	}
	if _, found, _ := s.ByNonce(t.Context(), "", "n2"); found {
		t.Fatal("unknown nonce found")
	}
}

func TestRecentIsTheTailInOrderAndSurvivesReopening(t *testing.T) {
	s, _, book := open(t)
	for i, text := range []string{"a", "b", "c", "d"} {
		role := RoleOwner
		if i%2 == 1 {
			role = RoleSteve
		}
		if _, _, err := s.Post(t.Context(), Message{Role: role, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	s.Post(t.Context(), Message{Thread: "task-3", Role: RoleOwner, Text: "elsewhere"})
	again := New(book, nil)
	got, err := again.Recent(t.Context(), Main, 3)
	if err != nil || len(got) != 3 || got[0].Text != "b" || got[2].Text != "d" || got[1].Role != RoleOwner {
		t.Fatalf("recent = %+v %v", got, err)
	}
}

func TestPostRefusesWhatIsNotAMessage(t *testing.T) {
	s, _, _ := open(t)
	for _, m := range []Message{{Role: RoleOwner}, {Role: "admin", Text: "x"}} {
		if _, _, err := s.Post(t.Context(), m); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v accepted: %v", m, err)
		}
	}
}
