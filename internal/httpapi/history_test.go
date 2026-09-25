package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
)

func historyRequest(t *testing.T, s *Server, query string) (*httptest.ResponseRecorder, []readmodel.HistoryEntry, string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.history(w, httptest.NewRequest(http.MethodGet, "/history"+query, nil))
	var page struct {
		Entries []readmodel.HistoryEntry `json:"entries"`
		Next    string                   `json:"next"`
	}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatalf("history contract: %v; body=%s", err, w.Body.String())
		}
		if page.Entries == nil {
			t.Fatal("history entries must be an array, not null")
		}
	}
	return w, page.Entries, page.Next
}

func TestHistoryHTTPContract(t *testing.T) {
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	book, err := ledger.Open(t.TempDir(), ledger.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	if _, err := book.Begin(t.Context(), "eight", "test", "open", "test", nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := book.Begin(t.Context(), "ten", "test", "open", "test", nil); err != nil {
		t.Fatal(err)
	}
	store := readmodel.Observations{Book: book}
	if err := store.Save(t.Context(), 1, []readmodel.Observation{{At: now.Add(-time.Hour), Kind: "node.up", Subject: "nine"}}, 0); err != nil {
		t.Fatal(err)
	}
	model := readmodel.New(readmodel.Sources{Ledger: readmodel.Ledger{Book: book}, Observations: store})
	if err := model.LoadObservations(); err != nil {
		t.Fatal(err)
	}
	s := &Server{model: model}
	cursor := ""
	for i, subject := range []string{"ten", "nine", "eight"} {
		w, entries, next := historyRequest(t, s, "?limit=1&cursor="+url.QueryEscape(cursor))
		if w.Code != http.StatusOK || len(entries) != 1 || entries[0].Subject != subject {
			t.Fatalf("page %d: status=%d body=%s", i, w.Code, w.Body.String())
		}
		if (next == "") != (i == 2) {
			t.Fatalf("page %d next=%q", i, next)
		}
		cursor = next
	}
}

func TestHistoryHTTPRejectsInvalidAndLegacyPagination(t *testing.T) {
	s := &Server{model: readmodel.New(readmodel.Sources{})}
	for _, query := range []string{"?cursor=1", "?cursor=%ZZ", "?cursor=a;cursor=b", "?limit=", "?cursor=bad!", "?cursor=e30", "?cursor=a&cursor=b", "?before=0", "?before=42", "?limit=bad", "?limit=-1", "?limit=0", "?limit=201", "?limit=1&limit=2"} {
		w, _, _ := historyRequest(t, s, query)
		if w.Code != http.StatusBadRequest {
			t.Errorf("query %q: status=%d body=%s", query, w.Code, w.Body.String())
		}
	}
	w, entries, next := historyRequest(t, s, "")
	if w.Code != http.StatusOK || len(entries) != 0 || next != "" {
		t.Fatalf("empty history: %s", w.Body.String())
	}
}

func TestHistoryHTTPExpiredCursorAndSourceFailure(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	store := readmodel.Observations{Book: book}
	obs := []readmodel.Observation{{At: time.Now().UTC(), Kind: "node.up", Subject: "first"}, {At: time.Now().UTC(), Kind: "node.up", Subject: "second"}}
	if err := store.Save(t.Context(), 1, obs, 0); err != nil {
		t.Fatal(err)
	}
	model := readmodel.New(readmodel.Sources{Observations: store})
	if err := model.LoadObservations(); err != nil {
		t.Fatal(err)
	}
	s := &Server{model: model}
	_, _, cursor := historyRequest(t, s, "?limit=1")
	if cursor == "" {
		t.Fatal("missing continuation")
	}
	// Retention forgets both observations the cursor was pinned to.
	if err := store.Save(t.Context(), 3, []readmodel.Observation{{At: time.Now().UTC(), Kind: "node.up", Subject: "third"}}, 3); err != nil {
		t.Fatal(err)
	}
	if err := model.LoadObservations(); err != nil {
		t.Fatal(err)
	}
	w, _, _ := historyRequest(t, s, "?limit=1&cursor="+url.QueryEscape(cursor))
	if w.Code != http.StatusConflict {
		t.Fatalf("expired cursor: %d %s", w.Code, w.Body.String())
	}
	s = &Server{model: readmodel.New(readmodel.Sources{Ledger: readmodel.Ledger{}})}
	w, _, _ = historyRequest(t, s, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("source failure: %d %s", w.Code, w.Body.String())
	}
}
