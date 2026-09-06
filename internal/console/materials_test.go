package console

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/turn"
)

type materialQueueHandler struct{ queueHandler }

func (h *materialQueueHandler) Context(context.Context, string) (turn.Context, error) {
	return turn.Context{Project: &turn.ContextProject{ID: "scratch", Bound: true}}, nil
}

func TestQueuedMaterialsFreezeSelectionAndSurviveRestart(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	materials, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	defer materials.Close()
	input := material.CaptureInput{Project: "scratch", Title: "source", MIME: "text/plain", Source: material.Source{Kind: "upload"}, Data: []byte("original line\nsecond line")}
	m, err := materials.Capture(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	h := &materialQueueHandler{queueHandler{started: make(chan *queueCall, 4)}}
	s := New(h, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	authorize := func(context.Context, string, string, string) error { return nil }
	s.SetMaterials(materials, authorize)
	first := enqueueForTest(t, s, "a", "hold")
	one := nextCall(t, &h.queueHandler)
	selector := &material.Selector{Kind: "lines", Start: 1, End: 1}
	second, err := s.Submit(t.Context(), consoleapi.Submission{Conversation: "a", Input: "read", CommandID: "frozen", Locale: "en", Refs: []material.Ref{{ID: m.ID, Selector: selector}}})
	if err != nil {
		t.Fatal(err)
	}
	selector.Start = 99
	input.Data = []byte("replacement source content")
	if _, err := materials.Capture(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	raw, _, err := doc.Load()
	if err != nil {
		t.Fatal(err)
	}
	one.finish <- nil
	_ = awaitExchange(t, s, first.ID)
	two := nextCall(t, &h.queueHandler)
	if !strings.Contains(two.req.Input, "original line") || strings.Contains(two.req.Input, "second line") || strings.Contains(two.req.Input, "replacement") || two.req.ExpectedProject != "scratch" || two.req.Locale != "en" {
		t.Fatalf("unfrozen request %+v", two.req)
	}
	two.finish <- nil
	_ = awaitExchange(t, s, second.ID)
	restartedHandler := &materialQueueHandler{queueHandler{started: make(chan *queueCall, 1)}}
	restarted := New(restartedHandler, "owner", nil)
	restarted.SetMaterials(materials, authorize)
	restartDoc := &memDoc{}
	_ = restartDoc.Save(raw)
	if err := restarted.Persist(restartDoc); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Drain(); err != nil {
		t.Fatal(err)
	}
	resumed := nextCall(t, &restartedHandler.queueHandler)
	if !strings.Contains(resumed.req.Input, "original line") {
		t.Fatal("restart lost frozen input")
	}
	resumed.finish <- nil
	if got := awaitExchange(t, restarted, second.ID); got.Refs[0].Selector.Start != 1 || got.Locale != "en" {
		t.Fatalf("restart changed refs %+v", got)
	}
}

func TestQueuedMaterialsRecheckAuthorityBeforeExecution(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	materials, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	defer materials.Close()
	m, err := materials.Capture(t.Context(), material.CaptureInput{Project: "scratch", Title: "private", MIME: "text/plain", Source: material.Source{Kind: "upload"}, Data: []byte("private bytes")})
	if err != nil {
		t.Fatal(err)
	}
	h := &materialQueueHandler{queueHandler{started: make(chan *queueCall, 3)}}
	s := New(h, "owner", nil)
	var revoked atomic.Bool
	s.SetMaterials(materials, func(_ context.Context, _, owner, project string) error {
		if revoked.Load() || owner != "owner" || project != "scratch" {
			return material.ErrScope
		}
		return nil
	})
	first := enqueueForTest(t, s, "a", "hold")
	one := nextCall(t, &h.queueHandler)
	second, err := s.Submit(t.Context(), consoleapi.Submission{Conversation: "a", Input: "read", CommandID: "revoked", Refs: []material.Ref{{ID: m.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	revoked.Store(true)
	one.finish <- nil
	_ = awaitExchange(t, s, first.ID)
	got := awaitExchange(t, s, second.ID)
	if got.State != "failed" {
		t.Fatal("revoked material executed")
	}
	noCall(t, &h.queueHandler)
	list := s.Replies("a")
	if !strings.Contains(list[len(list)-1].Error, material.ErrScope.Error()) {
		t.Fatalf("wrong failure %+v", list)
	}
	if _, err := s.Submit(t.Context(), consoleapi.Submission{Conversation: "a", Input: "read", CommandID: "new", Refs: []material.Ref{{ID: m.ID}}}); !errors.Is(err, material.ErrScope) {
		t.Fatal(err)
	}
}
