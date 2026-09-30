package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/procgroup"
)

func TestNodeKillOnLoadedRecordUsesOriginalIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		cause      error
	}{{"confirmed", "", nil}, {"running", "stop_running", procgroup.ErrRunning}, {"unproven", "stop_unproven", procgroup.ErrUnproven}, {"unsupported", "stop_unsupported", procgroup.ErrUnsupported}} {
		t.Run(tc.name, func(t *testing.T) {
			store, record := recordsFixture(t)
			before := record
			record.State.State = nodewire.SessionInterrupted
			record.State.Sequence++
			record.Process = sessionProcess{Identity: procgroup.Identity{Group: 123, Leader: 123, Start: 42, Mark: "original"}, Place: procgroup.Place{Boot: "boot"}}
			if err := store.save(before, record); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s := &SessionService{server: NewServer(ServerConfig{Name: "worker"}), records: store, placeKnown: true, place: record.Process.Place, unverifiedProcesses: map[string]bool{record.State.ID: true}, sessions: map[string]*ownedSession{}}
			s.settle = func(id procgroup.Identity, ran, here procgroup.Place, within time.Duration) error {
				calls++
				if id != record.Process.Identity || ran != record.Process.Place || here != record.Process.Place || within > recordedGroupWithin {
					t.Error("kill changed recorded process identity or deadline")
				}
				return tc.cause
			}
			req := nodeSessionRequest("kill")
			req.ID = record.State.ID
			state, err := s.closedState(req)
			if tc.code == "" {
				if err != nil || !state.ProcessStopped {
					t.Fatalf("kill = %+v %v", state, err)
				}
			} else {
				var e *SessionError
				if !errors.As(err, &e) || e.Code != tc.code || state.ProcessStopped {
					t.Fatalf("kill = %+v %v, want %s", state, err, tc.code)
				}
			}
			if calls != 1 {
				t.Fatalf("recorded kill calls=%d", calls)
			}
		})
	}
}
func TestNodeKillRejectsAnotherExecutionBeforeSignalling(t *testing.T) {
	store, record := recordsFixture(t)
	before := record
	record.State.State = nodewire.SessionInterrupted
	record.State.Sequence++
	if err := store.save(before, record); err != nil {
		t.Fatal(err)
	}
	s := &SessionService{server: NewServer(ServerConfig{Name: "worker"}), records: store, unverifiedProcesses: map[string]bool{record.State.ID: true}, placeKnown: true, settle: func(procgroup.Identity, procgroup.Place, procgroup.Place, time.Duration) error {
		t.Fatal("signalled another binding")
		return nil
	}}
	req := nodeSessionRequest("kill")
	req.ID = record.State.ID
	req.Binding.AttemptID = "new"
	if _, err := s.closedState(req); err == nil {
		t.Fatal("another binding authorized kill")
	}
}
func TestNodeAbortClassificationRemainsUncertain(t *testing.T) {
	store, record := recordsFixture(t)
	before := record
	record.State.State = nodewire.SessionInterrupted
	record.State.Sequence++
	if err := store.save(before, record); err != nil {
		t.Fatal(err)
	}
	s := &SessionService{server: NewServer(ServerConfig{Name: "worker"}), records: store, unverifiedProcesses: map[string]bool{record.State.ID: true}, placeKnown: true, settle: func(procgroup.Identity, procgroup.Place, procgroup.Place, time.Duration) error {
		return procgroup.ErrRunning
	}}
	req := nodeSessionRequest("abort")
	req.ID = record.State.ID
	_, err := s.closedState(req)
	var e *SessionError
	if !errors.As(err, &e) || e.Code != "uncertain" {
		t.Fatalf("abort error changed: %v", err)
	}
}

type unsupportedGroupTransport struct{ acphost.LocalTransport }

func (t unsupportedGroupTransport) Start(ctx context.Context) (acphost.Process, error) {
	p, err := t.LocalTransport.Start(ctx)
	if err != nil {
		return nil, err
	}
	return unsupportedGroupProcess{p}, nil
}

type unsupportedGroupProcess struct{ acphost.Process }

func (p unsupportedGroupProcess) KillNow(ctx context.Context) error {
	p.Process.Kill()
	for !p.Process.Stopped() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return procgroup.ErrUnsupported
}
func TestLiveKillPreservesUnsupportedGroupClassification(t *testing.T) {
	h := acphost.New(acphost.Config{Transport: unsupportedGroupTransport{acphost.LocalTransport{Command: buildMockAgent(t), ProcessDir: t.TempDir()}}})
	defer h.Close()
	sid, generation, err := h.OpenSession(t.Context(), "", acphost.SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	store, record := recordsFixture(t)
	before := record
	record.Generation = generation
	record.UpstreamID = string(sid)
	record.State.Sequence++
	if err := store.save(before, record); err != nil {
		t.Fatal(err)
	}
	s := &SessionService{server: NewServer(ServerConfig{Name: "worker"}), records: store, sessions: map[string]*ownedSession{}}
	one := &ownedSession{service: s, record: record, host: h, changed: make(chan struct{})}
	req := nodeSessionRequest("kill")
	req.ID = record.State.ID
	_, err = one.kill(t.Context(), req)
	var failure *SessionError
	if !errors.As(err, &failure) || failure.Code != "stop_unsupported" {
		t.Fatalf("unsupported group got silently confirmed: %v", err)
	}
}
