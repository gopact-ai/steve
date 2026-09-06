package project

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestDeclareRejectsOverlappingOwnershipAtomically(t *testing.T) {
	for _, path := range []string{"/shared", "/shared/sub", "/", "/shared/./sub/.."} {
		t.Run(path, func(t *testing.T) {
			s := openStore(t)
			if err := s.Declare(t.Context(), []Project{{ID: "existing", Home: Home{Path: "/shared"}}}); err != nil {
				t.Fatal(err)
			}
			err := s.Declare(t.Context(), []Project{
				{ID: "valid-first", Home: Home{Node: "other", Path: "/independent"}},
				{ID: "conflicting", Home: Home{Path: path}},
			})
			if err == nil {
				t.Fatal("declared overlapping workspace ownership")
			}
			all, err := s.List(t.Context())
			if err != nil || len(all) != 1 || all[0].ID != "existing" {
				t.Fatalf("failed declaration partially committed: %+v, %v", all, err)
			}
		})
	}
}

func TestDeclareChecksBatchCopiesAndKeepsOriginalOnFailure(t *testing.T) {
	s := openStore(t)
	if err := s.Declare(t.Context(), []Project{{ID: "p", Home: Home{Path: "/original"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Declare(t.Context(), []Project{
		{ID: "p", Home: Home{Path: "/changed"}},
		{ID: "q", Home: Home{Node: "other", Path: "/other"}, Copies: map[string]Copy{"": {Path: "/changed/sub"}}},
	}); err == nil {
		t.Fatal("a same-batch home/copy overlap was accepted")
	}
	p, _, _ := s.Get(t.Context(), "p")
	if p.Home.Path != "/original" {
		t.Fatalf("failed batch changed an existing project: %+v", p)
	}
	if err := s.Declare(t.Context(), []Project{{ID: "first", Home: Home{Path: "/first"}}, {ID: "bad"}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if _, ok, _ := s.Get(t.Context(), "first"); ok {
		t.Fatal("shape validation failure partially committed")
	}
}

func TestDeclareReplacementUsesTheWholeFinalOwnershipSet(t *testing.T) {
	s := openStore(t)
	projects := []Project{{ID: "p", Home: Home{Path: "/one"}}, {ID: "q", Home: Home{Path: "/two"}}}
	if err := s.Declare(t.Context(), projects); err != nil {
		t.Fatal(err)
	}
	if err := s.Declare(t.Context(), projects); err != nil {
		t.Fatalf("same declaration conflicts with itself: %v", err)
	}
	projects[0].Home.Path, projects[1].Home.Path = "/two", "/one"
	if err := s.Declare(t.Context(), projects); err != nil {
		t.Fatalf("final disjoint batch was compared against replaced ownership: %v", err)
	}
	if err := s.Declare(t.Context(), []Project{{ID: "remote", Home: Home{Node: "other", Path: "/one"}}}); err != nil {
		t.Fatalf("same path on another node refused: %v", err)
	}
}

func TestConcurrentDeclarationsHaveOneDirectoryOwner(t *testing.T) {
	s := openStore(t)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := range 10 {
		wg.Go(func() {
			// Separate Store objects prove the guarantee is in the ledger transaction.
			writer := Open(s.l)
			if err := writer.Declare(t.Context(), []Project{{ID: fmt.Sprintf("p-%d", i), Home: Home{Path: "/shared"}}}); err == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	all, err := s.List(t.Context())
	if err != nil || successes.Load() != 1 || len(all) != 1 {
		t.Fatalf("directory has multiple owners: successes=%d projects=%+v err=%v", successes.Load(), all, err)
	}
}

func TestConcurrentCopyAndDeclarationShareOwnershipGuard(t *testing.T) {
	s := openStore(t)
	if err := s.Declare(t.Context(), []Project{{ID: "p", Home: Home{Path: "/home-p"}}}); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := Open(s.l).SetCopy(t.Context(), "p", Copy{Node: "other", Path: "/shared"}); err == nil {
			successes.Add(1)
		}
	})
	wg.Go(func() {
		if err := Open(s.l).Declare(t.Context(), []Project{{ID: "q", Home: Home{Node: "other", Path: "/shared"}}}); err == nil {
			successes.Add(1)
		}
	})
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("concurrent home and copy both acquired ownership: %d", successes.Load())
	}
}

func TestDeclareRollsBackTheBatchWhenAWriteFails(t *testing.T) {
	s := openStore(t)
	if err := s.Declare(t.Context(), []Project{{ID: "existing", Home: Home{Path: "/existing"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_project BEFORE INSERT ON bindings
			WHEN NEW.kind = 'project' AND NEW.id = 'reject'
			BEGIN SELECT RAISE(ABORT, 'injected project write failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := s.Declare(t.Context(), []Project{
		{ID: "existing", Home: Home{Path: "/moved"}},
		{ID: "valid-first", Home: Home{Path: "/valid"}},
		{ID: "reject", Home: Home{Path: "/reject"}},
	})
	if err == nil {
		t.Fatal("failed batch write was acknowledged")
	}
	all, err := s.List(t.Context())
	if err != nil || len(all) != 1 || all[0].ID != "existing" || all[0].Home.Path != "/existing" {
		t.Fatalf("failed write left part of the batch behind: %+v, %v", all, err)
	}
}
