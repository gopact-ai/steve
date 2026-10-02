package contentreplica

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

type failingRetentionReader struct {
	ledger.Reader
	match string
}

func (r failingRetentionReader) QueryRow(query string, args ...any) *ledger.Row {
	if strings.Contains(query, r.match) {
		return r.Reader.QueryRow(`SELECT data FROM missing_retention_owner_table`)
	}
	return r.Reader.QueryRow(query, args...)
}

// Retire, release and receiver GC all use this same complete catalog read.
// A failed owner enumeration cannot be a valid catalog with empty roots.
func TestRetentionCatalogClosesOnEveryOwnerEnumerationReadError(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	for _, match := range []string{"SELECT kind, id, data FROM bindings", "FROM operations WHERE kind='workspace-recovery'"} {
		err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
			_, err := readRetentionCatalog(failingRetentionReader{Reader: tx, match: match})
			return err
		})
		if err == nil {
			t.Fatalf("owner read error became an empty retention catalog: %s", match)
		}
	}
}
