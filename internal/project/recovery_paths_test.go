package project

import (
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestRecoveryHomeGuardUsesQualifiedMetadataPaths(t *testing.T) {
	for _, tc := range []struct {
		name, home, same, different string
	}{
		{"drive", `C:\root\repo`, `C:/root/temp/../repo`, `D:\root\repo`},
		{"unc", `\\server\share\repo`, `\\server\share\temp\..\repo`, `\\server\other\repo`},
		{"posix", `/srv/repo`, `/srv/temp/../repo`, `/srv/other`},
		{"posix-backslash", `/srv/a\b`, `/srv/./a\b`, `/srv/a/b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			p := Project{ID: "p", Home: Home{Node: "node", Path: tc.home}}
			if err := s.Declare(t.Context(), []Project{p}); err != nil {
				t.Fatal(err)
			}
			check := func(node, directory string) error {
				return s.l.Update(t.Context(), func(tx *ledger.Tx) error {
					return CheckHomeTx(tx, p.ID, Home{Node: node, Path: directory})
				})
			}
			if err := check("node", tc.same); err != nil {
				t.Fatalf("same qualified metadata directory refused: %v", err)
			}
			for _, other := range []Home{{Node: "node", Path: tc.different}, {Node: "other", Path: tc.same}, {Node: "node"}} {
				if err := check(other.Node, other.Path); err == nil {
					t.Fatalf("different target admitted: %+v", other)
				}
			}
		})
	}
}

func TestRecoveryContainerGuardUsesQualifiedMetadataParents(t *testing.T) {
	for _, tc := range []struct {
		name, work, within, sibling string
	}{
		{"drive", `C:\root\container\work`, `C:/root/container/marker`, `C:/root/container-other`},
		{"unc", `\\server\share\container\work`, `\\server\share\container\marker`, `\\server\share\container-other`},
		{"posix", `/srv/container/work`, `/srv/container/marker`, `/srv/container-other`},
		{"posix-backslash", `/srv/a\b/work`, `/srv/a\b/marker`, `/srv/a/b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			p := Project{ID: "other", Home: Home{Node: "node", Path: tc.sibling}}
			if err := s.Declare(t.Context(), []Project{p}); err != nil {
				t.Fatal(err)
			}
			ws := Workspace{Kind: KindWorktree, RecoveryID: "episode", Node: "node", Path: tc.work}
			check := func() error {
				return s.l.Read(t.Context(), func(tx *ledger.ReadTx) error {
					return CheckRecoveryWorkspaceTx(tx, ws)
				})
			}
			if err := check(); err != nil {
				t.Fatalf("separate container refused: %v", err)
			}
			p.Home.Path = tc.within
			if err := s.Declare(t.Context(), []Project{p}); err != nil {
				t.Fatal(err)
			}
			if err := check(); err == nil {
				t.Fatal("recovery container's sibling file was not protected")
			}
			ws.Node = "different-node"
			if err := check(); err != nil {
				t.Fatalf("unrelated node blocked: %v", err)
			}
		})
	}
}

func TestRecoveryContainerGuardUsesNativeLocation(t *testing.T) {
	s := openStore(t)
	ws := Workspace{Kind: KindWorktree, RecoveryID: "episode", Path: filepath.Join(t.TempDir(), "container", "work")}
	if err := s.l.Read(t.Context(), func(tx *ledger.ReadTx) error {
		return CheckRecoveryWorkspaceTx(tx, ws)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryDeclarationFingerprintStaysHistorical(t *testing.T) {
	p := Project{ID: "p", Home: Home{Node: "node", Path: `C:\root\temp\..\repo`}}
	const historical = "a2a25992af0bdb050e27a10a97b1d68251daedd8e4d570951d41fa2869997b19"
	if got := RecoveryIdentity(p); got != historical {
		t.Fatalf("persisted declaration hash changed: %s", got)
	}
}
