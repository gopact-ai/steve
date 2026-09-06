package intent

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func newRecoveryService(t *testing.T) (*Service, *ledger.Ledger) {
	t.Helper()
	l, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return New(l), l
}

func dispatchedIntent(t *testing.T, s *Service) Intent {
	t.Helper()
	it, err := s.Claim(t.Context(), "task", "attempt", "channel_send", []byte(`{"content":"news"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatched(t.Context(), it.ID); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestActiveDispatchedIntentCannotBeRecoveredOrResolved(t *testing.T) {
	s, _ := newRecoveryService(t)
	it := dispatchedIntent(t, s)
	unknown, err := s.Unresolved(t.Context())
	if err != nil || len(unknown) != 0 {
		t.Fatalf("an active request became unknown: %+v, %v", unknown, err)
	}
	if _, err := s.Resolve(t.Context(), it.ID, "new", "owner"); err == nil {
		t.Fatal("an active request was manually resolved")
	}
	_, err = s.Claim(t.Context(), "task", "attempt", "channel_send", []byte(`{"content":"news"}`))
	var blocked Blocked
	if !errors.As(err, &blocked) || blocked.Previous.State != Dispatched {
		t.Fatalf("same-attempt retry must wait for the active operation: %v", err)
	}
	if err := s.Confirmed(t.Context(), it.ID, "receipt"); err != nil {
		t.Fatalf("inspection disturbed the running operation: %v", err)
	}
}

func TestPendingResolutionDoesNotMutateTheLedger(t *testing.T) {
	s, book := newRecoveryService(t)
	it := dispatchedIntent(t, s)
	if pending, err := s.PendingResolution(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("active dispatch presented for resolution: %+v, %v", pending, err)
	}
	restarted := New(book)
	pending, err := restarted.PendingResolution(t.Context())
	if err != nil || len(pending) != 1 || pending[0].ID != it.ID {
		t.Fatalf("abandoned dispatch missing: %+v, %v", pending, err)
	}
	stored, err := restarted.Get(t.Context(), it.ID)
	if err != nil || stored.State != Dispatched {
		t.Fatalf("read query changed state: %+v, %v", stored, err)
	}
	if _, err := restarted.Resolve(t.Context(), it.ID, "new", "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestFailedTerminalRecordingCanBeReconciled(t *testing.T) {
	for _, terminal := range []string{"confirmed", "lost", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			s, _ := newRecoveryService(t)
			it := dispatchedIntent(t, s)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var err error
			switch terminal {
			case "confirmed":
				err = s.Confirmed(ctx, it.ID, "receipt")
			case "lost":
				err = s.Lost(ctx, it.ID, context.DeadlineExceeded)
			case "failed":
				err = s.Failed(ctx, it.ID, errors.New("provider refused"))
			}
			if err == nil {
				t.Fatal("cancelled terminal write unexpectedly succeeded")
			}
			for range 2 {
				unknown, err := s.Unresolved(t.Context())
				if err != nil || len(unknown) != 1 || unknown[0].ID != it.ID || unknown[0].State != Unknown {
					t.Fatalf("lost terminal write is not recoverable: %+v, %v", unknown, err)
				}
			}
			resolved, err := s.Resolve(t.Context(), it.ID, "new", "owner")
			if err != nil || resolved.State != Failed {
				t.Fatalf("reconciliation failed: %+v, %v", resolved, err)
			}
			if _, err := s.Claim(t.Context(), "task", "attempt", "channel_send", []byte(`{"content":"news"}`)); err != nil {
				t.Fatalf("reconciled operation remains blocked: %v", err)
			}
		})
	}
}

func TestRestartedServiceRecoversDispatchBeforeAllowingRetry(t *testing.T) {
	for _, entry := range []string{"list", "retry", "resolve"} {
		t.Run(entry, func(t *testing.T) {
			previous, book := newRecoveryService(t)
			it := dispatchedIntent(t, previous)
			// The restarted process has the ledger but no running request.
			restarted := New(book)
			switch entry {
			case "list":
				unknown, err := restarted.Unresolved(t.Context())
				if err != nil || len(unknown) != 1 || unknown[0].ID != it.ID {
					t.Fatalf("restart did not expose unresolved dispatch: %+v, %v", unknown, err)
				}
			case "retry":
				_, err := restarted.Claim(t.Context(), "task", "attempt", "channel_send", []byte(`{"content":"news"}`))
				var blocked Blocked
				if !errors.As(err, &blocked) || blocked.Previous.State != Unknown {
					t.Fatalf("restart retry was not blocked by a resolvable intent: %v", err)
				}
			}
			resolved, err := restarted.Resolve(t.Context(), it.ID, "happened", "owner")
			if err != nil || resolved.State != Succeeded {
				t.Fatalf("abandoned dispatch cannot be resolved: %+v, %v", resolved, err)
			}
			unknown, err := restarted.Unresolved(t.Context())
			if err != nil || len(unknown) != 0 {
				t.Fatalf("resolved dispatch was recovered a second time: %+v, %v", unknown, err)
			}
		})
	}
}
