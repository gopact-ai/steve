package plugins

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBundleRejectsUntrustedArchivesBeforeWriting(t *testing.T) {
	good := example(t, "github")
	for _, tc := range []string{"absolute", "parent", "symlink", "hardlink", "duplicate", "file-directory", "trailing", "noncanonical", "too-large"} {
		t.Run(tc, func(t *testing.T) {
			files, err := archiveFiles(good.Data)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			writer := tar.NewWriter(&buf)
			put := func(h *tar.Header, data []byte) {
				t.Helper()
				if err := writer.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
				if len(data) > 0 {
					if _, err := writer.Write(data); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, name := range sortedKeys(files) {
				data := files[name].data
				header := &tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), Typeflag: tar.TypeReg}
				if tc == "noncanonical" {
					header.Uid = 123
				}
				put(header, data)
			}
			switch tc {
			case "absolute":
				put(&tar.Header{Name: "/outside", Mode: 0644, Typeflag: tar.TypeReg}, nil)
			case "parent":
				put(&tar.Header{Name: "../outside", Mode: 0644, Typeflag: tar.TypeReg}, nil)
			case "symlink":
				put(&tar.Header{Name: "link", Linkname: "/outside", Mode: 0644, Typeflag: tar.TypeSymlink}, nil)
			case "hardlink":
				put(&tar.Header{Name: "link", Linkname: "plugin.json", Mode: 0644, Typeflag: tar.TypeLink}, nil)
			case "duplicate":
				put(&tar.Header{Name: ManifestFile, Mode: 0644, Typeflag: tar.TypeReg}, nil)
			case "file-directory":
				put(&tar.Header{Name: "skills", Mode: 0644, Typeflag: tar.TypeReg}, nil)
			case "too-large":
				if err := writer.WriteHeader(&tar.Header{Name: "large", Mode: 0644, Typeflag: tar.TypeReg, Size: MaxFileBytes + 1}); err != nil {
					t.Fatal(err)
				}
			}
			if tc != "too-large" {
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			data := buf.Bytes()
			if tc == "trailing" {
				data = append(append([]byte(nil), good.Data...), []byte("junk")...)
			}
			if _, err := DecodeBundle(data); err == nil {
				t.Fatal("accepted untrusted archive")
			}
		})
	}
}

func TestBundledProgramIsInspectedWithoutExecutingIt(t *testing.T) {
	dir := fixtureDirectory(t)
	m := example(t, "github").Manifest
	marker := filepath.Join(t.TempDir(), "executed")
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	code := []byte("#!/bin/sh\ntouch '" + marker + "'\n")
	if err := os.WriteFile(filepath.Join(dir, "bin/tool"), code, 0700); err != nil {
		t.Fatal(err)
	}
	m.MCP["github"] = MCPServer{Transport: "stdio", Program: &Program{Path: "bin/tool"}}
	writeManifest(t, dir, m)
	bundle, err := ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: filepath.Join(t.TempDir(), "store")}
	if _, err := store.Prepare(t.Context(), "program", bundle.Digest, Source{Kind: "directory", Location: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("preparation executed program: %v", err)
	}
}

func FuzzManifest(f *testing.F) {
	f.Add([]byte(`{"schema":1,"api":"steve.plugins.v1","id":"test/example","version":"1.0.0","description":"test","skills":{"test":"skills/test"}}`))
	f.Add([]byte(`{"schema":1,"Schema":2}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		m, err := ParseManifest(raw)
		if err != nil {
			return
		}
		canonical, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		second, err := ParseManifest(canonical)
		if err != nil {
			t.Fatalf("accepted manifest cannot round trip: %v", err)
		}
		again, err := json.Marshal(second)
		if err != nil || !bytes.Equal(canonical, again) {
			t.Fatal("canonical manifest is unstable")
		}
	})
}

func TestBundleRejectsPortablePathCollisions(t *testing.T) {
	for _, pair := range [][2]string{{"README.md", "readme.md"}, {"Skills/a", "skills/b"}, {"caf\u00e9.txt", "cafe\u0301.txt"}} {
		files, err := archiveFiles(example(t, "github").Data)
		if err != nil {
			t.Fatal(err)
		}
		files[pair[0]] = packageFile{data: []byte("one")}
		files[pair[1]] = packageFile{data: []byte("two")}
		if _, err := packFiles(files); err == nil {
			t.Fatalf("accepted collision %q", pair)
		}
	}
}
