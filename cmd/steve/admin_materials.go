package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/project"
)

func (a *fleetAdmin) materialProject(ctx context.Context, id string) error {
	if a.materials == nil || a.projects == nil {
		return errors.New("materials are not configured")
	}
	p, ok, err := a.projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return project.ErrUnknown
	}
	if !p.Level.OrDefault().Admits(a.materialLevel.OrDefault()) {
		return material.ErrScope
	}
	if p.Level == project.LevelSealed && p.Home.Node != "" {
		return errors.New("sealed project content cannot be captured on this hub")
	}
	if a.cfg == nil || a.materialOwner() == "" {
		return errors.New("console owner is not configured")
	}
	return nil
}

func (a *fleetAdmin) CaptureMaterial(ctx context.Context, req consoleapi.MaterialCapture) (material.Material, error) {
	if err := a.materialProject(ctx, req.Project); err != nil {
		return material.Material{}, err
	}
	source := req.Source
	var data []byte
	title := req.Title
	mimeType := "text/plain; charset=utf-8"
	switch source.Kind {
	case "reply":
		if a.console == nil || source.Conversation == "" || source.ReplyID == "" || source.Revision == "" {
			return material.Material{}, fmt.Errorf("%w: reply version is required", material.ErrInvalid)
		}
		found := false
		for _, reply := range a.console.Replies(source.Conversation) {
			if reply.ID != source.ReplyID {
				continue
			}
			if reply.ProjectID != req.Project {
				return material.Material{}, material.ErrScope
			}
			if reply.Revision != source.Revision {
				return material.Material{}, fmt.Errorf("%w: source reply changed", material.ErrConflict)
			}
			data = []byte(reply.Text)
			found = true
			if title == "" {
				title = reply.Title
			}
			break
		}
		if !found {
			return material.Material{}, material.ErrNotFound
		}
	case "snapshot-file":
		if a.attempts == nil || a.artifacts == nil || source.Attempt == "" || source.Commit == "" || source.Path == "" {
			return material.Material{}, material.ErrInvalid
		}
		record, err := a.attempts.Get(ctx, source.Attempt)
		if err != nil {
			return material.Material{}, err
		}
		if record.Project != req.Project {
			return material.Material{}, material.ErrScope
		}
		allowed := source.Commit == record.Base
		if record.Result != nil && source.Commit == record.Result.Artifact {
			allowed = true
		}
		if !allowed {
			return material.Material{}, fmt.Errorf("%w: snapshot does not belong to this attempt", material.ErrScope)
		}
		data, err = a.artifacts.FileContent(ctx, record.Project, source.Commit, source.Path, material.MaxBlobBytes)
		if err != nil {
			return material.Material{}, err
		}
		mimeType = http.DetectContentType(data)
		if guessed := mime.TypeByExtension(path.Ext(source.Path)); guessed != "" && strings.HasPrefix(guessed, "text/") {
			mimeType = guessed
		}
		if title == "" {
			title = source.Path
		}
	default:
		return material.Material{}, fmt.Errorf("%w: unsupported capture source", material.ErrInvalid)
	}
	if title == "" {
		title = "Reply"
	}
	return a.materials.Capture(ctx, material.CaptureInput{Project: req.Project, Title: title, MIME: mimeType, Source: source, Data: data})
}
func (a *fleetAdmin) UploadMaterial(ctx context.Context, id, title, mimeType string, r io.Reader) (material.Material, error) {
	if err := a.materialProject(ctx, id); err != nil {
		return material.Material{}, err
	}
	return a.materials.Upload(ctx, id, title, mimeType, r)
}
func (a *fleetAdmin) ListMaterials(ctx context.Context, id string) ([]material.Material, error) {
	if err := a.materialProject(ctx, id); err != nil {
		return nil, err
	}
	return a.materials.List(ctx, id)
}
func (a *fleetAdmin) GetMaterial(ctx context.Context, projectID, id string) (material.Material, error) {
	if err := a.materialProject(ctx, projectID); err != nil {
		return material.Material{}, err
	}
	return a.materials.Get(ctx, projectID, id)
}
func (a *fleetAdmin) MaterialContent(ctx context.Context, projectID, id string) (material.Material, []byte, error) {
	if err := a.materialProject(ctx, projectID); err != nil {
		return material.Material{}, nil, err
	}
	return a.materials.Content(ctx, projectID, id)
}
func (a *fleetAdmin) ResolveMaterials(ctx context.Context, projectID string, refs []material.Ref) ([]material.Frozen, error) {
	if err := a.materialProject(ctx, projectID); err != nil {
		return nil, err
	}
	return a.materials.ResolveRefs(ctx, projectID, refs)
}
func (a *fleetAdmin) MaterialAnnotations(ctx context.Context, projectID string) ([]material.Annotation, error) {
	if err := a.materialProject(ctx, projectID); err != nil {
		return nil, err
	}
	return a.materials.Annotations(ctx, projectID)
}
func (a *fleetAdmin) SaveMaterialAnnotation(ctx context.Context, in material.AnnotationInput) (material.Annotation, error) {
	if err := a.materialProject(ctx, in.Project); err != nil {
		return material.Annotation{}, err
	}
	return a.materials.SaveAnnotation(ctx, a.materialOwner(), in)
}

func (a *fleetAdmin) authorizeMaterials(ctx context.Context, conversation, principal, projectID string) error {
	if err := a.materialProject(ctx, projectID); err != nil {
		return err
	}
	if principal != a.materialOwner() {
		return errors.New("material requester is no longer the console owner")
	}
	current, err := a.coordinator.Context(ctx, conversation)
	if err != nil {
		return err
	}
	if current.Project == nil || current.Project.ID != projectID {
		return material.ErrScope
	}
	return nil
}

var _ consoleapi.MaterialAdmin = (*fleetAdmin)(nil)

func (a *fleetAdmin) materialOwner() string {
	if a.owner != "" {
		return a.owner
	}
	configMu.RLock()
	defer configMu.RUnlock()
	if a.cfg == nil {
		return ""
	}
	return a.cfg.EffectiveOwnerID()
}
