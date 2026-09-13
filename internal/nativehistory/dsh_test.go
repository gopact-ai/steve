package nativehistory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDshImportsRootSessionAndIndependentCompressedEventFrames(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		home := t.TempDir()
		name := "sessions/-work/root-session/session.jsonl"
		header := []byte("{\"type\":\"session\",\"version\":1,\"id\":\"root-session\",\"cwd\":\"/work\",\"createdAt\":0,\"delegationDepth\":0}\n")
		event := []byte("{\"type\":\"text\",\"text\":\"context marker\"}\n")
		raw := append(append([]byte{}, header...), event...)
		if compressed {
			name += ".zstd"
			encoder, err := zstd.NewWriter(nil)
			if err != nil {
				t.Fatal(err)
			}
			raw = append(encoder.EncodeAll(header, nil), encoder.EncodeAll(event, nil)...)
			encoder.Close()
		}
		writeFixture(t, home, name, string(raw))
		writeFixture(t, home, "sessions/-work/child/session.jsonl", "{\"type\":\"session\",\"id\":\"child\",\"cwd\":\"/work\",\"origin\":\"subagent\"}\n")
		src := Source{Harness: "dsh", Home: home}
		entries, err := List(t.Context(), src)
		if err != nil || len(entries) != 1 || entries[0].NativeID != "root-session" || entries[0].Workdir != "/work" {
			t.Fatalf("DSH metadata: %+v %v", entries, err)
		}
		store := t.TempDir()
		ref, err := Snapshot(t.Context(), store, ImportRequest{CommandID: "import", Source: src, NativeID: entries[0].NativeID, Revision: entries[0].Revision})
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "runtime")
		if err := Materialize(t.Context(), store, ref, dest); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(got) != string(raw) {
			t.Fatal("DSH storage encoding or event frames changed")
		}
	}
}

func TestDshOpaqueThreadIDResumesFromEncodedDirectory(t *testing.T) {
	home := t.TempDir()
	id := "lark/chat:one/thread/two"
	name := "sessions/-work/lark~002fchat~003aone~002fthread~002ftwo/session.jsonl"
	writeFixture(t, home, name, `{"type":"session","id":"`+id+`","cwd":"/work"}`+"\n")
	src := Source{Harness: "dsh", Home: home}
	entries, err := List(t.Context(), src)
	if err != nil || len(entries) != 1 || entries[0].NativeID != id {
		t.Fatalf("opaque history: %+v %v", entries, err)
	}
	store := t.TempDir()
	ref, err := Snapshot(t.Context(), store, ImportRequest{CommandID: "opaque", Source: src, NativeID: id, Revision: entries[0].Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := ref.Validate("dsh", "/work"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "runtime")
	if err := Materialize(t.Context(), store, ref, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
		t.Fatal(err)
	}
}
