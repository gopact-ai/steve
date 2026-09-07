package consoleapi

import (
	"context"
	"io"

	"github.com/gopact-ai/steve/internal/material"
)

// MaterialCapture identifies an existing reply or snapshot. The adapter reads
// its authoritative project/content; HTTP callers cannot supply replacement text.
type MaterialCapture struct {
	Project string          `json:"project"`
	Title   string          `json:"title,omitempty"`
	Source  material.Source `json:"source"`
}

// MaterialAdmin is optional. Its implementation authorizes the caller and
// resolves capture provenance before calling material.Store. It is deliberately
// separate from Admin so adapters without materials remain read-only for them.
type MaterialAdmin interface {
	CaptureMaterial(context.Context, MaterialCapture) (material.Material, error)
	UploadMaterial(context.Context, string, string, string, io.Reader) (material.Material, error)
	ListMaterials(context.Context, string) ([]material.Material, error)
	GetMaterial(context.Context, string, string) (material.Material, error)
	MaterialContent(context.Context, string, string) (material.Material, []byte, error)
	ResolveMaterials(context.Context, string, []material.Ref) ([]material.Frozen, error)
	MaterialAnnotations(context.Context, string) ([]material.Annotation, error)
	SaveMaterialAnnotation(context.Context, material.AnnotationInput) (material.Annotation, error)
}
