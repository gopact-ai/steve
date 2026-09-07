package coordination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// durableCounter exercises replay against independently durable application
// state. Its command receipts and counter share one atomic filesystem write.
type durableCounter struct {
	mu      sync.Mutex
	path    string
	count   int
	results map[string][]byte
	fail    bool
}

type counterSnapshot struct {
	Count   int               `json:"count"`
	Results map[string][]byte `json:"results"`
}

func openCounter(t *testing.T, dir string) *durableCounter {
	t.Helper()
	c := &durableCounter{path: filepath.Join(dir, "counter.json"), results: map[string][]byte{}}
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return c
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(data); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *durableCounter) Apply(command AppliedCommand) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return nil, errors.New("disk unavailable")
	}
	if result, ok := c.results[command.ID]; ok {
		return bytes.Clone(result), nil
	}
	var increment int
	if err := json.Unmarshal(command.Payload, &increment); err != nil {
		return nil, err
	}
	c.count += increment
	result := []byte(fmt.Sprint(c.count))
	c.results[command.ID] = result
	if err := c.persist(); err != nil {
		return nil, err
	}
	return bytes.Clone(result), nil
}

func (c *durableCounter) Snapshot() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return json.Marshal(counterSnapshot{Count: c.count, Results: c.results})
}

func (c *durableCounter) Restore(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var state counterSnapshot
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	c.count = state.Count
	c.results = state.Results
	return c.persist()
}

func (c *durableCounter) persist() error {
	data, err := json.Marshal(counterSnapshot{Count: c.count, Results: c.results})
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(c.path+".tmp", c.path)
}

func (c *durableCounter) value() int { c.mu.Lock(); defer c.mu.Unlock(); return c.count }

func TestApplicationReplayAndSnapshotKeepWritesExactlyOnce(t *testing.T) {
	c := newTestCluster(t, 1, func(_ string, dir string) Application { return openCounter(t, dir) })
	n := c.leader()
	request := AppCommand{WriterGeneration: 1, ID: "app-first", CallerNodeID: "node-1", CoordinatorEpoch: 1, ExpectedVersion: 0, Payload: []byte("7")}
	first, err := n.ApplyApp(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Data) != "7" || first.AppVersion != 1 {
		t.Fatalf("first result: %+v", first)
	}
	if _, err = n.ApplyApp(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	_, err = n.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "app-conflict", CallerNodeID: "node-1", CoordinatorEpoch: 1, ExpectedVersion: 0, Payload: []byte("100")})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale app version accepted: %v", err)
	}
	_, err = n.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "app-wrong-epoch", CallerNodeID: "node-1", CoordinatorEpoch: 2, ExpectedVersion: 1, Payload: []byte("100")})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale epoch accepted: %v", err)
	}
	c.stop("node-1")
	restart := func() {
		config := c.configs["node-1"]
		config.Application = openCounter(t, config.DataDir)
		var err error
		n, err = Open(config)
		if err != nil {
			t.Fatal(err)
		}
		c.configs["node-1"] = config
		c.mu.Lock()
		c.nodes["node-1"] = n
		c.mu.Unlock()
		n = c.leader()
		if _, err := n.ReadState(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	restart()
	if value := c.configs["node-1"].Application.(*durableCounter).value(); value != 7 {
		t.Fatalf("log replay repeated write: %d", value)
	}
	if n.Status().AppVersion != 1 {
		t.Fatal("log replay lost app version")
	}
	if err := n.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := n.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "app-second", CallerNodeID: "node-1", CoordinatorEpoch: 1, ExpectedVersion: 1, Payload: []byte("5")})
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Data) != "12" {
		t.Fatalf("second result: %+v", second)
	}
	c.stop("node-1")
	restart()
	if value := c.configs["node-1"].Application.(*durableCounter).value(); value != 12 {
		t.Fatalf("snapshot plus log replay lost or repeated writes: %d", value)
	}
	retried, err := n.ApplyApp(context.Background(), request)
	if err != nil || string(retried.Data) != "7" || retried.Index != first.Index {
		t.Fatalf("snapshot lost original command result: %+v %v", retried, err)
	}
}

func TestApplicationStateSurvivesCoordinatorFailureAndFencesOldWriter(t *testing.T) {
	c := newTestCluster(t, 3, func(_ string, dir string) Application { return openCounter(t, dir) })
	n := c.leader()
	_, err := n.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "before-failure", CallerNodeID: "node-1", CoordinatorEpoch: 1, Payload: []byte("9")})
	if err != nil {
		t.Fatal(err)
	}
	c.stop("node-1")
	n = c.leader()
	state, err := n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.AppVersion != 1 {
		t.Fatal("majority lost committed application write")
	}
	_, err = n.Transfer(context.Background(), TransferRequest{ID: "resume", Actor: "user", ExpectedEpoch: 1, TargetNodeID: n.Status().NodeID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = n.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "old-writer", CallerNodeID: "node-1", CoordinatorEpoch: 1, ExpectedVersion: 1, Payload: []byte("100")})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("old writer was not fenced: %v", err)
	}
	result, err := n.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "new-writer", CallerNodeID: n.Status().NodeID, CoordinatorEpoch: 2, ExpectedVersion: 1, Payload: []byte("3")})
	if err != nil || string(result.Data) != "12" {
		t.Fatalf("new coordinator could not resume: %+v %v", result, err)
	}
}

func TestApplicationDiskFailureStopsReplica(t *testing.T) {
	c := newTestCluster(t, 1, func(_ string, dir string) Application { return openCounter(t, dir) })
	n := c.leader()
	app := c.configs["node-1"].Application.(*durableCounter)
	app.mu.Lock()
	app.fail = true
	app.mu.Unlock()
	_, err := n.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "disk-failure", CallerNodeID: "node-1", CoordinatorEpoch: 1, Payload: []byte("1")})
	if !errors.Is(err, ErrApplication) {
		t.Fatalf("application failure was hidden: %v", err)
	}
	eventually(t, time.Second, func() bool { return !n.Status().Healthy && !n.Status().IsLeader })
	if n.Status().AppVersion != 0 {
		t.Fatal("failed application write advanced version")
	}
}

func TestCallerMustHoldCurrentCoordinatorRole(t *testing.T) {
	c := newTestCluster(t, 2, func(_ string, dir string) Application { return openCounter(t, dir) })
	n := c.leader()
	_, err := n.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "wrong-caller", CallerNodeID: "node-2", CoordinatorEpoch: 1, Payload: []byte("1")})
	if !errors.Is(err, ErrNotCoordinator) {
		t.Fatalf("non-coordinator caller was accepted: %v", err)
	}
}

func TestJoinInstallsApplicationDataThatPredatesFirstReplicatedCommand(t *testing.T) {
	c := newTestCluster(t, 2, func(id, dir string) Application {
		app := openCounter(t, dir)
		if id == "node-1" {
			app.count = 41
			if err := app.persist(); err != nil {
				t.Fatal(err)
			}
		}
		return app
	})
	if c.leader().Status().AppVersion != 0 {
		t.Fatal("test baseline unexpectedly has replicated writes")
	}
	if value := c.configs["node-2"].Application.(*durableCounter).value(); value != 41 {
		t.Fatalf("new member missed application baseline: %d", value)
	}
}
