package skills

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
