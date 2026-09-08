// Package logs installs the slog handler both steve binaries log through.
package logs

import (
	"context"
	"io"
	"log"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Both binaries log through log/slog. handler keeps the line shape log.Printf
// produced — the standard log date and time prefix, then the message text
// unchanged — and appends the record's attributes as key=value fields. The
// troubleshooting table in docs/operations.md and greps over the hub log
// keep matching because fields supplement the message instead of replacing
// parts of it; WARN and ERROR records end with a level field, INFO lines
// are byte-identical to the log.Printf output they replace.
//
// Installing the handler as slog's default also routes the log package's
// default logger through it, so packages that still call log.Printf write
// the same prefix to the same writer.
type handler struct {
	out    io.Writer
	mu     *sync.Mutex
	attrs  string
	groups []string
}

// NewHandler returns a handler writing the log.Printf line shape to out.
func NewHandler(out io.Writer) slog.Handler {
	return &handler{out: out, mu: &sync.Mutex{}}
}

// Install installs the handler once, on the writer log.Printf was using at
// the time, so every entry point of a binary logs alike.
var Install = sync.OnceFunc(func() {
	slog.SetDefault(slog.New(NewHandler(log.Writer())))
})

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.Grow(24 + len(r.Message) + len(h.attrs))
	at := r.Time
	if at.IsZero() {
		at = time.Now()
	}
	b.WriteString(at.Format("2006/01/02 15:04:05 "))
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		appendLogAttr(&b, h.groups, a)
		return true
	})
	if r.Level != slog.LevelInfo {
		b.WriteString(" level=")
		b.WriteString(r.Level.String())
	}
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, b.String())
	return err
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range attrs {
		appendLogAttr(&b, h.groups, a)
	}
	next := *h
	next.attrs = b.String()
	return &next
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string(nil), h.groups...), name)
	return &next
}

// appendLogAttr writes one " key=value" field, flattening groups into
// dotted keys and quoting values that would not survive a split on spaces.
func appendLogAttr(b *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		members := a.Value.Group()
		if len(members) == 0 {
			return
		}
		if a.Key != "" {
			groups = append(append([]string(nil), groups...), a.Key)
		}
		for _, member := range members {
			appendLogAttr(b, groups, member)
		}
		return
	}
	b.WriteByte(' ')
	for _, g := range groups {
		b.WriteString(g)
		b.WriteByte('.')
	}
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(quoteLogValue(logValueText(a.Value)))
}

func logValueText(v slog.Value) string {
	switch v.Kind() {
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return err.Error()
		}
	}
	return v.String()
}

func quoteLogValue(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if r <= ' ' || r == '"' || r == '=' || r == 0x7f {
			return strconv.Quote(s)
		}
	}
	return s
}
