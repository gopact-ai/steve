package artifact

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func corruptArtifactRow(t *testing.T, book *ledger.Ledger, table, kind, id, data string) {
	t.Helper()
	query := "UPDATE operations SET data = ? WHERE kind = ? AND id = ?"
	if table == "bindings" {
		query = "UPDATE bindings SET data = ? WHERE kind = ? AND id = ?"
	}
	result, err := book.DB().Exec(query, data, kind, id)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("corrupt %s %s/%s: affected=%d err=%v", table, kind, id, n, err)
	}
}

func checkArtifactCorruption(t *testing.T, err error, kind string, ids ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("corrupt ledger rows were reported as a healthy list")
	}
	for _, text := range append([]string{kind}, ids...) {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("error %q does not locate %q", err, text)
		}
	}
	var syntax *json.SyntaxError
	var wrongType *json.UnmarshalTypeError
	if !errors.As(err, &syntax) || !errors.As(err, &wrongType) {
		t.Errorf("error does not preserve both JSON causes: %v", err)
	}
	if strings.Contains(err.Error(), "private-payload") {
		t.Errorf("error exposes row contents: %v", err)
	}
}

func TestLandingsReportCorruptionWithValidRows(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, p := newStoreWith(t, &localNode{}, project.Home{Path: t.TempDir()}, ledger.Options{Now: func() time.Time { return now }})
	for _, id := range []string{"old", "bad-syntax", "other-project", "bad-type", "new"} {
		projectID := p.ID
		if id == "other-project" {
			projectID = "other"
		}
		now = now.Add(time.Minute)
		land := Landing{ID: id, Project: projectID, State: LandProposed, StartedAt: now}
		if _, err := s.ledger.Begin(t.Context(), id, landKind, LandCommitted, "test", land); err != nil {
			t.Fatal(err)
		}
	}
	corruptArtifactRow(t, s.ledger, "operations", landKind, "bad-syntax", `{"project":"p","private":"private-payload"`)
	corruptArtifactRow(t, s.ledger, "operations", landKind, "bad-type", `{"project":"p","round":"private-payload"}`)

	for _, projectID := range []string{p.ID, "missing"} {
		t.Run(projectID, func(t *testing.T) {
			got, err := s.Landings(t.Context(), projectID)
			checkArtifactCorruption(t, err, landKind, "bad-syntax", "bad-type")
			var ids []string
			for _, land := range got {
				ids = append(ids, land.ID)
				if land.State != LandCommitted {
					t.Errorf("landing state came from stale payload: %+v", land)
				}
			}
			var want []string
			if projectID == p.ID {
				want = []string{"new", "old"}
			}
			if !slices.Equal(ids, want) {
				t.Fatalf("valid landings = %v; want %v", ids, want)
			}
		})
	}
}

func TestArtifactFactsReportCorruptionWithValidRows(t *testing.T) {
	tests := []struct {
		kind  string
		value func(string, string, time.Time) any
		list  func(*Store, string) ([]string, error)
	}{
		{
			kind: attestationKind,
			value: func(id, artifactID string, at time.Time) any {
				return Attestation{ID: id, Artifact: artifactID, At: at}
			},
			list: func(s *Store, artifactID string) ([]string, error) {
				rows, err := s.Attestations(t.Context(), artifactID)
				var ids []string
				for _, row := range rows {
					ids = append(ids, row.ID)
				}
				return ids, err
			},
		},
		{
			kind: replicaKind,
			value: func(id, artifactID string, at time.Time) any {
				return Replica{Node: id, Artifact: artifactID, At: at}
			},
			list: func(s *Store, artifactID string) ([]string, error) {
				rows, err := s.Replicas(t.Context(), artifactID)
				var ids []string
				for _, row := range rows {
					ids = append(ids, row.Node)
				}
				return ids, err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			s, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
			for i, id := range []string{"new", "other", "old"} {
				artifactID := "a"
				if id == "other" {
					artifactID = "b"
				}
				if err := s.ledger.PutBinding(t.Context(), tt.kind, id, tt.value(id, artifactID, time.Unix(int64(3-i), 0))); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := tt.list(s, "a"); err != nil || !slices.Equal(got, []string{"old", "new"}) {
				t.Fatalf("healthy list = %v, %v", got, err)
			}
			for id, data := range map[string]string{
				"bad-syntax": `{"artifact":"a","private":"private-payload"`,
				"bad-type":   `{"artifact":["private-payload"]}`,
			} {
				if err := s.ledger.PutBinding(t.Context(), tt.kind, id, tt.value(id, "a", time.Time{})); err != nil {
					t.Fatal(err)
				}
				corruptArtifactRow(t, s.ledger, "bindings", tt.kind, id, data)
			}
			for _, artifactID := range []string{"", "a", "missing"} {
				got, err := tt.list(s, artifactID)
				checkArtifactCorruption(t, err, tt.kind, "bad-syntax", "bad-type")
				var want []string
				switch artifactID {
				case "":
					want = []string{"old", "other", "new"}
				case "a":
					want = []string{"old", "new"}
				}
				if !slices.Equal(got, want) {
					t.Errorf("list(%q) = %v; want %v", artifactID, got, want)
				}
			}
		})
	}
}

func TestDirectSourceRejectsCorruptReplicaList(t *testing.T) {
	s, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
	s.setReplica(t.Context(), "a", "source", 1, ReplicaVerified, "")
	s.setReplica(t.Context(), "a", "bad", 1, ReplicaVerified, "")
	corruptArtifactRow(t, s.ledger, "bindings", replicaKind, "a@bad", `{`)
	if source, ok := s.directSource(t.Context(), "a", "target"); ok || source != "" {
		t.Fatalf("partial replica list authorized a direct transfer from %q", source)
	}
}

func TestAttestedRejectsCorruptFact(t *testing.T) {
	s, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
	a, err := s.Attest(t.Context(), Attestation{Artifact: "a", By: "attempt", Verdict: "pass"})
	if err != nil {
		t.Fatal(err)
	}
	corruptArtifactRow(t, s.ledger, "bindings", attestationKind, a.ID, `{"verdict":"pass","receipts":[{}],"at":false}`)
	if passed, err := s.Attested(t.Context(), "a", "attempt"); passed || err == nil {
		t.Fatalf("corrupt fact authorized a verified result: passed=%v err=%v", passed, err)
	}
}

func TestLandOnceRejectsCorruptLanding(t *testing.T) {
	s, p := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
	const id = "corrupt-landing"
	if _, err := s.ledger.Begin(t.Context(), id, landKind, LandCommitted, "test", Landing{ID: id}); err != nil {
		t.Fatal(err)
	}
	corruptArtifactRow(t, s.ledger, "operations", landKind, id, `{`)
	if _, err := s.LandOnce(t.Context(), id, p, "a", "test"); err == nil {
		t.Fatal("corrupt committed landing was accepted")
	}
	op, found, err := s.ledger.Operation(t.Context(), id)
	if err != nil || !found || op.Revision != 1 || op.State != LandCommitted || string(op.Data) != "{" {
		t.Fatalf("rejected landing changed durable operation: %+v, %v", op, err)
	}
}
