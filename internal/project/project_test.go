package project

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	l, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return Open(l)
}

func TestDeclareNormalizesAndSealedIsDurableAtHome(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.Declare(ctx, []Project{
		{ID: "steve", Home: Home{Path: "/w/steve"}},
		{ID: "secret", Level: LevelSealed, Home: Home{Node: "vault", Path: "/v"}},
	}); err != nil {
		t.Fatal(err)
	}
	steve, ok, _ := s.Get(ctx, "steve")
	if !ok || steve.Level != LevelInternal || steve.Repo != RepoInPlace || !steve.Durable("") {
		t.Fatalf("defaults = %+v", steve)
	}
	secret, _, _ := s.Get(ctx, "secret")
	if !secret.Durable("vault") || secret.Durable("") {
		t.Fatalf("sealed durable places = %v", secret.DurablePlaces)
	}
	for _, bad := range []Project{
		{ID: "", Home: Home{Path: "/x"}},
		{ID: "a/b", Home: Home{Path: "/x"}},
		{ID: "x", Level: "top", Home: Home{Path: "/x"}},
		{ID: "x", Repo: "shared", Home: Home{Path: "/x"}},
		{ID: "x"},
	} {
		if err := s.Declare(ctx, []Project{bad}); err == nil {
			t.Fatalf("declared %+v", bad)
		}
	}
	if !LevelInternal.Admits(LevelSealed) || LevelSealed.Admits(LevelInternal) {
		t.Fatal("level order is wrong")
	}
}

func TestBindIsVersionedAndRefusesUnknown(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	_ = s.Declare(ctx, []Project{{ID: "a", Home: Home{Path: "/a"}}, {ID: "b", Home: Home{Path: "/b"}}})
	if _, err := s.Bind(ctx, "oc_1", "nope", "u"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("bind unknown = %v", err)
	}
	if _, ok, _ := s.Binding(ctx, "oc_1"); ok {
		t.Fatal("a refused bind left a binding behind")
	}
	first, err := s.Bind(ctx, "oc_1", "a", "u")
	if err != nil || first.Version != 1 || first.ProjectID != "a" {
		t.Fatalf("first = %+v err=%v", first, err)
	}
	second, err := s.Bind(ctx, "oc_1", "b", "u")
	if err != nil || second.Version != 2 {
		t.Fatalf("second = %+v err=%v", second, err)
	}
	got, ok, _ := s.Binding(ctx, "oc_1")
	if !ok || got.ProjectID != "b" || got.Version != 2 || got.By != "u" {
		t.Fatalf("binding = %+v", got)
	}
	// Another conversation has its own version line.
	other, _ := s.Bind(ctx, "oc_2", "a", "u")
	if other.Version != 1 {
		t.Fatalf("other conversation version = %d", other.Version)
	}
}

func TestMaterializeServesCanonicalAtHomeOnly(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	_ = s.Declare(ctx, []Project{{ID: "p", Home: Home{Node: "node-a", Path: "/srv/p"}}})
	ws, err := s.Materialize(ctx, Request{Project: "p", Node: "node-a"})
	if err != nil || ws.Path != "/srv/p" || ws.Kind != KindCanonical {
		t.Fatalf("home = %+v err=%v", ws, err)
	}
	_, err = s.Materialize(ctx, Request{Project: "p", Node: ""})
	if !errors.Is(err, ErrNotHome) {
		t.Fatalf("hub = %v", err)
	}
	_, err = s.Materialize(ctx, Request{Project: "p", Node: "node-a", Isolated: true})
	if !errors.Is(err, ErrNotMaterializable) {
		t.Fatalf("isolated = %v", err)
	}
	if _, err := s.Materialize(ctx, Request{Project: "q", Node: "node-a"}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown = %v", err)
	}
}

func TestCopiesPlaceATurnAwayFromHome(t *testing.T) {
	s := openStore(t)
	s.Levels = func(node string) Level {
		if node == "node-low" {
			return LevelPublic
		}
		return LevelRestricted
	}
	ctx := context.Background()
	_ = s.Declare(ctx, []Project{{ID: "p", Home: Home{Node: "", Path: "/srv/p"}}, {ID: "q", Home: Home{Node: "node-a", Path: "/srv/q"}}})
	// Nowhere yet: the refusal names the home.
	_, err := s.Materialize(ctx, Request{Project: "p", Node: "node-a"})
	var notHome NotHomeError
	if !errors.As(err, &notHome) || len(notHome.Places) != 1 || notHome.Places[0].Kind != KindCanonical {
		t.Fatalf("before copy: %v", err)
	}
	ws, err := s.SetCopy(ctx, "p", Copy{Node: "node-a", Path: "/home/a/p/", By: "owner"})
	if err != nil || ws.ID != "copy:p/node-a" || ws.Kind != KindCopy || ws.Path != "/home/a/p" {
		t.Fatalf("add = %+v err=%v", ws, err)
	}
	got, err := s.Materialize(ctx, Request{Project: "p", Node: "node-a"})
	if err != nil || got.ID != "copy:p/node-a" || got.Path != "/home/a/p" {
		t.Fatalf("placed = %+v err=%v", got, err)
	}
	if home, err := s.Materialize(ctx, Request{Project: "p", Node: ""}); err != nil || home.Kind != KindCanonical {
		t.Fatalf("home still canonical: %+v %v", home, err)
	}
	p, _, _ := s.Get(ctx, "p")
	if all := p.Workspaces(); len(all) != 2 || all[0].Kind != KindCanonical || all[1].ID != "copy:p/node-a" {
		t.Fatalf("workspaces = %+v", all)
	}
	if c, ok := p.CopyOn("node-a"); !ok || c.Origin != OriginAdopted || c.State != CopyReady || c.By != "owner" || c.At.IsZero() {
		t.Fatalf("copy record = %+v", c)
	}
	// A copy being cloned is listed but not placed.
	if _, err := s.SetCopy(ctx, "p", Copy{Node: "node-b", Path: "/home/b/p", Origin: OriginCloned, State: CopyProvisioning}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Materialize(ctx, Request{Project: "p", Node: "node-b"}); !errors.Is(err, ErrNotHome) {
		t.Fatalf("provisioning copy placed: %v", err)
	}
	if _, err := s.SetCopy(ctx, "p", Copy{Node: "node-b", Path: "/home/b/p", Origin: OriginCloned, State: CopyReady}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Materialize(ctx, Request{Project: "p", Node: "node-b"}); err != nil {
		t.Fatalf("ready copy not placed: %v", err)
	}
	// One copy per machine; a directory belongs to one workspace, on any project.
	if _, err := s.SetCopy(ctx, "p", Copy{Node: "node-a", Path: "/home/a/other"}); err == nil {
		t.Fatal("second copy on node-a accepted")
	}
	if _, err := s.SetCopy(ctx, "q", Copy{Node: "", Path: "/srv/p/sub"}); err == nil {
		t.Fatal("nested directory accepted")
	}
	if _, err := s.SetCopy(ctx, "q", Copy{Node: "node-a", Path: "/tmp/q"}); err == nil {
		t.Fatal("copy on the home machine accepted")
	}
	if _, err := s.SetCopy(ctx, "p", Copy{Node: "node-c", Path: "relative"}); err == nil {
		t.Fatal("relative path accepted")
	}
	if _, err := s.SetCopy(ctx, "q", Copy{Node: "node-low", Path: "/tmp/q"}); err == nil {
		t.Fatal("copy on a machine below the project's level accepted")
	}
	_ = s.Declare(ctx, []Project{{ID: "s", Level: LevelSealed, Home: Home{Node: "node-a", Path: "/srv/s"}}})
	if _, err := s.SetCopy(ctx, "s", Copy{Node: "node-b", Path: "/tmp/s"}); err == nil {
		t.Fatal("sealed project copy accepted")
	}
	// Elsewhere still refuses, naming every place.
	_, err = s.Materialize(ctx, Request{Project: "p", Node: "node-c"})
	if !errors.As(err, &notHome) || len(notHome.Places) != 3 || notHome.PlaceList() == "" {
		t.Fatalf("node-c: %v", err)
	}
	if err := s.DeleteCopy(ctx, "p", "node-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCopy(ctx, "p", "node-a"); err == nil {
		t.Fatal("removing twice succeeded")
	}
	if _, err := s.Materialize(ctx, Request{Project: "p", Node: "node-a"}); !errors.Is(err, ErrNotHome) {
		t.Fatalf("after remove: %v", err)
	}
	// Declaring again from config keeps the record of a copy at the same
	// place, restarts one at a new place, and forgets one not declared.
	_, _ = s.SetCopy(ctx, "p", Copy{Node: "node-a", Path: "/home/a/p", Origin: OriginCloned, Source: "git@x:p.git", By: "owner"})
	_ = s.Declare(ctx, []Project{{ID: "p", Home: Home{Path: "/srv/p"}, Copies: map[string]Copy{"node-a": {Path: "/home/a/p"}, "node-b": {Path: "/home/b/p2"}}}})
	p, _, _ = s.Get(ctx, "p")
	if c := p.Copies["node-a"]; c.Origin != OriginCloned || c.Source != "git@x:p.git" {
		t.Fatalf("same-place copy lost its record: %+v", c)
	}
	if c := p.Copies["node-b"]; c.Origin != OriginAdopted || c.Path != "/home/b/p2" || c.State != CopyReady {
		t.Fatalf("moved copy not restarted: %+v", c)
	}
	_ = s.Declare(ctx, []Project{{ID: "p", Home: Home{Path: "/srv/p"}}})
	p, _, _ = s.Get(ctx, "p")
	if len(p.Copies) != 0 {
		t.Fatalf("undeclared copies survived: %+v", p.Copies)
	}
}
