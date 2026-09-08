package readmodel

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// The service seam can return partial records with an error: callers must
// retain those facts without treating the omitted records as known absent.
type sourceFixture struct {
	fail        map[string]error
	live        []attempt.Record
	projects    []project.Project
	disclosures []project.DisclosureRequest
	effects     []intent.Intent
}

func newSourceFixture() *sourceFixture {
	return &sourceFixture{fail: map[string]error{},
		projects:    []project.Project{{ID: "p", Home: project.Home{Path: "/work/p"}}, {ID: "q", Home: project.Home{Path: "/work/q"}}},
		disclosures: []project.DisclosureRequest{{ID: "disclosure", TaskID: "1", Project: "p"}},
		effects:     []intent.Intent{{ID: "effect", TaskID: "1", Tool: "send"}},
	}
}
func (s *sourceFixture) adapter() Ledger {
	return Ledger{Attempts: s, Artifacts: s, Projects: s, Intents: s}
}
func (s *sourceFixture) Live(context.Context) ([]attempt.Record, error) {
	return s.live, s.fail["live"]
}
func (s *sourceFixture) Closed(context.Context) ([]attempt.Record, error) {
	return []attempt.Record{{Spec: attempt.Spec{Agent: "local"}, State: attempt.Bound, StartedAt: time.Now().Add(-time.Minute), Usage: &attempt.Usage{Reported: true, Input: 100}}}, s.fail["usage"]
}
func (s *sourceFixture) Reservations(context.Context) ([]attempt.Reservation, error) {
	return []attempt.Reservation{{ID: "reservation"}}, s.fail["reservations"]
}
func (s *sourceFixture) Attestations(context.Context, string) ([]artifact.Attestation, error) {
	return []artifact.Attestation{{Artifact: "artifact", Verdict: "pass"}}, s.fail["attestations"]
}
func (s *sourceFixture) Replicas(context.Context, string) ([]artifact.Replica, error) {
	return []artifact.Replica{{Artifact: "artifact", Node: "node"}}, s.fail["replicas"]
}
func (s *sourceFixture) List(context.Context) ([]project.Project, error) {
	return s.projects, s.fail["projects"]
}
func (s *sourceFixture) PendingDisclosures(context.Context) ([]project.DisclosureRequest, error) {
	return s.disclosures, s.fail["disclosures"]
}
func (s *sourceFixture) Grants(context.Context, string) ([]project.Grant, error) {
	return []project.Grant{{Project: "p", Principal: "owner"}}, s.fail["grants"]
}
func (s *sourceFixture) PendingResolution(context.Context) ([]intent.Intent, error) {
	return s.effects, s.fail["effects"]
}
func (s *sourceFixture) Landings(_ context.Context, id string) ([]artifact.Landing, error) {
	return []artifact.Landing{{ID: "land-" + id, Project: id, State: artifact.LandCommitted}}, s.fail["landings/"+id]
}

func sourceHealth(t *testing.T, snap Snapshot, name string) SourceHealth {
	t.Helper()
	for _, source := range snap.Sources {
		if source.Name == name {
			return source
		}
	}
	t.Fatalf("missing source %s", name)
	return SourceHealth{}
}

func TestLedgerQueriesReportErrorsAndKeepPartialContributions(t *testing.T) {
	for _, query := range []string{"live", "projects", "landings/p", "usage"} {
		t.Run(query, func(t *testing.T) {
			s := newSourceFixture()
			s.live = []attempt.Record{{Spec: attempt.Spec{ID: "live", Agent: "local", TaskID: "2", Workspace: project.Workspace{Path: "/work/p"}}, State: attempt.Running}}
			s.fail[query] = errors.New("injected " + query)
			m := fixture(t)
			m.src.Ledger = s.adapter()
			snap := m.Snapshot(t.Context())
			group := strings.Split(query, "/")[0]
			if h := sourceHealth(t, snap, "ledger-"+group); !h.Wired || !strings.Contains(h.Error, "injected "+query) {
				t.Fatalf("query failure hidden: %+v", h)
			}
			if h := sourceHealth(t, snap, "ledger"); !strings.Contains(h.Error, group) {
				t.Fatalf("aggregate health lost query: %+v", h)
			}
			if len(snap.Attempts) != 1 || len(snap.Projects) != 2 || len(snap.Landings) != 2 || snap.Usage.Total.Tokens.Total != 100 || len(snap.Inbox) != 2 {
				t.Fatalf("partial result discarded: attempts=%d projects=%d landings=%d usage=%+v inbox=%d", len(snap.Attempts), len(snap.Projects), len(snap.Landings), snap.Usage, len(snap.Inbox))
			}
			if group != "usage" && sourceHealth(t, snap, "ledger-usage").Error != "" {
				t.Fatal("unrelated failure marked usage unreadable")
			}
			if group != "live" && sourceHealth(t, snap, "ledger-live").Error != "" {
				t.Fatal("unrelated failure marked activity unreadable")
			}
		})
	}
}

func TestEveryFactSubqueryContributesItsFailure(t *testing.T) {
	for _, query := range []string{"reservations", "attestations", "replicas", "disclosures", "grants", "effects"} {
		t.Run(query, func(t *testing.T) {
			s := newSourceFixture()
			before, err := s.adapter().Facts(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("injected " + query)
			s.fail[query] = cause
			after, err := s.adapter().Facts(t.Context())
			if !errors.Is(err, cause) || !strings.Contains(err.Error(), query) {
				t.Fatalf("subquery failure hidden: %v", err)
			}
			wantKnown := query != "disclosures" && query != "effects"
			if after.attentionKnown != wantKnown {
				t.Fatalf("attention completeness=%v for %s", after.attentionKnown, query)
			}
			after.attentionKnown = before.attentionKnown
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("partial facts discarded: before=%+v after=%+v", before, after)
			}
			m := fixture(t)
			m.src.Ledger = s.adapter()
			snap := m.Snapshot(t.Context())
			if !strings.Contains(sourceHealth(t, snap, "ledger-facts").Error, query) || len(snap.Inbox) != 2 {
				t.Fatal("snapshot lost error or available inbox entries")
			}
		})
	}
}

func TestUnreadActivityDoesNotBecomeIdle(t *testing.T) {
	s := newSourceFixture()
	s.disclosures, s.effects = nil, nil
	s.fail["live"] = errors.New("live query failed")
	m := fixture(t)
	m.src.Ledger = s.adapter()
	snap := m.Snapshot(t.Context())
	for _, task := range snap.Tasks {
		if task.Execution != "unknown" || task.Lane != "unknown" {
			t.Fatalf("unread activity became idle: %+v", task)
		}
	}
	for _, agent := range snap.Agents {
		if agent.ActivityKnown == nil || *agent.ActivityKnown {
			t.Fatalf("agent activity claimed complete: %+v", agent)
		}
	}
	for _, p := range snap.Projects {
		for _, ws := range p.Workspaces {
			if ws.ActivityKnown == nil || *ws.ActivityKnown {
				t.Fatalf("workspace activity claimed complete: %+v", ws)
			}
		}
	}
	// A partial live child is positive evidence for its ancestors even
	// though the remaining activity list is unknown.
	s.live = []attempt.Record{{Spec: attempt.Spec{ID: "live", Agent: "local", TaskID: "2", Workspace: project.Workspace{Path: "/work/p"}}, State: attempt.Running}}
	snap = m.Snapshot(t.Context())
	for _, task := range snap.Tasks {
		if task.Execution != "running" || task.Lane != "running" {
			t.Fatalf("known child execution lost during partial failure: %+v", task)
		}
	}
	if snap.Projects[0].Workspaces[0].Busy != true || *snap.Projects[0].Workspaces[0].ActivityKnown {
		t.Fatal("partial positive activity was lost or marked complete")
	}
	delete(s.fail, "live")
	s.live = nil
	snap = m.Snapshot(t.Context())
	for _, task := range snap.Tasks {
		if task.Execution != "idle" || task.Lane != "pending" {
			t.Fatalf("healthy empty result did not restore idle: %+v", task)
		}
	}
}

func TestUnreadAttentionDoesNotBecomeNoPendingWork(t *testing.T) {
	for _, query := range []string{"disclosures", "effects", "replicas"} {
		s := newSourceFixture()
		s.disclosures, s.effects = nil, nil
		s.fail[query] = errors.New("injected")
		m := fixture(t)
		m.src.Ledger = s.adapter()
		snap := m.Snapshot(t.Context())
		if (sourceHealth(t, snap, "ledger-attention").Error == "") != (query == "replicas") {
			t.Fatalf("%s failure gave incorrect inbox completeness", query)
		}
		for _, task := range snap.Tasks {
			want := "unknown"
			if query == "replicas" {
				want = "pending"
			}
			if task.Execution != "idle" || task.Lane != want {
				t.Fatalf("%s failure derived %+v", query, task)
			}
		}
		body, err := json.Marshal(snap.Facts)
		if err != nil || strings.Contains(string(body), "null") || strings.Contains(string(body), "attentionKnown") {
			t.Fatalf("JSON facts shape changed: %s err=%v", body, err)
		}
	}
}

func TestAllQueryFailuresAreAggregatedWithoutOverwriting(t *testing.T) {
	s := newSourceFixture()
	queries := []string{"live", "projects", "landings/p", "landings/q", "usage", "reservations", "attestations", "replicas", "disclosures", "grants", "effects"}
	for _, query := range queries {
		s.fail[query] = errors.New("injected " + query)
	}
	m := fixture(t)
	m.src.Ledger = s.adapter()
	snap := m.Snapshot(t.Context())
	message := sourceHealth(t, snap, "ledger").Error
	for _, query := range queries {
		if !strings.Contains(message, "injected "+query) {
			t.Fatalf("query %s missing from %q", query, message)
		}
	}
	if len(snap.Inbox) != 2 {
		t.Fatal("available attention disappeared")
	}
}

func TestUnconfiguredLedgerDoesNotClaimIdle(t *testing.T) {
	m := fixture(t)
	snap := m.Snapshot(t.Context())
	if sourceHealth(t, snap, "ledger-live").Wired {
		t.Fatal("missing ledger claimed wired")
	}
	for _, task := range snap.Tasks {
		if task.Execution != "unknown" || task.Lane != "unknown" {
			t.Fatalf("unconfigured ledger claimed idle: %+v", task)
		}
	}
	m.src.Ledger = Ledger{}
	snap = m.Snapshot(t.Context())
	if sourceHealth(t, snap, "ledger-live").Error == "" || sourceHealth(t, snap, "ledger-facts").Error == "" {
		t.Fatal("unconfigured services claimed healthy")
	}
}

func TestClosedLedgerCannotProduceAHealthyIdleSnapshot(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		book.Close()
		t.Fatal(err)
	}
	m := fixture(t)
	m.src.Ledger = Ledger{Book: book, Attempts: attempt.New(book), Artifacts: artifact.New(t.TempDir(), book, projects, nil), Projects: projects, Intents: intent.New(book)}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot(t.Context())
	for _, name := range []string{"ledger", "ledger-live", "ledger-projects", "ledger-landings", "ledger-facts", "ledger-attention", "ledger-usage"} {
		if sourceHealth(t, snap, name).Error == "" {
			t.Fatalf("closed database reported %s healthy", name)
		}
	}
	for _, task := range snap.Tasks {
		if task.Execution != "unknown" || task.Lane != "unknown" {
			t.Fatalf("closed database reported idle: %+v", task)
		}
	}
}

func TestSnapshotObservesPendingEffectsWithoutRecoveryWrites(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	old := intent.New(book)
	it, err := old.Claim(t.Context(), "1", "attempt", "send", []byte(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Dispatched(t.Context(), it.ID); err != nil {
		t.Fatal(err)
	}
	before, _, err := book.Operation(t.Context(), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := fixture(t)
	// A new service has no live provider call for this dispatched intent.
	m.src.Ledger = Ledger{Book: book, Intents: intent.New(book)}
	for range 2 {
		snap := m.Snapshot(t.Context())
		if len(snap.Facts.Effects) != 1 || len(snap.Inbox) != 1 || snap.Facts.Effects[0].Error == "" {
			t.Fatalf("pending effect missing from read model: %+v", snap.Facts.Effects)
		}
	}
	after, _, err := book.Operation(t.Context(), it.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot performed recovery write: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestUnsettledWriterRemainsOccupiedAndNeedsIndependentConfirmation(t *testing.T) {
	s := newSourceFixture()
	s.disclosures, s.effects = nil, nil
	s.live = []attempt.Record{{
		Spec:  attempt.Spec{ID: "quarantined", Agent: "local", TaskID: "2", Project: "p", Workspace: project.Workspace{Path: "/work/p"}},
		State: attempt.Running, Unsettled: true, Error: "original process exit was not confirmed", StartedAt: time.Now().Add(-time.Minute),
	}}
	m := fixture(t)
	m.src.Ledger = s.adapter()
	m.activity = map[string]Activity{"local": {Agent: "local", TaskID: "2", Tool: "old-tool", Detail: "still compiling", At: time.Now()}}
	snap := m.Snapshot(t.Context())
	if len(snap.Attempts) != 1 || !snap.Attempts[0].Unsettled || snap.Attempts[0].Error != s.live[0].Error {
		t.Fatalf("quarantine evidence missing: %+v", snap.Attempts)
	}
	if len(snap.Inbox) != 1 || snap.Inbox[0].Type != "writer" || snap.Inbox[0].Resolvable || len(snap.Inbox[0].Choices) != 0 {
		t.Fatalf("writer was hidden or presented as directly resolvable: %+v", snap.Inbox)
	}
	request := snap.Inbox[0]
	if request.AttemptID != "quarantined" || request.Node != "hub-1" || request.Workspace != "/work/p" || len(snap.Facts.Effects) != 0 {
		t.Fatalf("writer identity confused with an external effect: request=%+v effects=%+v", request, snap.Facts.Effects)
	}
	for _, task := range snap.Tasks {
		if task.Execution != "unknown" || task.Attention != 1 || task.Lane != "needs_you" {
			t.Fatalf("quarantine failed to roll up independently of execution: %+v", task)
		}
	}
	if !snap.Projects[0].Workspaces[0].Busy {
		t.Fatal("unconfirmed writer released workspace occupancy")
	}
	for _, agent := range snap.Agents {
		if agent.ID != "local" {
			continue
		}
		if agent.Busy != 1 || len(agent.Activities) != 1 || agent.Activities[0].Kind != "writer" || agent.Activities[0].Tool != "" || strings.Contains(agent.Activities[0].Detail, "compiling") {
			t.Fatalf("quarantined writer reused stale live progress: %+v", agent)
		}
	}

	// Another branch in the tree can still be demonstrably executing.
	sibling, err := m.src.Tasks.Create(task.Task{Goal: "healthy sibling", Parent: "1", Member: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	s.live = append(s.live, attempt.Record{Spec: attempt.Spec{ID: "healthy", Agent: "builder", TaskID: sibling.ID}, State: attempt.Running})
	snap = m.Snapshot(t.Context())
	for _, task := range snap.Tasks {
		want, attention := ExecutionUnknown, 1
		if task.ID == "1" || task.ID == sibling.ID {
			want = ExecutionRunning
		}
		if task.ID == sibling.ID {
			attention = 0
		}
		if task.Execution != want || task.Attention != attention {
			t.Fatalf("known execution or inherited attention lost: %+v", task)
		}
	}
	if len(snap.Inbox) != 1 {
		t.Fatalf("healthy running work added an inbox item: %+v", snap.Inbox)
	}
}
