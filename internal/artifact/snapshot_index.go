package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A snapshot stages a whole work tree. From an empty index git must hash
// every file to learn it is unchanged; from the index the previous
// snapshot of the same directory left behind it trusts matching stat data
// and hashes only what moved. The index is a cache, never a record: the
// snapshot still resets it to the parent tree, so a stale or missing one
// costs time, not correctness.
const (
	snapshotIndexDir = "steve-snapshot-index"
	// An index nobody refreshed for this long belongs to a directory that
	// is gone or idle; rebuilding it once is cheaper than keeping it.
	snapshotIndexTTL = 7 * 24 * time.Hour
	snapshotPruneGap = time.Hour
)

var snapshotPruned sync.Map // cache dir -> time.Time of the last sweep

// snapshotIndex gives one snapshot of workTree a private index. With seed
// it starts from the cached index of the directory's last snapshot. keep
// publishes the private index as the new cache; cleanup discards it and is
// safe after keep. Concurrent snapshots of one directory each work on a
// copy, and whichever keeps last wins — any of them is a valid cache.
func (r *Repo) snapshotIndex(workTree string, seed bool) (index string, keep func(), cleanup func(), err error) {
	dir := filepath.Join(r.Dir, snapshotIndexDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, nil, err
	}
	sum := sha256.Sum256([]byte(workTree))
	cached := filepath.Join(dir, hex.EncodeToString(sum[:16]))
	// The copy keeps the cache's old mtime, so its age is in its name.
	f, err := os.CreateTemp(dir, fmt.Sprintf("tmp-%d-*", time.Now().Unix()))
	if err != nil {
		return "", nil, nil, err
	}
	index = f.Name()
	cleanup = func() { os.Remove(index) }
	copied := seed && copyIndex(f, cached)
	f.Close()
	if !copied {
		// git starts from no index file, not from an empty one.
		os.Remove(index)
	}
	keep = func() {
		if os.Rename(index, cached) == nil {
			r.pruneSnapshotIndexes(dir)
		}
	}
	return index, keep, cleanup, nil
}

// copyIndex copies the cached index into dst with its modification time:
// git judges which entries are racily clean against the index file's own
// timestamp, and a fresher copy would vouch for files it never saw.
func copyIndex(dst *os.File, cached string) bool {
	src, err := os.Open(cached)
	if err != nil {
		return false
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return false
	}
	if _, err := io.Copy(dst, src); err != nil {
		return false
	}
	return os.Chtimes(dst.Name(), info.ModTime(), info.ModTime()) == nil
}

func (r *Repo) pruneSnapshotIndexes(dir string) {
	now := time.Now()
	if last, ok := snapshotPruned.Load(dir); ok && now.Sub(last.(time.Time)) < snapshotPruneGap {
		return
	}
	snapshotPruned.Store(dir, now)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if rest, ok := strings.CutPrefix(name, "tmp-"); ok {
			// One this old was left by a process that died mid-snapshot.
			stamp, _, _ := strings.Cut(rest, "-")
			if created, err := strconv.ParseInt(stamp, 10, 64); err == nil && now.Sub(time.Unix(created, 0)) > snapshotPruneGap {
				os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > snapshotIndexTTL {
			os.Remove(filepath.Join(dir, name))
		}
	}
}
