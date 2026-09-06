package material

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/ledger"
)

const ExportSchema = 1

// ExportProject is self-contained, including tombstones and every referenced
// blob. Import preserves project identity; moving execution to another node
// does not silently rewrite a material's provenance or annotation anchor.
func (s *Store) ExportProject(ctx context.Context, project string) (ProjectExport, error) {
	out := ProjectExport{Schema: ExportSchema, Project: project, Materials: []Material{}, Annotations: []Annotation{}, Blobs: map[string][]byte{}}
	if !validName(project) {
		return out, ErrInvalid
	}
	err := s.book.Update(ctx, func(tx *ledger.Tx) error {
		materials, err := tx.Bindings(materialKind)
		if err != nil {
			return err
		}
		notes, err := tx.Bindings(annotationKind)
		if err != nil {
			return err
		}
		for _, raw := range materials {
			var m Material
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			if m.Project == project {
				out.Materials = append(out.Materials, m)
			}
		}
		for _, raw := range notes {
			var a Annotation
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			if a.Project == project {
				out.Annotations = append(out.Annotations, a)
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	for _, m := range out.Materials {
		if _, ok := out.Blobs[m.Digest]; !ok {
			data, err := s.readBlob(m.Digest)
			if err != nil {
				return out, err
			}
			out.Blobs[m.Digest] = data
		}
	}
	sort.Slice(out.Materials, func(i, j int) bool { return out.Materials[i].ID < out.Materials[j].ID })
	sort.Slice(out.Annotations, func(i, j int) bool { return out.Annotations[i].ID < out.Annotations[j].ID })
	return out, nil
}
func (s *Store) ImportProject(ctx context.Context, in ProjectExport) error {
	if in.Schema != ExportSchema || !validName(in.Project) {
		return fmt.Errorf("%w: unsupported material export", ErrInvalid)
	}
	materials := map[string]Material{}
	notes := map[string]Annotation{}
	for _, m := range in.Materials {
		data, ok := in.Blobs[m.Digest]
		if !ok || m.Project != in.Project || identity(m) != m.ID || m.CreatedAt.IsZero() || sum(data) != m.Digest || int64(len(data)) != m.Size {
			return fmt.Errorf("%w: material identity or blob mismatch", ErrInvalid)
		}
		if _, ok := materials[m.ID]; ok {
			return fmt.Errorf("%w: duplicate material", ErrInvalid)
		}
		if !validName(m.Title) {
			return ErrInvalid
		}
		if err := validateSource(m.Source); err != nil {
			return err
		}
		kind, mimeType, width, height, err := inspect(data, m.MIME)
		if err != nil {
			return err
		}
		if kind != m.Kind || mimeType != m.MIME || width != m.Width || height != m.Height {
			return fmt.Errorf("%w: material type mismatch", ErrInvalid)
		}
		materials[m.ID] = m
	}
	for _, a := range in.Annotations {
		m, ok := materials[a.Ref.ID]
		if !ok || a.Project != in.Project || !validName(a.ID) || !validName(a.Author) || a.Revision < 1 || a.CreatedAt.IsZero() || a.UpdatedAt.Before(a.CreatedAt) || !utf8.ValidString(a.Body) || len(a.Body) > MaxAnnotationBytes {
			return fmt.Errorf("%w: annotation identity mismatch", ErrInvalid)
		}
		if _, ok := notes[a.ID]; ok {
			return fmt.Errorf("%w: duplicate annotation", ErrInvalid)
		}
		data := in.Blobs[m.Digest]
		if m.Kind == "text" {
			if _, err := selectText(string(data), a.Ref.Selector); err != nil {
				return err
			}
		} else if m.Kind == "image" {
			if _, err := selectImage(data, a.Ref.Selector); err != nil {
				return err
			}
		} else if a.Ref.Selector != nil {
			return ErrInvalid
		}
		notes[a.ID] = a
	}
	// Unreferenced blobs are rejected so an import cannot become an arbitrary
	// write side channel. Objects land first; the ledger publishes them atomically.
	used := map[string]bool{}
	for _, m := range materials {
		used[m.Digest] = true
	}
	if len(used) != len(in.Blobs) {
		return fmt.Errorf("%w: unreferenced export blob", ErrInvalid)
	}
	for digest, data := range in.Blobs {
		if err := s.writeBlob(digest, data); err != nil {
			return err
		}
	}
	return s.book.Update(ctx, func(tx *ledger.Tx) error {
		for _, m := range materials {
			var old Material
			ok, err := binding(tx, materialKind, m.ID, &old)
			if err != nil {
				return err
			}
			if ok && !reflect.DeepEqual(old, m) {
				return fmt.Errorf("%w: material already differs", ErrConflict)
			}
			if !ok {
				if err := tx.PutBinding(materialKind, m.ID, m); err != nil {
					return err
				}
			}
		}
		for _, a := range notes {
			var old Annotation
			ok, err := binding(tx, annotationKind, a.ID, &old)
			if err != nil {
				return err
			}
			if ok && !reflect.DeepEqual(old, a) {
				return fmt.Errorf("%w: annotation already differs", ErrConflict)
			}
			if !ok {
				if err := tx.PutBinding(annotationKind, a.ID, a); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
