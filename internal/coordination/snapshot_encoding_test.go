package coordination

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/logs"
	"github.com/hashicorp/raft"
)

type snapshotBytesApplication struct{ data []byte }

func (a *snapshotBytesApplication) Apply(AppliedCommand) ([]byte, error) { return nil, nil }
func (a *snapshotBytesApplication) Snapshot() ([]byte, error)            { return bytes.Clone(a.data), nil }
func (a *snapshotBytesApplication) Restore(data []byte) error {
	a.data = bytes.Clone(data)
	return nil
}

type snapshotMemorySink struct {
	bytes.Buffer
	closed, canceled bool
	writeErr         error
}

func (s *snapshotMemorySink) ID() string   { return "test" }
func (s *snapshotMemorySink) Close() error { s.closed = true; return nil }
func (s *snapshotMemorySink) Cancel() error {
	s.canceled = true
	return nil
}
func (s *snapshotMemorySink) Write(raw []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.Buffer.Write(raw)
}

func TestSnapshotDoesNotBase64EncodeApplication(t *testing.T) {
	app := &snapshotBytesApplication{data: bytes.Repeat([]byte{0, 255, 128, 1}, 1<<18)}
	m := newMachine("snapshot", app)
	m.receipts["command"] = receipt{Fingerprint: "fingerprint", Result: Result{Data: []byte("original")}}
	snapshot, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Persist happens after Apply may resume. Both the application and the
	// control projection must still belong to the same captured boundary.
	app.data[0] = 77
	m.receipts["command"] = receipt{Fingerprint: "new"}
	m.state.Revision++
	sink := &snapshotMemorySink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatal(err)
	}
	snapshot.Release()
	if sink.Len() > len(app.data)+4096 {
		t.Fatalf("application snapshot expanded: payload=%d snapshot=%d", len(app.data), sink.Len())
	}
	if !sink.closed || sink.canceled {
		t.Fatalf("sink lifecycle: closed=%v canceled=%v", sink.closed, sink.canceled)
	}
	targetApp := &snapshotBytesApplication{}
	target := newMachine("snapshot", targetApp)
	if err := target.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	if targetApp.data[0] != 0 || target.state.Revision != 0 ||
		target.receipts["command"].Fingerprint != "fingerprint" ||
		string(target.receipts["command"].Result.Data) != "original" {
		t.Fatal("snapshot no longer represents its capture boundary")
	}
}

func TestSnapshotEnvelopeRejectsDamageAndCancelsFailedSink(t *testing.T) {
	m := newMachine("snapshot", &snapshotBytesApplication{data: []byte("payload")})
	snapshot, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()
	refused := errors.New("sink refused")
	badSink := &snapshotMemorySink{writeErr: refused}
	if err := snapshot.Persist(badSink); !errors.Is(err, refused) || !badSink.canceled || badSink.closed {
		t.Fatalf("failed sink lifecycle: err=%v canceled=%v closed=%v", err, badSink.canceled, badSink.closed)
	}
	sink := &snapshotMemorySink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatal(err)
	}
	raw := sink.Bytes()
	changed := bytes.Clone(raw)
	changed[len(changed)-1] ^= 1
	for name, input := range map[string][]byte{
		"short": raw[:8], "truncated": raw[:len(raw)-1], "changed": changed,
		"trailing": append(bytes.Clone(raw), 1), "old-format": []byte(`{"format":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			app := &snapshotBytesApplication{data: []byte("unchanged")}
			target := newMachine("snapshot", app)
			if err := target.Restore(io.NopCloser(bytes.NewReader(input))); err == nil {
				t.Fatal("damaged snapshot accepted")
			}
			if string(app.data) != "unchanged" {
				t.Fatal("damaged snapshot changed application")
			}
		})
	}
}

// logBuffer holds log output that a goroutine left by an earlier test may
// also write to.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A restored snapshot leaves one line in the process log with the applied
// index it restores, the bytes read and how long it took, so the log shows
// how often a replica catches up through a whole snapshot. A snapshot that
// is refused, or whose application fails to restore, leaves no such line.
func TestRestoreLogsItsAppliedIndexBytesAndDuration(t *testing.T) {
	output := &logBuffer{}
	// Setting slog's default also redirects the log package, which setting
	// it back does not undo.
	previousLogger := slog.Default()
	previousWriter, previousFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(logs.NewHandler(output)))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	source := newMachine("snapshot", &snapshotBytesApplication{data: bytes.Repeat([]byte("x"), 4096)})
	source.StoreConfiguration(41, raft.Configuration{Servers: []raft.Server{{ID: "a", Address: "a", Suffrage: raft.Voter}}})
	// A command after the configuration takes the applied index past the
	// configuration index, which the line must not report instead.
	initialize, err := json.Marshal(command{Kind: "initialize", ID: "init", ClusterID: "snapshot", Member: Member{NodeID: "a", Address: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := source.Apply(&raft.Log{Index: 42, Data: initialize}).(receipt); !ok || r.err() != nil {
		t.Fatalf("initialize: %#v", r)
	}
	snapshot, err := source.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()
	sink := &snapshotMemorySink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatal(err)
	}
	raw := sink.Bytes()
	for name, refused := range map[string]struct {
		cluster string
		app     Application
		input   []byte
		stops   bool
	}{
		"truncated":     {"snapshot", &snapshotBytesApplication{}, raw[:len(raw)-1], false},
		"other-cluster": {"other", &snapshotBytesApplication{}, raw, false},
		// The snapshot passes every check, then the application cannot
		// decode its part and the replica stops.
		"application": {"snapshot", &retentionApplication{}, raw, true},
	} {
		target := newMachine(refused.cluster, refused.app)
		if err := target.Restore(io.NopCloser(bytes.NewReader(refused.input))); err == nil {
			t.Fatalf("%s snapshot accepted", name)
		}
		if stopped := !target.healthy(); stopped != refused.stops {
			t.Fatalf("%s snapshot: replica stopped %v, want %v", name, stopped, refused.stops)
		}
	}
	if strings.Contains(output.String(), "snapshot restored") {
		t.Fatalf("a snapshot that was not restored was logged as restored:\n%s", output.String())
	}
	target := newMachine("snapshot", &snapshotBytesApplication{})
	if err := target.Restore(io.NopCloser(bytes.NewReader(raw))); err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} coordination: snapshot restored applied_index=42 bytes=` + strconv.Itoa(len(raw)) + ` took=(\S+)$`)
	logged := line.FindAllStringSubmatch(output.String(), -1)
	if len(logged) != 1 {
		t.Fatalf("restore of %d bytes at applied index 42 logged %d matching lines, want 1:\n%s", len(raw), len(logged), output.String())
	}
	if _, err := time.ParseDuration(logged[0][1]); err != nil {
		t.Fatalf("took=%q is not a duration: %v", logged[0][1], err)
	}
}

type snapshotDiscardSink struct{}

func (snapshotDiscardSink) ID() string                  { return "discard" }
func (snapshotDiscardSink) Write(b []byte) (int, error) { return len(b), nil }
func (snapshotDiscardSink) Close() error                { return nil }
func (snapshotDiscardSink) Cancel() error               { return nil }

func BenchmarkSnapshotApplicationImage(b *testing.B) {
	m := newMachine("snapshot", &snapshotBytesApplication{data: bytes.Repeat([]byte("x"), 8<<20)})
	b.ReportAllocs()
	b.SetBytes(8 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snapshot, err := m.Snapshot()
		if err != nil {
			b.Fatal(err)
		}
		if err := snapshot.Persist(snapshotDiscardSink{}); err != nil {
			b.Fatal(err)
		}
		snapshot.Release()
	}
}
