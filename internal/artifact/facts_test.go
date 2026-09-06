package artifact

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestReplicasFollowTheNodeGeneration(t *testing.T) {
	ctx := context.Background()
	node := &localNode{root: t.TempDir(), state: t.TempDir(), gen: 1}
	canonical := t.TempDir()
	write(t, canonical, "f", "0")
	store, _ := newStore(t, node, project.Home{Path: canonical})
	ws, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Owner: "att-1"})
	if err != nil {
		t.Fatal(err)
	}
	base := ws.Base
	replicas, _ := store.Replicas(ctx, base)
	if len(replicas) != 1 || replicas[0].State != ReplicaVerified || replicas[0].Generation != 1 {
		t.Fatalf("replica after first materialise = %+v", replicas)
	}
	// The node reconnects as a new generation: the copy is quarantined,
	// checked, and found present — verified again under the new generation.
	node.gen = 2
	if _, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Base: base, Owner: "att-2"}); err != nil {
		t.Fatal(err)
	}
	r, _ := store.replica(ctx, base, "node-a")
	if r.State != ReplicaVerified || r.Generation != 2 {
		t.Fatalf("replica after reconnect = %+v", r)
	}
	// The record is trusted within a generation: forge it to "verified"
	// for an artifact the node does not have and the checkout fails,
	// which is the honest outcome of a lying record — not a silent push.
	store.setReplica(ctx, "0123456789abcdef0123456789abcdef01234567", "node-a", 2, ReplicaVerified, "forged")
	if _, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Base: "0123456789abcdef0123456789abcdef01234567", Owner: "att-3"}); err == nil {
		t.Fatal("a forged replica record materialised nothing and reported success")
	}
}

func TestAttestationsAreDurableFacts(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
	if _, err := store.Attest(ctx, Attestation{Artifact: "a1", By: "att-1"}); err == nil {
		t.Fatal("an attestation without a verdict was recorded")
	}
	a, err := store.Attest(ctx, Attestation{Artifact: "a1", Project: "p", Step: "s", Kind: "command", Verifier: "go test", By: "att-1", Verdict: "pass"})
	if err != nil || len(a.Receipts) != 1 || a.At.IsZero() {
		t.Fatalf("attest = %+v err=%v", a, err)
	}
	ok, _ := store.Attested(ctx, "a1", "att-1")
	if !ok {
		t.Fatal("a passing attestation was not found")
	}
	if _, err := store.Attest(ctx, Attestation{Artifact: "a1", Kind: "agent", Verifier: "claude", By: "att-2", Verdict: "fail", Detail: "no"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.Attested(ctx, "a1", "att-2"); ok {
		t.Fatal("a failing attestation counted as attested")
	}
	if ok, _ := store.Attested(ctx, "a1", "att-9"); ok {
		t.Fatal("another attempt's attestation was borrowed")
	}
	all, _ := store.Attestations(ctx, "a1")
	if len(all) != 2 {
		t.Fatalf("attestations = %d", len(all))
	}
}

func TestLandUnderTheParentsLeaseIsFencedOnIt(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-child"})
	write(t, ws.Path, "b", "from child")
	result, _, _ := store.Publish(ctx, ws, ws.Base, "att-child", "child")

	// The parent turn holds the canonical lock in place.
	parent, err := store.ledger.Acquire(ctx, "canonical:p", "att-parent", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A plain Land cannot get the lock; landing under the parent's lease can.
	if _, err := store.Land(ctx, p, result.ID, "test"); !errors.Is(err, ledger.ErrHeld) {
		t.Fatalf("plain land while the parent holds the lock = %v", err)
	}
	land, err := store.LandUnder(ctx, p, result.ID, "task #2", parent)
	if err != nil || land.State != LandCommitted || read(t, canonical, "b") != "from child" {
		t.Fatalf("land under parent = %+v err=%v b=%q", land, err, read(t, canonical, "b"))
	}
	// The parent's lease survived: landing under it releases nothing.
	if _, err := store.ledger.Renew(ctx, parent, time.Minute); err != nil {
		t.Fatalf("the parent's lease was released by the landing: %v", err)
	}
	// A stale lease is refused before anything is written.
	stale := parent
	stale.Epoch = 99
	write(t, ws.Path, "c", "again")
	again, _, _ := store.Publish(ctx, ws, result.ID, "att-child", "child again")
	if _, err := store.LandUnder(ctx, p, again.ID, "task #2", stale); err == nil || read(t, canonical, "c") == "again" {
		t.Fatalf("a stale lease landed: err=%v c=%q", err, read(t, canonical, "c"))
	}
	// A lease for something else is not a canonical lock.
	other, _ := store.ledger.Acquire(ctx, "attempt:x", "x", time.Minute)
	if _, err := store.LandUnder(ctx, p, again.ID, "task #2", other); err == nil {
		t.Fatal("a non-canonical lease was accepted")
	}
}

func TestSealedLandingOnOldGitUsesTheLegacyMerge(t *testing.T) {
	ctx := context.Background()
	node := &localNode{root: t.TempDir(), state: t.TempDir(), level: "sealed"}
	canonical := filepath.Join(node.root, "vault")
	write(t, canonical, "a", "a0")
	write(t, canonical, "b", "b0")
	book, _ := ledger.Open(t.TempDir(), ledger.Options{})
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	_ = projects.Declare(ctx, []project.Project{{ID: "p", Level: project.LevelSealed, Home: project.Home{Node: "node-a", Path: canonical}}})
	p, _, _ := projects.Get(ctx, "p")
	store := New(filepath.Join(t.TempDir(), "artifacts"), book, projects, node)
	store.LegacyMerge = true

	ws, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Owner: "att-1"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws.Path, "a", "a1")
	result, _, _ := store.Publish(ctx, ws, ws.Base, "att-1", "step")
	// The user edited b in place meanwhile: disjoint, merges cleanly.
	write(t, canonical, "b", "b-user")
	land, err := store.Land(ctx, p, result.ID, "test")
	if err != nil || land.State != LandCommitted {
		t.Fatalf("legacy land = %+v err=%v", land, err)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b-user" {
		t.Fatalf("a=%q b=%q", read(t, canonical, "a"), read(t, canonical, "b"))
	}
	// Same path on both sides: a named conflict, nothing written.
	ws2, _ := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Base: ws.Base, Owner: "att-2"})
	write(t, ws2.Path, "b", "b-other")
	clash, _, _ := store.Publish(ctx, ws2, ws.Base, "att-2", "clash")
	var conflict Conflict
	if _, err := store.Land(ctx, p, clash.ID, "test"); !errors.As(err, &conflict) || len(conflict.Paths) != 1 || conflict.Paths[0] != "b" {
		t.Fatalf("legacy conflict = %v", err)
	}
	if read(t, canonical, "b") != "b-user" {
		t.Fatal("a conflicted legacy landing wrote to the canonical")
	}
}

func TestDeriveLowersALabelOnlyWithApprovalAndNewContent(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "a", "secret")
	book, _ := ledger.Open(t.TempDir(), ledger.Options{})
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	// Internal data on an internal hub; lowering it to public is the case.
	_ = projects.Declare(ctx, []project.Project{{ID: "p", Level: project.LevelInternal, Home: project.Home{Path: canonical}}})
	store := New(filepath.Join(t.TempDir(), "artifacts"), book, projects, &localNode{})
	ws, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws.Path, "a", "redacted")
	derived, _, _ := store.Publish(ctx, ws, ws.Base, "att-1", "redact")
	if _, err := store.Derive(ctx, ws.Base, ws.Base, project.LevelPublic, "att-1", "appr-1"); err == nil {
		t.Fatal("an artifact was derived from itself")
	}
	if _, err := store.Derive(ctx, ws.Base, derived.ID, project.LevelPublic, "att-1", ""); err == nil {
		t.Fatal("a label was lowered without approval")
	}
	d, err := store.Derive(ctx, ws.Base, derived.ID, project.LevelPublic, "att-1", "appr-1")
	if err != nil || d.Label != project.LevelPublic {
		t.Fatalf("derive = %+v err=%v", d, err)
	}
	m, _, _ := store.Manifest(ctx, derived.ID)
	if m.Label != project.LevelPublic {
		t.Fatalf("derived manifest label = %s", m.Label)
	}
}

func TestLandUnderLiveParentRequiresItsExactLeaseNotSourceMetadata(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	write(t, canonical, "a", "before")
	s, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, err := s.Materialize(ctx, project.Request{Project: p.ID, Isolated: true, Owner: "child"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws.Path, "b", "child result")
	result, _, err := s.Publish(ctx, ws, ws.Base, "child", "child")
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(s.ledger)
	parent, err := attempts.Open(ctx, attempt.Spec{ID: "parent", Kind: attempt.KindChat, Project: p.ID, Workspace: project.Workspace{ID: "canonical:" + p.ID, Project: p.ID, Kind: project.KindCanonical, Path: canonical}, Scope: attempt.ScopeUnrestricted})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		if _, err := attempts.Advance(ctx, parent.ID, phase, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	var lease ledger.Lease
	for _, held := range parent.Leases {
		if held.Key == "canonical:"+p.ID {
			lease = held
		}
	}
	if _, err := s.Land(ctx, p, result.ID, "ordinary"); !errors.Is(err, attempt.ErrStopConfirmationRequired) {
		t.Fatalf("ordinary landing bypassed active writer: %v", err)
	}
	stale := lease
	stale.Epoch++
	if _, err := s.LandUnder(ctx, p, result.ID, "forged parent", stale); err == nil {
		t.Fatal("invalid borrowed tuple gained exemption")
	}
	land, err := s.LandUnder(ctx, p, result.ID, "authorized child", lease)
	if err != nil || land.State != LandCommitted || read(t, canonical, "b") != "child result" {
		t.Fatalf("valid parent cooperation blocked: %+v %v", land, err)
	}
	if err := s.ledger.Check(ctx, lease); err != nil {
		t.Fatal("child released live parent fence", err)
	}
}
