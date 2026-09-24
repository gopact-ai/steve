package readmodel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/task"
)

type usageLedgerFixture struct {
	LedgerSource
	closed []attempt.Record
	err    error
	reads  int
}

func TestUsageSummaryReadsOnlyUsageAndPreservesTaskMetadata(t *testing.T) {
	store, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.Create(task.Task{Goal: "Root goal", Origin: "schedule:42", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	title := "Scheduled root title"
	if _, err := store.SetMeta(root.ID, task.MetaPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(task.Task{Goal: "Child", Parent: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	record := usageRecord(time.Now().Add(-time.Hour), "agent", "model", true, 11, 7)
	record.TaskID = child.ID
	// Every other LedgerSource method is deliberately unavailable: this read
	// must not fall back through Snapshot or fetch live/workspace/history data.
	source := &usageLedgerFixture{closed: []attempt.Record{record}}
	model := New(Sources{Tasks: store, Ledger: source})
	response := model.UsageSummary(t.Context())
	if source.reads != 1 || response.At.IsZero() || response.Usage == nil {
		t.Fatalf("usage read = %+v, calls=%d", response, source.reads)
	}
	want := recordUsage(source.closed, response.At, []Task{{ID: root.ID, Title: title, Goal: root.Goal, Origin: root.Origin, ProjectID: root.ProjectID}, {ID: child.ID, Parent: root.ID}})
	assertUsageEquivalent(t, *response.Usage, want)
	source.closed[0].Usage.Input = 21
	next := model.UsageSummary(t.Context())
	if source.reads != 2 || next.Usage.Total.Tokens.Total != 28 || response.Usage.Total.Tokens.Total != 18 {
		t.Fatal("usage read was cached or a later read mutated an earlier result")
	}
}

func TestUsageSummaryDistinguishesEmptyFailurePartialAndUnwired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wired  bool
		err    error
		closed []attempt.Record
		data   bool
	}{
		{name: "healthy empty", wired: true, data: true},
		{name: "unwired"},
		{name: "failed", wired: true, err: errors.New("usage query unavailable")},
		{name: "partial", wired: true, err: errors.New("usage query incomplete"), closed: []attempt.Record{usageRecord(time.Now().Add(-time.Minute), "a", "m", true, 100, 20)}, data: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := Sources{}
			if tc.wired {
				sources.Ledger = &usageLedgerFixture{closed: tc.closed, err: tc.err}
			}
			got := New(sources).UsageSummary(t.Context())
			if (got.Usage != nil) != tc.data {
				t.Fatalf("unknown usage conflated with healthy zero: %+v", got)
			}
			if len(got.Sources) != 1 || got.Sources[0].Name != "ledger-usage" || got.Sources[0].Wired != tc.wired {
				t.Fatalf("usage source identity lost: %+v", got.Sources)
			}
			if tc.err != nil && got.Sources[0].Error != tc.err.Error() {
				t.Fatalf("usage source failure lost: %+v", got.Sources)
			}
			if tc.err == nil && got.Sources[0].Error != "" {
				t.Fatalf("invented source failure: %+v", got.Sources)
			}
			if tc.data && (got.Usage.Total.Attempts != len(tc.closed) || len(got.Usage.Periods) != 3) {
				t.Fatalf("available evidence discarded: %+v", got.Usage)
			}
		})
	}
}

func (l *usageLedgerFixture) UsageSamples(context.Context) ([]attempt.UsageSample, error) {
	l.reads++
	return usageSamples(l.closed), l.err
}

func TestSnapshotDoesNotReadOrExposeUsage(t *testing.T) {
	source := &usageLedgerFixture{LedgerSource: newSourceFixture().adapter()}
	model := New(Sources{Ledger: source})
	for range 3 {
		snapshot := model.Snapshot(t.Context())
		if source.reads != 0 {
			t.Fatalf("base snapshot read closed attempt history %d times", source.reads)
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, exists := fields["usage"]; exists {
			t.Fatal("base snapshot exposes usage instead of separating the read")
		}
		for _, health := range snapshot.Sources {
			if health.Name == "ledger-usage" {
				t.Fatal("base snapshot claims health for a source it must not read")
			}
		}
	}
}
