package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// pendingRemember is a write-ahead record for one scope. It contains the
// chosen fact ID and stable receipt before the authoritative file changes.
// Mutation entry points recover it while holding the same scope file lock.
type pendingRemember struct {
	Scope      Scope          `json:"scope"`
	BeforeHash string         `json:"before_hash"`
	After      string         `json:"after"`
	Request    requestReceipt `json:"request"`
}

type requestFiles struct{ receipts, pending string }

func (m *Markdown) requestFiles(scope Scope) (requestFiles, error) {
	path, err := m.Path(scope)
	if err != nil {
		return requestFiles{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return requestFiles{}, err
	}
	return requestFiles{receipts: path + ".requests.jsonl", pending: path + ".pending.json"}, nil
}

func bodyHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// RememberOnce owns both the fact write and its request receipt. A changed
// retry still returns the first receipt, even after Forget removed its fact.
// The boolean reports a replay, which must not be indexed or audited as a new write.
func (m *Markdown) RememberOnce(ctx context.Context, scope Scope, section, text, key string, now time.Time) (Receipt, bool, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, false, err
	}
	if key == "" {
		return Receipt{}, false, errors.New("idempotency key is required")
	}
	unlock, err := m.lock(scope)
	if err != nil {
		return Receipt{}, false, err
	}
	defer unlock()
	if err := m.recoverRemember(scope, now); err != nil {
		return Receipt{}, false, err
	}
	files, err := m.requestFiles(scope)
	if err != nil {
		return Receipt{}, false, err
	}
	keys, err := loadReceipts(files.receipts, now)
	if err != nil {
		return Receipt{}, false, fmt.Errorf("memory receipts: %w", err)
	}
	k := requestKey{Scope: scope, Key: key}
	if entry, ok := keys[k]; ok {
		return entry.Receipt, true, nil
	}
	before, err := m.read(scope)
	if err != nil {
		return Receipt{}, false, err
	}
	after, r, err := prepareRemember(scope, section, text, before)
	if err != nil {
		return Receipt{}, false, err
	}
	pending := pendingRemember{Scope: scope, BeforeHash: bodyHash(before), After: after, Request: requestReceipt{Scope: scope, Key: key, At: now, Receipt: r}}
	raw, err := json.Marshal(pending)
	if err != nil {
		return Receipt{}, false, err
	}
	if err := atomicMemoryFile(files.pending, raw); err != nil {
		return Receipt{}, false, fmt.Errorf("prepare memory request: %w", err)
	}
	if err := m.recoverRemember(scope, now); err != nil {
		return Receipt{}, false, err
	}
	return r, false, nil
}

// Reads observe the current Markdown only; recovery is a mutation and never
// runs from List/Text/Snapshot. Every mutation calls this under the scope lock.
func (m *Markdown) recoverRemember(scope Scope, now time.Time) error {
	files, err := m.requestFiles(scope)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(files.pending)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pending memory request: %w", err)
	}
	var pending pendingRemember
	if err := json.Unmarshal(raw, &pending); err != nil {
		return fmt.Errorf("read pending memory request: %w", err)
	}
	if pending.Scope != scope || pending.Request.Scope != scope || pending.Request.Key == "" || pending.Request.Receipt.ID == "" || pending.BeforeHash == "" {
		return errors.New("pending memory request is invalid for this scope")
	}
	// Commit evidence has no replay TTL while its WAL remains. An expired
	// receipt still proves that applying After again would be a duplicate.
	keys, err := readReceipts(files.receipts)
	if err != nil {
		return fmt.Errorf("read memory receipts: %w", err)
	}
	key := requestKey{Scope: scope, Key: pending.Request.Key}
	if entry, ok := keys[key]; ok {
		if entry.Receipt == pending.Request.Receipt && entry.At.Equal(pending.Request.At) {
			// A durable receipt proves this mutation completed. A later user
			// edit or forget must not be overwritten during delayed cleanup.
			return removePending(files.pending)
		}
		if entry.At.Add(idempotencyTTL).After(pending.Request.At) {
			return errors.New("pending memory request conflicts with a recorded receipt")
		}
		// A newly accepted request may reuse a key after its previous receipt
		// expired. That older operation is not evidence for this WAL's write.
		delete(keys, key)
	}
	current, err := m.read(scope)
	if err != nil {
		return err
	}
	switch bodyHash(current) {
	case bodyHash(pending.After):
		if err := m.syncMemory(scope); err != nil {
			return fmt.Errorf("sync recovered memory: %w", err)
		}
	case pending.BeforeHash:
		if err := m.write(scope, pending.After); err != nil {
			return fmt.Errorf("apply prepared memory request: %w", err)
		}
	default:
		return errors.New("memory changed outside the pending request; recovery was stopped to preserve those edits")
	}
	expireReceipts(keys, now)
	keys[key] = pending.Request
	if err := saveReceipts(files.receipts, keys); err != nil {
		return fmt.Errorf("persist memory receipt: %w", err)
	}
	return removePending(files.pending)
}

func removePending(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear committed memory request: %w", err)
	}
	return syncMemoryDir(filepath.Dir(path))
}

func (m *Markdown) syncMemory(scope Scope) error {
	path, err := m.writeTarget(scope)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return syncMemoryDir(filepath.Dir(path))
}

func syncMemoryDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func atomicMemoryFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".memory-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return err
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
	return syncMemoryDir(dir)
}

func loadReceipts(path string, now time.Time) (map[requestKey]requestReceipt, error) {
	keys, err := readReceipts(path)
	if err != nil {
		return nil, err
	}
	expireReceipts(keys, now)
	return keys, nil
}

func expireReceipts(keys map[requestKey]requestReceipt, now time.Time) {
	for key, entry := range keys {
		if !entry.At.Add(idempotencyTTL).After(now) {
			delete(keys, key)
		}
	}
}

func readReceipts(path string) (map[requestKey]requestReceipt, error) {
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
		keys[requestKey{Scope: entry.Scope, Key: entry.Key}] = entry
	}
}

// Rewrite only the live receipts and rename atomically: a crash must not
// leave a partial line that loses earlier successful requests on restart.
func saveReceipts(path string, keys map[requestKey]requestReceipt) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, entry := range keys {
		if err := enc.Encode(entry); err != nil {
			return err
		}
	}
	return atomicMemoryFile(path, body.Bytes())
}
