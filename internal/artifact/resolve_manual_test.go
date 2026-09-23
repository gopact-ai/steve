package artifact

import (
	"context"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/project"
)

// A person resolving a conflict has to end up exactly where an agent
// resolving it ends up: the canonical workspace holds the text they wrote,
// and the queue is unblocked because the canonical name moved.
func TestResolveByHandLandsThePersonsTextAndUnblocksTheQueue(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	base := canonicalOf(t, store, "p")
	write(t, ws.Path, "a", "mine")
	first, _, _ := store.Publish(ctx, ws, base, "att-1", "one")
	ws2, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Base: base, Owner: "att-2"})
	write(t, ws2.Path, "a", "theirs")
	second, _, _ := store.Publish(ctx, ws2, base, "att-2", "two")
	if land, err := store.Land(ctx, p, first.ID, "test"); err != nil || land.State != LandCommitted {
		t.Fatalf("first landing = %+v err=%v", land, err)
	}
	if _, err := store.Land(ctx, p, second.ID, "test"); err == nil {
		t.Fatal("the second landing was expected to conflict")
	}
	stuck, err := store.Stuck(ctx, "p")
	if err != nil || len(stuck) != 1 {
		t.Fatalf("stuck = %+v err=%v", stuck, err)
	}
	blocked := stuck[0]

	land, err := store.ResolveByHand(ctx, p, blocked, []Edit{{Path: "a", Text: "mine and theirs"}}, "console")
	if err != nil || land.State != LandCommitted {
		t.Fatalf("resolving by hand = %+v err=%v", land, err)
	}
	if body := read(t, canonical, "a"); body != "mine and theirs" {
		t.Fatalf("canonical holds %q, want the text the person wrote", body)
	}
	// The canonical name moved, so what was stuck can merge now: that is
	// the whole point of resolving, and it is what the agent path relies
	// on too.
	if land, err := store.Land(ctx, p, blocked.Artifact, "test"); err != nil || land.State != LandCommitted {
		t.Fatalf("relanding the blocked result = %+v err=%v", land, err)
	}
}

// A resolution is checked before any of it is written: half-merged text
// sent straight back is the commonest way to "resolve" nothing at all, and
// a path outside the conflict is not a resolution of it.
func TestResolveByHandRefusesMarkersMissingFilesAndForeignPaths(t *testing.T) {
	blocked := Stuck{Artifact: "art", Marked: "marked", Paths: []string{"a", "docs/b"}}
	cases := []struct {
		name  string
		edits []Edit
		want  string
	}{
		{"markers left in", []Edit{{Path: "a", Text: "<<<<<<< ours\nmine\n=======\ntheirs\n>>>>>>> theirs\n"}, {Path: "docs/b", Text: "fine"}}, "conflict markers"},
		{"a file not resolved", []Edit{{Path: "a", Text: "fine"}}, "has not been resolved"},
		{"a file outside the conflict", []Edit{{Path: "a", Text: "fine"}, {Path: "docs/b", Text: "fine"}, {Path: "elsewhere", Text: "fine"}}, "not one of this conflict's files"},
		{"an escaping path", []Edit{{Path: "../../etc/passwd", Text: "fine"}}, "not one of this conflict's files"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := resolvedFiles(blocked, c.edits)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("resolvedFiles = %v, want an error about %q", err, c.want)
			}
		})
	}
	ok, err := resolvedFiles(blocked, []Edit{{Path: "a", Text: "one side"}, {Path: "./docs/b", Text: "the other, ======= inline is fine"}})
	if err != nil || ok["a"] != "one side" || !strings.Contains(ok["docs/b"], "inline is fine") {
		t.Fatalf("a real resolution = %v err=%v", ok, err)
	}
}

// Every project's conflicts have to be readable in one question: a console
// that asks per project can only report on the projects it thought to ask
// about.
func TestAllStuckSpansProjectsAndFindsOneByArtifact(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	base := canonicalOf(t, store, "p")
	write(t, ws.Path, "a", "mine")
	first, _, _ := store.Publish(ctx, ws, base, "att-1", "one")
	ws2, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Base: base, Owner: "att-2"})
	write(t, ws2.Path, "a", "theirs")
	second, _, _ := store.Publish(ctx, ws2, base, "att-2", "two")
	_, _ = store.Land(ctx, p, first.ID, "test")
	_, _ = store.Land(ctx, p, second.ID, "test")

	all, err := store.AllStuck(ctx)
	if err != nil || len(all) != 1 || all[0].Artifact != second.ID || all[0].Project != "p" {
		t.Fatalf("AllStuck = %+v err=%v", all, err)
	}
	one, ok, err := store.StuckOne(ctx, second.ID)
	if err != nil || !ok || one.Landing != all[0].Landing {
		t.Fatalf("StuckOne = %+v ok=%v err=%v", one, ok, err)
	}
	if _, ok, err := store.StuckOne(ctx, "art-nothing"); ok || err != nil {
		t.Fatalf("StuckOne for an unknown artifact = ok %v err %v", ok, err)
	}
}
