package memory

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRememberIdempotency(t *testing.T) {
	svc, m := newTestService(t)
	ctx := t.Context()
	who := Actor{By: "agent"}
	first, err := svc.Remember(ctx, Global, "偏好", "默认用简体中文回答。", "intent-1", who)
	if err != nil || !first.New {
		t.Fatalf("first remember: %+v, %v", first, err)
	}
	for _, text := range []string{"默认用简体中文回答。", "A retry changed its text."} {
		replay, err := svc.Remember(ctx, Global, "人", text, "intent-1", who)
		if err != nil || replay != first {
			t.Fatalf("replay: %+v, %v; want %+v", replay, err, first)
		}
	}
	second, err := svc.Remember(ctx, Global, "偏好", "默认用简体中文回答", "intent-2", who)
	if err != nil || second.ID != first.ID || second.New {
		t.Fatalf("semantic deduplication: %+v, %v", second, err)
	}
	path, _ := m.Path(Global)
	raw, err := os.ReadFile(path)
	if err != nil || strings.Count(string(raw), "<!-- m:") != 1 {
		t.Fatalf("memory must have one fact: %s, %v", raw, err)
	}
	audit, err := os.ReadFile(svc.AuditPath())
	if err != nil || strings.Count(string(audit), "\n") != 2 || !strings.Contains(string(audit), `"idempotency_key":"intent-1"`) || !strings.Contains(string(audit), `"receipt":`) {
		t.Fatalf("successful requests must be audited once: %s, %v", audit, err)
	}
	if _, err := svc.Remember(ctx, Global, "人", "A later fact changes the budget usage.", "later", who); err != nil {
		t.Fatal(err)
	}
	reopened := NewService(NewMarkdown(m.HomePath, m.Dir), svc.AuditPath())
	if got, err := reopened.Remember(ctx, Global, "", "changed again", "intent-1", who); err != nil || got != first {
		t.Fatalf("reopened service lost the original receipt: %+v, %v", got, err)
	}
	// A retry cannot undo a later forget, even with a fresh service.
	if _, err := reopened.Forget(ctx, Global, first.ID, who); err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Remember(ctx, Global, "", "默认用简体中文回答。", "intent-1", who); err != nil || got != first {
		t.Fatalf("replay after forget: %+v, %v", got, err)
	}
	items, err := reopened.List(ctx, Global)
	if err != nil || len(items) != 1 || items[0].ID == first.ID {
		t.Fatalf("retry resurrected the forgotten fact: %+v, %v", items, err)
	}
}

func TestRememberIdempotencyScopeAndExpiry(t *testing.T) {
	svc, m := newTestService(t)
	ctx := t.Context()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	who := Actor{By: "agent"}
	seen := make(map[string]bool)
	for _, scope := range []Scope{Global, ProjectScope("one"), ProjectScope("two")} {
		r, err := svc.Remember(ctx, scope, "", "a fact", "shared-key", who)
		if err != nil || !r.New || seen[r.ID] {
			t.Fatalf("scope %s: %+v, %v", scope, r, err)
		}
		seen[r.ID] = true
	}
	now = now.Add(24*time.Hour - time.Second)
	if r, err := svc.Remember(ctx, Global, "", "near expiry", "shared-key", who); err != nil || !seen[r.ID] {
		t.Fatalf("key expired early: %+v, %v", r, err)
	}
	now = now.Add(time.Second)
	if r, err := svc.Remember(ctx, Global, "", "after expiry", "shared-key", who); err != nil || seen[r.ID] || !r.New {
		t.Fatalf("expired key did not allow a new intent: %+v, %v", r, err)
	}
	files, err := m.requestFiles(Global)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := loadReceipts(files.receipts, now)
	if err != nil || len(keys) != 1 {
		t.Fatalf("expired receipts were not pruned: %+v, %v", keys, err)
	}
	if _, err := svc.Remember(ctx, Global, "", "", "failed-key", who); err == nil {
		t.Fatal("empty fact succeeded")
	}
	if r, err := svc.Remember(ctx, Global, "", "retry after failure", "failed-key", who); err != nil || !r.New {
		t.Fatalf("failure was cached: %+v, %v", r, err)
	}
}

func TestConcurrentRememberIdempotency(t *testing.T) {
	svc, m := newTestService(t)
	var wg sync.WaitGroup
	receipts := make(chan Receipt, 12)
	for i := range 12 {
		wg.Go(func() {
			// Separate services exercise the persisted lock as well as the
			// in-process lock; different texts cannot semantically dedupe.
			writer := svc
			if i%2 == 0 {
				writer = NewService(NewMarkdown(m.HomePath, m.Dir), svc.AuditPath())
			}
			r, err := writer.Remember(t.Context(), Global, "", strings.Repeat("x", i+1), "concurrent", Actor{By: "agent"})
			if err != nil {
				t.Error(err)
				return
			}
			receipts <- r
		})
	}
	wg.Wait()
	close(receipts)
	var first Receipt
	for r := range receipts {
		if first.ID == "" {
			first = r
		}
		if r != first {
			t.Fatalf("concurrent replay changed the receipt: %+v != %+v", r, first)
		}
	}
	items, err := svc.List(t.Context(), Global)
	if err != nil || len(items) != 1 {
		t.Fatalf("concurrent requests wrote more than once: %+v, %v", items, err)
	}
}

func TestRememberIdempotencyProcessRestart(t *testing.T) {
	if dir := os.Getenv("STEVE_MEMORY_RESTART_TEST"); dir != "" {
		svc := NewService(NewMarkdown("", dir), filepath.Join(dir, "audit.jsonl"))
		r, err := svc.Remember(context.Background(), ProjectScope("test"), "", "first process", "restart", Actor{By: "agent"})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "receipt.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRememberIdempotencyProcessRestart$")
	cmd.Env = append(os.Environ(), "STEVE_MEMORY_RESTART_TEST="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("first process: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var first Receipt
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	svc := NewService(NewMarkdown("", dir), filepath.Join(dir, "audit.jsonl"))
	got, err := svc.Remember(t.Context(), ProjectScope("test"), "", "second process", "restart", Actor{By: "agent"})
	if err != nil || got != first {
		t.Fatalf("receipt did not survive the process exit: %+v, %v; want %+v", got, err, first)
	}
	items, err := svc.List(t.Context(), ProjectScope("test"))
	if err != nil || len(items) != 1 || items[0].Text != "first process" {
		t.Fatalf("restarted process wrote again: %+v, %v", items, err)
	}
}
