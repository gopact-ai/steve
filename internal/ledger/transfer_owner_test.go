package ledger

import (
	"errors"
	"testing"
	"time"
)

func TestImportFactsComposesOwnerValidationAndAtomicWrites(t *testing.T) {
	l := open(t, t.TempDir(), &clock{t: time.Now()})
	f, docs := importFixture()
	fail := errors.New("last owner rejected")
	validations, imports := 0, 0
	owner := func(tx *Tx, validateOnly bool) error {
		if validateOnly {
			validations++
			return nil
		}
		imports++
		if err := tx.PutBinding("owner-record", "one", map[string]string{"id": "one"}); err != nil {
			return err
		}
		return fail
	}
	if err := l.ValidateImport(t.Context(), f, docs, nil, owner); err != nil {
		t.Fatal(err)
	}
	if got := importCounts(t, l); validations != 1 || imports != 0 || got[0]+got[1]+got[2]+got[3] != 0 {
		t.Fatalf("validation mutated facts: counts=%v validate=%d import=%d", got, validations, imports)
	}
	if err := l.ImportFacts(t.Context(), f, docs, nil, owner); !errors.Is(err, fail) {
		t.Fatalf("import error: %v", err)
	}
	if got := importCounts(t, l); got[0]+got[1]+got[2]+got[3] != 0 {
		t.Fatalf("last owner failure partially committed: %v", got)
	}
	fail = nil
	if err := l.ImportFacts(t.Context(), f, docs, nil, owner); err != nil {
		t.Fatal(err)
	}
	if got := importCounts(t, l); got[0] != 1 || got[1] != 1 || got[2] != 1 || got[3] != 3 {
		t.Fatalf("retry missing facts or owner record: %v", got)
	}
}
