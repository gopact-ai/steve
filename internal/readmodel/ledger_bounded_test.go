package readmodel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

type boundedArtifactFixture struct {
	*sourceFixture
	fullReads, landingReads, attestationReads, replicaReads, projectReads int
}

func (s *boundedArtifactFixture) Attestations(context.Context, string) ([]artifact.Attestation, error) {
	s.fullReads++
	return nil, errors.New("unexpected full attestation scan")
}

func (s *boundedArtifactFixture) Replicas(context.Context, string) ([]artifact.Replica, error) {
	s.fullReads++
	return nil, errors.New("unexpected full replica scan")
}

func (s *boundedArtifactFixture) Landings(context.Context, string) ([]artifact.Landing, error) {
	s.fullReads++
	return nil, errors.New("unexpected per-project landing scan")
}

func (s *boundedArtifactFixture) RecentAttestations(context.Context) ([]artifact.Attestation, error) {
	s.attestationReads++
	return []artifact.Attestation{{Artifact: "recent-attestation"}}, s.fail["attestations"]
}

func (s *boundedArtifactFixture) RecentReplicas(context.Context) ([]artifact.Replica, error) {
	s.replicaReads++
	return []artifact.Replica{{Artifact: "recent-replica"}}, s.fail["replicas"]
}

func (s *boundedArtifactFixture) RecentLandings(context.Context) ([]artifact.Landing, error) {
	s.landingReads++
	return []artifact.Landing{{ID: "recent-landing", Project: "retired", State: artifact.LandMergeConflicted, Conflict: "marked", Paths: []string{"file"}}}, s.fail["landings"]
}

func (s *boundedArtifactFixture) List(context.Context) ([]project.Project, error) {
	s.projectReads++
	return s.projects, s.fail["projects"]
}

func TestLedgerUsesOnlyBoundedArtifactReads(t *testing.T) {
	s := &boundedArtifactFixture{sourceFixture: newSourceFixture()}
	cause := errors.New("partial recent source")
	s.fail["attestations"], s.fail["replicas"], s.fail["landings"] = cause, cause, cause
	l := Ledger{Artifacts: s, Projects: s, Attempts: s.sourceFixture, Intents: s.sourceFixture}
	landings, err := l.RecentLandings(t.Context())
	if !errors.Is(err, cause) || len(landings) != 1 || landings[0].ID != "recent-landing" || !landings[0].Resolvable || len(landings[0].Files) != 1 {
		t.Errorf("bounded landing contribution lost: %+v, %v", landings, err)
	}
	facts, err := l.Facts(t.Context())
	if !errors.Is(err, cause) || len(facts.Attestations) != 1 || len(facts.Replicas) != 1 || !facts.attentionKnown {
		t.Errorf("bounded facts or independent attention completeness lost: %+v, %v", facts, err)
	}
	if s.fullReads != 0 || s.projectReads != 0 || s.landingReads != 1 || s.attestationReads != 1 || s.replicaReads != 1 {
		t.Fatalf("query counts: full=%d projects=%d landings=%d attestations=%d replicas=%d",
			s.fullReads, s.projectReads, s.landingReads, s.attestationReads, s.replicaReads)
	}
}

func TestRecentLandingsDoesNotRequireLiveProjectDeclarations(t *testing.T) {
	s := &boundedArtifactFixture{sourceFixture: newSourceFixture()}
	l := Ledger{Artifacts: s}
	got, err := l.RecentLandings(t.Context())
	if err != nil || len(got) != 1 || got[0].Project != "retired" {
		t.Fatalf("global landing history depends on active projects: %+v, %v", got, err)
	}
}

func recentLedgerFixture(t *testing.T) Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	artifacts := artifact.New(t.TempDir(), book, projects, nil)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 50 {
		id := fmt.Sprintf("row-%02d", i)
		projectID := "p"
		if i%2 == 1 {
			projectID = "retired"
		}
		at := recent.Add(time.Duration(i) * time.Second)
		if _, err := book.Begin(t.Context(), id, "landing", artifact.LandCommitted, "test",
			artifact.Landing{ID: id, Project: projectID, StartedAt: at}); err != nil {
			t.Fatal(err)
		}
		if _, err := artifacts.Attest(t.Context(), artifact.Attestation{ID: id, Artifact: id, At: at, By: "test", Verdict: "pass"}); err != nil {
			t.Fatal(err)
		}
		for _, binding := range []struct {
			kind string
			data any
		}{
			{"replica", artifact.Replica{Artifact: id, Node: "node", At: at}},
			{"pending-landing", artifact.Pending{Project: "p", Artifact: id, At: old,
				Blocked: &artifact.Blocked{Landing: "old-" + id, Marked: "marked", Paths: []string{"file"}, At: old}}},
			{"reservation", attempt.Reservation{ID: id}},
			{"grant", project.Grant{Project: "p", Principal: id, Role: project.RoleRead}},
		} {
			if err := book.PutBinding(t.Context(), binding.kind, id, binding.data); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := book.Begin(t.Context(), "disclosure-"+id, "disclosure-request", project.DisclosureProposed, "test",
			project.DisclosureRequest{ID: "disclosure-" + id, Project: "p", TaskID: "1", ProposedAt: old}); err != nil {
			t.Fatal(err)
		}
		if _, err := book.Begin(t.Context(), "effect-"+id, "intent", string(intent.Unknown), "test",
			intent.Intent{ID: "effect-" + id, TaskID: "1", Tool: "send", At: old}); err != nil {
			t.Fatal(err)
		}
	}
	return Ledger{Book: book, Projects: projects, Artifacts: artifacts, Attempts: attempt.New(book), Intents: intent.New(book)}
}

func TestBoundedArtifactViewsKeepAllUnresolvedWork(t *testing.T) {
	l := recentLedgerFixture(t)
	lands, err := l.RecentLandings(t.Context())
	if err != nil || len(lands) != 20 || lands[0].ID != "row-49" || lands[0].Project != "retired" || lands[19].ID != "row-30" {
		t.Fatalf("global top 20=%+v err=%v", lands, err)
	}
	facts, err := l.Facts(t.Context())
	if err != nil || len(facts.Attestations) != 30 || facts.Attestations[0].Artifact != "row-20" ||
		len(facts.Replicas) != 40 || facts.Replicas[0].Artifact != "row-10" {
		t.Fatalf("bounded facts=%+v err=%v", facts, err)
	}
	if len(facts.Disclosures) != 50 || len(facts.Effects) != 50 || len(facts.Reservations) != 50 || len(facts.Grants) != 50 || !facts.attentionKnown {
		t.Fatalf("old unresolved work or complete grant/reservation sets were truncated: %+v", facts)
	}
	conflicts, err := l.Conflicts(t.Context())
	if err != nil || len(conflicts) != 50 || !conflicts[0].Resolvable {
		t.Fatalf("old unresolved conflicts disappeared: count=%d err=%v", len(conflicts), err)
	}
	var before int
	if err := l.Book.DB().QueryRow("SELECT COUNT(*) FROM events").Scan(&before); err != nil {
		t.Fatal(err)
	}
	m := fixture(t)
	m.src.Ledger = l
	snap := m.Snapshot(t.Context())
	if len(snap.Landings) != 20 || len(snap.Conflicts) != 50 || len(snap.Inbox) != 100 {
		t.Fatalf("snapshot lost unresolved work: landings=%d conflicts=%d inbox=%d", len(snap.Landings), len(snap.Conflicts), len(snap.Inbox))
	}
	var after int
	if err := l.Book.DB().QueryRow("SELECT COUNT(*) FROM events").Scan(&after); err != nil || before != after {
		t.Fatalf("read view changed journal: %d -> %d, %v", before, after, err)
	}
}

func TestBoundedSQLCorruptionPreservesPartialSourceHealth(t *testing.T) {
	l := recentLedgerFixture(t)
	for _, query := range []string{
		"UPDATE operations SET data = '{' WHERE kind = 'landing' AND id = 'row-49'",
		"UPDATE bindings SET data = '{' WHERE kind = 'attestation' AND id = 'row-49'",
		"UPDATE bindings SET data = '{' WHERE kind = 'replica' AND id = 'row-49'",
	} {
		if _, err := l.Book.DB().Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	m := fixture(t)
	m.src.Ledger = l
	snap := m.Snapshot(t.Context())
	if len(snap.Landings) != 19 || len(snap.Facts.Attestations) != 29 || len(snap.Facts.Replicas) != 39 ||
		len(snap.Conflicts) != 50 || len(snap.Inbox) != 100 {
		t.Fatalf("partial SQL contribution lost: landings=%d attestations=%d replicas=%d conflicts=%d inbox=%d",
			len(snap.Landings), len(snap.Facts.Attestations), len(snap.Facts.Replicas), len(snap.Conflicts), len(snap.Inbox))
	}
	for _, group := range []string{"ledger-landings", "ledger-facts"} {
		if !strings.Contains(sourceHealth(t, snap, group).Error, "row-49") {
			t.Errorf("%s did not locate selected corruption", group)
		}
	}
	if !snap.Facts.attentionKnown || sourceHealth(t, snap, "ledger-attention").Error != "" {
		t.Fatal("informational recent corruption invalidated otherwise complete attention")
	}
	if _, err := l.Book.DB().Exec("UPDATE operations SET data = '{' WHERE kind = 'disclosure-request' AND id = 'disclosure-row-00'"); err != nil {
		t.Fatal(err)
	}
	snap = m.Snapshot(t.Context())
	if snap.Facts.attentionKnown || !strings.Contains(sourceHealth(t, snap, "ledger-attention").Error, "disclosure-row-00") ||
		len(snap.Facts.Disclosures) != 49 || len(snap.Facts.Effects) != 50 || len(snap.Inbox) != 99 {
		t.Fatal("an unread old disclosure became healthy absence or discarded other pending work")
	}
}
