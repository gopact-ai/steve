package cluster

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/hashicorp/raft"
)

func TestReplicaReceiptCheckpointSurvivesRaftRestartAndFurtherWrites(t *testing.T) {
	config := raft.DefaultConfig()
	config.HeartbeatTimeout = 150 * time.Millisecond
	config.ElectionTimeout = 150 * time.Millisecond
	config.LeaderLeaseTimeout = 75 * time.Millisecond
	config.CommitTimeout = 5 * time.Millisecond
	config.SnapshotInterval = time.Hour
	dir := t.TempDir()
	options := Config{
		LedgerDir: filepath.Join(dir, "ledger"), PollInterval: 100 * time.Millisecond,
		Coordination: coordination.Config{
			ClusterID: "retention", NodeID: "node", FailureDomain: "node", StorageLevel: "restricted",
			DataDir: filepath.Join(dir, "raft"), Bootstrap: true, BindAddress: "127.0.0.1:0",
			RaftConfig: config,
		},
	}
	runtime, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	options.Coordination.BindAddress = runtime.Status().Address
	active := ready(t, runtime)
	const writes = coordination.ApplicationReceiptWindow + 256
	for i := uint64(0); i < writes; i++ {
		if err := active.Ledger.PutBinding(t.Context(), "receipt-test", "value", i); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if (i+1)%1024 == 0 {
			t.Logf("durably replicated %d application writes", i+1)
		}
	}
	before, err := runtime.ReadState(t.Context())
	if err != nil || before.AppReplayFloor != 256 {
		t.Fatalf("logical replay floor: %+v %v", before, err)
	}
	if err := runtime.service.Snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	var floor, count uint64
	if err := runtime.book.DB().QueryRow(`SELECT replay_floor FROM replica_state WHERE singleton=1`).Scan(&floor); err != nil {
		t.Fatal(err)
	}
	if err := runtime.book.DB().QueryRow(`SELECT COUNT(*) FROM replica_commands`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if floor != 256 || count != coordination.ApplicationReceiptWindow {
		t.Fatalf("successful Raft snapshot did not compact physical receipts: floor=%d count=%d", floor, count)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = Open(options)
	if err != nil {
		t.Fatal(err)
	}
	active = ready(t, runtime)
	var value uint64
	if ok, err := active.Ledger.GetBinding(t.Context(), "receipt-test", "value", &value); err != nil || !ok || value != writes-1 {
		t.Fatalf("restart lost application facts: %d %v %v", value, ok, err)
	}
	state, err := runtime.ReadState(t.Context())
	if err != nil || state.AppVersion != before.AppVersion || state.AppReplayFloor != 256 {
		t.Fatalf("restart lost replay boundary: %+v %v", state, err)
	}
	if _, err := runtime.service.ApplyApp(t.Context(), coordination.AppCommand{
		ID: "expired", CallerNodeID: "node", CoordinatorEpoch: state.Coordinator.Epoch,
		WriterGeneration: state.WriterGeneration, ExpectedVersion: 0, Payload: []byte("must not apply"),
	}); !errors.Is(err, coordination.ErrReceiptExpired) {
		t.Fatalf("expired replay reached application or lost classification: %v", err)
	}
	if err := active.Ledger.PutBinding(t.Context(), "receipt-test", "value", writes); err != nil {
		t.Fatalf("new generation cannot write after checkpoint restore: %v", err)
	}
}
