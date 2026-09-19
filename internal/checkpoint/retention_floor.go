package checkpoint

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const retentionFloorName = "retention-floor"
const retentionFloorBytes = 80

func retentionFloorData(value uint64) string {
	order := fmt.Sprintf("%016x", value)
	return fmt.Sprintf("%s%x", order, sha256.Sum256([]byte(order)))
}

// A fixed-size admission floor replaces completed per-receipt tombstones.
// Exact active markers remain exceptions; a floor never authorizes deletion.
func (s *Store) retentionFloorLocked() (uint64, bool, error) {
	if err := s.checkOpenLocked(); err != nil {
		return 0, false, err
	}
	info, err := s.root.Lstat(retentionFloorName)
	if errors.Is(err, os.ErrNotExist) {
		if _, existed := s.sizes[retentionFloorName]; existed {
			return 0, false, ErrIntegrity
		}
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() || info.Size() != retentionFloorBytes {
		return 0, false, ErrIntegrity
	}
	file, err := s.root.Open(retentionFloorName)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, retentionFloorBytes+1))
	if err != nil {
		return 0, false, err
	}
	if len(raw) != retentionFloorBytes {
		return 0, false, ErrIntegrity
	}
	value, err := strconv.ParseUint(string(raw[:16]), 16, 64)
	if err != nil || retentionFloorData(value) != string(raw) {
		return 0, false, ErrIntegrity
	}
	return value, true, nil
}

func (s *Store) advanceRetentionFloorLocked(next uint64) error {
	current, exists, err := s.retentionFloorLocked()
	if err != nil {
		return err
	}
	if exists && current >= next {
		// A prior rename may have succeeded before its directory fsync failed.
		return s.syncDir(".")
	}
	if !exists {
		if retentionFloorBytes > s.cfg.Limits.MaxBytes-s.used-s.reservedBytes || s.objects+s.reservedObjects >= s.cfg.Limits.MaxObjects {
			return ErrQuota
		}
		for name := range s.sizes {
			if strings.HasPrefix(name, "retained/") || strings.HasPrefix(name, "retired/") {
				return fmt.Errorf("%w: retention index without admission floor", ErrIntegrity)
			}
		}
	}
	tmp := tempName()
	file, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(tmp)
	if _, err := io.WriteString(file, retentionFloorData(next)); err != nil {
		_ = file.Close()
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, retentionFloorName); err != nil {
		return err
	}
	if !exists {
		s.used += retentionFloorBytes
		s.objects++
		s.sizes[retentionFloorName] = retentionFloorBytes
	}
	return s.syncDir(".")
}

// OpenRetained requires the admission fence on every reopen, even when all
// receipts have been compacted. Losing that file is corruption, not a fresh
// store that may accept previously released uploads.
func OpenRetained(cfg Config) (*Store, error) {
	return open(cfg, true)
}

func (s *Store) openRetentionFloor(required, fresh bool) error {
	_, exists, err := s.retentionFloorLocked()
	if err != nil {
		return err
	}
	if !required || exists {
		return nil
	}
	if !fresh {
		return fmt.Errorf("%w: missing retention admission floor", ErrIntegrity)
	}
	return s.advanceRetentionFloorLocked(0)
}
