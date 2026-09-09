package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// panicOnceWritten panics from Err once the restore has begun filling the
// destination, which is the only way a caller can leave Restore without a
// return statement.
type panicOnceWritten struct {
	context.Context
	directory string
}

func (c panicOnceWritten) Err() error {
	if _, err := os.Stat(filepath.Join(c.directory, "context.json")); err == nil {
		panic("blob store gave up mid-restore")
	}
	return c.Context.Err()
}

func TestAbandonedRestoreLeavesNoRecoveryDirectory(t *testing.T) {
	s := newTestStore(t, "node-a", &recordingRecords{}, nil, testPolicy{}, Limits{})
	snapshot := testSnapshot(t, s, 1)
	m, err := s.Commit(context.Background(), snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "recovered")
	func() {
		defer func() {
			if recover() == nil {
				t.Error("restore returned instead of unwinding")
			}
		}()
		_, _ = s.Restore(panicOnceWritten{Context: context.Background(), directory: directory}, m, directory)
	}()
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("partial recovery directory survived the panic: %v", err)
	}
}
