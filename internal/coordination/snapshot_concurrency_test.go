package coordination

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// slowCheckpointApplication counts applied commands. A checkpoint holds the
// count when the snapshot is taken; producing its bytes waits until the
// test lets it go.
type slowCheckpointApplication struct {
	calls   int
	release chan struct{}
}

func (a *slowCheckpointApplication) Apply(AppliedCommand) ([]byte, error) {
	a.calls++
	return json.Marshal(a.calls)
}
func (a *slowCheckpointApplication) Snapshot() ([]byte, error) { return json.Marshal(a.calls) }
func (a *slowCheckpointApplication) Restore(raw []byte) error  { return json.Unmarshal(raw, &a.calls) }
func (a *slowCheckpointApplication) SnapshotCheckpoint(uint64) (Checkpoint, error) {
	return slowCheckpoint{boundary: a.calls, release: a.release}, nil
}

type slowCheckpoint struct {
	boundary int
	release  chan struct{}
}

func (c slowCheckpoint) Encode() ([]byte, error) {
	<-c.release
	return json.Marshal(c.boundary)
}
func (slowCheckpoint) Persisted() error { return nil }
func (slowCheckpoint) Release()         {}

// Taking a snapshot only fixes its boundary. The application's bytes are
// produced while the snapshot is persisted, commands keep applying
// meanwhile, and the snapshot holds the state as of its boundary.
func TestSnapshotPersistRunsBesideApply(t *testing.T) {
	app := &slowCheckpointApplication{release: make(chan struct{})}
	m := newMachine("retention", app)
	m.state.Coordinator = Assignment{NodeID: "node", Epoch: 1}
	m.state.WriterGeneration = 1
	released := false
	release := func() {
		if !released {
			released = true
			close(app.release)
		}
	}
	t.Cleanup(release)
	for version, id := range []string{"one", "two"} {
		if r := applyReceiptCommand(t, m, id, uint64(version)); r.err() != nil {
			t.Fatal(r.err())
		}
	}
	boundary := m.state.AppliedIndex

	type taken struct {
		snapshot raft.FSMSnapshot
		err      error
	}
	snapshots := make(chan taken, 1)
	go func() {
		s, err := m.Snapshot()
		snapshots <- taken{s, err}
	}()
	var snapshot raft.FSMSnapshot
	select {
	case got := <-snapshots:
		if got.err != nil {
			t.Fatal(got.err)
		}
		snapshot = got.snapshot
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("taking a snapshot waited for the application's bytes")
	}
	defer snapshot.Release()

	sink := &snapshotMemorySink{}
	persisted := make(chan error, 1)
	go func() { persisted <- snapshot.Persist(sink) }()
	applied := make(chan receipt, 1)
	go func() { applied <- applyReceiptCommand(t, m, "three", 2) }()
	select {
	case r := <-applied:
		if r.err() != nil {
			t.Fatal(r.err())
		}
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("a command waited for the snapshot being persisted")
	}
	select {
	case err := <-persisted:
		t.Fatalf("the snapshot was persisted before its application bytes were produced: %v", err)
	default:
	}
	release()
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}

	metadata, application, err := decodeSnapshot(bytes.NewReader(sink.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var data snapshotData
	if err := json.Unmarshal(metadata, &data); err != nil {
		t.Fatal(err)
	}
	if data.State.AppliedIndex != boundary || string(application) != "2" {
		t.Fatalf("snapshot holds index %d with application %s, want index %d with 2", data.State.AppliedIndex, application, boundary)
	}
	if _, ok := data.Receipts["app/2/three"]; ok || len(data.Receipts) != 2 {
		t.Fatal("the snapshot holds a receipt applied after its boundary")
	}
}

type failingCheckpoint struct{ released *bool }

func (failingCheckpoint) Encode() ([]byte, error) { return nil, errors.New("copy refused") }
func (failingCheckpoint) Persisted() error        { return errors.New("persisted without an encoding") }
func (c failingCheckpoint) Release()              { *c.released = true }

// A checkpoint that cannot be encoded fails its snapshot before anything
// reaches the sink, and the boundary is let go with the snapshot.
func TestSnapshotCheckpointEncodingFailureCancelsTheSink(t *testing.T) {
	var released bool
	snapshot := &encodedSnapshot{metadata: []byte(`{"format":3}`), checkpoint: failingCheckpoint{&released}, persisted: func() error {
		return errors.New("persisted without an encoding")
	}}
	sink := &snapshotMemorySink{}
	if err := snapshot.Persist(sink); err == nil || !sink.canceled || sink.closed || sink.Len() != 0 {
		t.Fatalf("persist = %v, canceled=%v closed=%v wrote=%d", err, sink.canceled, sink.closed, sink.Len())
	}
	snapshot.Release()
	if !released {
		t.Fatal("the checkpoint's boundary was kept after its snapshot was released")
	}
}
