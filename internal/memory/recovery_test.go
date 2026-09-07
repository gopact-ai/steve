package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/home"
)

// stageRemember builds each durable crash boundary using the same prepared
// mutation shape as the store, while leaving its process before completion.
func stageRemember(t *testing.T, m *Markdown, scope Scope, key, text, stage string, now time.Time) Receipt {
	t.Helper()
	unlock, err := m.lock(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	before, err := m.read(scope)
	if err != nil {
		t.Fatal(err)
	}
	after, r, err := prepareRemember(scope, "", text, before)
	if err != nil {
		t.Fatal(err)
	}
	files, err := m.requestFiles(scope)
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingRemember{Scope: scope, BeforeHash: bodyHash(before), After: after, Request: requestReceipt{Scope: scope, Key: key, At: now, Receipt: r}}
	raw, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicMemoryFile(files.pending, raw); err != nil {
		t.Fatal(err)
	}
	if stage != "prepared" {
		if err := m.write(scope, after); err != nil {
			t.Fatal(err)
		}
	}
	if stage == "receipted" {
		if err := saveReceipts(files.receipts, map[requestKey]requestReceipt{{Scope: scope, Key: key}: pending.Request}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestRememberRecoversStableReceiptAtEveryCrashBoundary(t *testing.T) {
	for _, stage := range []string{"prepared", "fact-written", "receipted"} {
		t.Run(stage, func(t *testing.T) {
			for _, global := range []bool{false, true} {
				svc, m := newTestService(t)
				scope := ProjectScope("p")
				if global {
					scope = Global
				}
				first := stageRemember(t, m, scope, "key", "first fact", stage, time.Now())
				restarted := NewService(NewMarkdown(m.HomePath, m.Dir), svc.AuditPath())
				got, err := restarted.Remember(t.Context(), scope, "", "changed retry", "key", Actor{By: "agent"})
				if err != nil || got != first {
					t.Fatalf("receipt changed after %s: got=%+v want=%+v err=%v", stage, got, first, err)
				}
				items, err := restarted.List(t.Context(), scope)
				if err != nil || len(items) != 1 || items[0].Text != "first fact" {
					t.Fatalf("retry duplicated/replaced facts: %+v %v", items, err)
				}
				files, _ := m.requestFiles(scope)
				if _, err := os.Stat(files.pending); !os.IsNotExist(err) {
					t.Fatalf("pending not cleared: %v", err)
				}
			}
		})
	}
}

func TestForgetAfterCrashPreservesReceiptWithoutResurrectingFact(t *testing.T) {
	svc, m := newTestService(t)
	scope := ProjectScope("p")
	first := stageRemember(t, m, scope, "key", "forget this", "fact-written", time.Now())
	restarted := NewService(NewMarkdown(m.HomePath, m.Dir), svc.AuditPath())
	if _, err := restarted.Forget(t.Context(), scope, first.ID, Actor{By: "console"}); err != nil {
		t.Fatal(err)
	}
	restarted = NewService(NewMarkdown(m.HomePath, m.Dir), svc.AuditPath())
	got, err := restarted.Remember(t.Context(), scope, "", "a changed retry", "key", Actor{})
	if err != nil || got != first {
		t.Fatalf("forgotten request lost original receipt: %+v %v", got, err)
	}
	items, err := restarted.List(t.Context(), scope)
	if err != nil || len(items) != 0 {
		t.Fatalf("retry resurrected forgotten fact: %+v %v", items, err)
	}
}

func TestOtherMutationsRecoverPendingBeforeChangingFacts(t *testing.T) {
	for _, replace := range []bool{false, true} {
		svc, m := newTestService(t)
		scope := ProjectScope("p")
		first := stageRemember(t, m, scope, "key", "first fact", "prepared", time.Now())
		restarted := NewService(NewMarkdown(m.HomePath, m.Dir), svc.AuditPath())
		if replace {
			if err := restarted.Replace(t.Context(), scope, "# Memory\n\n- manual replacement\n", Actor{}); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := restarted.Remember(t.Context(), scope, "", "later fact", "", Actor{}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := restarted.Remember(t.Context(), scope, "", "retry", "key", Actor{})
		if err != nil || got != first {
			t.Fatalf("later mutation displaced pending receipt: %+v %v", got, err)
		}
		plain, err := restarted.Text(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if replace && strings.Contains(plain, "first fact") {
			t.Fatal("replay overwrote a subsequent replacement")
		}
	}
}

func TestRecoveryNeverOverwritesAnUnrecognizedExternalEdit(t *testing.T) {
	svc, m := newTestService(t)
	scope := ProjectScope("p")
	stageRemember(t, m, scope, "key", "first fact", "fact-written", time.Now())
	path, _ := m.Path(scope)
	external := "# Memory\n\n- user's independent edit\n"
	if err := os.WriteFile(path, []byte(external), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := NewService(NewMarkdown(m.HomePath, m.Dir), svc.AuditPath())
	if _, err := restarted.Remember(t.Context(), scope, "", "retry", "key", Actor{}); err == nil {
		t.Fatal("recovery replaced an unrecognized edit")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != external {
		t.Fatal("external edit was overwritten")
	}
}

func TestReadingPendingMemoryDoesNotPerformRecoveryWrites(t *testing.T) {
	_, m := newTestService(t)
	scope := ProjectScope("p")
	stageRemember(t, m, scope, "key", "first fact", "prepared", time.Now())
	items, err := m.List(context.Background(), scope)
	if err != nil || len(items) != 0 {
		t.Fatalf("read changed or recovered memory: %+v %v", items, err)
	}
	files, _ := m.requestFiles(scope)
	if _, err := os.Stat(files.pending); err != nil {
		t.Fatal("read removed pending mutation")
	}
}

func TestReceiptWriteFailureCannotLetForgetRaceAheadOfRecovery(t *testing.T) {
	svc, m := newTestService(t)
	scope := ProjectScope("p")
	first := stageRemember(t, m, scope, "key", "first fact", "fact-written", time.Now())
	files, err := m.requestFiles(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(files.receipts, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Forget(t.Context(), scope, first.ID, Actor{}); err == nil {
		t.Fatal("forget passed an unwritable pending receipt")
	}
	items, err := svc.List(t.Context(), scope)
	if err != nil || len(items) != 1 {
		t.Fatalf("failed recovery changed facts: %+v %v", items, err)
	}
	if err := os.Remove(files.receipts); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Forget(t.Context(), scope, first.ID, Actor{}); err != nil {
		t.Fatal(err)
	}
	restarted := NewService(NewMarkdown(m.HomePath, m.Dir), svc.AuditPath())
	got, err := restarted.Remember(t.Context(), scope, "", "different", "key", Actor{})
	if err != nil || got != first {
		t.Fatalf("replay after recovered forget: %+v %v", got, err)
	}
	items, err = restarted.List(t.Context(), scope)
	if err != nil || len(items) != 0 {
		t.Fatalf("retry resurrected facts: %+v %v", items, err)
	}
}

func TestReceiptIsDurableWithoutAnAuditFileAndMetadataIsPrivate(t *testing.T) {
	_, m := newTestService(t)
	scope := ProjectScope("p")
	s := NewService(m, "")
	first, err := s.Remember(t.Context(), scope, "", "first fact", "key", Actor{})
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewService(NewMarkdown(m.HomePath, m.Dir), "")
	got, err := restarted.Remember(t.Context(), scope, "", "changed retry", "key", Actor{})
	if err != nil || got != first {
		t.Fatalf("no-audit restart lost receipt: %+v %v", got, err)
	}
	files, _ := m.requestFiles(scope)
	info, err := os.Stat(files.receipts)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("receipt permissions: %v %v", info, err)
	}
	stageRemember(t, m, scope, "second-key", "next fact", "prepared", time.Now())
	info, err = os.Stat(files.pending)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("pending permissions: %v %v", info, err)
	}
	plain, err := s.Snapshot(t.Context(), scope)
	if err != nil || strings.Contains(plain, "second-key") || strings.Contains(plain, "receipt") {
		t.Fatalf("private transaction data leaked into prompt: %q %v", plain, err)
	}
}

func TestDurableGlobalWriterKeepsHomeSymlinkBoundary(t *testing.T) {
	_, m := newTestService(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.HomePath, home.FileMemory)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := m.write(Global, "overwrite"); err == nil {
		t.Fatal("global writer escaped home through symlink")
	}
	raw, err := os.ReadFile(outside)
	if err != nil || string(raw) != "private" {
		t.Fatal("external target overwritten")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(m.HomePath, "inside.md")
	if err := os.WriteFile(inside, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, path); err != nil {
		t.Fatal(err)
	}
	if err := m.write(Global, "after"); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(inside)
	if err != nil || string(raw) != "after" {
		t.Fatal("inside-home symlink target not updated")
	}
}

func TestExpiredReceiptStillProvesPendingWriteWasCommitted(t *testing.T) {
	for _, external := range []string{"", "# Memory\n\n- independent edit\n"} {
		t.Run(map[bool]string{true: "undo-to-before", false: "different-edit"}[external == ""], func(t *testing.T) {
			svc, m := newTestService(t)
			scope := ProjectScope("p")
			stageRemember(t, m, scope, "expired-key", "revoked fact", "receipted", time.Now().Add(-25*time.Hour))
			path, err := m.Path(scope)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(external), 0600); err != nil {
				t.Fatal(err)
			}
			restarted := NewService(NewMarkdown(m.HomePath, m.Dir), svc.AuditPath())
			if _, err := restarted.Remember(t.Context(), scope, "", "later fact", "", Actor{}); err != nil {
				t.Fatalf("expired commit evidence caused a permanent recovery conflict: %v", err)
			}
			items, err := restarted.List(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if external != "" {
				want = 2
			}
			if len(items) != want {
				t.Fatalf("recovery resurrected a revoked fact: %+v", items)
			}
			for _, item := range items {
				if item.Text == "revoked fact" {
					t.Fatal("expired receipt made recovery replay an already committed write")
				}
			}
			files, err := m.requestFiles(scope)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(files.pending); !os.IsNotExist(err) {
				t.Fatalf("committed pending write was not cleared: %v", err)
			}
		})
	}
}
