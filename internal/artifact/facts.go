package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/project"
)

// Attestation is a fact about an artifact: who checked it, how, and what
// they found. It is recorded durably before the artifact's name is bound,
// so a bind can require it and a reader can trust it after the attempt
// that produced it is gone.
type Attestation struct {
	ID       string    `json:"id"`
	Artifact string    `json:"artifact"`
	Project  string    `json:"project"`
	TaskID   string    `json:"task_id,omitempty"`
	Step     string    `json:"step,omitempty"`
	Kind     string    `json:"kind"`     // command | agent
	Verifier string    `json:"verifier"` // the command, or the agent id
	Node     string    `json:"node,omitempty"`
	Verdict  string    `json:"verdict"` // pass | fail
	Detail   string    `json:"detail,omitempty"`
	By       string    `json:"by"` // the attempt
	At       time.Time `json:"at"`
	Receipts []Receipt `json:"receipts,omitempty"`
}

const attestationKind = "attestation"

// Attest records an attestation with the hub's receipt.
func (s *Store) Attest(ctx context.Context, a Attestation) (Attestation, error) {
	if a.Artifact == "" || a.By == "" || a.Verdict == "" {
		return a, fmt.Errorf("attestation needs an artifact, an attempt and a verdict")
	}
	if a.ID == "" {
		a.ID = a.Artifact + "/" + a.By
	}
	if a.At.IsZero() {
		a.At = s.now().UTC()
	}
	a.Receipts = []Receipt{{Place: "", At: s.now().UTC()}}
	return a, s.ledger.PutBinding(ctx, attestationKind, a.ID, a)
}

// Attested says whether the attempt's attestation of the artifact is on
// record, durable, and a pass.
func (s *Store) Attested(ctx context.Context, artifactID, by string) (bool, error) {
	var a Attestation
	ok, err := s.ledger.GetBinding(ctx, attestationKind, artifactID+"/"+by, &a)
	if err != nil || !ok {
		return false, err
	}
	return a.Verdict == "pass" && len(a.Receipts) > 0, nil
}

// Attestations lists every attestation of an artifact, oldest first.
func (s *Store) Attestations(ctx context.Context, artifactID string) ([]Attestation, error) {
	raw, err := s.ledger.Bindings(ctx, attestationKind)
	if err != nil {
		return nil, err
	}
	var out []Attestation
	for _, data := range raw {
		var a Attestation
		if err := json.Unmarshal(data, &a); err == nil && (artifactID == "" || a.Artifact == artifactID) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Replica is where a copy of an artifact's objects is, and what the hub
// knows about it. A node's generation moves every time it reconnects;
// a copy from an older generation is quarantined until it is checked
// again, because nothing that happened to that machine in between is
// known here.
type Replica struct {
	Artifact   string    `json:"artifact"`
	Node       string    `json:"node"`
	Generation int64     `json:"generation"`
	State      string    `json:"state"`
	At         time.Time `json:"at"`
	Note       string    `json:"note,omitempty"`
}

const (
	ReplicaTransferring = "transferring"
	ReplicaPresent      = "present"
	ReplicaVerified     = "verified"
	ReplicaQuarantined  = "quarantined"
	ReplicaLost         = "lost"
	ReplicaEvicted      = "evicted"
	replicaKind         = "replica"
)

func (s *Store) replica(ctx context.Context, artifactID, node string) (Replica, bool) {
	var r Replica
	ok, err := s.ledger.GetBinding(ctx, replicaKind, artifactID+"@"+node, &r)
	return r, err == nil && ok
}

func (s *Store) setReplica(ctx context.Context, artifactID, node string, generation int64, state, note string) {
	r := Replica{Artifact: artifactID, Node: node, Generation: generation, State: state, At: s.now().UTC(), Note: note}
	_ = s.ledger.PutBinding(ctx, replicaKind, artifactID+"@"+node, r)
}

// Replicas lists every replica record, optionally of one artifact.
func (s *Store) Replicas(ctx context.Context, artifactID string) ([]Replica, error) {
	raw, err := s.ledger.Bindings(ctx, replicaKind)
	if err != nil {
		return nil, err
	}
	var out []Replica
	for _, data := range raw {
		var r Replica
		if err := json.Unmarshal(data, &r); err == nil && (artifactID == "" || r.Artifact == artifactID) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// generationOf is the node's current generation, or 0 when the node side
// does not track one.
func (s *Store) generationOf(ctx context.Context, node string) int64 {
	if g, ok := s.nodes.(interface {
		Generation(ctx context.Context, node string) (int64, error)
	}); ok {
		gen, err := g.Generation(ctx, node)
		if err == nil {
			return gen
		}
	}
	return 0
}

// ensureOnNode makes sure the node holds the artifact, keeping the replica
// record honest at every step: a known verified copy of this generation is
// trusted; anything else is checked; a missing copy is pushed and checked.
func (s *Store) ensureOnNode(ctx context.Context, p project.Project, node, bare string, hub *Repo, sha string) error {
	gen := s.generationOf(ctx, node)
	if r, ok := s.replica(ctx, sha, node); ok {
		if r.State == ReplicaVerified && r.Generation == gen {
			return nil
		}
		if r.Generation != gen && r.State != ReplicaLost && r.State != ReplicaEvicted {
			s.setReplica(ctx, sha, node, gen, ReplicaQuarantined, fmt.Sprintf("node came back as generation %d", gen))
		}
	}
	if s.nodeHas(ctx, node, bare, sha) {
		s.setReplica(ctx, sha, node, gen, ReplicaVerified, "")
		return nil
	}
	if metadataOnly(p) {
		s.setReplica(ctx, sha, node, gen, ReplicaLost, "sealed artifact missing at its only place")
		return fmt.Errorf("artifact %s is not at %s, the only place sealed project %s lives", short(sha), node, p.ID)
	}
	s.setReplica(ctx, sha, node, gen, ReplicaTransferring, "")
	if err := s.push(ctx, node, bare, hub, sha, nil); err != nil {
		s.setReplica(ctx, sha, node, gen, ReplicaLost, err.Error())
		return err
	}
	s.setReplica(ctx, sha, node, gen, ReplicaPresent, "")
	if !s.nodeHas(ctx, node, bare, sha) {
		s.setReplica(ctx, sha, node, gen, ReplicaLost, "pushed but not found afterwards")
		return fmt.Errorf("artifact %s did not arrive at %s", short(sha), node)
	}
	s.setReplica(ctx, sha, node, gen, ReplicaVerified, "")
	return nil
}

// Derivation records that an artifact at a lower label was made from a
// higher one: content changed on purpose so it may leave the level it
// came from. Lowering below the source's label needs an approval id.
type Derivation struct {
	Source   string        `json:"source"`
	Derived  string        `json:"derived"`
	Label    project.Level `json:"label"`
	By       string        `json:"by"`
	Approval string        `json:"approval,omitempty"`
	At       time.Time     `json:"at"`
}

const derivationKind = "derivation"

// Derive labels an already published artifact as derived from another.
func (s *Store) Derive(ctx context.Context, source, derived string, label project.Level, by, approval string) (Derivation, error) {
	from, ok, err := s.Manifest(ctx, source)
	if err != nil || !ok {
		return Derivation{}, fmt.Errorf("source artifact %s is unknown", short(source))
	}
	to, ok, err := s.Manifest(ctx, derived)
	if err != nil || !ok {
		return Derivation{}, fmt.Errorf("derived artifact %s is unknown", short(derived))
	}
	if source == derived {
		return Derivation{}, fmt.Errorf("an artifact cannot be derived from itself: content must change to change label")
	}
	if !label.Valid() {
		return Derivation{}, fmt.Errorf("label %q is not a level", label)
	}
	// Lowering is when the source's level would not admit a place at the
	// new label: that needs someone's approval on record.
	if !from.Label.OrDefault().Admits(label) && approval == "" {
		return Derivation{}, fmt.Errorf("lowering %s from %s to %s needs an approval", short(derived), from.Label.OrDefault(), label)
	}
	d := Derivation{Source: source, Derived: derived, Label: label, By: by, Approval: approval, At: s.now().UTC()}
	if err := s.ledger.PutBinding(ctx, derivationKind, derived, d); err != nil {
		return Derivation{}, err
	}
	to.Label = label
	if _, err := s.record(ctx, to); err != nil {
		return Derivation{}, err
	}
	return d, nil
}
