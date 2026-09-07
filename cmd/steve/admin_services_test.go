package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestHubRestartReceiptPrecedesStopAndSurvivesReadiness(t *testing.T) {
	var stopped atomic.Int32
	var sealed atomic.Bool
	doc := &ledger.FileDocument{Path: filepath.Join(t.TempDir(), "restart.json")}
	admin := &fleetAdmin{}
	gate := func() (func(), error) {
		if !sealed.CompareAndSwap(false, true) {
			return nil, errors.New("already sealed")
		}
		return func() { sealed.Store(false) }, nil
	}
	first, err := newHubServices(admin, execution.New(context.Background(), nil), gate, func() { stopped.Add(1) }, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ready(); err != nil {
		t.Fatal(err)
	}
	req := consoleapi.RestartRequest{CommandID: "one"}
	accepted, err := first.Restart(t.Context(), "hub", req)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != "accepted" || !sealed.Load() || stopped.Load() != 0 {
		t.Fatalf("accept=%+v stop=%d", accepted, stopped.Load())
	}
	if _, err := first.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "two"}); err == nil {
		t.Fatal("concurrent restart accepted")
	}
	first.RestartAccepted("hub", req.CommandID)
	first.RestartAccepted("hub", req.CommandID)
	if stopped.Load() != 1 {
		t.Fatal("duplicate stop")
	}
	next, err := newHubServices(admin, execution.New(context.Background(), nil), gate, func() { stopped.Add(1) }, doc)
	if err != nil {
		t.Fatal(err)
	}
	beforeReady, _ := next.RestartStatus(t.Context(), "hub", req.CommandID)
	if beforeReady.State != "accepted" {
		t.Fatal("reported success before readiness")
	}
	if err := next.ready(); err != nil {
		t.Fatal(err)
	}
	replay, err := next.Restart(t.Context(), "hub", req)
	if err != nil {
		t.Fatal(err)
	}
	if replay.State != "restarted" || replay.Incarnation == accepted.Incarnation || replay.PreviousIncarnation != accepted.Incarnation {
		t.Fatalf("replay=%+v", replay)
	}
	next.RestartAccepted("hub", req.CommandID)
	if stopped.Load() != 1 {
		t.Fatal("replay repeated a restart")
	}
}
func TestHubRestartRefusesBusyWithoutClosingAdmission(t *testing.T) {
	r := execution.New(context.Background(), nil)
	running, err := r.Begin(t.Context(), execution.Key{})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Finish(nil)
	var sealed atomic.Bool
	s, err := newHubServices(&fleetAdmin{}, r, func() (func(), error) { sealed.Store(true); return func() { sealed.Store(false) }, nil }, func() { t.Error("stopped busy service") }, &ledger.FileDocument{Path: filepath.Join(t.TempDir(), "restart.json")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "busy"})
	var failure *consoleapi.ServiceError
	if !errors.As(err, &failure) || failure.Code != "busy" || sealed.Load() {
		t.Fatalf("error=%v sealed=%v", err, sealed.Load())
	}
	if _, err = s.RestartStatus(t.Context(), "hub", "busy"); err == nil {
		t.Fatal("busy request became accepted")
	}
}
