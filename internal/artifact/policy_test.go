package artifact

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/project"
)

type testArtifactPolicy struct {
	limits gitrepo.Limits
	review gitrepo.ReviewLimits
}

func TestStorePolicyPinsRepoAndRefreshesConsumers(t *testing.T) {
	work := t.TempDir()
	write(t, work, "a", "abcdefgh")
	write(t, work, "b", "abcdefgh")
	store, p := newStore(t, &localNode{}, project.Home{Path: work})
	var current atomic.Pointer[testArtifactPolicy]
	old := &testArtifactPolicy{gitrepo.Limits{MaxFiles: 2, MaxBytes: 16, MaxFileBytes: 8}, gitrepo.ReviewLimits{MaxFileBytes: 8, MaxEntries: 2, MaxChanges: 2, MaxDiffBytes: 4096}}
	current.Store(old)
	var calls atomic.Int64
	store.Policy = func() (gitrepo.Limits, gitrepo.ReviewLimits) {
		calls.Add(1)
		policy := current.Load()
		return policy.limits, policy.review
	}
	repo, err := store.Repo(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("gitrepo.Repo sampled policy %d times", calls.Load())
	}
	sha, _, err := repo.Snapshot(t.Context(), work, "", "initial", false)
	if err != nil {
		t.Fatal(err)
	}
	current.Store(&testArtifactPolicy{gitrepo.Limits{MaxFileBytes: 1}, gitrepo.ReviewLimits{MaxFileBytes: 3, MaxEntries: 1, MaxChanges: 1, MaxDiffBytes: 20}})
	if _, _, err := repo.Snapshot(t.Context(), work, sha, "pinned", false); err != nil {
		t.Fatalf("old repo changed limits: %v", err)
	}
	text, _, _, truncated, err := repo.File(t.Context(), sha, "a")
	if err != nil || text != "abcdefgh" || truncated {
		t.Fatalf("old repo changed review policy: %q %v %v", text, truncated, err)
	}
	fresh, err := store.Repo(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = fresh.Snapshot(t.Context(), work, sha, "new limits", false)
	var large gitrepo.TooLarge
	if !errors.As(err, &large) || large.Limit != 1 {
		t.Fatalf("fresh snapshot ignored limits: %v", err)
	}
	text, _, _, truncated, err = store.File(t.Context(), p.ID, sha, "a")
	if err != nil || text != "abc" || !truncated {
		t.Fatalf("File ignored policy: %q %v %v", text, truncated, err)
	}
	entries, truncated, err := store.Tree(t.Context(), p.ID, sha, "")
	if err != nil || len(entries) != 1 || !truncated {
		t.Fatalf("Tree ignored policy: %v %v %v", entries, truncated, err)
	}
	changes, truncated, err := store.Changes(t.Context(), p.ID, "", sha)
	if err != nil || len(changes) != 1 || !truncated {
		t.Fatalf("Changes ignored policy: %v %v %v", changes, truncated, err)
	}
	diff, truncated, err := store.FileDiff(t.Context(), p.ID, "", sha, "a")
	if err != nil || len(diff) != 20 || !truncated {
		t.Fatalf("FileDiff ignored policy: %q %v %v", diff, truncated, err)
	}
	if calls.Load() != 6 {
		t.Fatalf("consumers resampled policy: %d calls, want 6", calls.Load())
	}
}

type policySwitchNode struct {
	*localNode
	switchPolicy func()
}

func (n *policySwitchNode) Artifact(ctx context.Context, node string, req ops.Request) (ops.Result, error) {
	if req.Op == ops.Init {
		n.switchPolicy()
	}
	return n.localNode.Artifact(ctx, node, req)
}

func TestStorePolicyRemoteSnapshotPinsBeforeNodeWork(t *testing.T) {
	work := t.TempDir()
	write(t, work, "a", "abcdefgh")
	inner := &localNode{root: t.TempDir(), state: t.TempDir()}
	store, p := newStore(t, inner, project.Home{Node: "node", Path: work})
	var current atomic.Pointer[testArtifactPolicy]
	current.Store(&testArtifactPolicy{limits: gitrepo.Limits{MaxFileBytes: 8}})
	store.Policy = func() (gitrepo.Limits, gitrepo.ReviewLimits) { p := current.Load(); return p.limits, p.review }
	store.nodes = &policySwitchNode{localNode: inner, switchPolicy: func() {
		current.Store(&testArtifactPolicy{limits: gitrepo.Limits{MaxFileBytes: 1}})
	}}
	first, _, err := store.SnapshotCanonical(t.Context(), p, "", "test", "old policy")
	if err != nil {
		t.Fatalf("in-flight remote snapshot changed policy: %v", err)
	}
	_, _, err = store.SnapshotCanonical(t.Context(), p, first.ID, "test", "new policy")
	var large gitrepo.TooLarge
	if !errors.As(err, &large) || large.Limit != 1 {
		t.Fatalf("remote snapshot ignored new limits: %v", err)
	}
}

func TestStorePolicyFileContentSamplesOnceAndRefreshesTimeout(t *testing.T) {
	work := t.TempDir()
	write(t, work, "a", "abcdefgh")
	store, p := newStore(t, &localNode{}, project.Home{Path: work})
	snap, _, err := store.SnapshotCanonical(t.Context(), p, "", "test", "initial")
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[testArtifactPolicy]
	current.Store(&testArtifactPolicy{review: gitrepo.ReviewLimits{Timeout: time.Minute}})
	var calls atomic.Int64
	store.Policy = func() (gitrepo.Limits, gitrepo.ReviewLimits) {
		calls.Add(1)
		p := current.Load()
		return p.limits, p.review
	}
	data, err := store.FileContent(t.Context(), p.ID, snap.ID, "a", 8)
	if err != nil || string(data) != "abcdefgh" {
		t.Fatalf("capture ignored policy: %q %v", data, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("capture sampled policy %d times", calls.Load())
	}
	current.Store(&testArtifactPolicy{review: gitrepo.ReviewLimits{Timeout: time.Nanosecond}})
	if _, err := store.FileContent(t.Context(), p.ID, snap.ID, "a", 8); err == nil {
		t.Fatal("new capture ignored timeout")
	}
}

func TestStorePolicyConcurrentConsumers(t *testing.T) {
	work := t.TempDir()
	write(t, work, "a", "abcdefgh")
	store, p := newStore(t, &localNode{}, project.Home{Path: work})
	snap, _, err := store.SnapshotCanonical(t.Context(), p, "", "test", "initial")
	if err != nil {
		t.Fatal(err)
	}
	policies := []*testArtifactPolicy{
		{limits: gitrepo.Limits{MaxFileBytes: 3}, review: gitrepo.ReviewLimits{MaxFileBytes: 3}},
		{limits: gitrepo.Limits{MaxFileBytes: 8}, review: gitrepo.ReviewLimits{MaxFileBytes: 8}},
	}
	var current atomic.Pointer[testArtifactPolicy]
	current.Store(policies[0])
	store.Policy = func() (gitrepo.Limits, gitrepo.ReviewLimits) { p := current.Load(); return p.limits, p.review }
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				current.Store(policies[i%2])
			}
		}
	})
	defer func() { close(stop); writer.Wait() }()
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 5 {
				repo, err := store.Repo(t.Context(), p.ID)
				if err != nil {
					t.Error(err)
					return
				}
				text, _, _, _, err := repo.File(t.Context(), snap.ID, "a")
				if err != nil || int64(len(text)) != repo.Limits.MaxFileBytes {
					t.Errorf("torn policy in actual read: bytes=%d limits=%+v err=%v", len(text), repo.Limits, err)
				}
			}
		})
	}
	readers.Wait()
}
