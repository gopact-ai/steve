package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
)

type memoryReplicator struct {
	book         *ledger.Ledger
	reject       error
	proposals    int
	lostResponse bool
	prepareErr   error
}

func (r *memoryReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	if r.prepareErr != nil {
		return ledger.ReplicaPosition{}, r.prepareErr
	}
	v, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: v, CoordinatorEpoch: 1}, err
}

type cachedMemoryIndex struct{ reads int }

func (*cachedMemoryIndex) Name() string                      { return "test-index" }
func (*cachedMemoryIndex) Index(context.Context, Item) error { return nil }
func (*cachedMemoryIndex) Drop(context.Context, Item) error  { return nil }
func (r *cachedMemoryIndex) Recall(context.Context, Scope, string, int) ([]Hit, error) {
	r.reads++
	return []Hit{{Item: Item{Text: "cached fact"}}}, nil
}

func TestLedgerMemoryRecallCannotBypassReadAuthorityThroughIndex(t *testing.T) {
	svc, _, replica := replicatedMemory(t)
	index := &cachedMemoryIndex{}
	svc.SetRetriever(index)
	replica.prepareErr = errors.New("generation is inactive")
	if hits, _, err := svc.Recall(t.Context(), Global, "cached", 1); err == nil || len(hits) > 0 || index.reads != 0 {
		t.Fatalf("optional index bypassed ledger authority: %+v %v index reads=%d", hits, err, index.reads)
	}
}
func (r *memoryReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	r.proposals++
	if r.reject != nil {
		return nil, r.reject
	}
	result, err := r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
	if err == nil && r.lostResponse {
		return nil, errors.New("commit response lost")
	}
	return result, err
}

func replicatedMemory(t *testing.T) (*Service, *LedgerStore, *memoryReplicator) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	r := &memoryReplicator{book: book}
	if err := book.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	store := NewLedgerStore(book)
	return NewService(store, ""), store, r
}

func TestLedgerMemoryPreservesReceiptsIDsAndAuditAcrossGenerations(t *testing.T) {
	svc, store, replica := replicatedMemory(t)
	who := Actor{By: "agent", Agent: "worker", Conversation: "session-1"}
	first, err := svc.Remember(t.Context(), Global, "偏好", "默认用中文回答。", "request-1", who)
	if err != nil || !first.New {
		t.Fatalf("first memory write: %+v %v", first, err)
	}
	if _, err := svc.Remember(t.Context(), ProjectScope("project-1"), "坑", "项目约束", "project-request", who); err != nil {
		t.Fatal(err)
	}
	second := NewService(NewLedgerStore(replica.book), "")
	if replay, err := second.Remember(t.Context(), Global, "人", "changed retry", "request-1", who); err != nil || replay != first {
		t.Fatalf("new generation lost original receipt: %+v %v", replay, err)
	}
	text, err := second.Text(t.Context(), Global)
	if err != nil || !strings.Contains(text, "默认用中文回答") || strings.Contains(text, "<!--") || strings.Contains(text, "项目约束") {
		t.Fatalf("shared memory text: %q %v", text, err)
	}
	if err := second.Replace(t.Context(), Global, text+"\n- added in UI\n", Actor{By: "console"}); err != nil {
		t.Fatal(err)
	}
	items, _ := svc.List(t.Context(), Global)
	if len(items) != 2 || items[0].ID != first.ID {
		t.Fatalf("UI replacement lost stable IDs: %+v", items)
	}
	if _, err := second.Forget(t.Context(), Global, first.ID, who); err != nil {
		t.Fatal(err)
	}
	if replay, err := svc.Remember(t.Context(), Global, "", "resurrect", "request-1", who); err != nil || replay != first {
		t.Fatalf("replay after forgetting: %+v %v", replay, err)
	}
	items, _ = svc.List(t.Context(), Global)
	if len(items) != 1 || items[0].ID == first.ID {
		t.Fatal("retry resurrected forgotten memory")
	}
	audit, err := store.Audit(t.Context(), Global)
	if err != nil || len(audit) != 3 || audit[0].Actor != who || audit[0].Receipt == nil || *audit[0].Receipt != first {
		t.Fatalf("durable memory audit: %+v %v", audit, err)
	}
	if audit[1].Op != "replace" || audit[1].Actor.By != "console" || audit[2].Op != "forget" {
		t.Fatal("audit did not preserve mutation order and actors")
	}
}

func TestLedgerMemoryRejectsUncommittedFactReceiptAndAudit(t *testing.T) {
	svc, store, replica := replicatedMemory(t)
	replica.reject = errors.New("quorum unavailable")
	if receipt, err := svc.Remember(t.Context(), Global, "", "must not publish", "blocked", Actor{By: "agent"}); err == nil || receipt.ID != "" {
		t.Fatalf("rejected memory was acknowledged: %+v %v", receipt, err)
	}
	if items, err := store.List(t.Context(), Global); err != nil || len(items) != 0 {
		t.Fatalf("uncommitted fact visible: %+v %v", items, err)
	}
	if audit, err := store.Audit(t.Context(), Global); err != nil || len(audit) != 0 {
		t.Fatalf("uncommitted audit visible: %+v %v", audit, err)
	}
	replica.reject = nil
	if receipt, err := svc.Remember(t.Context(), Global, "", "retry after quorum returns", "blocked", Actor{By: "agent"}); err != nil || !receipt.New {
		t.Fatalf("rejected receipt blocked a fresh retry: %+v %v", receipt, err)
	}
}

func TestLedgerMemoryReadsDoNotAppendReplicationCommands(t *testing.T) {
	svc, store, replica := replicatedMemory(t)
	if _, err := svc.Remember(t.Context(), Global, "", "a searchable fact", "", Actor{}); err != nil {
		t.Fatal(err)
	}
	before, _ := replica.book.ReplicaVersion()
	proposals := replica.proposals
	if _, err := svc.Text(t.Context(), Global); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Snapshot(t.Context(), Global); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.List(t.Context(), Global); err != nil {
		t.Fatal(err)
	}
	if _, source, err := svc.Recall(t.Context(), Global, "searchable", 3); err != nil || source != "ledger" {
		t.Fatalf("recall source=%q error=%v", source, err)
	}
	if _, err := store.Audit(t.Context(), Global); err != nil {
		t.Fatal(err)
	}
	after, _ := replica.book.ReplicaVersion()
	if before != after || proposals != replica.proposals {
		t.Fatal("read-only memory access appended replication commands")
	}
}

func TestLedgerMemoryScopesAndIdempotencyExpiryRemainIndependent(t *testing.T) {
	svc, _, replica := replicatedMemory(t)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	first, err := svc.Remember(t.Context(), Global, "", "original global fact", "shared-key", Actor{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := svc.Remember(t.Context(), ProjectScope("work"), "", "project fact", "shared-key", Actor{})
	if err != nil || project.ID == first.ID {
		t.Fatalf("scope receipt isolation: %+v %v", project, err)
	}
	now = now.Add(idempotencyTTL)
	fresh := NewService(NewLedgerStore(replica.book), "")
	fresh.now = func() time.Time { return now }
	next, err := fresh.Remember(t.Context(), Global, "", "fact after expiry", "shared-key", Actor{})
	if err != nil || next.ID == first.ID || !next.New {
		t.Fatalf("expired receipt stayed active: %+v %v", next, err)
	}
}

func TestLedgerMemoryConcurrentServicesPreserveEveryFact(t *testing.T) {
	_, _, replica := replicatedMemory(t)
	var workers sync.WaitGroup
	for i := range 12 {
		workers.Go(func() {
			service := NewService(NewLedgerStore(replica.book), "")
			if _, err := service.Remember(t.Context(), Global, "", fmt.Sprintf("independent fact %d", i), "", Actor{By: "agent"}); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	store := NewLedgerStore(replica.book)
	items, err := store.List(t.Context(), Global)
	if err != nil || len(items) != 12 {
		t.Fatalf("concurrent services lost facts: %+v %v", items, err)
	}
	audit, err := store.Audit(t.Context(), Global)
	if err != nil || len(audit) != 12 {
		t.Fatalf("concurrent audit is incomplete: %+v %v", audit, err)
	}
}

func TestLedgerMemoryRecoversReceiptWhenCommitResponseIsLost(t *testing.T) {
	_, store, replica := replicatedMemory(t)
	now := time.Now()
	replica.lostResponse = true
	if receipt, _, err := store.RememberOnce(t.Context(), Global, "", "committed fact", "unknown-result", now); err == nil || receipt.ID != "" {
		t.Fatalf("unknown outcome reported as success: %+v %v", receipt, err)
	}
	replica.lostResponse = false
	reloaded := NewLedgerStore(replica.book)
	receipt, replayed, err := reloaded.RememberOnce(t.Context(), Global, "", "changed retry", "unknown-result", now)
	if err != nil || !replayed || receipt.ID == "" {
		t.Fatalf("committed receipt was not recovered: %+v %v %v", receipt, replayed, err)
	}
	items, err := reloaded.List(t.Context(), Global)
	if err != nil || len(items) != 1 || items[0].Text != "committed fact" {
		t.Fatalf("retry changed committed facts: %+v %v", items, err)
	}
	audit, err := reloaded.Audit(t.Context(), Global)
	if err != nil || len(audit) != 1 {
		t.Fatalf("committed memory audit duplicated: %+v %v", audit, err)
	}
}

func TestLedgerMemoryBootstrapAndSnapshotRestoreKeepOneAuthority(t *testing.T) {
	svc, store, replica := replicatedMemory(t)
	files := map[string]string{home.FileSoul: "# Soul\nShared identity", home.FileUser: "# User\nOwner preferences", home.FileMemory: "# Memory\n\n## 偏好\n- imported fact\n"}
	if changed, err := store.BootstrapHome(t.Context(), files, Actor{By: "onboard"}); err != nil || !changed {
		t.Fatalf("bootstrap: %v %v", changed, err)
	}
	if err := svc.Replace(t.Context(), Global, "# Memory\n\n- saved from UI\n", Actor{By: "console"}); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.BootstrapHome(t.Context(), files, Actor{By: "onboard"}); err != nil || changed {
		t.Fatalf("bootstrap overwrote newer content: %v %v", changed, err)
	}
	snapshot, err := replica.book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if err := restored.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := restored.AttachReplication(&memoryReplicator{book: restored}); err != nil {
		t.Fatal(err)
	}
	other := NewLedgerStore(restored)
	loaded, err := other.HomeFiles(t.Context())
	if err != nil || loaded[home.FileSoul] != files[home.FileSoul] || !strings.Contains(loaded[home.FileMemory], "saved from UI") || strings.Contains(loaded[home.FileMemory], "imported fact") {
		t.Fatalf("restored home does not use authoritative memory: %+v %v", loaded, err)
	}
	if err := other.WriteHomeFile(t.Context(), home.FileUser, "# User\nUpdated profile", Actor{By: "console"}); err != nil {
		t.Fatal(err)
	}
	loaded, _ = NewLedgerStore(restored).HomeFiles(t.Context())
	if !strings.Contains(loaded[home.FileUser], "Updated profile") {
		t.Fatal("new generation did not read profile edit")
	}
	if err := other.WriteHomeFile(t.Context(), "auth.json", "secret", Actor{}); err == nil {
		t.Fatal("identity store accepted an authentication file")
	}
}
