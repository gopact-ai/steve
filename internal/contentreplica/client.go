package contentreplica

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
)

type Client struct{ cfg Config }

func (c *Client) MaxObjectBytes() int64 { return c.cfg.MaxObjectBytes }

func New(cfg Config) (*Client, error) {
	if cfg.MaxObjectBytes == 0 {
		cfg.MaxObjectBytes = DefaultMaxObjectBytes
	}
	if !validID(cfg.NodeID) || cfg.Local == nil || cfg.Policy == nil || cfg.Scope == nil || cfg.Members == nil || cfg.MaxObjectBytes < 1 || cfg.MaxObjectBytes >= int64(^uint64(0)>>1) {
		return nil, ErrInvalid
	}
	return &Client{cfg: cfg}, nil
}

func (c *Client) CheckLocal(ctx context.Context, project string) (Scope, error) {
	scope, err := c.cfg.Scope(ctx, project)
	if err != nil {
		return Scope{}, err
	}
	if scope.ProjectID != project {
		return Scope{}, ErrPlacement
	}
	if err := validateScope(scope); err != nil {
		return Scope{}, err
	}
	if _, err := placement(ctx, c.cfg.Policy, scope, c.cfg.NodeID); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

func (c *Client) Prepare(ctx context.Context, project, kind, key string, ref BlobRef, source io.ReadSeeker) (Manifest, error) {
	if source == nil {
		return Manifest{}, ErrInvalid
	}
	scope, err := c.CheckLocal(ctx, project)
	if err != nil {
		return Manifest{}, err
	}
	object := Object{Scope: scope, Kind: kind, Key: key, Blob: ref}
	if err := validateObject(object, c.cfg.MaxObjectBytes); err != nil {
		return Manifest{}, err
	}
	members, err := c.members(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if !slices.Contains(members, c.cfg.NodeID) {
		return Manifest{}, ErrPlacement
	}
	m := Manifest{ID: object.ID(), Object: object, RequiredCopies: 2, Protection: Replicated}
	if scope.Level == "sealed" {
		m.RequiredCopies, m.Protection = 1, SealedHome
	} else if len(members) == 1 {
		m.RequiredCopies, m.Protection = 1, SingleNode
	}
	// Prefer the local physical node, then other committed members. Only
	// admissible targets are contacted; voters have no implicit storage right.
	targets := append([]string{c.cfg.NodeID}, members...)
	seen, domains := map[string]bool{}, map[string]bool{}
	var failures []error
	for _, node := range targets {
		if seen[node] {
			continue
		}
		seen[node] = true
		where, err := placement(ctx, c.cfg.Policy, scope, node)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if domains[where.FailureDomain] {
			continue
		}
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return Manifest{}, err
		}
		var receipt Receipt
		if node == c.cfg.NodeID {
			receipt, err = c.cfg.Local.Put(ctx, object, source)
		} else if c.cfg.Remote == nil {
			err = ErrIncomplete
		} else {
			receipt, err = c.cfg.Remote.Put(ctx, node, object, source)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("content replica %s: %w", node, err))
			continue
		}
		if receipt.ObjectID != m.ID || receipt.NodeID != node || receipt.FailureDomain != where.FailureDomain || receipt.StoredAt.IsZero() {
			return Manifest{}, ErrIntegrity
		}
		m.Receipts = append(m.Receipts, receipt)
		domains[where.FailureDomain] = true
		if len(domains) >= m.RequiredCopies {
			break
		}
	}
	if err := validateManifest(m, c.cfg.MaxObjectBytes); err != nil {
		return Manifest{}, errors.Join(err, errors.Join(failures...))
	}
	current, err := c.CheckLocal(ctx, project)
	if err != nil || current != scope {
		return Manifest{}, errors.Join(ErrPlacement, err)
	}
	latest, err := c.members(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if m.Protection == SingleNode && len(latest) != 1 {
		return Manifest{}, fmt.Errorf("%w: membership changed while storing content", ErrIncomplete)
	}
	for _, receipt := range m.Receipts {
		if !slices.Contains(latest, receipt.NodeID) {
			return Manifest{}, ErrPlacement
		}
		where, err := placement(ctx, c.cfg.Policy, scope, receipt.NodeID)
		if err != nil || where.FailureDomain != receipt.FailureDomain {
			return Manifest{}, errors.Join(ErrPlacement, err)
		}
	}
	return m, nil
}

func (c *Client) members(ctx context.Context) ([]string, error) {
	nodes, err := c.cfg.Members(ctx)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 || len(nodes) > 256 {
		return nil, ErrInvalid
	}
	nodes = slices.Clone(nodes)
	sort.Strings(nodes)
	for _, node := range nodes {
		if !validID(node) {
			return nil, ErrInvalid
		}
	}
	return slices.Compact(nodes), nil
}

// Read stages and verifies each candidate separately. Truncated/corrupt
// streams never leak into a consumer's destination before a healthy retry.
func (c *Client) Read(ctx context.Context, m Manifest, into io.Writer) (Manifest, error) {
	if into == nil {
		return Manifest{}, ErrInvalid
	}
	if err := validateManifest(m, c.cfg.MaxObjectBytes); err != nil {
		return Manifest{}, err
	}
	scope, err := c.CheckLocal(ctx, m.Object.Scope.ProjectID)
	if err != nil || scope != m.Object.Scope {
		return Manifest{}, errors.Join(ErrPlacement, err)
	}
	tmp, err := os.CreateTemp("", "steve-content-read-*")
	if err != nil {
		return Manifest{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	try := func(node string) error {
		if err := tmp.Truncate(0); err != nil {
			return err
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		limited := &limitedWriter{into: tmp, left: m.Object.Blob.Size}
		var err error
		if node == c.cfg.NodeID {
			err = c.cfg.Local.Get(ctx, m.Object, limited)
		} else if c.cfg.Remote == nil {
			err = ErrIncomplete
		} else {
			err = c.cfg.Remote.Get(ctx, node, m.Object, limited)
		}
		if err != nil {
			return err
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := Verify(ctx, tmp, m.Object.Blob); err != nil {
			return err
		}
		_, err = tmp.Seek(0, io.SeekStart)
		return err
	}
	var failures []error
	finish := func() (Manifest, error) {
		current, err := c.CheckLocal(ctx, m.Object.Scope.ProjectID)
		if err != nil || current != scope {
			return Manifest{}, errors.Join(ErrPlacement, err)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return Manifest{}, err
		}
		repaired, err := c.cfg.Local.Put(ctx, m.Object, tmp)
		if err != nil {
			return Manifest{}, err
		}
		local, err := placement(ctx, c.cfg.Policy, scope, c.cfg.NodeID)
		if err != nil || repaired.NodeID != c.cfg.NodeID || repaired.ObjectID != m.ID || repaired.FailureDomain != local.FailureDomain || repaired.StoredAt.IsZero() {
			return Manifest{}, errors.Join(ErrIntegrity, err)
		}
		updated := m
		updated.Receipts = slices.Clone(m.Receipts)
		updated.Receipts = slices.DeleteFunc(updated.Receipts, func(r Receipt) bool { return r.NodeID == repaired.NodeID })
		updated.Receipts = append(updated.Receipts, repaired)
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return Manifest{}, err
		}
		return updated, copyContext(ctx, into, tmp, m.Object.Blob.Size)
	}
	if err := try(c.cfg.NodeID); err == nil {
		return finish()
	} else {
		failures = append(failures, err)
	}
	for _, receipt := range m.Receipts {
		if receipt.NodeID == c.cfg.NodeID {
			continue
		}
		where, err := placement(ctx, c.cfg.Policy, scope, receipt.NodeID)
		if err != nil || where.FailureDomain != receipt.FailureDomain {
			continue
		}
		if err := try(receipt.NodeID); err != nil {
			failures = append(failures, err)
			continue
		}
		return finish()
	}
	return Manifest{}, errors.Join(ErrIncomplete, errors.Join(failures...))
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

type limitedWriter struct {
	into io.Writer
	left int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, ErrTooLarge
	}
	n, err := w.into.Write(p)
	w.left -= int64(n)
	return n, err
}

func Verify(ctx context.Context, source io.Reader, ref BlobRef) error {
	if ref.Size < 0 || ref.Size >= int64(^uint64(0)>>1) || !digest(ref.SHA256, 64) {
		return ErrInvalid
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(contextReader{ctx, source}, ref.Size+1))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n != ref.Size || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return ErrIntegrity
	}
	return nil
}

func copyContext(ctx context.Context, into io.Writer, source io.Reader, size int64) error {
	n, err := io.Copy(into, io.LimitReader(contextReader{ctx, source}, size+1))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n != size {
		return ErrIntegrity
	}
	return nil
}
