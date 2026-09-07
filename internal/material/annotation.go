package material

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/ledger"
)

func binding(tx *ledger.Tx, kind, id string, out any) (bool, error) {
	var raw string
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, kind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), out)
}
func (s *Store) SaveAnnotation(ctx context.Context, author string, in AnnotationInput) (Annotation, error) {
	if !validName(author) || !validName(in.ID) || !validName(in.Project) || in.ExpectedRevision < 0 || !utf8.ValidString(in.Body) {
		return Annotation{}, ErrInvalid
	}
	if len(in.Body) > MaxAnnotationBytes {
		return Annotation{}, ErrTooLarge
	}
	if _, err := s.resolveRef(ctx, in.Project, in.Ref); err != nil {
		return Annotation{}, err
	}
	var result Annotation
	err := s.book.Update(ctx, func(tx *ledger.Tx) error {
		var old Annotation
		ok, err := binding(tx, annotationKind, in.ID, &old)
		if err != nil {
			return err
		}
		if ok && old.Project != in.Project {
			return ErrScope
		}
		if ok && (old.Author != author || !reflect.DeepEqual(old.Ref, in.Ref)) {
			return fmt.Errorf("%w: annotation owner and anchor are immutable", ErrConflict)
		}
		if ok && old.Body == in.Body && old.Deleted == in.Deleted && (old.Revision == in.ExpectedRevision || old.Revision == in.ExpectedRevision+1) {
			result = old
			return nil
		}
		if (ok && old.Revision != in.ExpectedRevision) || (!ok && (in.ExpectedRevision != 0 || in.Deleted)) {
			return ErrConflict
		}
		now := time.Now().UTC()
		result = Annotation{ID: in.ID, Project: in.Project, Ref: in.Ref, Body: in.Body, Author: author, Revision: in.ExpectedRevision + 1, CreatedAt: now, UpdatedAt: now, Deleted: in.Deleted}
		if ok {
			result.CreatedAt = old.CreatedAt
		}
		return tx.PutBinding(annotationKind, in.ID, result)
	})
	return result, err
}
func (s *Store) Annotations(ctx context.Context, project string) ([]Annotation, error) {
	records, err := s.book.Bindings(ctx, annotationKind)
	if err != nil {
		return nil, err
	}
	out := []Annotation{}
	for _, raw := range records {
		var a Annotation
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		if a.Project == project && !a.Deleted {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}
