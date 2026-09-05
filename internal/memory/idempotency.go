package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const idempotencyTTL = 24 * time.Hour

type requestKey struct {
	Scope Scope
	Key   string
}

type requestReceipt struct {
	Scope   Scope     `json:"scope"`
	Key     string    `json:"idempotency_key"`
	At      time.Time `json:"at"`
	Receipt Receipt   `json:"receipt"`
}

// rememberOnce serializes lookup, write and receipt persistence so two
// concurrent retries cannot both write. Only successful receipts are kept;
// replay does not extend their lifetime or consult the current memory.
func (s *Service) rememberOnce(scope Scope, key string, write func() (Receipt, error)) (Receipt, error) {
	s.requests.Lock()
	defer s.requests.Unlock()
	path := ""
	if s.auditPath != "" {
		path = filepath.Join(filepath.Dir(s.auditPath), "idempotency.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return Receipt{}, err
		}
		unlock, err := acquire(path + ".lock")
		if err != nil {
			return Receipt{}, err
		}
		defer unlock()
		now := s.now()
		keys, err := loadReceipts(path, now)
		if err != nil {
			return Receipt{}, fmt.Errorf("memory idempotency: %w", err)
		}
		// A prior persistence failure still has its receipt in this
		// process. Retry saving it before acknowledging another request.
		pending := false
		for k, entry := range s.keys {
			if _, ok := keys[k]; !ok && entry.At.Add(idempotencyTTL).After(now) {
				keys[k] = entry
				pending = true
			}
		}
		if pending {
			if err := saveReceipts(path, keys); err != nil {
				return Receipt{}, fmt.Errorf("persist memory idempotency receipt: %w", err)
			}
		}
		s.keys = keys
	}
	if s.keys == nil {
		s.keys = make(map[requestKey]requestReceipt)
	}
	now := s.now()
	for k, entry := range s.keys {
		if !entry.At.Add(idempotencyTTL).After(now) {
			delete(s.keys, k)
		}
	}
	k := requestKey{Scope: scope, Key: key}
	if entry, ok := s.keys[k]; ok {
		return entry.Receipt, nil
	}
	r, err := write()
	if err != nil {
		return r, err
	}
	s.keys[k] = requestReceipt{Scope: scope, Key: key, At: s.now(), Receipt: r}
	if path != "" {
		if err := saveReceipts(path, s.keys); err != nil {
			return Receipt{}, fmt.Errorf("persist memory idempotency receipt: %w", err)
		}
	}
	return r, nil
}

func loadReceipts(path string, now time.Time) (map[requestKey]requestReceipt, error) {
	keys := make(map[requestKey]requestReceipt)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return keys, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var entry requestReceipt
		if err := dec.Decode(&entry); errors.Is(err, io.EOF) {
			return keys, nil
		} else if err != nil {
			return nil, err
		}
		if entry.At.Add(idempotencyTTL).After(now) {
			keys[requestKey{Scope: entry.Scope, Key: entry.Key}] = entry
		}
	}
}

// Rewrite only the live receipts and rename atomically: a crash must not
// leave a partial line that loses earlier successful requests on restart.
func saveReceipts(path string, keys map[requestKey]requestReceipt) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".idempotency-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, entry := range keys {
		if err := enc.Encode(entry); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
