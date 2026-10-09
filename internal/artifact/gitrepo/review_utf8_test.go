package gitrepo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/budget"
)

func TestFileDiffTruncationKeepsCompleteUTF8(t *testing.T) {
	for _, runeText := range []string{"λ", "中", "🙂"} {
		for kept := 1; kept <= len(runeText); kept++ {
			t.Run(fmt.Sprintf("rune%d-kept%d", len(runeText), kept), func(t *testing.T) {
				repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
				if err != nil {
					t.Fatal(err)
				}
				work := t.TempDir()
				path := filepath.Join(work, "boundary.txt")
				if err := os.WriteFile(path, []byte("probe\n"), 0600); err != nil {
					t.Fatal(err)
				}
				probe, _, err := repo.Snapshot(t.Context(), work, "", "probe", false)
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color"}
				raw, err := repo.Git(t.Context(), nil, append(args, EmptyTree, probe, "--", "boundary.txt")...)
				if err != nil || !strings.HasSuffix(raw, "+probe\n") {
					t.Fatalf("real Git probe: %v", err)
				}
				header := len(raw) - len("probe\n")
				body := strings.Repeat("a", budget.ReviewDiffBytes-header-kept) + runeText + "\n"
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				commit, _, err := repo.Snapshot(t.Context(), work, probe, "unicode", false)
				if err != nil {
					t.Fatal(err)
				}
				raw, err = repo.Git(t.Context(), nil, append(args, EmptyTree, commit, "--", "boundary.txt")...)
				start := budget.ReviewDiffBytes - kept
				if err != nil || !utf8.ValidString(raw) || strings.Index(raw, runeText) != start {
					t.Fatal("real Git fixture did not place the rune at the byte limit")
				}
				diff, cut, err := repo.FileDiff(t.Context(), "", commit, "boundary.txt")
				end := start
				if kept == len(runeText) {
					end = budget.ReviewDiffBytes
				}
				if err != nil || !cut || !utf8.ValidString(diff) || diff != raw[:end] {
					t.Fatalf("truncated text prefix: bytes=%d valid=%v cut=%v err=%v", len(diff), utf8.ValidString(diff), cut, err)
				}
				encoded, err := json.Marshal(struct{ Diff string }{diff})
				var decoded struct{ Diff string }
				if err != nil || json.Unmarshal(encoded, &decoded) != nil || decoded.Diff != diff {
					t.Fatal("JSON replaced committed UTF-8 bytes")
				}
			})
		}
	}
}
