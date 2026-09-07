package contentreplica_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/project"
)

type corruptTransport struct {
	base         *transport
	bad          map[string]bool
	wrongReceipt bool
}

func TestArtifactRepairsMissingAndCorruptObjectsWithinAnExistingCommit(t *testing.T) {
	for _, damage := range []string{"missing-blob", "missing-tree", "corrupt-blob"} {
		t.Run(damage, func(t *testing.T) {
			_, r, client := newCluster(t, "internal", "a", "b", "c")
			book := openBook(t)
			projects := project.Open(book)
			work := t.TempDir()
			if err := os.WriteFile(filepath.Join(work, "answer.txt"), []byte("original bytes\n"), 0600); err != nil {
				t.Fatal(err)
			}
			p := project.Project{ID: "p", Home: project.Home{Path: work}}
			if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
				t.Fatal(err)
			}
			p, _, _ = projects.Get(t.Context(), "p")
			dir := t.TempDir()
			a := artifact.New(dir, book, projects, artifact.LocalNodes{Dir: t.TempDir()})
			a.SetReplication(client("a"))
			snapshot, _, err := a.SnapshotCanonical(t.Context(), p, "", "worker", "snapshot")
			if err != nil {
				t.Fatal(err)
			}
			repoDir := filepath.Join(dir, "objects", "p.git")
			spec := snapshot.ID + ":answer.txt"
			if damage == "missing-tree" {
				spec = snapshot.ID + "^{tree}"
			}
			raw, err := exec.Command("git", "--git-dir", repoDir, "rev-parse", spec).Output()
			if err != nil {
				t.Fatal(err)
			}
			id := strings.TrimSpace(string(raw))
			objectPath := filepath.Join(repoDir, "objects", id[:2], id[2:])
			if damage == "corrupt-blob" {
				file, err := os.Open(objectPath)
				if err != nil {
					t.Fatal(err)
				}
				reader, err := zlib.NewReader(file)
				if err != nil {
					t.Fatal(err)
				}
				plain, err := io.ReadAll(reader)
				reader.Close()
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
				separator := bytes.IndexByte(plain, 0)
				plain[separator+1] = 'X'
				var corrupt bytes.Buffer
				writer := zlib.NewWriter(&corrupt)
				if _, err := writer.Write(plain); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(objectPath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(objectPath, corrupt.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(objectPath); err != nil {
				t.Fatal(err)
			}
			r.offline["a"] = true
			c := artifact.New(dir, book, projects, artifact.LocalNodes{Dir: t.TempDir()})
			c.SetReplication(client("c"))
			got, _, _, truncated, err := c.File(t.Context(), "p", snapshot.ID, "answer.txt")
			if err != nil || truncated || got != "original bytes\n" {
				t.Fatalf("incomplete commit cache not repaired: %q %v", got, err)
			}
		})
	}
}

func TestGitHistoryVerificationHasItsOwnBudget(t *testing.T) {
	_, _, client := newCluster(t, "internal", "a", "b")
	book := openBook(t)
	projects := project.Open(book)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "answer.txt"), []byte("small file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := project.Project{ID: "p", Home: project.Home{Path: work}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = projects.Get(t.Context(), "p")
	store := artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	store.SetReplication(client("a"))
	store.Limits.MaxBytes = 64
	first, _, err := store.SnapshotCanonical(t.Context(), p, "", "worker", "snapshot")
	if err != nil {
		t.Fatalf("Git metadata incorrectly consumed the workspace file budget: %v", err)
	}
	store.ContentLimits.MaxExpandedBytes = 64
	if _, _, err := store.SnapshotCanonical(t.Context(), p, first.ID, "worker", "snapshot"); !errors.Is(err, contentreplica.ErrTooLarge) {
		t.Fatalf("explicit closure limit did not apply: %v", err)
	}
}

func TestArtifactRepairsCorruptPackWithoutDiscardingItsOtherObjects(t *testing.T) {
	_, r, client := newCluster(t, "internal", "a", "b", "c")
	book := openBook(t)
	projects := project.Open(book)
	work := t.TempDir()
	for i := range 150 {
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("file%03d.txt", i)), []byte(fmt.Sprintf("original bytes for file %03d repeat repeat repeat\n", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := project.Project{ID: "p", Home: project.Home{Path: work}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = projects.Get(t.Context(), "p")
	dir := t.TempDir()
	a := artifact.New(dir, book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	a.SetReplication(client("a"))
	m, _, err := a.SnapshotCanonical(t.Context(), p, "", "worker", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 150 {
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("file%03d.txt", i)), []byte(fmt.Sprintf("independent branch file %03d distinct bytes\n", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	other, _, err := a.SnapshotCanonical(t.Context(), p, "", "worker", "independent root")
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{m.ID: "original bytes for file 000 repeat repeat repeat\n", other.ID: "independent branch file 000 distinct bytes\n"}
	first, last := m, other
	if first.ID > last.ID {
		first, last = last, first
	}
	repoDir := filepath.Join(dir, "objects", "p.git")
	if out, err := exec.Command("git", "--git-dir", repoDir, "repack", "-ad", "--window=0").CombinedOutput(); err != nil {
		t.Fatalf("pack: %s %v", out, err)
	}
	raw, err := exec.Command("git", "--git-dir", repoDir, "rev-parse", last.ID+":file000.txt").Output()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(raw))
	indexes, err := filepath.Glob(filepath.Join(repoDir, "objects", "pack", "*.idx"))
	if err != nil {
		t.Fatal(err)
	}
	damaged := false
	for _, index := range indexes {
		raw, err := exec.Command("git", "verify-pack", "-v", index).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 5 || fields[0] != id {
				continue
			}
			offset, _ := strconv.Atoi(fields[4])
			length, _ := strconv.Atoi(fields[3])
			pack := strings.TrimSuffix(index, ".idx") + ".pack"
			data, err := os.ReadFile(pack)
			if err != nil {
				t.Fatal(err)
			}
			start := offset
			for data[start]&128 != 0 {
				start++
			}
			start++
			compressed := data[start : offset+length]
			mutated := corruptCompressedObject(t, compressed)
			copy(data[start:offset+length], mutated)
			if err := os.Chmod(pack, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pack, data, 0600); err != nil {
				t.Fatal(err)
			}
			damaged = true
			break
		}
	}
	if !damaged {
		t.Fatal("did not locate packed blob")
	}
	r.offline["a"] = true
	c := artifact.New(dir, book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	c.SetReplication(client("c"))
	got, _, _, _, err := c.File(t.Context(), "p", first.ID, "file000.txt")
	if err != nil || got != expected[first.ID] {
		t.Fatalf("corrupt pack not repaired: %q %v", got, err)
	}
	if got, _, _, _, err := c.File(t.Context(), "p", last.ID, "file000.txt"); err != nil || got != expected[last.ID] {
		t.Fatalf("other registered branch lost: %q %v", got, err)
	}
	quarantined, _ := filepath.Glob(filepath.Join(repoDir, "objects", "content-quarantine", "*", "*.pack"))
	if len(quarantined) != 1 {
		t.Fatal("corrupt pack's unrelated objects were not retained")
	}
}

func corruptCompressedObject(t *testing.T, original []byte) []byte {
	t.Helper()
	reader, err := zlib.NewReader(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	for position := 2; position < len(original)-4; position++ {
		for bit := range 8 {
			mutated := bytes.Clone(original)
			mutated[position] ^= 1 << bit
			reader, err := zlib.NewReader(bytes.NewReader(mutated))
			if err != nil {
				continue
			}
			data, err := io.ReadAll(reader)
			reader.Close()
			if !errors.Is(err, zlib.ErrChecksum) || len(data) != len(plain) || bytes.Equal(data, plain) {
				continue
			}
			binary.BigEndian.PutUint32(mutated[len(mutated)-4:], adler32.Checksum(data))
			reader, err = zlib.NewReader(bytes.NewReader(mutated))
			if err != nil {
				continue
			}
			_, err = io.ReadAll(reader)
			reader.Close()
			if err == nil {
				return mutated
			}
		}
	}
	t.Fatal("could not produce same-length valid compressed corruption")
	return nil
}

func TestPublicProtectionReadsFollowRepairsAndCannotBeDowngraded(t *testing.T) {
	p, r, client := newCluster(t, "internal", "a", "b")
	single, err := contentreplica.New(contentreplica.Config{NodeID: "a", Local: r.stores["a"], Remote: r, Policy: p, Scope: func(context.Context, string) (contentreplica.Scope, error) { return p.scope, nil }, Members: func(context.Context) ([]string, error) { return []string{"a"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	book := openBook(t)
	store, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetReplication(single)
	data := []byte("original single copy")
	m, err := store.Upload(t.Context(), "p", "note", "text/plain", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	old := *m.Content
	updated, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, m.Digest, checkpoint.Reference(data), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, updated); return err }); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(t.Context(), "p", m.ID)
	if err != nil || got.Content == nil || !got.Content.Recoverable() {
		t.Fatalf("Get hid upgraded protection: %+v %v", got, err)
	}
	listed, err := store.List(t.Context(), "p")
	if err != nil || len(listed) != 1 || !listed[0].Content.Recoverable() {
		t.Fatalf("List hid upgraded protection: %+v %v", listed, err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, old); return err }); err != nil {
		t.Fatal(err)
	}
	current, _, err := contentreplica.Lookup(t.Context(), book, old.ID)
	if err != nil || !current.Recoverable() {
		t.Fatalf("old single snapshot downgraded protection: %+v %v", current, err)
	}
}

func (r corruptTransport) Put(ctx context.Context, node string, object contentreplica.Object, source io.Reader) (contentreplica.Receipt, error) {
	receipt, err := r.base.Put(ctx, node, object, source)
	if err == nil && r.wrongReceipt {
		receipt.NodeID = "forged-node"
	}
	return receipt, err
}
func (r corruptTransport) Get(ctx context.Context, node string, object contentreplica.Object, into io.Writer) error {
	if r.bad[node] {
		_, err := into.Write([]byte("bad"))
		return err
	}
	return r.base.Get(ctx, node, object, into)
}

func customClient(t *testing.T, p *places, r *transport, node string, remote contentreplica.Transport) *contentreplica.Client {
	t.Helper()
	c, err := contentreplica.New(contentreplica.Config{NodeID: node, Local: r.stores[node], Remote: remote, Policy: p, Scope: func(context.Context, string) (contentreplica.Scope, error) { return p.scope, nil }, Members: func(context.Context) ([]string, error) { return []string{"a", "b", "c"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCorruptReplicaCannotLeakPartialBytesOrFakeCompletion(t *testing.T) {
	p, r, client := newCluster(t, "internal", "a", "b", "c")
	data := []byte("complete validated content")
	ref := checkpoint.Reference(data)
	m, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	c := customClient(t, p, r, "c", corruptTransport{base: r, bad: map[string]bool{"a": true, "b": true}})
	var output bytes.Buffer
	if _, err := c.Read(t.Context(), m, &output); !errors.Is(err, contentreplica.ErrIncomplete) || output.Len() != 0 {
		t.Fatalf("corrupt replicas released content: %q %v", output.Bytes(), err)
	}
	c = customClient(t, p, r, "c", corruptTransport{base: r, bad: map[string]bool{"a": true}})
	repaired, err := c.Read(t.Context(), m, &output)
	if err != nil || !bytes.Equal(output.Bytes(), data) || len(repaired.Receipts) != 3 {
		t.Fatalf("healthy fallback=%q %+v %v", output.Bytes(), repaired, err)
	}
}

func TestPlacementChecksPrecedeTransferAndRequireIndependentMachines(t *testing.T) {
	p, r, client := newCluster(t, "restricted", "a", "b", "c")
	p.denied["b"] = true
	p.domains["c"] = p.domains["a"]
	calls := 0
	r.before = func(string, contentreplica.Object) { calls++ }
	data := []byte("classified")
	ref := checkpoint.Reference(data)
	if _, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data)); !errors.Is(err, contentreplica.ErrIncomplete) {
		t.Fatalf("same failure domain accepted: %v", err)
	}
	if calls != 0 {
		t.Fatal("sent classified bytes to disallowed or duplicate physical target")
	}
	p.denied["a"] = true
	if _, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data)); !errors.Is(err, contentreplica.ErrPlacement) {
		t.Fatalf("disallowed local cache accepted: %v", err)
	}
}

func TestWrongReceiptTruncationAndReceiverLimitAreRejected(t *testing.T) {
	p, r, client := newCluster(t, "internal", "a", "b", "c")
	data := []byte("declared bytes")
	ref := checkpoint.Reference(data)
	c := customClient(t, p, r, "a", corruptTransport{base: r, wrongReceipt: true})
	if _, err := c.Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data)); !errors.Is(err, contentreplica.ErrIntegrity) {
		t.Fatalf("forged receiver identity accepted: %v", err)
	}
	if _, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data[:3])); err == nil {
		t.Fatal("truncated source accepted")
	}
	store, err := contentreplica.Open(contentreplica.StoreConfig{Dir: t.TempDir(), NodeID: "a", Policy: p, Limits: checkpoint.Limits{MaxBlobBytes: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Put(t.Context(), contentreplica.Object{Scope: p.scope, Kind: contentreplica.Material, Key: ref.SHA256, Blob: ref}, bytes.NewReader(data)); !errors.Is(err, contentreplica.ErrTooLarge) {
		t.Fatalf("receiver byte limit bypassed: %v", err)
	}
}

func TestAcknowledgedContentSurvivesStoreReopen(t *testing.T) {
	dir := t.TempDir()
	p := &places{scope: contentreplica.Scope{ProjectID: "p", Level: "internal", HomeNodeID: "a"}, domains: map[string]string{"a": "physical-a"}}
	cfg := contentreplica.StoreConfig{Dir: dir, NodeID: "a", Policy: p}
	store, err := contentreplica.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("durable")
	ref := checkpoint.Reference(data)
	object := contentreplica.Object{Scope: p.scope, Kind: contentreplica.Material, Key: ref.SHA256, Blob: ref}
	receipt, err := store.Put(t.Context(), object, bytes.NewReader(data))
	if err != nil || receipt.ObjectID != object.ID() {
		t.Fatalf("receipt=%+v %v", receipt, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = contentreplica.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var got bytes.Buffer
	if err := store.Get(t.Context(), object, &got); err != nil || !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("reopened data=%q %v", got.Bytes(), err)
	}
}

func TestRestoreDoesNotPublishCacheBeforeRepairReceiptCommits(t *testing.T) {
	_, r, client := newCluster(t, "internal", "a", "b", "c")
	book := openBook(t)
	a, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetReplication(client("a"))
	m, err := a.Upload(t.Context(), "p", "note", "text/plain", bytes.NewBufferString("restore only after recorded"))
	if err != nil {
		t.Fatal(err)
	}
	r.offline["a"] = true
	dir := t.TempDir()
	c, err := material.Open(dir, book)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReplication(client("c"))
	if _, err := book.DB().Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Content(t.Context(), "p", m.ID); err == nil {
		t.Fatal("read-only writer acknowledged repaired cache")
	}
	if _, err := os.Stat(filepath.Join(dir, m.Digest)); !os.IsNotExist(err) {
		t.Fatal("unrecorded repaired cache was published")
	}
	if _, err := book.DB().Exec("PRAGMA query_only=OFF"); err != nil {
		t.Fatal(err)
	}
	if _, data, err := c.Content(t.Context(), "p", m.ID); err != nil || string(data) != "restore only after recorded" {
		t.Fatalf("retry repair=%q %v", data, err)
	}
	manifest, _, err := contentreplica.Lookup(t.Context(), book, m.Content.ID)
	if err != nil || len(manifest.Receipts) != 3 {
		t.Fatalf("retry lost local receipt %+v %v", manifest, err)
	}
}

func TestArtifactWithoutSecondReceiptCannotMoveCanonicalReference(t *testing.T) {
	_, r, client := newCluster(t, "internal", "a", "b")
	r.offline["b"] = true
	book := openBook(t)
	projects := project.Open(book)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "x"), []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	p := project.Project{ID: "p", Home: project.Home{Path: work}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = projects.Get(t.Context(), "p")
	store := artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	store.SetReplication(client("a"))
	if _, _, err := store.SnapshotCanonical(t.Context(), p, "", "worker", "candidate"); !errors.Is(err, contentreplica.ErrIncomplete) {
		t.Fatalf("snapshot without replica acknowledged: %v", err)
	}
	if _, ok, err := book.Name(t.Context(), artifact.CanonicalRef("p")); err != nil || ok {
		t.Fatalf("canonical ref moved before durable copies: %v %v", ok, err)
	}
	manifests, _ := book.Bindings(t.Context(), "artifact")
	if len(manifests) != 0 {
		t.Fatal("incomplete artifact manifest published")
	}
}
