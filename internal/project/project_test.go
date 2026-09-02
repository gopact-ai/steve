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
