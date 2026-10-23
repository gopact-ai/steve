package clientsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/clientapi"
)

// StreamEvent is one message of the event stream: a change, or a reset
// telling the client to re-read what it shows.
type StreamEvent struct {
	Reset bool
	Event clientapi.Event
}

// Stream is one open GET /v1/events connection.
type Stream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
}

// Events opens the event stream. after is the last cursor received, sent
// as Last-Event-ID; thread keeps only that thread's events.
func (c *Client) Events(ctx context.Context, thread, after string) (*Stream, error) {
	path := "/v1/events"
	if thread != "" {
		path += "?" + url.Values{"thread": {thread}}.Encode()
	}
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if after != "" {
		req.Header.Set("Last-Event-ID", after)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, maxRefusal))
		return nil, refusal(res, raw)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		res.Body.Close()
		return nil, ErrNotV1
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 64<<10), maxBody)
	return &Stream{body: res.Body, scanner: scanner}, nil
}

// Next blocks for the next event. It returns io.EOF when the server ends
// the stream.
func (s *Stream) Next() (StreamEvent, error) {
	var kind string
	var data strings.Builder
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" {
			if kind == "" && data.Len() == 0 {
				continue
			}
			if kind == "reset" {
				return StreamEvent{Reset: true}, nil
			}
			var ev clientapi.Event
			if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
				return StreamEvent{}, fmt.Errorf("decode event: %w", err)
			}
			return StreamEvent{Event: ev}, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			kind = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return StreamEvent{}, err
	}
	return StreamEvent{}, io.EOF
}

// Close ends the connection.
func (s *Stream) Close() error { return s.body.Close() }

// Link is the state of a followed stream.
type Link int

const (
	Connecting Link = iota
	Connected
	Disconnected
)

func (l Link) String() string {
	switch l {
	case Connected:
		return "connected"
	case Disconnected:
		return "reconnecting"
	}
	return "connecting"
}

// Update is what Follow delivers: a change in the link, or an event.
// Reset and a fresh Connected both mean the client re-reads what it shows.
type Update struct {
	Link  Link
	Err   error
	Reset bool
	Event *clientapi.Event
}

// Follow keeps the event stream open until ctx ends, reconnecting after
// the last cursor received so a server restart or dropped connection
// resumes where it left off. Updates are delivered in order on the
// returned channel, which is closed when ctx ends.
func (c *Client) Follow(ctx context.Context, thread, after string) <-chan Update {
	out := make(chan Update, 64)
	go func() {
		defer close(out)
		send := func(u Update) bool {
			select {
			case out <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}
		cursor, backoff := after, followBackoff
		for ctx.Err() == nil {
			stream, err := c.Events(ctx, thread, cursor)
			if err != nil {
				if ctx.Err() != nil || !send(Update{Link: Disconnected, Err: err}) {
					return
				}
				if !sleep(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, followBackoffMax)
				continue
			}
			backoff = followBackoff
			if !send(Update{Link: Connected}) {
				stream.Close()
				return
			}
			for {
				ev, err := stream.Next()
				if err != nil {
					stream.Close()
					if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
						send(Update{Link: Disconnected, Err: err})
					}
					break
				}
				if ev.Reset {
					if !send(Update{Link: Connected, Reset: true}) {
						stream.Close()
						return
					}
					continue
				}
				if ev.Event.Cursor != "" {
					cursor = ev.Event.Cursor
				}
				event := ev.Event
				if !send(Update{Link: Connected, Event: &event}) {
					stream.Close()
					return
				}
			}
			if !sleep(ctx, backoff) {
				return
			}
		}
	}()
	return out
}

// followBackoff is the first wait before reconnecting; it doubles up to
// followBackoffMax while the server stays away.
var followBackoff, followBackoffMax = 250 * time.Millisecond, 5 * time.Second

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
