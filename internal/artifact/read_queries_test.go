package artifact

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func recentReadStore(t testing.TB) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return New(t.TempDir(), book, nil, nil), dir
}

func seedRecentHistory(t testing.TB, s *Store, n int) {
	t.Helper()
	// Payload time intentionally disagrees with storage time. One old record
	// has a decode error: a bounded recent read must not load old payloads.
	if _, err := s.ledger.DB().Exec(`WITH RECURSIVE seq(n) AS
		(SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < ?)
		INSERT INTO operations(id,kind,state,revision,incarnation,data,created_at,updated_at)
		SELECT printf('landing-%06d',n),'landing','committed',1,1,
			json_object('id',printf('landing-%06d',n),'project','p-'||(n%200),
				'started_at','2026-01-01T00:00:00.'||rtrim(printf('%09d',n),'0')||'Z',
				'state','proposed','round',CASE WHEN n=1 THEN 'old-corruption' ELSE 1 END),
			'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM seq`, n); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{attestationKind, replicaKind} {
		if _, err := s.ledger.DB().Exec(`WITH RECURSIVE seq(n) AS
			(SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < ?)
			INSERT INTO bindings(kind,id,data,updated_at)
			SELECT ?,printf('fact-%06d',n),
				json_object('id',printf('fact-%06d',n),'artifact',CASE WHEN n=1 THEN 17 ELSE printf('artifact-%06d',n) END,
					'at','2026-01-01T00:00:00.'||rtrim(printf('%09d',n),'0')||'Z'),
				'2026-01-01T00:00:00Z' FROM seq`, n, kind); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecentArtifactReadsStayBoundedAsHistoryGrows(t *testing.T) {
	for _, n := range []int{10000, 100000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s, dir := recentReadStore(t)
			seedRecentHistory(t, s, n)
			lands, err := s.RecentLandings(t.Context())
			if err != nil || len(lands) != 20 {
				t.Fatalf("recent landings=%d err=%v", len(lands), err)
			}
			for i, land := range lands {
				if land.ID != fmt.Sprintf("landing-%06d", n-i) || land.State != LandCommitted {
					t.Fatalf("global landing rank %d: %+v", i, land)
				}
			}
			as, err := s.RecentAttestations(t.Context())
			if err != nil || len(as) != 30 || as[0].ID != fmt.Sprintf("fact-%06d", n-29) || as[29].ID != fmt.Sprintf("fact-%06d", n) {
				t.Fatalf("recent attestations=%+v err=%v", as, err)
			}
			rs, err := s.RecentReplicas(t.Context())
			if err != nil || len(rs) != 40 || rs[0].Artifact != fmt.Sprintf("artifact-%06d", n-39) || rs[39].Artifact != fmt.Sprintf("artifact-%06d", n) {
				t.Fatalf("recent replicas=%+v err=%v", rs, err)
			}
			diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer diagnostic.Close()
			for _, query := range []struct {
				name, sql, index string
				args             []any
				limit            int
			}{
				{"landings", recentLandingsQuery, "operations_recent_landings", nil, 20},
				{"attestations", recentFactsQuery, "bindings_recent_artifact_facts", []any{attestationKind, 30}, 30},
				{"replicas", recentFactsQuery, "bindings_recent_artifact_facts", []any{replicaKind, 40}, 40},
			} {
				t.Run(query.name, func(t *testing.T) {
					rows, err := diagnostic.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.sql, query.args...)
					if err != nil {
						t.Fatal(err)
					}
					var plan []string
					for rows.Next() {
						var id, parent, unused int
						var detail string
						if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
							t.Fatal(err)
						}
						plan = append(plan, detail)
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					rows.Close()
					explain := strings.Join(plan, "\n")
					if !strings.Contains(explain, query.index) || strings.Contains(explain, "TEMP B-TREE") {
						t.Fatalf("recent query must read the ordered index without sorting history:\n%s", explain)
					}
					rows, err = diagnostic.QueryContext(t.Context(), query.sql, query.args...)
					if err != nil {
						t.Fatal(err)
					}
					count := 0
					for rows.Next() {
						count++
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					rows.Close()
					if count != query.limit {
						t.Fatalf("SQL read %d rows; bound=%d", count, query.limit)
					}
					t.Logf("history=%d rows=%d plan=%s", n, count, explain)
				})
			}
		})
	}
}

func TestRecentArtifactTimeOrderPreservesNanosOffsetsAndEffectiveLandingTime(t *testing.T) {
	s, _ := recentReadStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, offset := range []time.Duration{0, time.Nanosecond, 10 * time.Nanosecond, 100 * time.Millisecond, time.Second, time.Second} {
		id := fmt.Sprintf("row-%d", i)
		at := base.Add(offset)
		if i%2 == 1 {
			at = at.In(time.FixedZone("east", 8*3600))
		} else {
			at = at.In(time.FixedZone("west", -5*3600))
		}
		land := Landing{ID: id, Project: "p", StartedAt: at.UTC()}
		if i == 4 {
			land.StartedAt, land.EndedAt = base.Add(-time.Hour), at.UTC()
		}
		if _, err := s.ledger.Begin(t.Context(), id, landKind, LandCommitted, "test", land); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Attest(t.Context(), Attestation{ID: id, Artifact: id, At: at, By: "test", Verdict: "pass"}); err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return at }
		s.setReplica(t.Context(), id, "node", 1, ReplicaVerified, "")
	}
	lands, err := s.RecentLandings(t.Context())
	var got []string
	for _, land := range lands {
		got = append(got, land.ID)
	}
	if want := []string{"row-5", "row-4", "row-3", "row-2", "row-1", "row-0"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("effective landing order=%v err=%v", got, err)
	}
	as, err := s.RecentAttestations(t.Context())
	got = nil
	for _, a := range as {
		got = append(got, a.ID)
	}
	want := []string{"row-0", "row-1", "row-2", "row-3", "row-4", "row-5"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("attestation time order=%v err=%v", got, err)
	}
	rs, err := s.RecentReplicas(t.Context())
	got = nil
	for _, r := range rs {
		got = append(got, r.Artifact)
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("replica time order=%v err=%v", got, err)
	}
}

func TestAttestStoresRecentTimestampInUTC(t *testing.T) {
	s, _ := recentReadStore(t)
	at := time.Date(2026, 1, 1, 8, 0, 0, 10, time.FixedZone("east", 8*3600))
	a, err := s.Attest(t.Context(), Attestation{ID: "offset", Artifact: "a", By: "test", Verdict: "pass", At: at})
	if err != nil {
		t.Fatal(err)
	}
	var stored Attestation
	if found, err := s.ledger.GetBinding(t.Context(), attestationKind, a.ID, &stored); !found || err != nil {
		t.Fatalf("attestation read=%v %v", found, err)
	}
	if !stored.At.Equal(at) || stored.At.Location() != time.UTC || a.At.Location() != time.UTC {
		t.Fatalf("attestation timestamp is not canonical UTC: returned=%s stored=%s", a.At, stored.At)
	}
}

func TestRecentArtifactReadsRejectNonUTCTimeWithoutSilentlyMisordering(t *testing.T) {
	s, _ := recentReadStore(t)
	at := time.Date(2026, 1, 1, 8, 0, 0, 0, time.FixedZone("east", 8*3600))
	if _, err := s.ledger.Begin(t.Context(), "non-utc", landKind, LandCommitted, "test", Landing{ID: "non-utc", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.ledger.PutBinding(t.Context(), attestationKind, "non-utc", Attestation{At: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.ledger.PutBinding(t.Context(), replicaKind, "non-utc", Replica{At: at}); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(context.Context) error{
		func(ctx context.Context) error {
			got, err := s.RecentLandings(ctx)
			if len(got) != 0 {
				t.Error("non-UTC landing trusted")
			}
			return err
		},
		func(ctx context.Context) error {
			got, err := s.RecentAttestations(ctx)
			if len(got) != 0 {
				t.Error("non-UTC attestation trusted")
			}
			return err
		},
		func(ctx context.Context) error {
			got, err := s.RecentReplicas(ctx)
			if len(got) != 0 {
				t.Error("non-UTC replica trusted")
			}
			return err
		},
	} {
		if err := read(t.Context()); err == nil || !strings.Contains(err.Error(), "non-utc") || !strings.Contains(err.Error(), "UTC") {
			t.Fatalf("non-UTC row was not located: %v", err)
		}
	}
}

func TestRecentArtifactCorruptionKeepsValidCandidatesWithoutRefilling(t *testing.T) {
	s, _ := recentReadStore(t)
	seedRecentHistory(t, s, 100)
	for _, kind := range []string{landKind, attestationKind, replicaKind} {
		for _, row := range []struct{ id, data string }{
			{"bad-syntax", `{"private":"private-payload"`},
			{"bad-type", `{"id":["private-payload"],"artifact":["private-payload"],"started_at":"9999-12-31T23:59:59Z","at":"9999-12-31T23:59:59Z"}`},
		} {
			if kind == landKind {
				if _, err := s.ledger.Begin(t.Context(), row.id, kind, LandCommitted, "test", Landing{}); err != nil {
					t.Fatal(err)
				}
				corruptArtifactRow(t, s.ledger, "operations", kind, row.id, row.data)
			} else {
				if err := s.ledger.PutBinding(t.Context(), kind, row.id, struct{}{}); err != nil {
					t.Fatal(err)
				}
				corruptArtifactRow(t, s.ledger, "bindings", kind, row.id, row.data)
			}
		}
	}
	lands, err := s.RecentLandings(t.Context())
	checkArtifactCorruption(t, err, landKind, "bad-syntax", "bad-type")
	if len(lands) != 18 {
		t.Fatalf("landing decode escaped its 20-row window: %d", len(lands))
	}
	as, err := s.RecentAttestations(t.Context())
	checkArtifactCorruption(t, err, attestationKind, "bad-syntax", "bad-type")
	if len(as) != 28 {
		t.Fatalf("attestation decode escaped its 30-row window: %d", len(as))
	}
	rs, err := s.RecentReplicas(t.Context())
	checkArtifactCorruption(t, err, replicaKind, "bad-syntax", "bad-type")
	if len(rs) != 38 {
		t.Fatalf("replica decode escaped its 40-row window: %d", len(rs))
	}
}

func TestRecentArtifactReadsReportUnavailableSource(t *testing.T) {
	s, _ := recentReadStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, read := range []func(context.Context) error{
		func(ctx context.Context) error { _, err := s.RecentLandings(ctx); return err },
		func(ctx context.Context) error { _, err := s.RecentAttestations(ctx); return err },
		func(ctx context.Context) error { _, err := s.RecentReplicas(ctx); return err },
	} {
		if err := read(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled query reported healthy: %v", err)
		}
		if err := read(t.Context()); err != nil {
			t.Fatalf("healthy empty query failed: %v", err)
		}
	}
}

func TestRecentArtifactReadsRequireIndexes(t *testing.T) {
	s, _ := recentReadStore(t)
	for _, index := range []string{"operations_recent_landings", "bindings_recent_artifact_facts"} {
		if _, err := s.ledger.DB().Exec("DROP INDEX " + index); err != nil {
			t.Fatal(err)
		}
	}
	for _, read := range []func(context.Context) error{
		func(ctx context.Context) error { _, err := s.RecentLandings(ctx); return err },
		func(ctx context.Context) error { _, err := s.RecentAttestations(ctx); return err },
		func(ctx context.Context) error { _, err := s.RecentReplicas(ctx); return err },
	} {
		if err := read(t.Context()); err == nil || !strings.Contains(err.Error(), "no such index") {
			t.Fatalf("missing index silently fell back to a full scan: %v", err)
		}
	}
}

func TestRecentArtifactRootTypeCorruptionStaysVisible(t *testing.T) {
	s, _ := recentReadStore(t)
	seedRecentHistory(t, s, 100)
	if err := s.ledger.PutBinding(t.Context(), replicaKind, "bad-root", []string{"private-payload"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecentReplicas(t.Context())
	if err == nil || !strings.Contains(err.Error(), "bad-root") || len(got) != 39 {
		t.Fatalf("root type corruption disappeared behind the limit: count=%d err=%v", len(got), err)
	}
	if strings.Contains(err.Error(), "private-payload") {
		t.Fatal("root type error included private row contents")
	}
}

func BenchmarkRecentArtifactReadsGrowingHistory(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s, _ := recentReadStore(b)
			seedRecentHistory(b, s, n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				lands, err := s.RecentLandings(b.Context())
				if err != nil || len(lands) != 20 {
					b.Fatalf("landings=%d err=%v", len(lands), err)
				}
				as, err := s.RecentAttestations(b.Context())
				if err != nil || len(as) != 30 {
					b.Fatalf("attestations=%d err=%v", len(as), err)
				}
				rs, err := s.RecentReplicas(b.Context())
				if err != nil || len(rs) != 40 {
					b.Fatalf("replicas=%d err=%v", len(rs), err)
				}
			}
		})
	}
}
