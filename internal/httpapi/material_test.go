package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/material"
)

type materialAdapter struct {
	consoleapi.Admin
	store *material.Store
}

func (a materialAdapter) CaptureMaterial(ctx context.Context, in consoleapi.MaterialCapture) (material.Material, error) {
	return a.store.Capture(ctx, material.CaptureInput{Project: in.Project, Title: "Stored reply", MIME: "text/plain", Source: in.Source, Data: []byte("Authoritative source")})
}
func (a materialAdapter) UploadMaterial(ctx context.Context, p, n, m string, r io.Reader) (material.Material, error) {
	return a.store.Upload(ctx, p, n, m, r)
}
func (a materialAdapter) ListMaterials(ctx context.Context, p string) ([]material.Material, error) {
	return a.store.List(ctx, p)
}
func (a materialAdapter) GetMaterial(ctx context.Context, p, id string) (material.Material, error) {
	return a.store.Get(ctx, p, id)
}
func (a materialAdapter) MaterialContent(ctx context.Context, p, id string) (material.Material, []byte, error) {
	return a.store.Content(ctx, p, id)
}
func (a materialAdapter) ResolveMaterials(ctx context.Context, p string, refs []material.Ref) ([]material.Frozen, error) {
	return a.store.ResolveRefs(ctx, p, refs)
}
func (a materialAdapter) MaterialAnnotations(ctx context.Context, p string) ([]material.Annotation, error) {
	return a.store.Annotations(ctx, p)
}
func (a materialAdapter) SaveMaterialAnnotation(ctx context.Context, in material.AnnotationInput) (material.Annotation, error) {
	return a.store.SaveAnnotation(ctx, "owner", in)
}
func TestMaterialHTTPAuthenticationImmutableCaptureAndAttachment(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	store, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &Server{token: "fixture-token", admin: materialAdapter{store: store}}
	mux := http.NewServeMux()
	s.materialRoutes(mux)
	call := func(method, url, body, mime, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("Content-Type", mime)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/console/materials/upload?project=p&name=test.txt", "secret", "text/plain", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("POST", "/console/materials/capture", `{"project":"p","source":{"kind":"reply","conversation":"console:a","reply_id":"r"},"data":"fabricated"}`, "application/json", "fixture-token"); w.Code != 400 {
		t.Fatalf("capture trusted client bytes: %d %s", w.Code, w.Body.String())
	}
	w := call("POST", "/console/materials/capture", `{"project":"p","source":{"kind":"reply","conversation":"console:a","reply_id":"r"}}`, "application/json", "fixture-token")
	var captured material.Material
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &captured) != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("Authoritative source")) {
		t.Fatal("capture metadata leaked content bytes")
	}
	w = call("GET", "/console/materials/"+captured.ID+"/content?project=q", "", "", "fixture-token")
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = call("POST", "/console/materials/upload?project=p&name=payload.html", "<script>alert(1)</script>", "text/html", "fixture-token")
	var upload material.Material
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &upload) != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	w = call("GET", "/console/materials/"+upload.ID+"/content?project=p", "", "", "fixture-token")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("unsafe attachment headers: %v", w.Header())
	}
	body, _ := json.Marshal(material.AnnotationInput{Project: "p", Ref: material.Ref{ID: captured.ID}, Body: "note"})
	w = call("PUT", "/console/annotations/note", string(body), "application/json", "fixture-token")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	w = call("PUT", "/console/annotations/note", strings.Replace(string(body), `"note"`, `"changed"`, 1), "application/json", "fixture-token")
	if w.Code != 409 {
		t.Fatalf("stale edit accepted: %d %s", w.Code, w.Body.String())
	}
}
