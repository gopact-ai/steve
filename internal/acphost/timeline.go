package acphost

import (
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/view"
)

// Thought spans keep byte ranges into the already bounded thought buffer.
// Keeping another raw copy here would bypass the 64 KiB retention limit.
type collectedSpan struct {
	view.Span
	start, end int
}

func (c *collector) appendTextSpan(text string) {
	if n := len(c.timeline); n > 0 && c.timeline[n-1].Kind == "text" {
		c.timeline[n-1].Text += text
		return
	}
	c.timeline = append(c.timeline, collectedSpan{Span: view.Span{Kind: "text", Text: text, At: time.Now().UTC()}})
}

func (c *collector) appendThoughtSpan(bytes int) {
	if n := len(c.timeline); n > 0 && c.timeline[n-1].Kind == "thought" {
		c.timeline[n-1].end += bytes
		return
	}
	c.timeline = append(c.timeline, collectedSpan{Span: view.Span{Kind: "thought", At: time.Now().UTC()}, start: c.thoughtBytes, end: c.thoughtBytes + bytes})
}

func (c *collector) timelineSnapshot() []view.Span {
	var out []view.Span
	for _, entry := range c.timeline {
		span := entry.Span
		if span.Kind == "thought" {
			if c.thoughtHead == "" {
				span.Text = c.thought[entry.start:entry.end]
			} else {
				headEnd := len(c.thoughtHead)
				tailStart := c.thoughtBytes - len(c.thought)
				if entry.start < headEnd {
					span.Text = c.thoughtHead[entry.start:min(entry.end, headEnd)]
				}
				if entry.start <= headEnd && entry.end > headEnd {
					span.Text += fmt.Sprintf("\n[… 省略 %d 字节 …]\n", tailStart-headEnd)
				}
				if entry.end > tailStart {
					span.Text += c.thought[max(entry.start-tailStart, 0) : entry.end-tailStart]
				}
			}
		}
		out = append(out, span)
	}
	return out
}
