package artifact

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestCompletionBlockerReadMatchesTransactionalLandingGuard(t *testing.T) {
	for _, scenario := range []string{"locked", "merge-conflict", "superseded", "no-commit", "different-artifact", "different-target", "different-attempt", "different-epoch", "write-lease", "snapshot-taken", "paths", "wal", "unfinished", "apply-conflict", "unapplied"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
			refused := Landing{ID: "refused", Project: "p", Artifact: "result", Target: project.Home{Path: "/canonical"},
				Source: &Source{AttemptID: "attempt", Execution: &task.ExecutionToken{TaskID: "child", Epoch: 1}},
				State:  LandMergeConflicted, EndedAt: time.Unix(101, 0).UTC()}
			accepted := refused
			accepted.ID, accepted.State = "accepted", LandCommitted
			switch scenario {
			case "locked":
				refused.State = LandLocked
			case "different-artifact":
				accepted.Artifact = "other-result"
			case "different-target":
				accepted.Target.Path = "/other"
			case "different-attempt":
				accepted.Source = &Source{AttemptID: "other", Execution: refused.Source.Execution}
			case "different-epoch":
				accepted.Source = &Source{AttemptID: "attempt", Execution: &task.ExecutionToken{TaskID: "child", Epoch: 2}}
			case "write-lease":
				refused.Lease = &ledger.Lease{Key: "canonical:p"}
			case "snapshot-taken":
				refused.Now = "snapshot"
			case "paths":
				refused.Paths = []string{"file"}
			case "wal":
				refused.Round = 1
			case "unfinished":
				refused.EndedAt = time.Time{}
			case "apply-conflict":
				refused.State = LandApplyConflicted
			case "unapplied":
				refused.Now, refused.Merged, refused.Paths, refused.Unapplied = "snapshot", "candidate", []string{"file"}, true
			}
			for _, land := range []Landing{refused, accepted} {
				if (scenario == "no-commit" || scenario == "merge-conflict") && land.ID == accepted.ID {
					continue
				}
				if _, err := s.ledger.Begin(t.Context(), land.ID, landKind, land.State, "test", land); err != nil {
					t.Fatal(err)
				}
			}
			var before int
			_ = s.ledger.DB().QueryRow("SELECT COUNT(*) FROM events").Scan(&before)
			got, err := s.CompletionBlockers(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var after int
			_ = s.ledger.DB().QueryRow("SELECT COUNT(*) FROM events").Scan(&after)
			if before != after {
				t.Fatal("completion read wrote business events")
			}
			guard := s.ledger.Update(t.Context(), func(tx *ledger.Tx) error { return CheckTaskLandingsTx(tx, map[string]bool{"child": true}) })
			if (len(got) == 0) != (guard == nil) {
				t.Fatalf("read blockers %v disagree with transaction %v", got, guard)
			}
			if len(got) != 0 && !reflect.DeepEqual(got, []string{"child"}) {
				t.Fatalf("blocker task identity lost: %v", got)
			}
		})
	}
}

func TestCompletionBlockerReadRejectsUnreadableStandingOrHistoricalRecords(t *testing.T) {
	for _, scenario := range []string{"pending", "committed-payload", "committed-envelope"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
			var query string
			switch scenario {
			case "pending":
				query = `INSERT INTO bindings VALUES('pending-landing','bad','{')`
			case "committed-payload":
				query = `INSERT INTO operations VALUES('bad','landing','committed',1,1,'{','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`
			case "committed-envelope":
				query = `INSERT INTO operations VALUES('bad','landing','committed',1,1,'{"id":"bad"}','not a time','2026-09-01T00:00:00Z')`
			}
			if _, err := s.ledger.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CompletionBlockers(t.Context()); err == nil {
				t.Fatal("unreadable record became known absence of blockers")
			}
		})
	}
}

func TestCompletionBlockerReadCostDoesNotGrowWithCommittedHistory(t *testing.T) {
	s, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
	p := Pending{Artifact: "result", Source: &Source{Execution: &task.ExecutionToken{TaskID: "child", Epoch: 1}}}
	if err := s.ledger.PutBinding(t.Context(), pendingKind, "p/result", p); err != nil {
		t.Fatal(err)
	}
	read := func() {
		t.Helper()
		got, err := s.CompletionBlockers(t.Context())
		if err != nil || !reflect.DeepEqual(got, []string{"child"}) {
			t.Fatalf("standing blocker lost: %v, %v", got, err)
		}
	}
	before := testing.AllocsPerRun(3, read)
	if _, err := s.ledger.DB().Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<5000)
		INSERT INTO operations SELECT 'history-'||n,'landing','committed',1,1,
		json_object('id','history-'||n,'project','p','artifact','result-'||n,
			'source',json_object('execution',json_object('task_id','other-'||n,'epoch',1),'attempt_id','attempt-'||n),
			'started_at','2026-09-01T00:00:00Z','ended_at','2026-09-01T00:00:00Z'),
		'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z' FROM seq`); err != nil {
		t.Fatal(err)
	}
	after := testing.AllocsPerRun(3, read)
	t.Logf("standing completion read allocations: %.0f -> %.0f", before, after)
	if after > before+30 {
		t.Fatalf("committed history entered standing completion read: %.0f -> %.0f", before, after)
	}
	rows, err := s.ledger.DB().Query("EXPLAIN QUERY PLAN " + standingTaskLandingsSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&plan, detail)
	}
	if err := rows.Err(); err != nil || !strings.Contains(plan.String(), "operations_uncommitted_landings") {
		t.Fatalf("standing read lost its exact partial index: %s, %v", plan.String(), err)
	}
}
