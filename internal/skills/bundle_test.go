package skills

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResolveRefsPreservesNamesAndInput(t *testing.T) {
	root := t.TempDir()
	original := bundleSkill(t, root, "alpha", "instructions")
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(original.Path, link); err != nil {
		t.Fatal(err)
	}
	refs := []Ref{{Name: "selected-name", Path: link}}
	before := slices.Clone(refs)
	resolved, err := ResolveRefs(refs)
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(original.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(refs, before) || len(resolved) != 1 || resolved[0] != (Ref{Name: "selected-name", Path: real}) {
		t.Fatalf("refs = %v, resolved = %v", refs, resolved)
	}
	if err := os.RemoveAll(original.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRefs(refs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("broken root error = %v", err)
	}
}

func bundleSkill(t *testing.T, root, name, body string) Ref {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Ref{Name: name, Path: dir}
}

// A bundle is content-addressed: the same skills pack to the same bytes
// regardless of order or timestamps, a change to any file changes the
// hash, and unpacking yields the same per-skill hashes the packer saw.
func TestBundleIsContentAddressed(t *testing.T) {
	root := t.TempDir()
	a := bundleSkill(t, root, "alpha", "# alpha\n")
	b := bundleSkill(t, root, "beta", "# beta\n")
	one, err := Pack([]Ref{a, b})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	_ = os.Chtimes(filepath.Join(a.Path, "SKILL.md"), time.Now(), time.Now())
	two, err := Pack([]Ref{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if one.Hash != two.Hash || !bytes.Equal(one.Data, two.Data) {
		t.Fatal("order or mtime changed the bundle")
	}
	if len(one.Skills) != 2 || one.Skills[0].Name != "alpha" || one.Skills[1].Name != "beta" {
		t.Fatalf("entries = %+v", one.Skills)
	}
	_ = os.WriteFile(filepath.Join(a.Path, "SKILL.md"), []byte("# alpha v2\n"), 0o644)
	three, _ := Pack([]Ref{a, b})
	if three.Hash == one.Hash || three.Skills[0].Hash == one.Skills[0].Hash || three.Skills[1].Hash != one.Skills[1].Hash {
		t.Fatal("a content change did not move exactly the changed skill's hash")
	}

	dest := t.TempDir()
	entries, err := Unpack(three.Data, dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0] != three.Skills[0] || entries[1] != three.Skills[1] {
		t.Fatalf("unpacked entries %+v, packed %+v", entries, three.Skills)
	}
	if info, err := os.Stat(filepath.Join(dest, "alpha", "scripts", "run.sh")); err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("executable bit lost: %v %v", info, err)
	}
	if HashOf(three.Data) != three.Hash {
		t.Fatal("HashOf disagrees with Pack")
	}
}

func TestPackOfNothingIsStable(t *testing.T) {
	one, err := Pack([]Ref{})
	if err != nil {
		t.Fatal(err)
	}
	two, err := Pack([]Ref{})
	if err != nil {
		t.Fatal(err)
	}
	if one.Hash != two.Hash {
		t.Fatalf("hashes differ: %q != %q", one.Hash, two.Hash)
	}
	if len(one.Skills) != 0 || len(two.Skills) != 0 {
		t.Fatalf("skills are not empty: %+v, %+v", one.Skills, two.Skills)
	}
	if len(one.Data) == 0 || len(two.Data) == 0 {
		t.Fatal("packed data is empty")
	}

	entries, err := Unpack(one.Data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unpacked entries = %+v", entries)
	}
}

// A bundle from the network cannot write outside its directory.
func TestUnpackRefusesEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "/abs/evil", "skill/../../evil", ".hidden/x", "noskill"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
		_ = tw.Close()
		if _, err := Unpack(buf.Bytes(), t.TempDir()); err == nil {
			t.Errorf("entry %q was accepted", name)
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "skill/link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	_ = tw.Close()
	if _, err := Unpack(buf.Bytes(), t.TempDir()); err == nil {
		t.Error("a symlink was accepted")
	}
}

// A bundle is bounded by what it unpacks to, not only by what arrived: a
// sparse entry turns a header into as many zeros as it claims, and each
// entry is a file created on disk.
func TestUnpackBoundsWhatABundleExpandsTo(t *testing.T) {
	var sparse bytes.Buffer
	tw := tar.NewWriter(&sparse)
	for _, name := range []string{"skill/a", "skill/b"} {
		records := paxRecord("GNU.sparse.major", "0") + paxRecord("GNU.sparse.minor", "1") +
			paxRecord("GNU.sparse.size", "20971520") + paxRecord("GNU.sparse.numblocks", "1") + paxRecord("GNU.sparse.map", "0,0")
		writeRawPAX(t, tw, &sparse, records)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sparse.Bytes()) > 1<<20 {
		t.Fatalf("the sparse bundle is %d bytes", len(sparse.Bytes()))
	}
	if _, err := Unpack(sparse.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("a bundle expanding past MaxBundleBytes: %v", err)
	}

	var many bytes.Buffer
	tw = tar.NewWriter(&many)
	for i := range maxUnpackEntries + 1 {
		if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("skill/%d", i), Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(many.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("a bundle of %d entries: %v", maxUnpackEntries+1, err)
	}
}

func paxRecord(key, value string) string {
	record := " " + key + "=" + value + "\n"
	size := len(record)
	for size != len(strconv.Itoa(size))+len(record) {
		size = len(strconv.Itoa(size)) + len(record)
	}
	return strconv.Itoa(size) + record
}

// writeRawPAX writes records as a PAX extended header, which tar.Writer
// only produces from records it knows how to encode.
func writeRawPAX(t *testing.T, tw *tar.Writer, out *bytes.Buffer, records string) {
	t.Helper()
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	start := out.Len()
	if err := tw.WriteHeader(&tar.Header{Name: "pax", Mode: 0o644, Size: int64(len(records)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(records)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	block := out.Bytes()[start : start+512]
	block[156] = tar.TypeXHeader
	copy(block[148:156], "        ")
	var sum int
	for _, b := range block {
		sum += int(b)
	}
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

// What the receiving side refuses is refused when packing, where the
// owner can see which skills are at fault.
func TestPackAndPackImportRefuseMoreEntriesThanUnpackAccepts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "many")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# many\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range maxUnpackEntries {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Pack([]Ref{{Name: "many", Path: dir}}); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("Pack: %v", err)
	}
	if _, err := PackImport(t.Context(), dir); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("PackImport: %v", err)
	}
}

// A backslash is a path separator on Windows, so an entry name holding one
// could climb out of the skill there; bundles never carry one.
func TestUnpackRefusesBackslashesInEntryNames(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: `skill/..\..\x`, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(buf.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("Unpack of a backslashed entry: %v", err)
	}
}
