package nativehistory

import (
	"errors"
	"strconv"
	"testing"
)

func TestStorageChecksReservationBeforeExistingEntries(t *testing.T) {
	for _, reserved := range []int{MaxStoreEntries - 1, MaxStoreEntries, MaxStoreEntries + 1} {
		t.Run(strconv.Itoa(reserved), func(t *testing.T) {
			store := t.TempDir()
			release, err := LockStorage(t.Context(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			err = CheckStorage(t.Context(), store, reserved, 0)
			if reserved > MaxStoreEntries {
				if !errors.Is(err, ErrStorageFull) {
					t.Fatalf("over-capacity reservation in an empty locked store: %v", err)
				}
			} else if err != nil {
				t.Fatalf("within-capacity reservation: %v", err)
			}
		})
	}
}

func TestStorageChecksByteReservationInAnEmptyStore(t *testing.T) {
	for _, reserved := range []int64{MaxStoreBytes - 1, MaxStoreBytes, MaxStoreBytes + 1} {
		t.Run(strconv.FormatInt(reserved, 10), func(t *testing.T) {
			store := t.TempDir()
			release, err := LockStorage(t.Context(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			err = CheckStorage(t.Context(), store, 0, reserved)
			if reserved > MaxStoreBytes {
				if !errors.Is(err, ErrStorageFull) {
					t.Fatalf("over-capacity byte reservation: %v", err)
				}
			} else if err != nil {
				t.Fatalf("within-capacity byte reservation: %v", err)
			}
		})
	}
}
