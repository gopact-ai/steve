package nativehistory

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDshImportsRootSessionAndIndependentCompressedEventFrames(t *testing.T) {
	for _, version := range []int{1, 3} {
		for _, compressed := range []bool{false, true} {
			home := t.TempDir()
			name := "sessions/-work/root-session/session.jsonl"
			if version == 3 {
				name = "sessions/-work/root-session/session.v3.jsonl"
			}
			header := []byte(fmt.Sprintf("{\"type\":\"session\",\"version\":%d,\"id\":\"root-session\",\"cwd\":\"/work\",\"createdAt\":0,\"delegationDepth\":0}\n", version))
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
			attachment := "selected attachment bytes"
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(attachment)))
			attachmentPath := "attachments/v1/objects/" + digest[:2] + "/" + digest
			writeFixture(t, home, attachmentPath, attachment)
			refEvent := []byte(`{"type":"user","images":[{"attachmentId":"sha256:` + digest + `"}]}` + "\n")
			if compressed {
				encoder, _ := zstd.NewWriter(nil)
				refEvent = encoder.EncodeAll(refEvent, nil)
				encoder.Close()
			}
			raw = append(raw, refEvent...)
			writeFixture(t, home, name, string(raw))
			writeFixture(t, home, "attachments/v1/objects/unselected", "must stay behind")
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
			if got, err := os.ReadFile(filepath.Join(dest, attachmentPath)); err != nil || string(got) != attachment {
				t.Fatal("selected DSH attachment missing")
			}
			if _, err := os.Stat(filepath.Join(dest, "attachments/v1/objects/unselected")); !os.IsNotExist(err) {
				t.Fatal("unrelated attachment copied")
			}
		}
	}
}

func TestDshUsesLatestGenerationAndCompressesOnlyTheRuntimeCopy(t *testing.T) {
	home := t.TempDir()
	legacy := "sessions/-work/root/session.jsonl"
	current := "sessions/-work/root/session.v3.jsonl"
	writeFixture(t, home, legacy, `{"type":"session","version":0,"id":"root","cwd":"/work"}`+"\n")
	text := `{"type":"session","version":3,"id":"root","cwd":"/work","delegationDepth":0}` + "\n" + `{"type":"text","text":"updated history"}` + "\n"
	writeFixture(t, home, current, text)
	writeFixture(t, home, "sessions/-work/child/session.v3.jsonl", `{"type":"session","version":3,"id":"child","cwd":"/work","delegationDepth":1}`+"\n")
	source := Source{Harness: "dsh", Home: home}
	entries, err := List(t.Context(), source)
	if err != nil || len(entries) != 1 || entries[0].NativeID != "root" {
		t.Fatalf("generation discovery: %+v %v", entries, err)
	}
	store := t.TempDir()
	ref, err := Snapshot(t.Context(), store, ImportRequest{CommandID: "generation", Source: source, NativeID: "root", Revision: entries[0].Revision})
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "home")
	if err := Materialize(t.Context(), store, ref, dest); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDshRuntime(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{legacy, current} {
		original, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(filepath.Join(dest, name+".zstd"))
		if err != nil {
			t.Fatal(err)
		}
		decoder, err := zstd.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(decoder)
		decoder.Close()
		f.Close()
		if err != nil || string(got) != string(original) {
			t.Fatal("runtime conversion changed history", err)
		}
		if _, err := os.Stat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Fatal("mixed runtime encodings")
		}
	}
	after, err := List(t.Context(), source)
	if err != nil || after[0].Revision != entries[0].Revision {
		t.Fatal("source history changed", err)
	}
	if got, err := List(t.Context(), Source{Harness: "dsh", Home: dest}); err != nil || len(got) != 1 {
		t.Fatalf("runtime discovery: %+v %v", got, err)
	}
}

func TestDshRejectsFutureOrMixedGenerationsInsteadOfFallingBack(t *testing.T) {
	for _, extra := range []string{"session.v4.jsonl", "session.v3.jsonl.zstd"} {
		home := t.TempDir()
		writeFixture(t, home, "sessions/-work/root/session.jsonl", `{"type":"session","id":"root","cwd":"/work"}`+"\n")
		writeFixture(t, home, "sessions/-work/root/"+extra, "unsupported")
		if _, err := List(t.Context(), Source{Harness: "dsh", Home: home}); err == nil {
			t.Fatalf("fell back to obsolete history for %s", extra)
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
