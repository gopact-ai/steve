package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollectExtractsUserTurnsOnly(t *testing.T) {
	root := t.TempDir()
	codex := filepath.Join(root, ".codex", "sessions", "2026")
	claude := filepath.Join(root, ".claude", "projects", "demo")
	cursor := filepath.Join(root, ".cursor", "projects", "demo", "agent-transcripts", "s1")
	grok := filepath.Join(root, ".grok", "sessions", "demo")
	kimi := filepath.Join(root, ".kimi-code", "sessions", "demo")
	for _, dir := range []string{codex, claude, cursor, grok, kimi} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string, recent bool) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := time.Now()
		if !recent {
			mod = time.Now().Add(-48 * time.Hour)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(codex, "a.jsonl"), `{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"ship the feishu bot"}]}}`+"\n"+`{"type":"response_item","payload":{"role":"assistant","content":[{"text":"ok"}]}}`+"\n"+`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"<environment_context>cwd</environment_context>"}]}}`+"\n", true)
	write(filepath.Join(claude, "b.jsonl"), `{"type":"user","message":{"role":"user","content":"我在 Asia/Shanghai，叫我 LPX"}}`+"\n", true)
	write(filepath.Join(cursor, "c.jsonl"), `{"role":"user","message":{"content":"prefer short replies"}}`+"\n", true)
	write(filepath.Join(grok, "d.jsonl"), `{"type":"user","content":[{"type":"text","text":"use grok for reviews"}]}`+"\n", true)
	write(filepath.Join(kimi, "e.jsonl"), `{"role":"user","content":"kimi for chinese drafts"}`+"\n", true)
	got := Collect(root)
	blob := Format(got)
	if !strings.Contains(blob, "ship the feishu bot") || !strings.Contains(blob, "Asia/Shanghai") || !strings.Contains(blob, "prefer short replies") || !strings.Contains(blob, "use grok for reviews") || !strings.Contains(blob, "kimi for chinese drafts") {
		t.Fatalf("missing user text: %s", blob)
	}
	if strings.Contains(blob, "environment_context") || strings.Contains(blob, `"ok"`) {
		t.Fatalf("kept noise: %s", blob)
	}
}

func TestCollectEmptyHome(t *testing.T) {
	if got := Collect(t.TempDir()); len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}
