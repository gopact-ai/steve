package journal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newJournal(t *testing.T, opts Options) *Journal {
	t.Helper()
	j, err := New(t.TempDir(), "stream-1", opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func appendLine(t *testing.T, l *Log, line string) uint64 {
	t.Helper()
	seq, err := l.Append([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestAppendReplayAndExit(t *testing.T) {
	j := newJournal(t, Options{})
	for i, line := range []string{"{\"one\":1}\n", "two\twith tab\n", "\n"} {
		if n := appendLine(t, j.Out, line); n != uint64(i+1) {
			t.Fatal(n)
		}
	}
	appendLine(t, j.In, "input\n")
	if err := j.Finish(17); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := j.Out.Replay(1, &got); err != nil {
		t.Fatal(err)
	}
	if got.String() != "two\twith tab\n\nexit 17\n" {
		t.Fatalf("replay = %q", got.String())
	}
	raw, err := os.ReadFile(filepath.Join(j.Dir, "out.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "1\t{\"one\":1}\n2\ttwo\twith tab\n3\t\n4\texit 17\n" {
		t.Fatalf("disk = %q", raw)
	}
	raw, _ = os.ReadFile(filepath.Join(j.Dir, "in.jsonl"))
	if string(raw) != "1\tinput\n" {
		t.Fatalf("input = %q", raw)
	}
	if err := j.Out.Replay(5, io.Discard); !errors.Is(err, ErrAhead) {
		t.Fatal(err)
	}
}

func TestRotationBoundsBothDirectionsAndTooOld(t *testing.T) {
	j := newJournal(t, Options{SegmentBytes: 24, MaxBytes: 72})
	for range 10 {
		appendLine(t, j.Out, "out-line\n")
		appendLine(t, j.In, "in--line\n")
	}
	if j.total > 72 {
		t.Fatalf("retained %d bytes", j.total)
	}
	for _, l := range []*Log{j.Out, j.In} {
		if l.dropped == 0 {
			t.Fatal("nothing rotated out")
		}
		if err := l.Replay(l.dropped-1, io.Discard); !errors.Is(err, ErrTooOld) {
			t.Fatalf("old cursor: %v", err)
		}
		var replay bytes.Buffer
		if err := l.Replay(l.dropped, &replay); err != nil {
			t.Fatal(err)
		}
		if uint64(bytes.Count(replay.Bytes(), []byte{'\n'})) != l.seq-l.dropped {
			t.Fatalf("missing rows: %q", replay.String())
		}
	}
	var size int64
	files, _ := filepath.Glob(filepath.Join(j.Dir, "*.jsonl"))
	for _, path := range files {
		info, _ := os.Stat(path)
		size += info.Size()
	}
	if size != j.total {
		t.Fatalf("disk = %d, retained = %d", size, j.total)
	}
}

type blockingWriter struct {
	entered, release chan struct{}
	bytes.Buffer
}

func (w *blockingWriter) Write(b []byte) (int, error) {
	select {
	case <-w.entered:
	default:
		close(w.entered)
	}
	<-w.release
	return w.Buffer.Write(b)
}

func TestReplaySnapshotDoesNotBlockAppendOrLoseRotatedFiles(t *testing.T) {
	j := newJournal(t, Options{SegmentBytes: 15, MaxBytes: 40})
	appendLine(t, j.Out, "one\n")
	appendLine(t, j.Out, "two\n")
	w := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- j.Out.Replay(0, w) }()
	<-w.entered
	for range 20 {
		appendLine(t, j.Out, "new\n")
	}
	close(w.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if w.String() != "one\ntwo\n" {
		t.Fatalf("snapshot = %q", w.String())
	}
}

func TestRetainsEndedStreamsForOneHour(t *testing.T) {
	state := t.TempDir()
	j, err := New(state, "ended", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.Finish(0); err != nil {
		t.Fatal(err)
	}
	marker, _ := os.Stat(filepath.Join(j.Dir, "ended"))
	live, err := New(state, "live", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := Prune(state, marker.ModTime().Add(Retention-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(j.Dir); err != nil {
		t.Fatal("pruned early", err)
	}
	_ = j.Close()
	if err := Prune(state, marker.ModTime().Add(Retention)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(j.Dir); !os.IsNotExist(err) {
		t.Fatal("retained expired stream", err)
	}
	if _, err := os.Stat(live.Dir); err != nil {
		t.Fatal("pruned live stream", err)
	}
}

func TestWriteAndSyncFailureDisableBothDirections(t *testing.T) {
	for _, syncFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "append", true: "fsync"}[syncFailure], func(t *testing.T) {
			j := newJournal(t, Options{})
			appendLine(t, j.Out, "one\n")
			j.mu.Lock()
			_ = j.Out.segments[0].file.Close()
			if syncFailure {
				j.sync()
			}
			j.mu.Unlock()
			if !syncFailure {
				_, _ = j.Out.Append([]byte("two\n"))
			}
			if !errors.Is(j.Err(), ErrUnresumable) {
				t.Fatal(j.Err())
			}
			if _, err := j.In.Append([]byte("input\n")); !errors.Is(err, ErrUnresumable) {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(j.Dir, "unresumable")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompleteLinesAndLimit(t *testing.T) {
	j := newJournal(t, Options{})
	for _, bad := range []string{"half", "one\ntwo\n", ""} {
		if _, err := j.Out.Append([]byte(bad)); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if j.Out.Last() != 0 {
		t.Fatal("counted partial input")
	}
	appendLine(t, j.Out, strings.Repeat("x", MaxLine-1)+"\n")
	if _, err := j.Out.Append([]byte(strings.Repeat("x", MaxLine) + "\n")); !errors.Is(err, ErrLineTooLong) {
		t.Fatal(err)
	}
	if err := j.Out.Replay(0, io.Discard); !errors.Is(err, ErrUnresumable) {
		t.Fatal(err)
	}
}

func TestStreamIDCannotEscapeStateOrOverwriteLogs(t *testing.T) {
	state := t.TempDir()
	for _, id := range []string{"", "..", "../other", "a/b"} {
		if _, err := New(state, id, Options{}); err == nil {
			t.Fatal(id)
		}
	}
	j, err := New(state, "once", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	appendLine(t, j.Out, "evidence\n")
	if _, err := New(state, "once", Options{}); !os.IsExist(err) {
		t.Fatal(err)
	}
}
