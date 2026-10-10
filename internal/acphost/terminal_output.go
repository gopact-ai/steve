package acphost

import (
	"sync"
	"unicode/utf8"
)

// terminalOutput retains a byte-bounded tail of normalized UTF-8 output.
// Invalid bytes each become U+FFFD; incomplete prefixes remain available for
// later writes. Write and Snapshot serialize access to the ring and decoder.
type terminalOutput struct {
	mu         sync.Mutex
	buf        []byte
	start      int
	size       int
	pending    [utf8.UTFMax]byte
	pendingLen int
	truncated  bool
	revision   uint64
}

// newTerminalOutput requires a limit in [0, 1<<20], supplied by the caller.
// The limit applies to encoded output bytes, including replacement characters.
func newTerminalOutput(limit int) *terminalOutput {
	return &terminalOutput{buf: make([]byte, limit)}
}

// Write consumes all input, including bytes that cannot fit in the tail.
// It never copies an input-sized chunk or allocates a growing decoder buffer.
func (o *terminalOutput) Write(p []byte) (int, error) {
	n := len(p)
	o.mu.Lock()
	defer o.mu.Unlock()
	if n != 0 {
		o.revision++
	}

	if len(o.buf) == 0 {
		if n > 0 {
			o.truncated = true
		}
		return n, nil
	}

	for len(p) > 0 || o.pendingLen > 0 {
		if o.pendingLen == 0 {
			if !utf8.FullRune(p) {
				o.pendingLen = copy(o.pending[:], p)
				break
			}
			consumed := o.appendDecoded(p)
			p = p[consumed:]
			continue
		}

		if !utf8.FullRune(o.pending[:o.pendingLen]) {
			if len(p) == 0 {
				break
			}
			o.pending[o.pendingLen] = p[0]
			o.pendingLen++
			p = p[1:]
			continue
		}

		consumed := o.appendDecoded(o.pending[:o.pendingLen])
		copy(o.pending[:], o.pending[consumed:o.pendingLen])
		o.pendingLen -= consumed
	}
	return n, nil
}

// Snapshot previews an incomplete prefix as one U+FFFD without consuming it.
// Preview-only clipping is reported for this snapshot, but does not evict the
// stored tail or set its sticky truncation flag. Completing the prefix can
// therefore restore a fitting tail and remove preview-only truncation.
func (o *terminalOutput) Snapshot() (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	length := o.size
	if o.pendingLen > 0 {
		length += len("\uFFFD")
	}
	if length == 0 {
		return "", o.truncated
	}

	// The copy is bounded by the byte limit plus one replacement character.
	text := make([]byte, length)
	copied := copy(text[:o.size], o.buf[o.start:])
	copy(text[copied:o.size], o.buf[:o.size-copied])
	if o.pendingLen > 0 {
		copy(text[o.size:], "\uFFFD")
	}

	truncated := o.truncated
	if len(text) > len(o.buf) {
		cut := len(text) - len(o.buf)
		for cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut++
		}
		text = text[cut:]
		truncated = true
	}
	return string(text), truncated
}

// appendDecoded requires a full rune, including a determinable invalid byte.
// It returns the source bytes consumed, not the normalized output byte count.
func (o *terminalOutput) appendDecoded(p []byte) int {
	r, consumed := utf8.DecodeRune(p)
	encoded := p[:consumed]
	if r == utf8.RuneError && consumed == 1 {
		encoded = []byte("\uFFFD")
	}

	if len(encoded) > len(o.buf) {
		// No earlier output can be part of a suffix after an oversized rune.
		o.start = 0
		o.size = 0
		o.truncated = true
		return consumed
	}
	for o.size+len(encoded) > len(o.buf) {
		o.start = (o.start + 1) % len(o.buf)
		o.size--
		for o.size > 0 && !utf8.RuneStart(o.buf[o.start]) {
			o.start = (o.start + 1) % len(o.buf)
			o.size--
		}
		o.truncated = true
	}

	end := (o.start + o.size) % len(o.buf)
	copied := copy(o.buf[end:], encoded)
	copy(o.buf, encoded[copied:])
	o.size += len(encoded)
	return consumed
}

func (o *terminalOutput) Version() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.revision
}
