package plan

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestCompletionGuardReadsPlansInCallerTransaction(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	for _, tc := range []struct {
		name string
		raw  string
		want error
		bad  bool
	}{
		{name: "absent"},
		{name: "empty", raw: `{}`},
		{name: "unrelated", raw: `{"by_task":{"other":"plan"}}`},
		{name: "empty mapping", raw: `{"by_task":{"root":""}}`},
		{name: "root", raw: `{"by_task":{"root":"plan"}}`, want: task.ErrCompleteRoot},
		{name: "descendant", raw: `{"by_task":{"child":"plan"}}`, want: task.ErrCompleteRoot},
		{name: "syntax", raw: `{`, bad: true},
		{name: "mapping type", raw: `{"by_task":[]}`, bad: true},
		{name: "owner schema", raw: `{"plans":1}`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rollback := errors.New("rollback test document")
			err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				if tc.raw != "" {
					if _, err := tx.Exec(`INSERT INTO bindings(kind, id, data, updated_at) VALUES ('document', 'plans', ?, '')`, tc.raw); err != nil {
						return err
					}
				}
				err := CheckTaskCompletionTx(tx, map[string]bool{"root": true, "child": true})
				if tc.bad {
					if err == nil {
						t.Error("unreadable plans admitted completion")
					}
				} else if !errors.Is(err, tc.want) {
					t.Errorf("guard = %v, want %v", err, tc.want)
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			if _, exists, err := book.Document("plans").Load(); err != nil || exists {
				t.Fatalf("guard committed caller's transaction: exists=%v err=%v", exists, err)
			}
		})
	}
}
