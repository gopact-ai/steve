package material

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func fixture(t *testing.T) *Store {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	s, err := Open(filepath.Join(t.TempDir(), "materials"), book)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func capture(t *testing.T, s *Store, text string) Material {
	t.Helper()
	m, err := s.Capture(t.Context(), CaptureInput{Project: "p", Title: "reply", MIME: "text/plain", Source: Source{Kind: "reply", Conversation: "console:a", ReplyID: "r1"}, Data: []byte(text)})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestCaptureIdentitySelectorsAndScope(t *testing.T) {
	s := fixture(t)
	a := capture(t, s, "one\ntwo\nthree")
	b := capture(t, s, "new version")
	if a.ID == b.ID {
		t.Fatal("changed source reused immutable id")
	}
	if capture(t, s, "one\ntwo\nthree").ID != a.ID {
		t.Fatal("capture was not idempotent")
	}
	refs := []Ref{{ID: a.ID, Selector: &Selector{Kind: "lines", Start: 2, End: 2}}}
	f, err := s.ResolveRefs(t.Context(), "p", refs)
	if err != nil || f[0].Text != "two" {
		t.Fatalf("range=%+v err=%v", f, err)
	}
	if _, err := s.ResolveRefs(t.Context(), "q", refs); !errors.Is(err, ErrScope) {
		t.Fatalf("scope error=%v", err)
	}
	if _, err := s.ResolveRefs(t.Context(), "p", []Ref{{ID: a.ID, Selector: &Selector{Kind: "quote", Quote: "not present"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.Capture(t.Context(), CaptureInput{Project: "p", Source: Source{Kind: "snapshot-file", Attempt: "a", Commit: "bad", Path: "../../secret"}, Data: []byte("x")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad source accepted: %v", err)
	}
}
func TestImageBytePixelValidationAndCrop(t *testing.T) {
	s := fixture(t)
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 6)))
	m, err := s.Upload(t.Context(), "p", "image.png", "image/png", &b)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.ResolveRefs(t.Context(), "p", []Ref{{ID: m.ID, Selector: &Selector{Kind: "rect", Rect: &Rect{X: .25, Y: 0, Width: .5, Height: .5}}}})
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(f[0].Media.Data))
	if err != nil || config.Width != 4 || config.Height != 3 {
		t.Fatalf("crop=%+v %v", config, err)
	}
	if _, err := s.Upload(t.Context(), "p", "fake.png", "image/png", bytes.NewBufferString("not image")); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.Upload(t.Context(), "p", "huge.bin", "application/octet-stream", bytes.NewReader(make([]byte, MaxBlobBytes+1))); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := s.ResolveRefs(t.Context(), "p", []Ref{{ID: m.ID, Selector: &Selector{Kind: "rect", Rect: &Rect{X: .9, Y: 0, Width: .5, Height: .5}}}}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestAnnotationsCASAndMigration(t *testing.T) {
	s := fixture(t)
	m := capture(t, s, "original")
	in := AnnotationInput{ID: "note-1", Project: "p", Ref: Ref{ID: m.ID}, Body: "look here"}
	a, err := s.SaveAnnotation(t.Context(), "owner", in)
	if err != nil || a.Revision != 1 {
		t.Fatalf("%+v %v", a, err)
	}
	again, err := s.SaveAnnotation(t.Context(), "owner", in)
	if err != nil || again.Revision != 1 {
		t.Fatalf("create replay=%+v %v", again, err)
	}
	in.Body = "new"
	if _, err := s.SaveAnnotation(t.Context(), "owner", in); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	in.ExpectedRevision = 1
	a, err = s.SaveAnnotation(t.Context(), "owner", in)
	if err != nil || a.Revision != 2 {
		t.Fatal(err)
	}
	exported, err := s.ExportProject(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	target := fixture(t)
	if err := target.ImportProject(t.Context(), exported); err != nil {
		t.Fatal(err)
	}
	if err := target.ImportProject(t.Context(), exported); err != nil {
		t.Fatal(err)
	}
	f, err := target.ResolveRefs(t.Context(), "p", []Ref{{ID: m.ID}})
	if err != nil || f[0].Text != "original" {
		t.Fatalf("%+v %v", f, err)
	}
	notes, err := target.Annotations(t.Context(), "p")
	if err != nil || len(notes) != 1 || notes[0].Revision != 2 {
		t.Fatalf("%+v %v", notes, err)
	}
	exported.Blobs[m.Digest] = []byte("tampered")
	if err := target.ImportProject(t.Context(), exported); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestBlobTamperAndSymlinkAreNotTrusted(t *testing.T) {
	s := fixture(t)
	m := capture(t, s, "original")
	os.WriteFile(filepath.Join(s.root.Name(), m.Digest), []byte("modified"), 0600)
	if _, err := s.ResolveRefs(t.Context(), "p", []Ref{{ID: m.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(s.root.Name(), m.Digest))
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("original"), 0600)
	os.Symlink(outside, filepath.Join(s.root.Name(), m.Digest))
	if _, _, err := s.Content(t.Context(), "p", m.ID); err == nil {
		t.Fatal("symlink blob was followed")
	}
}

func TestMetadataHasNoBytesAndPixelsHaveTheirOwnLimit(t *testing.T) {
	s := fixture(t)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	oversized := append([]byte(nil), encoded.Bytes()...)
	binary.BigEndian.PutUint32(oversized[16:20], 5000)
	binary.BigEndian.PutUint32(oversized[20:24], 5000)
	binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	if _, err := s.Upload(t.Context(), "p", "pixels.png", "image/png", bytes.NewReader(oversized)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("pixel limit = %v", err)
	}
	m, err := s.Upload(t.Context(), "p", "tiny.png", "image/png", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveRefs(t.Context(), "p", []Ref{{ID: m.ID}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(resolved)
	if err != nil || bytes.Contains(raw, []byte(`"Data"`)) || bytes.Contains(raw, []byte(`"data"`)) {
		t.Fatalf("blob appeared in metadata: %s %v", raw, err)
	}
	large := capture(t, s, strings.Repeat("x", MaxResolvedTextBytes+1))
	if _, err := s.ResolveRefs(t.Context(), "p", []Ref{{ID: large.ID}}); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := s.SaveAnnotation(t.Context(), "owner", AnnotationInput{ID: "whole-file", Project: "p", Ref: Ref{ID: large.ID}, Body: "Review this file"}); err != nil {
		t.Fatalf("prompt budget must not prevent annotation of a file: %v", err)
	}
}

func TestBinaryResourcesAndConflictingImportStayIsolated(t *testing.T) {
	s := fixture(t)
	m, err := s.Upload(t.Context(), "p", "payload.bin", "application/octet-stream", bytes.NewReader([]byte{0, 1, 2, 3}))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.ResolveRefs(t.Context(), "p", []Ref{{ID: m.ID}})
	if err != nil || f[0].Material.Kind != "binary" || f[0].Media == nil || !bytes.Equal(f[0].Media.Data, []byte{0, 1, 2, 3}) || strings.Contains(f[0].PromptText(), "图片") {
		t.Fatalf("binary resource confused with image: %+v %v", f, err)
	}
	if _, err := s.SaveAnnotation(t.Context(), "owner", AnnotationInput{ID: "same-note", Project: "p", Ref: Ref{ID: m.ID}, Body: "source note"}); err != nil {
		t.Fatal(err)
	}
	exported, err := s.ExportProject(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	target := fixture(t)
	other := capture(t, target, "other")
	if _, err := target.SaveAnnotation(t.Context(), "owner", AnnotationInput{ID: "same-note", Project: "p", Ref: Ref{ID: other.ID}, Body: "target note"}); err != nil {
		t.Fatal(err)
	}
	if err := target.ImportProject(t.Context(), exported); !errors.Is(err, ErrConflict) {
		t.Fatalf("annotation conflict=%v", err)
	}
	if _, err := target.Get(t.Context(), "p", m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed import partially published metadata: %v", err)
	}
}
