package coordination

import (
	"bytes"
	"errors"
	"io"
	"testing"
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
