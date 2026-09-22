package attempt

import (
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestGetTxUsesCommittedOperationColumnsAndRejectsInvalidRecords(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantError bool
	}{
		{"record", `{"id":"selected","state":"failed","revision":999}`, false},
		{"null", `null`, true},
		{"malformed", `{`, true},
		{"wrong-identity", `{"id":"other"}`, true},
		{"missing", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			if tc.raw != "" {
				if _, err := book.DB().Exec(`INSERT INTO operations VALUES('selected','attempt','running',7,1,?,'2026-09-19T00:00:00Z','2026-09-19T00:00:00Z')`, tc.raw); err != nil {
					t.Fatal(err)
				}
			}
			err = book.Read(t.Context(), func(tx *ledger.ReadTx) error {
				got, err := GetTx(tx, "selected")
				if err != nil {
					return err
				}
				if got.ID != "selected" || got.State != Running || got.Revision != 7 {
					t.Fatalf("committed columns lost to stale embedded data: %+v", got)
				}
				return nil
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("GetTx error=%v wantError=%v", err, tc.wantError)
			}
		})
	}
}
