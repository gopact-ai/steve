package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanLocalAndImportRoundTrip(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex", "skills", "notes")
	_ = os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: notes\ndescription: keep notes\n---\n# notes\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "scripts", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	// A second tool linking to the first's directory lists nothing new.
	_ = os.Symlink(filepath.Join(home, ".codex", "skills"), filepath.Join(home, ".agents"))
	found := ScanLocal(home)
	real, _ := filepath.EvalSymlinks(dir)
	if len(found) != 1 || found[0].Name != "notes" || found[0].Description != "keep notes" || found[0].Path != real {
		t.Fatalf("found = %+v", found)
	}
	encoded, err := PackImport(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "notes")
	if err := UnpackImport(encoded, dest); err != nil {
		t.Fatal(err)
	}
	if Describe(dest).Description != "keep notes" {
		t.Fatal("SKILL.md did not arrive")
	}
	if info, err := os.Stat(filepath.Join(dest, "scripts", "run.sh")); err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("script lost its bit: %v", err)
	}
	if err := UnpackImport(encoded, dest); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("overwrote: %v", err)
	}
	if err := UnpackImport("not base64!", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestUnpackImportClearsAnEarlierAttemptFirst(t *testing.T) {
	src := filepath.Join(t.TempDir(), "notes")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	encoded, err := PackImport(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "notes")
	stale := filepath.Join(dest+".loading", "stale.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UnpackImport(encoded, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "stale.txt")); err == nil {
		t.Fatal("an earlier attempt's file was merged into the import")
	}
	if os.Getuid() == 0 {
		t.Skip("root removes anything")
	}
	dest = filepath.Join(t.TempDir(), "notes")
	locked := filepath.Join(dest+".loading", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if err := UnpackImport(encoded, dest); err == nil || !strings.Contains(err.Error(), "clear an earlier import") {
		t.Fatalf("leftovers that will not go were merged: %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("a skill was installed on top of leftovers")
	}
}

// An import is bounded by what it unpacks to: gzip lets a small stream
// carry far more than the import cap, and each entry is a file on disk.
func TestUnpackImportBoundsWhatTheStreamExpandsTo(t *testing.T) {
	pack := func(files map[string]int) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for name, size := range files {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(size), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(make([]byte, size)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(buf.Bytes())
	}
	large := pack(map[string]int{"SKILL.md": 8, "a": 10 << 20, "b": 10 << 20})
	dest := filepath.Join(t.TempDir(), "large")
	if err := UnpackImport(large, dest); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("an import expanding past the import cap: %v", err)
	}
	if _, err := os.Lstat(dest); err == nil {
		t.Error("a refused import left its directory")
	}

	files := map[string]int{"SKILL.md": 8}
	for i := range maxUnpackEntries {
		files[fmt.Sprintf("f%d", i)] = 0
	}
	if err := UnpackImport(pack(files), filepath.Join(t.TempDir(), "many")); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("an import of %d entries: %v", len(files), err)
	}
}

// Entries that write nothing still cost a reader: an import stream of
// ignored entries is bounded by count and by the bytes it expands to.
func TestUnpackImportBoundsEntriesItIgnores(t *testing.T) {
	pack := func(write func(*tar.Writer)) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "SKILL.md", Mode: 0o644, Size: 0, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		write(tw)
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(buf.Bytes())
	}
	links := pack(func(tw *tar.Writer) {
		for i := range maxUnpackEntries {
			if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("l%d", i), Linkname: "SKILL.md", Typeflag: tar.TypeSymlink}); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := UnpackImport(links, filepath.Join(t.TempDir(), "links")); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("an import of %d ignored links: %v", maxUnpackEntries, err)
	}
	padded := pack(func(tw *tar.Writer) {
		const size = 64 << 20
		if err := tw.WriteHeader(&tar.Header{Name: "padding", Size: size, Typeflag: 'Z'}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(make([]byte, size)); err != nil {
			t.Fatal(err)
		}
	})
	if err := UnpackImport(padded, filepath.Join(t.TempDir(), "padded")); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("an import expanding to 64 MB of ignored data: %v", err)
	}
}

func TestCappedReaderEndsCleanlyAtItsCap(t *testing.T) {
	r := &cappedReader{r: strings.NewReader("abcd"), left: 4}
	if got, err := io.ReadAll(r); err != nil || string(got) != "abcd" {
		t.Fatalf("stream of exactly the cap = %q, %v", got, err)
	}
	r = &cappedReader{r: strings.NewReader("abcde"), left: 4}
	if _, err := io.ReadAll(r); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("stream past the cap: %v", err)
	}
}

// An import is the archive and the rest of its gzip stream. The tar reader
// stops at the archive's end blocks, so what follows them is read through
// the same cap, and gzip checks its framing and checksum on the way.
func TestImportRefusesWhatTheStreamCarriesPastTheArchive(t *testing.T) {
	archive := skillArchive(t)
	corrupted := gzipStream(t, archive, 0)
	corrupted[len(corrupted)-8] ^= 0xff // the member's CRC-32
	for name, c := range map[string]struct {
		stream  []byte
		refusal string
	}{
		"data past the stream cap":    {gzipStream(t, archive, importStreamCap), "expands to more than"},
		"bytes after the gzip member": {append(gzipStream(t, archive, 0), "this is not a gzip member"...), "invalid header"},
		"a corrupted checksum":        {corrupted, "invalid checksum"},
	} {
		dest := filepath.Join(t.TempDir(), "notes")
		if err := UnpackImport(base64.StdEncoding.EncodeToString(c.stream), dest); err == nil || !strings.Contains(err.Error(), c.refusal) {
			t.Errorf("%s: import = %v, want a refusal with %q", name, err, c.refusal)
		}
		for _, left := range []string{dest, dest + ".loading"} {
			if _, err := os.Lstat(left); err == nil {
				t.Errorf("%s: %s left behind", name, filepath.Base(left))
			}
		}
	}
	if err := UnpackImport(base64.StdEncoding.EncodeToString(gzipStream(t, archive, 0)), filepath.Join(t.TempDir(), "notes")); err != nil {
		t.Fatalf("the archive alone was refused: %v", err)
	}
}

// skillArchive is the tar archive of a skill that holds only its SKILL.md.
func skillArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("# notes\n")
	if err := tw.WriteHeader(&tar.Header{Name: "SKILL.md", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gzipStream compresses archive followed by trailing bytes of 'A' into one
// gzip member.
func gzipStream(t *testing.T, archive []byte, trailing int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(archive); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{'A'}, 1<<20)
	for trailing > 0 {
		n := min(trailing, len(chunk))
		if _, err := gz.Write(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		trailing -= n
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
