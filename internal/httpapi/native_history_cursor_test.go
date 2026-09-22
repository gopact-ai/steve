package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
)

func TestUnrelatedOwnerWritesPreserveNativeHTTPPages(t *testing.T) {
	for _, eventOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("event_only_%v", eventOnly), func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			tasks, err := task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			selected, err := tasks.Create(task.Task{Transport: "console", Channel: "selected"})
			if err != nil {
				t.Fatal(err)
			}
			unrelated, err := tasks.Create(task.Task{Transport: "console", Channel: "unrelated"})
			if err != nil {
				t.Fatal(err)
			}
			for i := range 3 {
				r := attempt.Record{
					Spec:      attempt.Spec{ID: fmt.Sprintf("selected-%d", i), TaskID: selected.ID},
					StartedAt: time.Unix(int64(i), 0), EndedAt: time.Unix(int64(i+1), 0),
				}
				if _, err := book.BeginGuarded(t.Context(), r.ID, "attempt", string(attempt.Bound), "fixture", r, func(tx *ledger.Tx) error {
					raw, _ := json.Marshal(r)
					return attempt.ImportHistoryTx(tx, []ledger.Operation{{ID: r.ID, Kind: "attempt", State: string(attempt.Bound), Data: raw}}, false)
				}); err != nil {
					t.Fatal(err)
				}
			}
			native := attempt.New(book)
			other, err := native.Open(t.Context(), attempt.Spec{TaskID: unrelated.ID, Kind: attempt.KindChat, Scope: attempt.ScopeNone})
			if err != nil {
				t.Fatal(err)
			}
			server := serve(t, readmodel.New(readmodel.Sources{Tasks: tasks}), ServerConfig{Token: "owner"})
			server.SetAdmin(&admin.Service{Tasks: tasks, Attempts: native})
			read := func(path string) consoleapi.AttemptHistoryPage {
				t.Helper()
				code, _, raw := taskMetaRequest(t, server, http.MethodGet, path, "owner", "")
				if code != http.StatusOK {
					t.Fatalf("read %s: HTTP %d %s", path, code, raw)
				}
				var page consoleapi.AttemptHistoryPage
				if err := json.Unmarshal(raw, &page); err != nil {
					t.Fatal(err)
				}
				return page
			}
			stablePath := "/console/attempts?conversation=selected"
			before := read(stablePath)
			if len(before.Items) != 3 {
				t.Fatal("invalid selected fixture")
			}
			for i, state := range []attempt.State{attempt.Prepared, attempt.Running, attempt.Failed} {
				first := read(stablePath + "&limit=1")
				taskFirst := read("/console/tasks/" + selected.ID + "/attempts?limit=1")
				if first.NextCursor == "" || first.Items[0].ID != "selected-2" {
					t.Fatal("invalid first page")
				}
				var eventsBefore, operationsBefore int64
				if err := book.DB().QueryRow(`SELECT (SELECT max(seq) FROM events),(SELECT max(rowid) FROM operations)`).
					Scan(&eventsBefore, &operationsBefore); err != nil {
					t.Fatal(err)
				}
				if eventOnly {
					// Genuine owner transition: no operation added, no selected
					// attempt or conversation binding changed.
					if _, err := native.Advance(t.Context(), other.ID, state, "review", nil); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := native.Open(t.Context(), attempt.Spec{ID: fmt.Sprintf("unrelated-%d", i),
						TaskID: unrelated.ID, Kind: attempt.KindChat, Scope: attempt.ScopeNone}); err != nil {
						t.Fatal(err)
					}
				}
				var eventsAfter, operationsAfter int64
				if err := book.DB().QueryRow(`SELECT (SELECT max(seq) FROM events),(SELECT max(rowid) FROM operations)`).
					Scan(&eventsAfter, &operationsAfter); err != nil {
					t.Fatal(err)
				}
				if eventOnly && operationsBefore != operationsAfter {
					t.Fatal("event-only counterexample changed operation inventory")
				}
				if eventsAfter <= eventsBefore {
					t.Fatal("owner mutation did not commit an event")
				}
				if after := read(stablePath); !reflect.DeepEqual(before, after) {
					t.Fatalf("selected history changed, invalid counterexample: before=%+v after=%+v", before, after)
				}
				for _, path := range []string{
					stablePath + "&limit=1&cursor=" + url.QueryEscape(first.NextCursor),
					"/console/tasks/" + selected.ID + "/attempts?limit=1&cursor=" + url.QueryEscape(taskFirst.NextCursor),
				} {
					code, _, raw := taskMetaRequest(t, server, http.MethodGet, path, "owner", "")
					if code != http.StatusOK {
						t.Fatalf("unrelated write invalidated stable history: HTTP %d %s", code, raw)
					}
					var next consoleapi.AttemptHistoryPage
					if err := json.Unmarshal(raw, &next); err != nil || len(next.Items) != 1 || next.Items[0].ID != "selected-1" {
						t.Fatalf("continuation lost rows: %+v %v", next, err)
					}
				}
				t.Logf("round=%d selected rows unchanged; unrelated owner events %d->%d operations %d->%d; conversation/task continuation both HTTP200 without refresh",
					i+1, eventsBefore, eventsAfter, operationsBefore, operationsAfter)
			}
		})
	}
}
