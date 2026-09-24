package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/readmodel"
)

type sent struct {
	event string
	id    string
	kind  string
}

// openEvents connects to the stream, resuming after the given id through
// the Last-Event-ID header or the after parameter.
func openEvents(t *testing.T, server *Server, header, after string) *bufio.Scanner {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	url := server.URL() + "/events?token=" + testToken
	if after != "" {
		url += "&after=" + after
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if header != "" {
		req.Header.Set("Last-Event-ID", header)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return bufio.NewScanner(res.Body)
}

func nextEvent(t *testing.T, stream *bufio.Scanner) sent {
	t.Helper()
	var got sent
	for stream.Scan() {
		line := stream.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			got.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			got.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			var ev readmodel.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatal(err)
			}
			got.kind = ev.Kind
		case line == "" && (got.kind != "" || got.event != ""):
			return got
		}
	}
	t.Fatalf("stream ended: %v", stream.Err())
	return got
}

// Each event carries an id; a client reconnecting after one is replayed only
// what followed it, not the recent events it already has.
func TestEventStreamResumesAfterTheLastEventID(t *testing.T) {
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub-1"}})
	server := serve(t, model, ServerConfig{Token: testToken})
	for _, kind := range []string{"one", "two", "three"} {
		model.Publish(readmodel.Event{Kind: kind})
	}
	first := openEvents(t, server, "", "")
	var seen []sent
	for range 3 {
		seen = append(seen, nextEvent(t, first))
	}
	for i, ev := range seen {
		if ev.id == "" || (i > 0 && ev.id == seen[i-1].id) {
			t.Fatalf("events without distinct ids: %+v", seen)
		}
	}
	for _, resume := range []struct{ name, header, after string }{
		{"header", seen[1].id, ""},
		{"parameter", "", seen[1].id},
	} {
		t.Run(resume.name, func(t *testing.T) {
			stream := openEvents(t, server, resume.header, resume.after)
			if got := nextEvent(t, stream); got != seen[2] {
				t.Fatalf("resumed with %+v, want %+v", got, seen[2])
			}
			model.Publish(readmodel.Event{Kind: "four-" + resume.name})
			for {
				got := nextEvent(t, stream)
				if got.kind == "four-"+resume.name {
					break
				}
				if !strings.HasPrefix(got.kind, "four-") {
					t.Fatalf("after the replay came %+v again", got)
				}
			}
		})
	}
}

// A client may name its last event both ways, as an EventSource that was
// opened with after and then reconnected by itself does; the later of the
// two is where it resumes.
func TestEventStreamResumesAfterTheLaterOfHeaderAndParameter(t *testing.T) {
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub-1"}})
	server := serve(t, model, ServerConfig{Token: testToken})
	for _, kind := range []string{"one", "two", "three"} {
		model.Publish(readmodel.Event{Kind: kind})
	}
	id := func(n uint64) string { return model.EventID(readmodel.Event{Cursor: n}) }
	for _, resume := range []struct{ name, header, after string }{
		{"later header", id(2), id(1)},
		{"later parameter", id(1), id(2)},
		{"foreign header", "elsewhere.9", id(2)},
		{"foreign parameter", id(2), "elsewhere.9"},
	} {
		t.Run(resume.name, func(t *testing.T) {
			if got := nextEvent(t, openEvents(t, server, resume.header, resume.after)); got.event != "" || got.kind != "three" {
				t.Fatalf("resumed with %+v, want three", got)
			}
		})
	}
}

// A client whose last event the stream cannot place is told to reset
// before the recent events are replayed: it may have missed events, so it
// re-reads the state rather than trusting what it holds. That is an id
// from another run of the hub, one that is malformed or not yet sent, and
// one older than the recent events kept.
func TestEventStreamTellsAClientItCannotResumeToReset(t *testing.T) {
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub-1"}})
	server := serve(t, model, ServerConfig{Token: testToken})
	const published = 300
	for i := range published {
		model.Publish(readmodel.Event{Kind: fmt.Sprintf("event-%d", i+1)})
	}
	oldest := nextEvent(t, openEvents(t, server, "", ""))
	var kept uint64
	if _, err := fmt.Sscanf(oldest.kind, "event-%d", &kept); err != nil {
		t.Fatal(err)
	}
	id := func(n uint64) string { return model.EventID(readmodel.Event{Cursor: n}) }
	epoch, _, _ := strings.Cut(id(1), ".")
	for _, resume := range []struct{ name, last string }{
		{"another run", "elsewhere.2"},
		{"no cursor", "garbage"},
		{"cursor not a number", epoch + ".x"},
		{"negative cursor", epoch + ".-1"},
		{"cursor overflowing", epoch + ".99999999999999999999999"},
		{"cursor not yet sent", id(published + 1)},
		{"older than kept", id(kept - 2)},
	} {
		t.Run(resume.name, func(t *testing.T) {
			stream := openEvents(t, server, resume.last, "")
			if got := nextEvent(t, stream); got.event != "reset" {
				t.Fatalf("resuming after %q began with %+v, not a reset", resume.last, got)
			}
			if got := nextEvent(t, stream); got != oldest {
				t.Fatalf("after the reset came %+v, want %+v", got, oldest)
			}
		})
	}
	// The event just before the oldest kept is the last one a client can
	// have and still miss nothing.
	if got := nextEvent(t, openEvents(t, server, id(kept-1), "")); got != oldest {
		t.Fatalf("resuming just before the kept events began with %+v, want %+v", got, oldest)
	}
	last := id(published)
	stream := openEvents(t, server, last, "")
	model.Publish(readmodel.Event{Kind: "next"})
	if got := nextEvent(t, stream); got.event != "" || got.kind != "next" {
		t.Fatalf("resuming after the last event began with %+v, want next", got)
	}
}
