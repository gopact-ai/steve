package acphost

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestTerminalOutputSplitRune(t *testing.T) {
	for _, text := range []string{"é", "界", "🙂"} {
		for split := 1; split < len(text); split++ {
			t.Run(fmt.Sprintf("%s/split%d", text, split), func(t *testing.T) {
				output := newTerminalOutput(16)
				writeTerminalOutputBytes(t, output, []byte("a"+text[:split]))
				requireTerminalOutputSnapshot(t, output, "a\uFFFD", false)
				writeTerminalOutputBytes(t, output, nil)
				requireTerminalOutputSnapshot(t, output, "a\uFFFD", false)
				writeTerminalOutputBytes(t, output, []byte(text[split:]+"\r\n"))
				requireTerminalOutputSnapshot(t, output, "a"+text+"\r\n", false)
				requireTerminalOutputBounds(t, output, 16)
			})
		}
	}
}

func TestTerminalOutputPartialPreviewDoesNotEvict(t *testing.T) {
	output := newTerminalOutput(3)
	writeTerminalOutputBytes(t, output, []byte("a\xc3"))
	preview, truncated := output.Snapshot()
	if preview != "\uFFFD" || !truncated {
		t.Fatalf("partial preview = (%q, %v), want replacement with clipping", preview, truncated)
	}
	requireTerminalOutputSnapshot(t, output, "\uFFFD", true)
	writeTerminalOutputBytes(t, output, []byte("\xa9"))
	requireTerminalOutputSnapshot(t, output, "aé", false)
	if preview != "\uFFFD" {
		t.Fatalf("earlier snapshot changed to %q", preview)
	}
	requireTerminalOutputBounds(t, output, 3)
}

func TestTerminalOutputTailAtRuneBoundary(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		writes    []string
		want      string
		truncated bool
	}{
		{"ascii tail", 5, []string{"abc", "defgh"}, "defgh", true},
		{"cut within rune", 6, []string{"a界🙂Z"}, "🙂Z", true},
		{"exact rune boundary", 8, []string{"a界🙂Z"}, "界🙂Z", true},
		{"exact fit", 4, []string{"🙂"}, "🙂", false},
		{"rune exceeds limit", 3, []string{"ab", "🙂"}, "", true},
		{"drain after oversized rune", 3, []string{"ab", "🙂", "xy"}, "xy", true},
		{"replacement tail", 4, []string{"\xff\xffZ"}, "\uFFFDZ", true},
		{"replacement exceeds limit", 2, []string{"\xff"}, "", true},
		{"preserve CRLF", 16, []string{"first\r", "\nlast\r\n"}, "first\r\nlast\r\n", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := newTerminalOutput(test.limit)
			for _, text := range test.writes {
				writeTerminalOutputBytes(t, output, []byte(text))
				requireTerminalOutputBounds(t, output, test.limit)
			}
			requireTerminalOutputSnapshot(t, output, test.want, test.truncated)
			writeTerminalOutputBytes(t, output, nil)
			requireTerminalOutputSnapshot(t, output, test.want, test.truncated)
		})
	}
}

func TestTerminalOutputOversizedChunk(t *testing.T) {
	input := append(bytes.Repeat([]byte("x"), 2<<20), []byte("A界🙂Z")...)
	for _, limit := range []int{8, 1 << 20} {
		t.Run(fmt.Sprintf("limit%d", limit), func(t *testing.T) {
			output := newTerminalOutput(limit)
			writeTerminalOutputBytes(t, output, input)
			want := "界🙂Z"
			if limit == 1<<20 {
				suffix := "A界🙂Z"
				want = strings.Repeat("x", limit-len(suffix)) + suffix
			}
			requireTerminalOutputSnapshot(t, output, want, true)
			requireTerminalOutputBounds(t, output, limit)
		})
	}
}

func TestTerminalOutputZeroLimit(t *testing.T) {
	output := newTerminalOutput(0)
	requireTerminalOutputSnapshot(t, output, "", false)
	writeTerminalOutputBytes(t, output, nil)
	writeTerminalOutputBytes(t, output, []byte{})
	requireTerminalOutputSnapshot(t, output, "", false)
	writeTerminalOutputBytes(t, output, []byte("\xe2"))
	requireTerminalOutputSnapshot(t, output, "", true)
	writeTerminalOutputBytes(t, output, bytes.Repeat([]byte("x"), 2<<20))
	requireTerminalOutputSnapshot(t, output, "", true)
	requireTerminalOutputBounds(t, output, 0)
}

func TestTerminalOutputInvalidUTF8(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"invalid byte", "A\xffB", "A\uFFFDB"},
		{"invalid run", "\xff\xfe", "\uFFFD\uFFFD"},
		{"continuation bytes", "\x80\xbf", "\uFFFD\uFFFD"},
		{"overlong encoding", "\xc0\xaf", "\uFFFD\uFFFD"},
		{"surrogate", "\xed\xa0\x80", "\uFFFD\uFFFD\uFFFD"},
		{"outside Unicode", "\xf4\x90\x80\x80", "\uFFFD\uFFFD\uFFFD\uFFFD"},
		{"broken prefix", "\xe2\x82X", "\uFFFD\uFFFDX"},
		{"broken four byte prefix", "\xf0\x9fX", "\uFFFD\uFFFDX"},
		{"literal replacement", "\xef\xbf\xbd", "\uFFFD"},
	}
	for _, test := range tests {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/split%v", test.name, split), func(t *testing.T) {
				output := newTerminalOutput(64)
				input := []byte(test.input)
				if split {
					for i := range input {
						writeTerminalOutputBytes(t, output, input[i:i+1])
						requireTerminalOutputBounds(t, output, 64)
					}
				} else {
					writeTerminalOutputBytes(t, output, input)
				}
				requireTerminalOutputSnapshot(t, output, test.want, false)
				requireTerminalOutputBounds(t, output, 64)
			})
		}
	}
}

func TestTerminalOutputBrokenPrefixAfterSnapshot(t *testing.T) {
	output := newTerminalOutput(16)
	writeTerminalOutputBytes(t, output, []byte("\xe2"))
	requireTerminalOutputSnapshot(t, output, "\uFFFD", false)
	writeTerminalOutputBytes(t, output, []byte("\x82"))
	requireTerminalOutputSnapshot(t, output, "\uFFFD", false)
	writeTerminalOutputBytes(t, output, []byte("X\r\n"))
	requireTerminalOutputSnapshot(t, output, "\uFFFD\uFFFDX\r\n", false)
	requireTerminalOutputBounds(t, output, 16)
}

func TestTerminalOutputManySmallWrites(t *testing.T) {
	const limit = 17
	output := newTerminalOutput(limit)
	pattern := []byte("a界🙂\r\n")
	for repeat := 0; repeat < 2048; repeat++ {
		for i := range pattern {
			writeTerminalOutputBytes(t, output, pattern[i:i+1])
			requireTerminalOutputBounds(t, output, limit)
		}
	}
	requireTerminalOutputSnapshot(t, output, "🙂\r\na界🙂\r\n", true)
	writeTerminalOutputBytes(t, output, []byte("END"))
	requireTerminalOutputSnapshot(t, output, "\r\na界🙂\r\nEND", true)
	requireTerminalOutputBounds(t, output, limit)
}

func TestTerminalOutputConcurrentSnapshots(t *testing.T) {
	stream := strings.Repeat("a界🙂\r\n", 256)
	for _, limit := range []int{17, len(stream) + utf8.UTFMax} {
		t.Run(fmt.Sprintf("limit%d", limit), func(t *testing.T) {
			output := newTerminalOutput(limit)
			start := make(chan struct{})
			done := make(chan struct{})
			failures := make(chan error, 4)
			var readers sync.WaitGroup
			for reader := 0; reader < 4; reader++ {
				readers.Add(1)
				go func() {
					defer readers.Done()
					<-start
					for {
						text, truncated := output.Snapshot()
						complete := strings.TrimSuffix(text, "\uFFFD")
						ordered := strings.Contains(stream, complete)
						if limit > len(stream) {
							ordered = !truncated && strings.HasPrefix(stream, complete)
						}
						if !utf8.ValidString(text) || len(text) > limit || !ordered {
							failures <- fmt.Errorf("snapshot was not a bounded UTF-8 stream tail: bytes=%d, truncated=%v", len(text), truncated)
							return
						}
						select {
						case <-done:
							return
						default:
						}
					}
				}()
			}
			close(start)
			for _, b := range []byte(stream) {
				n, err := output.Write([]byte{b})
				if n != 1 || err != nil {
					t.Errorf("Write consumed %d bytes, error %v", n, err)
					break
				}
			}
			close(done)
			readers.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			want, wantTruncated := stream, false
			if limit == 17 {
				want, wantTruncated = "🙂\r\na界🙂\r\n", true
			}
			requireTerminalOutputSnapshot(t, output, want, wantTruncated)
			requireTerminalOutputBounds(t, output, limit)
		})
	}
}

func writeTerminalOutputBytes(t *testing.T, output *terminalOutput, input []byte) {
	t.Helper()
	n, err := output.Write(input)
	if n != len(input) || err != nil {
		t.Fatalf("Write consumed %d of %d bytes, error %v", n, len(input), err)
	}
}

func requireTerminalOutputSnapshot(t *testing.T, output *terminalOutput, want string, wantTruncated bool) {
	t.Helper()
	text, truncated := output.Snapshot()
	if text != want || truncated != wantTruncated {
		t.Fatalf("Snapshot = (%q, %v), want (%q, %v)", text, truncated, want, wantTruncated)
	}
	if !utf8.ValidString(text) {
		t.Fatalf("Snapshot is not valid UTF-8: %q", text)
	}
}

func requireTerminalOutputBounds(t *testing.T, output *terminalOutput, limit int) {
	t.Helper()
	text, _ := output.Snapshot()
	if len(text) > limit || !utf8.ValidString(text) {
		t.Fatalf("Snapshot exceeds its byte limit or is invalid UTF-8: bytes=%d, limit=%d", len(text), limit)
	}
	output.mu.Lock()
	defer output.mu.Unlock()
	if len(output.buf) != limit || cap(output.buf) != limit ||
		output.size < 0 || output.size > limit ||
		output.pendingLen < 0 || output.pendingLen >= utf8.UTFMax {
		t.Fatalf("storage escaped its bounds: len=%d, cap=%d, retained=%d, pending=%d, limit=%d",
			len(output.buf), cap(output.buf), output.size, output.pendingLen, limit)
	}
}
