package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/material"
)

// materialRoutes is called by Serve. Every material URL carries project data
// and stays behind the same owner authentication as the rest of the console.
func (s *Server) materialRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /console/materials", s.guard(s.consoleMaterials))
	mux.HandleFunc("POST /console/materials/capture", s.guard(s.consoleCaptureMaterial))
	mux.HandleFunc("POST /console/materials/upload", s.guard(s.consoleUploadMaterial))
	mux.HandleFunc("POST /console/materials/resolve", s.guard(s.consoleResolveMaterials))
	mux.HandleFunc("GET /console/materials/{id}", s.guard(s.consoleMaterial))
	mux.HandleFunc("GET /console/materials/{id}/content", s.guard(s.consoleMaterialContent))
	mux.HandleFunc("GET /console/annotations", s.guard(s.consoleMaterialAnnotations))
	mux.HandleFunc("PUT /console/annotations/{id}", s.guard(s.consoleSaveMaterialAnnotation))
}
func (s *Server) materialAdmin(w http.ResponseWriter) (consoleapi.MaterialAdmin, bool) {
	a, ok := s.admin.(consoleapi.MaterialAdmin)
	if !ok {
		http.Error(w, "materials are not enabled", http.StatusNotImplemented)
	}
	return a, ok
}
func materialResponse(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err != nil {
		code := http.StatusInternalServerError
		message := "material operation failed"
		switch {
		case errors.Is(err, material.ErrNotFound):
			code = http.StatusNotFound
		case errors.Is(err, material.ErrScope):
			code = http.StatusForbidden
		case errors.Is(err, material.ErrInvalid):
			code = http.StatusBadRequest
		case errors.Is(err, material.ErrTooLarge):
			code = http.StatusRequestEntityTooLarge
		case errors.Is(err, material.ErrConflict):
			code = http.StatusConflict
		}
		if code != http.StatusInternalServerError {
			message = err.Error()
		}
		w.WriteHeader(code)
		writeJSON(w, map[string]string{"error": message})
		return
	}
	writeJSON(w, value)
}
func materialRequest(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		materialResponse(w, nil, material.ErrInvalid)
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		materialResponse(w, nil, material.ErrInvalid)
		return false
	}
	return true
}
func (s *Server) consoleMaterials(w http.ResponseWriter, r *http.Request) {
	a, ok := s.materialAdmin(w)
	if !ok {
		return
	}
	items, err := a.ListMaterials(r.Context(), r.URL.Query().Get("project"))
	materialResponse(w, map[string]any{"materials": items}, err)
}
func (s *Server) consoleMaterial(w http.ResponseWriter, r *http.Request) {
	a, ok := s.materialAdmin(w)
	if !ok {
		return
	}
	item, err := a.GetMaterial(r.Context(), r.URL.Query().Get("project"), r.PathValue("id"))
	materialResponse(w, item, err)
}
func (s *Server) consoleCaptureMaterial(w http.ResponseWriter, r *http.Request) {
	a, ok := s.materialAdmin(w)
	if !ok {
		return
	}
	var req consoleapi.MaterialCapture
	if !materialRequest(w, r, &req) {
		return
	}
	item, err := a.CaptureMaterial(r.Context(), req)
	materialResponse(w, item, err)
}
func (s *Server) consoleUploadMaterial(w http.ResponseWriter, r *http.Request) {
	a, ok := s.materialAdmin(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, material.MaxBlobBytes+1)
	item, err := a.UploadMaterial(r.Context(), r.URL.Query().Get("project"), r.URL.Query().Get("name"), r.Header.Get("Content-Type"), r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		err = material.ErrTooLarge
	}
	materialResponse(w, item, err)
}
func (s *Server) consoleResolveMaterials(w http.ResponseWriter, r *http.Request) {
	a, ok := s.materialAdmin(w)
	if !ok {
		return
	}
	var req struct {
		Project string         `json:"project"`
		Refs    []material.Ref `json:"refs"`
	}
	if !materialRequest(w, r, &req) {
		return
	}
	items, err := a.ResolveMaterials(r.Context(), req.Project, req.Refs)
	materialResponse(w, map[string]any{"materials": items}, err)
}
func (s *Server) consoleMaterialContent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.materialAdmin(w)
	if !ok {
		return
	}
	item, data, err := a.MaterialContent(r.Context(), r.URL.Query().Get("project"), r.PathValue("id"))
	if err != nil {
		materialResponse(w, nil, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", item.MIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(item.Title)}))
	w.Write(data)
}
func (s *Server) consoleMaterialAnnotations(w http.ResponseWriter, r *http.Request) {
	a, ok := s.materialAdmin(w)
	if !ok {
		return
	}
	items, err := a.MaterialAnnotations(r.Context(), r.URL.Query().Get("project"))
	materialResponse(w, map[string]any{"annotations": items}, err)
}
func (s *Server) consoleSaveMaterialAnnotation(w http.ResponseWriter, r *http.Request) {
	a, ok := s.materialAdmin(w)
	if !ok {
		return
	}
	var req material.AnnotationInput
	if !materialRequest(w, r, &req) {
		return
	}
	if req.ID != "" && req.ID != r.PathValue("id") {
		materialResponse(w, nil, material.ErrInvalid)
		return
	}
	req.ID = r.PathValue("id")
	item, err := a.SaveMaterialAnnotation(r.Context(), req)
	materialResponse(w, item, err)
}
