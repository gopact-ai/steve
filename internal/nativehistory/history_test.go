package nativehistory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, root, name, data string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSnapshotsPreserveOnlySelectedHistoryAndSurviveSourceRemoval(t *testing.T) {
	for _, fixture := range []struct{ harness, path, data, id string }{
		{"codex", "sessions/2026/09/14/rollout-original.jsonl", `{"type":"session_meta","payload":{"id":"native-codex","cwd":"/work"}}` + "\n", "native-codex"},
		{"claude-code", "projects/-work/native-claude.jsonl", `{"type":"queue-operation"}` + "\n" + `{"type":"user","sessionId":"native-claude","cwd":"/work","message":{"content":[{"type":"text","text":"retain marker"}]}}` + "\n", "native-claude"},
		{"grok", "sessions/%2Fwork/native-grok/summary.json", `{"info":{"session_id":"native-grok"},"generated_title":"retained Grok context"}`, "native-grok"},
	} {
		t.Run(fixture.harness, func(t *testing.T) {
			home := t.TempDir()
			writeFixture(t, home, fixture.path, fixture.data)
			writeFixture(t, home, "config.toml", "private hook must stay outside the snapshot")
			writeFixture(t, home, "auth.json", "not history")
			if fixture.harness == "claude-code" {
				writeFixture(t, home, "projects/-work/native-claude/subagents/agent-1.jsonl", "selected adjunct")
			}
			if fixture.harness == "grok" {
				writeFixture(t, home, "sessions/%2Fwork/native-grok/updates.jsonl", "selected transcript")
			}
			source := Source{Harness: fixture.harness, Home: home}
			entries, err := List(t.Context(), source)
			if err != nil || len(entries) != 1 {
				t.Fatalf("list: %+v %v", entries, err)
			}
			if entries[0].NativeID != fixture.id || entries[0].Workdir != "/work" {
				t.Fatalf("metadata: %+v", entries[0])
			}
			store := t.TempDir()
			req := ImportRequest{CommandID: "import-selected", Source: source, NativeID: fixture.id, Revision: entries[0].Revision}
			ref, err := Snapshot(t.Context(), store, req)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join(home, fixture.path))
			if err != nil || string(original) != fixture.data {
				t.Fatal("import modified original history")
			}
			if err := os.RemoveAll(home); err != nil {
				t.Fatal(err)
			}
			repeated, err := Snapshot(t.Context(), store, req)
			if err != nil || repeated != ref {
				t.Fatalf("lost import receipt: %+v %v", repeated, err)
			}
			dest := filepath.Join(t.TempDir(), "native-home")
			if err := Materialize(t.Context(), store, ref, dest); err != nil {
				t.Fatal(err)
			}
			copied, err := os.ReadFile(filepath.Join(dest, fixture.path))
			if err != nil || string(copied) != fixture.data {
				t.Fatal("selected history not materialized")
			}
			for _, name := range []string{"auth.json", "config.toml"} {
				if _, err := os.Stat(filepath.Join(dest, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("copied configuration: %s", name)
				}
			}
			if err := Materialize(t.Context(), store, ref, dest); err == nil {
				t.Fatal("existing runtime overwritten")
			}
			writeFixture(t, filepath.Join(store, ref.ID, "history"), fixture.path, fixture.data+"tampered")
			if err := Materialize(t.Context(), store, ref, filepath.Join(t.TempDir(), "other")); err == nil {
				t.Fatal("tampered snapshot accepted")
			}
		})
	}
}

func TestImportRefusesChangedAndLinkedHistory(t *testing.T) {
	home := t.TempDir()
	name := "sessions/2026/09/14/rollout-test.jsonl"
	raw := `{"type":"session_meta","payload":{"id":"native","cwd":"/work"}}` + "\n"
	writeFixture(t, home, name, raw)
	source := Source{Harness: "codex", Home: home}
	entries, err := List(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	req := ImportRequest{CommandID: "one", Source: source, NativeID: "native", Revision: entries[0].Revision}
	writeFixture(t, home, name, raw+`{"type":"new-turn"}`+"\n")
	if _, err := Snapshot(t.Context(), t.TempDir(), req); !errors.Is(err, ErrChanged) {
		t.Fatalf("source change accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(home, name)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(outside, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, name)); err != nil {
		t.Fatal(err)
	}
	entries, err = List(t.Context(), source)
	if err != nil || len(entries) != 0 {
		t.Fatalf("linked history advertised: %+v %v", entries, err)
	}
	for _, id := range []string{"../outside", "ns_managed", strings.Repeat("x", 257)} {
		if validNativeID(id) {
			t.Fatalf("invalid native id accepted: %q", id)
		}
	}
}
