package coordination

import (
	"errors"
	"testing"
)

func TestWriterGenerationRejectsLateCommandsFromEarlierActivation(t *testing.T) {
	cluster := newTestCluster(t, 1, func(_ string, dir string) Application { return openCounter(t, dir) })
	node := cluster.leader()
	state, err := node.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request := WriterRequest{ID: "first-writer", CallerNodeID: "node-1", CoordinatorEpoch: state.Coordinator.Epoch, ExpectedGeneration: state.WriterGeneration}
	first, err := node.BeginWriter(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	late := AppCommand{ID: "late-command", CallerNodeID: "node-1", CoordinatorEpoch: state.Coordinator.Epoch, ExpectedVersion: state.AppVersion, WriterGeneration: first.WriterGeneration, Payload: []byte("9")}
	second, err := node.BeginWriter(t.Context(), WriterRequest{ID: "second-writer", CallerNodeID: "node-1", CoordinatorEpoch: state.Coordinator.Epoch, ExpectedGeneration: first.WriterGeneration})
	if err != nil || second.WriterGeneration != first.WriterGeneration+1 {
		t.Fatalf("writer generation did not advance: %+v %v", second, err)
	}
	if _, err := node.ApplyApp(t.Context(), late); !errors.Is(err, ErrStaleWriter) {
		t.Fatalf("late prior-generation command accepted: %v", err)
	}
	late.ID, late.WriterGeneration = "current-command", second.WriterGeneration
	if _, err := node.ApplyApp(t.Context(), late); err != nil {
		t.Fatalf("new writer cannot commit: %v", err)
	}
	if _, err := node.BeginWriter(t.Context(), WriterRequest{ID: "stale-cas", CallerNodeID: "node-1", CoordinatorEpoch: state.Coordinator.Epoch, ExpectedGeneration: first.WriterGeneration}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale writer-generation CAS accepted: %v", err)
	}
	replay, err := node.BeginWriter(t.Context(), request)
	if err != nil || replay.WriterGeneration != first.WriterGeneration || node.Status().WriterGeneration != second.WriterGeneration {
		t.Fatalf("retried writer begin changed the current fence: %+v %v", replay, err)
	}
	if err := node.Snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	cluster.stop("node-1")
	config := cluster.configs["node-1"]
	config.Application = openCounter(t, config.DataDir)
	node, err = Open(config)
	if err != nil {
		t.Fatal(err)
	}
	cluster.mu.Lock()
	cluster.nodes["node-1"] = node
	cluster.mu.Unlock()
	node = cluster.leader()
	if node.Status().WriterGeneration != second.WriterGeneration {
		t.Fatal("snapshot/restart lost the committed writer fence")
	}
	late.ID, late.WriterGeneration = "late-after-restart", first.WriterGeneration
	if _, err := node.ApplyApp(t.Context(), late); !errors.Is(err, ErrStaleWriter) {
		t.Fatalf("restart admitted a prior-generation request: %v", err)
	}
}

func TestOnlyCurrentCoordinatorCanBeginAWriter(t *testing.T) {
	cluster := newTLSTestCluster(t, 2)
	client, err := NewClient(ClientConfig{TLS: cluster.identities["node-1"], Members: []Member{cluster.members["node-1"]}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	state, err := client.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Writer acquisition authenticates the node role, independently of owner
	// administrative authorization used for changing membership or policy.
	result, err := client.BeginWriter(t.Context(), WriterRequest{ID: "current-writer", CallerNodeID: "node-1", CoordinatorEpoch: state.Coordinator.Epoch, ExpectedGeneration: state.WriterGeneration})
	if err != nil || result.WriterGeneration != state.WriterGeneration+1 {
		t.Fatalf("authenticated coordinator cannot begin writer: %+v %v", result, err)
	}
	if _, err := cluster.clients["node-2"].BeginWriter(t.Context(), WriterRequest{ID: "wrong-node", CallerNodeID: "node-2", CoordinatorEpoch: state.Coordinator.Epoch, ExpectedGeneration: result.WriterGeneration}); !errors.Is(err, ErrNotCoordinator) {
		t.Fatalf("non-coordinator obtained writer permission: %v", err)
	}
}
