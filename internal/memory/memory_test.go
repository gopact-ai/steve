package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/home"
)

func newTestService(t *testing.T) (*Service, *Markdown) {
	t.Helper()
	homeDir := t.TempDir()
	if err := home.Bootstrap(homeDir, "ou_x"); err != nil {
		t.Fatal(err)
	}
	m := NewMarkdown(homeDir, filepath.Join(t.TempDir(), "memory"))
	return NewService(m, filepath.Join(m.Dir, "audit.jsonl")), m
}

func TestRememberRecallForget(t *testing.T) {
	svc, m := newTestService(t)
	ctx := context.Background()
	who := Actor{Conversation: "c1", Agent: "codex", By: "agent"}
	r, err := svc.Remember(ctx, Global, "偏好", "默认用简体中文回答。", "", who)
	if err != nil || r.ID == "" || !r.New {
		t.Fatalf("remember: %+v %v", r, err)
	}
	if again, _ := svc.Remember(ctx, Global, "偏好", "默认用简体中文回答", "", who); again.ID != r.ID || again.New {
		t.Fatalf("same fact got a second id: %+v", again)
	}
	text, _ := svc.Snapshot(ctx, Global)
	if strings.Count(text, "默认用简体中文回答") != 1 || !strings.Contains(text, "## 偏好\n\n- 默认用简体中文回答。\n") || strings.Contains(text, "<!--") {
		t.Fatalf("global snapshot = %q", text)
	}
	raw, _ := os.ReadFile(filepath.Join(m.HomePath, home.FileMemory))
	if !strings.Contains(string(raw), "<!-- m:"+r.ID+" -->") {
		t.Fatalf("id not kept in file: %s", raw)
	}
	if _, err := svc.Remember(ctx, Global, "people", "老王负责发布。", "", who); err != nil {
		t.Fatal(err)
	}
	hits, from, _ := svc.Recall(ctx, Global, "谁负责发布", 5)
	if len(hits) == 0 || !strings.Contains(hits[0].Text, "老王") || hits[0].Section != "人" || from != "markdown" {
		t.Fatalf("recall = %+v from %s", hits, from)
	}
	// A project's memory is its own file with its own headings.
	p := ProjectScope("steve")
	pr, err := svc.Remember(ctx, p, "pitfall", "测试要用真模型，mock 交不了差。", "", who)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := m.Path(p)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("project file missing: %v", err)
	}
	ptext, _ := svc.Snapshot(ctx, p)
	if !strings.Contains(ptext, "# Memory · steve") || !strings.Contains(ptext, "## 坑\n\n- 测试要用真模型") {
		t.Fatalf("project snapshot = %q", ptext)
	}
	if g, _ := svc.Snapshot(ctx, Global); strings.Contains(g, "真模型") {
		t.Fatal("project fact leaked into global")
	}
	if it, err := svc.Forget(ctx, p, pr.ID, who); err != nil || !strings.Contains(it.Text, "真模型") {
		t.Fatalf("forget: %+v %v", it, err)
	}
	if ptext, _ := svc.Snapshot(ctx, p); strings.Contains(ptext, "真模型") {
		t.Fatal("forget did not remove the fact")
	}
	if _, err := svc.Forget(ctx, p, pr.ID, who); err == nil {
		t.Fatal("forgetting twice succeeded")
	}
	audit, _ := os.ReadFile(filepath.Join(m.Dir, "audit.jsonl"))
	if n := strings.Count(string(audit), "\n"); n != 6 || strings.Contains(string(audit), "老王") {
		t.Fatalf("audit lines = %d:\n%s", n, audit)
	}
}

func TestOwnerEditsKeepWorking(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	p := ProjectScope("x")
	// The owner types a file by hand on the page: bullets without ids.
	if err := svc.Replace(ctx, p, "# Memory · x\n\n## 约定\n\n- 提交信息用中文。\n- 不用 pkill -f。\n", Actor{By: "console"}); err != nil {
		t.Fatal(err)
	}
	items, _ := svc.List(ctx, p)
	if len(items) != 2 || items[0].ID == "" || items[0].Section != "约定" {
		t.Fatalf("items = %+v", items)
	}
	// Forgetting a hand-typed fact works by its content id.
	if _, err := svc.Forget(ctx, p, items[1].ID, Actor{By: "agent"}); err != nil {
		t.Fatal(err)
	}
	text, _ := svc.Snapshot(ctx, p)
	if strings.Contains(text, "pkill") || !strings.Contains(text, "提交信息用中文") {
		t.Fatalf("snapshot = %q", text)
	}
	// The page saves the plain text back; Steve's ids survive the round trip.
	if _, err := svc.Remember(ctx, p, "坑", "别用 pkill -f。", "", Actor{By: "agent"}); err != nil {
		t.Fatal(err)
	}
	before, _ := svc.List(ctx, p)
	plain, _ := svc.Text(ctx, p)
	if strings.Contains(plain, "<!--") {
		t.Fatalf("page text carries ids: %q", plain)
	}
	if err := svc.Replace(ctx, p, strings.Replace(plain, "提交信息用中文。", "提交信息用中文，短。", 1), Actor{By: "console"}); err != nil {
		t.Fatal(err)
	}
	after, _ := svc.List(ctx, p)
	if len(after) != 2 || after[1].ID != before[1].ID || after[0].ID == before[0].ID || !strings.HasPrefix(after[0].ID, "h") {
		t.Fatalf("ids before %+v after %+v", before, after)
	}
	if _, err := svc.Forget(ctx, p, after[1].ID, Actor{By: "agent"}); err != nil {
		t.Fatal(err)
	}
	// A new section the file lacks is added at the end.
	if _, err := svc.Remember(ctx, p, "决策", "用 SQLite 不用 Postgres。", "", Actor{By: "agent"}); err != nil {
		t.Fatal(err)
	}
	text, _ = svc.Snapshot(ctx, p)
	if !strings.HasSuffix(text, "## 决策\n\n- 用 SQLite 不用 Postgres。\n") {
		t.Fatalf("snapshot = %q", text)
	}
	// An empty project is nothing to inject.
	if text, _ := svc.Snapshot(ctx, ProjectScope("nothing")); text != "" {
		t.Fatalf("empty project snapshot = %q", text)
	}
}

func TestScopesAndPaths(t *testing.T) {
	_, m := newTestService(t)
	if _, err := m.Path(ProjectScope("../x")); err == nil {
		t.Fatal("path escape accepted")
	}
	if _, err := ParseScope("project", ""); err == nil {
		t.Fatal("bare project without a binding parsed")
	}
	if s, _ := ParseScope("project", "steve"); s != ProjectScope("steve") {
		t.Fatalf("scope = %s", s)
	}
	if _, err := ParseScope("project:other", "steve"); err == nil {
		t.Fatal("an agent named another project")
	}
	if s, _ := ParseScope("", "steve"); s != Global {
		t.Fatalf("default scope = %s", s)
	}
}

func TestConcurrentRemembersAllLand(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := svc.Remember(ctx, Global, "项目", "事实 "+strings.Repeat("x", i+1), "", Actor{By: "agent"}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	items, _ := svc.List(ctx, Global)
	if len(items) != 20 {
		t.Fatalf("%d of 20 facts landed", len(items))
	}
}
