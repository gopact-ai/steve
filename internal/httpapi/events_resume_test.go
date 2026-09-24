package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/readmodel"
)

type sent struct {
	id   string
	kind string
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
		case strings.HasPrefix(line, "id: "):
			got.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			var ev readmodel.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatal(err)
			}
			got.kind = ev.Kind
		case line == "" && got.kind != "":
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
	// An id from another run of the hub says nothing about this one's
	// events: everything recent is replayed.
	stream := openEvents(t, server, "elsewhere.2", "")
	if got := nextEvent(t, stream); got != seen[0] {
		t.Fatalf("foreign id resumed at %+v, want %+v", got, seen[0])
	}
}
