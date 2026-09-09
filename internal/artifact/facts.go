package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
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
	if err := s.ledger.PutBinding(ctx, replicaKind, artifactID+"@"+node, r); err != nil {
		slog.Error(fmt.Sprintf("artifact: replica %s@%s not recorded as %s: %v", short(artifactID), node, state, err), "artifact", artifactID, "node", node)
	}
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

// generationNodes is a registry that numbers a node's incarnations: the
// generation moves when the node comes back, and a replica record made
// under an earlier one is no longer trusted.
type generationNodes interface {
	Generation(ctx context.Context, node string) (int64, error)
}

// generationOf is the node's current generation, or 0 when the node side
// does not track one.
func (s *Store) generationOf(ctx context.Context, node string) int64 {
	if g, ok := s.nodes.(generationNodes); ok {
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
	via := "hub"
	if s.Direct {
		if source, ok := s.directSource(ctx, sha, node); ok {
			if err := s.fetchDirect(ctx, p, source, node, bare, sha); err == nil {
				via = "direct from " + source
			} else {
				slog.Warn(fmt.Sprintf("artifact: direct transfer of %s %s → %s failed (%v); relaying through the hub", short(sha), source, node, err), "artifact", sha, "node", node, "project", p.ID)
			}
		}
	}
	if via == "hub" {
		if err := s.push(ctx, node, bare, hub, sha, nil); err != nil {
			s.setReplica(ctx, sha, node, gen, ReplicaLost, err.Error())
			return err
		}
	}
	s.setReplica(ctx, sha, node, gen, ReplicaPresent, via)
	if !s.nodeHas(ctx, node, bare, sha) {
		s.setReplica(ctx, sha, node, gen, ReplicaLost, "pushed but not found afterwards")
		return fmt.Errorf("artifact %s did not arrive at %s", short(sha), node)
	}
	s.setReplica(ctx, sha, node, gen, ReplicaVerified, via)
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

// directNodes is what a registry offers for node-to-node transfer.
type directNodes interface {
	Grant(ctx context.Context, node, token, name string, ttl time.Duration) error
	Fetch(ctx context.Context, node, peerAddr, token, name string) error
	PeerAddr(node string) (string, error)
}

// directSource picks a node other than target that holds a verified copy.
func (s *Store) directSource(ctx context.Context, sha, target string) (string, bool) {
	replicas, err := s.Replicas(ctx, sha)
	if err != nil {
		return "", false
	}
	for i := len(replicas) - 1; i >= 0; i-- {
		r := replicas[i]
		if r.Node != "" && r.Node != target && r.State == ReplicaVerified && r.Generation == s.generationOf(ctx, r.Node) {
			return r.Node, true
		}
	}
	return "", false
}

// fetchDirect moves an artifact from source to target without the hub in
// the data path: source bundles it, the hub grants target one fetch, target
// pulls and unbundles. Any failure falls back to the hub relay.
func (s *Store) fetchDirect(ctx context.Context, p project.Project, source, target, targetBare, sha string) error {
	direct, ok := s.nodes.(directNodes)
	if !ok {
		return fmt.Errorf("direct transfer is not available")
	}
	_, _, sourceState, err := s.nodes.Git(ctx, source)
	if err != nil {
		return err
	}
	_, _, targetState, err := s.nodes.Git(ctx, target)
	if err != nil {
		return err
	}
	name := "direct-" + short(sha) + "-" + fmt.Sprint(s.now().UnixNano()) + ".bundle"
	sourceBlob := filepath.Join(sourceState, "blobs", name)
	if _, err := s.nodes.Artifact(ctx, source, ops.Request{Op: ops.Bundle, Repo: nodeBare(sourceState, p.ID), Path: sourceBlob, Commit: sha}); err != nil {
		return fmt.Errorf("bundle on %s: %w", source, err)
	}
	defer func() {
		// The bundle is a temporary blob; one left behind costs disk, not
		// correctness, and the transfer's own error is what matters.
		_, _ = s.nodes.Artifact(ctx, source, ops.Request{Op: ops.Remove, Path: sourceBlob})
	}()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(raw[:])
	if err := direct.Grant(ctx, source, token, name, 2*time.Minute); err != nil {
		return fmt.Errorf("grant on %s: %w", source, err)
	}
	peer, err := direct.PeerAddr(source)
	if err != nil {
		return err
	}
	if err := direct.Fetch(ctx, target, peer, token, name); err != nil {
		return fmt.Errorf("fetch on %s from %s: %w", target, source, err)
	}
	targetBlob := filepath.Join(targetState, "blobs", name)
	defer func() {
		// Same as the source blob: temporary, and never worth failing over.
		_, _ = s.nodes.Artifact(ctx, target, ops.Request{Op: ops.Remove, Path: targetBlob})
	}()
	if _, err := s.nodes.Artifact(ctx, target, ops.Request{Op: ops.Unbundle, Repo: targetBare, Path: targetBlob}); err != nil {
		return fmt.Errorf("unbundle on %s: %w", target, err)
	}
	return nil
}
