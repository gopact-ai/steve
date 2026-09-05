package ability

import (
	"testing"
	"time"
)

// Partition hands a judge only the clauses made entirely of atoms it owns;
// a clause mixing owned and foreign atoms stays with the caller, and the
// two halves together are still the whole requirement.
func TestPartitionSplitsByOwnership(t *testing.T) {
	req, err := Compile([]string{"tool:docker", "model:gpt*", "tool:git|model:claude*", "hardware:gpu", "!tool:podman"})
	if err != nil {
		t.Fatal(err)
	}
	mine, rest := Partition(req, NodeOwned)
	if got := Text(mine); got != "tool:docker hardware:gpu !tool:podman" {
		t.Fatalf("node-owned clauses = %q", got)
	}
	if got := Text(rest); got != "model:gpt* tool:git|model:claude*" {
		t.Fatalf("remaining clauses = %q", got)
	}
	if mine, rest := Partition(Requirement{}, NodeOwned); !mine.Empty() || !rest.Empty() {
		t.Fatal("an empty requirement partitions into nothing")
	}
}

// An admission carries the verdict, the first definite failure as its
// code, and the revision of the snapshot it was judged on.
func TestAdmissionOfRecordsTheRevisionAndCode(t *testing.T) {
	now := time.Now().UTC()
	snap := &Snapshot{Schema: Schema, Node: "node-a", Generation: 7, Sequence: 3,
		Coverage: map[Kind]Coverage{Tool: Complete},
		Offers: []Capability{
			{Kind: Tool, ID: "docker", Evidence: []Evidence{{Kind: Observed, OK: true, At: now}}},
		}}
	if err := Validate(snap); err != nil {
		t.Fatal(err)
	}
	ok, _ := Compile([]string{"tool:docker"})
	adm := AdmissionOf(Match(ok, snap, "", now), snap, SourceNode, now)
	if !adm.OK() || adm.Code != CodeAdmitted || adm.Generation != 7 || adm.Sequence != 3 || adm.Digest != snap.Digest || adm.Node != "node-a" {
		t.Fatalf("admitted = %+v", adm)
	}
	missing, _ := Compile([]string{"tool:docker", "tool:podman"})
	adm = AdmissionOf(Match(missing, snap, "", now), snap, SourceNode, now)
	if !adm.Refused() || adm.Code != CodeAbsent || adm.Unmet() == "" {
		t.Fatalf("refused = %+v", adm)
	}
	unsure, _ := Compile([]string{"credential:prod"})
	adm = AdmissionOf(Match(unsure, snap, "codex", now), snap, SourceHub, now)
	if adm.OK() || adm.Refused() || adm.Code != CodeGated {
		t.Fatalf("unsure = %+v", adm)
	}
}
