package pluginledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/plugins"

	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
)

const packageRecordKind = "plugin-package"

type PackageRecord struct {
	Project  string                   `json:"project"`
	Digest   string                   `json:"digest"`
	Manifest plugins.Manifest         `json:"manifest"`
	Content  *contentreplica.Manifest `json:"content,omitempty"`
}

// Library records project-scoped package content in the application ledger.
// The physical cache is recoverable from the same replication mechanism as
// other project content; credentials remain solely in the local Store.
type Library struct {
	Store       *plugins.Store
	Ledger      *ledger.Ledger
	Replication contentreplica.Replicator
}

func libraryKey(project, digest string) string {
	return plugins.ContentDigest([]byte(project + "\x00" + digest))
}

func (l *Library) Add(ctx context.Context, project string, bundle plugins.Bundle) (PackageRecord, error) {
	if project == "" || len(project) > 256 || strings.ContainsAny(project, "\x00\r\n") || l.Store == nil || l.Ledger == nil {
		return PackageRecord{}, plugins.ErrInvalid
	}
	checked, err := plugins.DecodeBundle(bundle.Data)
	if err != nil {
		return PackageRecord{}, err
	}
	record := PackageRecord{Project: project, Digest: checked.Digest, Manifest: checked.Manifest}
	if l.Replication != nil {
		content, err := l.Replication.Prepare(ctx, project, contentreplica.PluginPackage, checked.Digest, contentreplica.BlobRef{SHA256: checked.Digest, Size: int64(len(checked.Data))}, bytes.NewReader(checked.Data))
		if err != nil {
			return record, err
		}
		record.Content = &content
	}
	if err := l.cache(ctx, project, checked); err != nil {
		return record, err
	}
	err = l.Ledger.Update(ctx, func(tx *ledger.Tx) error {
		if record.Content != nil {
			content, err := contentreplica.Record(tx, *record.Content)
			if err != nil {
				return err
			}
			record.Content = &content
		}
		return tx.PutBinding(packageRecordKind, libraryKey(project, checked.Digest), record)
	})
	return record, err
}

func (l *Library) Get(ctx context.Context, project, digest string) (plugins.Bundle, error) {
	record, err := l.Record(ctx, project, digest)
	if err != nil {
		return plugins.Bundle{}, err
	}
	if l.Replication != nil {
		if _, err := l.Replication.CheckLocal(ctx, project); err != nil {
			return plugins.Bundle{}, err
		}
	}
	if cached, err := l.Store.Read(digest); err == nil {
		if cached.Manifest.ID != record.Manifest.ID || cached.Manifest.Version != record.Manifest.Version {
			return plugins.Bundle{}, plugins.ErrIntegrity
		}
		return cached, nil
	}
	if record.Content == nil || l.Replication == nil {
		return plugins.Bundle{}, fmt.Errorf("%w: original package content is not locally available", plugins.ErrUnavailable)
	}
	content := packageBuffer{limit: plugins.MaxPackageBytes}
	restored, err := l.Replication.Read(ctx, *record.Content, &content)
	if err != nil {
		return plugins.Bundle{}, err
	}
	bundle, err := plugins.DecodeBundle(content.Bytes())
	if err != nil {
		return plugins.Bundle{}, err
	}
	if bundle.Digest != digest {
		return plugins.Bundle{}, plugins.ErrIntegrity
	}
	if err := l.cache(ctx, project, bundle); err != nil {
		return plugins.Bundle{}, err
	}
	err = l.Ledger.Update(ctx, func(tx *ledger.Tx) error {
		committed, err := contentreplica.Record(tx, restored)
		if err != nil {
			return err
		}
		record.Content = &committed
		return tx.PutBinding(packageRecordKind, libraryKey(project, digest), record)
	})
	return bundle, err
}

func (l *Library) Record(ctx context.Context, project, digest string) (PackageRecord, error) {
	var record PackageRecord
	if project == "" || !plugins.ValidDigest(digest) || l.Ledger == nil {
		return record, plugins.ErrInvalid
	}
	found, err := l.Ledger.GetBinding(ctx, packageRecordKind, libraryKey(project, digest), &record)
	if err != nil {
		return record, err
	}
	if !found {
		return record, plugins.ErrUnavailable
	}
	if record.Project != project || record.Digest != digest || record.Manifest.Validate() != nil {
		return record, plugins.ErrIntegrity
	}
	if record.Content != nil {
		current, found, err := contentreplica.Lookup(ctx, l.Ledger, record.Content.ID)
		if err != nil {
			return record, err
		}
		if !found || current.Object.Scope.ProjectID != project || current.Object.Kind != contentreplica.PluginPackage || current.Object.Key != digest || current.Object.Blob.SHA256 != digest || current.Object.Blob.Size > plugins.MaxPackageBytes {
			return record, plugins.ErrIntegrity
		}
		record.Content = &current
	}
	return record, nil
}

func (l *Library) List(ctx context.Context) ([]PackageRecord, error) {
	records, err := l.Ledger.Bindings(ctx, packageRecordKind)
	if err != nil {
		return nil, err
	}
	out := make([]PackageRecord, 0, len(records))
	for _, key := range slices.Sorted(maps.Keys(records)) {
		var item PackageRecord
		if err := plugins.DecodeStrict(records[key], &item); err != nil {
			return nil, err
		}
		if libraryKey(item.Project, item.Digest) != key {
			return nil, plugins.ErrIntegrity
		}
		checked, err := l.Record(ctx, item.Project, item.Digest)
		if err != nil {
			return nil, err
		}
		out = append(out, checked)
	}
	return out, nil
}

func (l *Library) cache(ctx context.Context, project string, bundle plugins.Bundle) error {
	_, err := l.Store.Install(ctx, plugins.InstallRequest{CommandID: libraryKey(project, bundle.Digest), ExpectedDigest: bundle.Digest, Source: plugins.Source{Kind: "bundle", Location: bundle.Digest}, Bundle: bundle})
	return err
}

// packageBuffer holds restored package bytes, refusing more than a package
// may contain.
type packageBuffer struct {
	bytes.Buffer
	limit int
}

func (b *packageBuffer) Write(raw []byte) (int, error) {
	if len(raw) > b.limit-b.Len() {
		return 0, errors.New("output exceeds limit")
	}
	return b.Buffer.Write(raw)
}
