package coordination

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/hashicorp/raft"
)

type retentionApplication struct {
	calls, checkpoints, persisted int
	floor                         uint64
}

func (a *retentionApplication) Apply(AppliedCommand) ([]byte, error) {
	a.calls++
	return json.Marshal(a.calls)
}
func (a *retentionApplication) Snapshot() ([]byte, error) { return json.Marshal(a.calls) }
func (a *retentionApplication) Restore(raw []byte) error  { return json.Unmarshal(raw, &a.calls) }
func (a *retentionApplication) SnapshotCheckpoint(floor uint64) ([]byte, func() error, error) {
	a.checkpoints++
	a.floor = floor
	raw, err := a.Snapshot()
	return raw, func() error { a.persisted++; return nil }, err
}

func applicationReceiptMachine() (*machine, *retentionApplication) {
	app := &retentionApplication{}
	m := newMachine("retention", app)
	m.state.Coordinator = Assignment{NodeID: "node", Epoch: 1}
	m.state.WriterGeneration = 1
	return m, app
}

func applyReceiptCommand(t *testing.T, m *machine, id string, version uint64) receipt {
	t.Helper()
	c := command{Kind: "app", ID: id, ClusterID: "retention", App: AppCommand{
		ID: id, CallerNodeID: "node", CoordinatorEpoch: 1, WriterGeneration: 1,
		ExpectedVersion: version, Payload: []byte("1"),
	}}
	c.Fingerprint = fingerprint(c.Kind, c.App)
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return m.Apply(&raft.Log{Index: m.state.AppliedIndex + 1, Data: raw}).(receipt)
}

func TestApplicationReceiptWindowExpiresWithoutRepeatingWork(t *testing.T) {
	m, app := applicationReceiptMachine()
	first := applyReceiptCommand(t, m, "first", 0)
	if first.err() != nil {
		t.Fatal(first.err())
	}
	if got := applyReceiptCommand(t, m, "first", 0); got.err() != nil || !bytes.Equal(got.Result.Data, first.Result.Data) || app.calls != 1 {
		t.Fatal("retained receipt repeated application work")
	}
	for version := uint64(1); version < 4096+256; version++ {
		if r := applyReceiptCommand(t, m, "write-"+strconv.FormatUint(version, 10), version); r.err() != nil {
			t.Fatal(r.err())
		}
	}
	m.receipts["membership"] = receipt{Fingerprint: "retained"}
	if got := applyReceiptCommand(t, m, "first", 0); !errors.Is(got.err(), ErrReceiptExpired) || app.calls != 4096+256 {
		t.Fatalf("expired request must be refused without execution, receipt=%+v calls=%d", got, app.calls)
	}
	if len(m.receipts) > 4097 {
		t.Fatalf("high-frequency application receipts still grow forever: %d", len(m.receipts))
	}
	if m.receipts["membership"].Fingerprint != "retained" {
		t.Fatal("application watermark deleted an administrative receipt")
	}
	before := len(m.receipts)
	for range 20 {
		applyReceiptCommand(t, m, "expired", 0)
	}
	if len(m.receipts) != before {
		t.Fatal("expired requests re-created deleted replay evidence")
	}
	calls := app.calls
	if got := applyReceiptCommand(t, m, "write-256", 256); got.err() != nil || app.calls != calls {
		t.Fatal("the inclusive replay floor no longer replays its retained result")
	}
	snapshot, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()
	sink := &snapshotMemorySink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatal(err)
	}
	target, targetApp := applicationReceiptMachine()
	if err := target.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	if got := applyReceiptCommand(t, target, "first", 0); !errors.Is(got.err(), ErrReceiptExpired) || targetApp.calls != app.calls {
		t.Fatal("restore forgot the logical replay floor")
	}
}

func TestApplicationReceiptIdentityIncludesExpectedVersion(t *testing.T) {
	m, app := applicationReceiptMachine()
	first := applyReceiptCommand(t, m, "scoped", 0)
	second := applyReceiptCommand(t, m, "scoped", 1)
	if first.err() != nil || second.err() != nil || app.calls != 2 {
		t.Fatal("distinct version-scoped identities collided")
	}
	if got := applyReceiptCommand(t, m, "scoped", 0); got.err() != nil || !bytes.Equal(got.Result.Data, first.Result.Data) || app.calls != 2 {
		t.Fatal("retry did not preserve its original version-scoped identity")
	}
	err := rpcFailure{Code: errorCode(ErrReceiptExpired), Message: "expired"}.err()
	if !errors.Is(err, ErrReceiptExpired) {
		t.Fatal("RPC erased replay-expired classification")
	}
}

func TestSnapshotPrunesOnlyAfterDurableSinkCloses(t *testing.T) {
	m, app := applicationReceiptMachine()
	applyReceiptCommand(t, m, "first", 0)
	snapshot, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()
	if app.checkpoints != 1 || app.persisted != 0 {
		t.Fatal("snapshot capture must prepare but not acknowledge receipt compaction")
	}
	refused := errors.New("snapshot disk unavailable")
	if err := snapshot.Persist(&snapshotMemorySink{writeErr: refused}); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if app.persisted != 0 {
		t.Fatal("failed snapshot pruned live application receipts")
	}
	if err := snapshot.Persist(&closeRefusedSink{}); err == nil || app.persisted != 0 {
		t.Fatal("uncommitted snapshot close pruned receipts")
	}
	sink := &snapshotMemorySink{}
	if err := snapshot.Persist(sink); err != nil || app.persisted != 1 {
		t.Fatalf("durable checkpoint was not acknowledged: persisted=%d err=%v", app.persisted, err)
	}
	restored, restoredApp := applicationReceiptMachine()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	if restoredApp.calls != 1 {
		t.Fatal("checkpoint did not restore application")
	}
}

type closeRefusedSink struct{ snapshotMemorySink }

func (*closeRefusedSink) Close() error { return errors.New("close refused") }

func TestSnapshotRejectsUnscopedReceiptFormatBeforeApplicationRestore(t *testing.T) {
	m, _ := applicationReceiptMachine()
	data := snapshotData{
		Format: 2, State: m.read(), HasApplication: true,
		Receipts: map[string]receipt{"unscoped-id": {Fingerprint: "old-layout"}},
	}
	metadata, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	sink := &snapshotMemorySink{}
	if err := (&encodedSnapshot{metadata: metadata, application: []byte("99")}).Persist(sink); err != nil {
		t.Fatal(err)
	}
	target, app := applicationReceiptMachine()
	if err := target.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old receipt identity format was not rejected: %v", err)
	}
	if app.calls != 0 {
		t.Fatal("incompatible snapshot mutated the application before refusal")
	}
}

func TestSnapshotCleanupFailureDoesNotCancelDurableSnapshot(t *testing.T) {
	snapshot := &encodedSnapshot{
		metadata: []byte(`{"format":3}`),
		persisted: func() error {
			return errors.New("temporary cleanup refusal")
		},
	}
	sink := &snapshotMemorySink{}
	if err := snapshot.Persist(sink); err != nil || !sink.closed || sink.canceled {
		t.Fatalf("cleanup failure invalidated a durable snapshot: err=%v closed=%v canceled=%v", err, sink.closed, sink.canceled)
	}
}

func TestSnapshotPersistAfterRestoreCannotPruneNewGeneration(t *testing.T) {
	m, app := applicationReceiptMachine()
	applyReceiptCommand(t, m, "first", 0)
	old, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	current, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Release()
	sink := &snapshotMemorySink{}
	if err := current.Persist(sink); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	before := app.persisted
	if err := old.Persist(&snapshotMemorySink{}); err != nil {
		t.Fatal(err)
	}
	if app.persisted != before {
		t.Fatal("a snapshot from before Restore pruned the new application generation")
	}
}
