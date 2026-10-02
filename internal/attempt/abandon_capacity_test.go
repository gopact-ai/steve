package attempt

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestAbandonedCapacityReleasePreservesAReplacementAndItsWriterLeases(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(map[bool]string{false: "original capacity", true: "replacement"}[replaced], func(t *testing.T) {
			s, r, _, _ := abandonedRecord(t)
			lease, err := s.l.Acquire(t.Context(), endpointKey(r.Node, r.Harness)+":slot:1", r.ID, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.l.Transition(t.Context(), r.ID, string(r.State), string(r.State), "fixture", nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
				var next Record
				if err := json.Unmarshal(op.Data, &next); err != nil {
					return err
				}
				next.Leases = append(next.Leases, lease)
				return setRecordDataTx(tx, op, next)
			})
			if err != nil {
				t.Fatal(err)
			}
			if replaced {
				if err := s.l.Release(t.Context(), lease); err != nil {
					t.Fatal(err)
				}
				if _, err := s.l.Acquire(t.Context(), lease.Key, "new-execution", time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			before := map[string]ledger.Lease{}
			for _, held := range r.Leases {
				got, _, err := s.l.LeaseOf(t.Context(), held.Key)
				if err != nil {
					t.Fatal(err)
				}
				before[held.Key] = got
			}
			if _, err := s.ProjectAbandonedCapacity(t.Context(), r.ID, 1); err != nil {
				t.Fatal(err)
			}
			got, _, err := s.l.LeaseOf(t.Context(), lease.Key)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if replaced {
				want = "new-execution"
			}
			if got.Holder != want {
				t.Fatalf("capacity holder=%s want=%s", got.Holder, want)
			}
			for key, original := range before {
				got, _, err := s.l.LeaseOf(t.Context(), key)
				if err != nil || got.Holder != original.Holder || got.Epoch != original.Epoch {
					t.Fatalf("physical writer changed: %s %+v %v", key, got, err)
				}
			}
		})
	}
}

func TestLateUsageNeverChangesTheAbandonmentSnapshot(t *testing.T) {
	s, r, proof, tasks := abandonedRecord(t)
	before, _ := tasks.Get(r.TaskID)
	if err := s.MarkUnsettled(t.Context(), r.ID, "late", errForceStopUnchanged, &Usage{Input: 700, Output: 900, Reported: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(t.Context(), r.ID)
	if got.Usage != nil {
		t.Fatal("late usage converted frozen unknown usage into a report")
	}
	if _, err := s.FailWith(t.Context(), r.ID, "late-finish", "observer ended", &Usage{Input: 1000, Output: 2000, Reported: true}); err != nil {
		t.Fatal(err)
	}
	proof.Session.ProcessStopped = true
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "physical-exit", proof); err != nil {
		t.Fatal(err)
	}
	after, _ := tasks.Get(r.TaskID)
	if before.Budget != after.Budget || !after.Attempts[0].AccountingFrozenAt.Equal(before.Attempts[0].AccountingFrozenAt) {
		t.Fatal("physical stop changed frozen accounting")
	}
	samples, err := s.UsageSamples(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Usage != nil || !samples[0].EndedAt.Equal(r.Abandoned.At) {
		t.Fatalf("frozen usage sample=%+v", samples)
	}
}
