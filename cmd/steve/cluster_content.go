package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/project"
)

const contentObjectHeader = "X-Steve-Content-Object"

// Each content operation rechecks the committed project declaration and local
// replica position. An incoming object's classification is never authoritative.
type peerContentPolicy struct{ peer *clusterPeer }

type contentPlacementState struct {
	state       coordination.State
	declaration platformconfig.Declaration
	project     project.Project
}

func (p *clusterPeer) contentState(ctx context.Context, projectID string) (contentPlacementState, error) {
	runtime := p.runtime.Load()
	if runtime == nil || projectID == "" {
		return contentPlacementState{}, contentreplica.ErrPlacement
	}
	for {
		state, err := runtime.ReadState(ctx)
		if err != nil {
			return contentPlacementState{}, err
		}
		version, err := runtime.Ledger().ReplicaVersion()
		if err != nil {
			return contentPlacementState{}, err
		}
		if version < state.AppVersion {
			if err := waitContentReplica(ctx); err != nil {
				return contentPlacementState{}, err
			}
			continue
		}
		d, ok, err := platformconfig.New(runtime.Ledger()).Load()
		if err != nil || !ok {
			return contentPlacementState{}, errors.Join(contentreplica.ErrPlacement, err)
		}
		cfg := &config.Config{}
		if err := d.Apply(cfg); err != nil {
			return contentPlacementState{}, err
		}
		_, declarationHash, err := config.ProjectDeclarations(cfg)
		if err != nil {
			return contentPlacementState{}, err
		}
		projects := project.Open(runtime.Ledger())
		projects.SetHubID(p.config.ClusterID)
		projects.RequireDeclaration(declarationHash)
		item, found, lookupErr := projects.Get(ctx, projectID)
		after, err := runtime.Ledger().ReplicaVersion()
		if err != nil {
			return contentPlacementState{}, err
		}
		if after != version {
			continue
		}
		if lookupErr != nil || !found {
			return contentPlacementState{}, errors.Join(contentreplica.ErrPlacement, lookupErr)
		}
		if item.Home.Node == "" || !item.Level.OrDefault().Valid() {
			return contentPlacementState{}, contentreplica.ErrPlacement
		}
		return contentPlacementState{state: state, declaration: d, project: item}, nil
	}
}

func waitContentReplica(ctx context.Context) error {
	timer := time.NewTimer(5 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func contentScope(item project.Project) contentreplica.Scope {
	return contentreplica.Scope{ProjectID: item.ID, Level: string(item.Level.OrDefault()), HomeNodeID: item.Home.Node}
}

func (policy peerContentPolicy) CheckpointPlacement(ctx context.Context, scope contentreplica.Scope, nodeID string) (contentreplica.Placement, error) {
	current, err := policy.peer.contentState(ctx, scope.ProjectID)
	if err != nil {
		return contentreplica.Placement{}, err
	}
	member, memberOK := current.state.Members[nodeID]
	assigned, nodeOK := current.declaration.Nodes[nodeID]
	if !memberOK || !nodeOK || current.state.Removing[nodeID] || contentScope(current.project) != scope {
		return contentreplica.Placement{}, contentreplica.ErrPlacement
	}
	level := project.Level(assigned.Level).OrDefault()
	if !level.Valid() || !current.project.Level.OrDefault().Admits(level) || scope.Level == "sealed" && nodeID != scope.HomeNodeID {
		return contentreplica.Placement{}, contentreplica.ErrPlacement
	}
	domain := member.FailureDomain
	if domain == "" {
		// An offline first installation can store one local copy even when
		// the OS has no machine identifier. It cannot claim independent copies.
		if len(current.state.Members) != 1 || nodeID != policy.peer.config.NodeID {
			return contentreplica.Placement{}, contentreplica.ErrPlacement
		}
		domain = "single-node:" + nodeID
	}
	return contentreplica.Placement{FailureDomain: domain}, nil
}

func (p *clusterPeer) contentReplicator(active cluster.Activation) (contentreplica.Replicator, error) {
	if active.NodeID != p.config.NodeID || active.Runtime == nil {
		return nil, contentreplica.ErrPlacement
	}
	client, err := contentreplica.New(contentreplica.Config{
		NodeID: p.config.NodeID, Local: peerLocalContent{peer: p}, Remote: peerContentTransport{peer: p, active: active}, Policy: peerContentPolicy{peer: p},
		Scope: func(ctx context.Context, id string) (contentreplica.Scope, error) {
			current, err := p.contentState(ctx, id)
			if err != nil {
				return contentreplica.Scope{}, err
			}
			return contentScope(current.project), nil
		},
		Members: func(ctx context.Context) ([]string, error) {
			state, err := active.Runtime.ReadState(ctx)
			if err != nil {
				return nil, err
			}
			members := make([]string, 0, len(state.Members))
			for id := range state.Members {
				members = append(members, id)
			}
			return members, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return generationContent{client: client, active: active}, nil
}

type generationContent struct {
	client *contentreplica.Client
	active cluster.Activation
}

func (c generationContent) MaxObjectBytes() int64 { return c.client.MaxObjectBytes() }

func contentGenerationState(ctx context.Context, active cluster.Activation) (coordination.State, error) {
	if active.Context == nil || active.Context.Err() != nil || active.Runtime == nil {
		return coordination.State{}, cluster.ErrInactive
	}
	state, err := active.Runtime.ReadState(ctx)
	if err != nil {
		return coordination.State{}, err
	}
	if state.Coordinator != active.Assignment || state.WriterGeneration != active.WriterGeneration {
		return coordination.State{}, cluster.ErrInactive
	}
	return state, active.Context.Err()
}

func (c generationContent) context(parent context.Context) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(c.active.Context, cancel)
	release := func() { stop(); cancel() }
	if _, err := contentGenerationState(ctx, c.active); err != nil {
		release()
		return nil, nil, err
	}
	return ctx, release, nil
}

func (c generationContent) CheckLocal(parent context.Context, id string) (contentreplica.Scope, error) {
	ctx, cancel, err := c.context(parent)
	if err != nil {
		return contentreplica.Scope{}, err
	}
	defer cancel()
	scope, err := c.client.CheckLocal(ctx, id)
	if err != nil {
		return contentreplica.Scope{}, err
	}
	if _, err := contentGenerationState(ctx, c.active); err != nil {
		return contentreplica.Scope{}, err
	}
	return scope, nil
}

func (c generationContent) Prepare(parent context.Context, projectID, kind, key string, ref contentreplica.BlobRef, source io.ReadSeeker) (contentreplica.Manifest, error) {
	ctx, cancel, err := c.context(parent)
	if err != nil {
		return contentreplica.Manifest{}, err
	}
	defer cancel()
	manifest, err := c.client.Prepare(ctx, projectID, kind, key, ref, source)
	if err != nil {
		return contentreplica.Manifest{}, err
	}
	if _, err := contentGenerationState(ctx, c.active); err != nil {
		return contentreplica.Manifest{}, err
	}
	return manifest, nil
}

func (c generationContent) Read(parent context.Context, manifest contentreplica.Manifest, into io.Writer) (contentreplica.Manifest, error) {
	ctx, cancel, err := c.context(parent)
	if err != nil {
		return contentreplica.Manifest{}, err
	}
	defer cancel()
	updated, err := c.client.Read(ctx, manifest, into)
	if err != nil {
		return contentreplica.Manifest{}, err
	}
	if _, err := contentGenerationState(ctx, c.active); err != nil {
		return contentreplica.Manifest{}, err
	}
	return updated, nil
}

func (p *clusterPeer) acquireContent() (*contentreplica.Store, func(), error) {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil, nil, cluster.ErrInactive
	}
	p.contentOps.Add(1)
	p.mu.Unlock()
	p.contentOnce.Do(func() {
		p.content, p.contentErr = contentreplica.Open(contentreplica.StoreConfig{Dir: filepath.Join(p.config.DataDir, "content"), NodeID: p.config.NodeID, Policy: peerContentPolicy{peer: p}})
	})
	if p.contentErr != nil {
		p.contentOps.Done()
		return nil, nil, p.contentErr
	}
	return p.content, p.contentOps.Done, nil
}

func (p *clusterPeer) closeContent() error {
	p.contentOps.Wait()
	if p.content != nil {
		return p.content.Close()
	}
	return nil
}

type peerLocalContent struct{ peer *clusterPeer }

func (local peerLocalContent) Put(ctx context.Context, object contentreplica.Object, source io.Reader) (contentreplica.Receipt, error) {
	store, release, err := local.peer.acquireContent()
	if err != nil {
		return contentreplica.Receipt{}, err
	}
	defer release()
	return store.Put(ctx, object, source)
}

func (local peerLocalContent) Get(ctx context.Context, object contentreplica.Object, into io.Writer) error {
	store, release, err := local.peer.acquireContent()
	if err != nil {
		return err
	}
	defer release()
	return store.Get(ctx, object, into)
}

type peerContentTransport struct {
	peer   *clusterPeer
	active cluster.Activation
}

func (transport peerContentTransport) request(ctx context.Context, method, nodeID string, object contentreplica.Object, source io.Reader) (*http.Response, error) {
	p := transport.peer
	if object.Blob.Size < 0 || object.Blob.Size > contentreplica.DefaultMaxObjectBytes {
		return nil, contentreplica.ErrTooLarge
	}
	if _, err := (peerContentPolicy{peer: p}).CheckpointPlacement(ctx, object.Scope, nodeID); err != nil {
		return nil, err
	}
	state, err := contentGenerationState(ctx, transport.active)
	if err != nil {
		return nil, err
	}
	if state.Coordinator.NodeID != p.config.NodeID {
		return nil, coordination.ErrNotCoordinator
	}
	member, ok := state.Members[nodeID]
	if !ok || state.Removing[nodeID] {
		return nil, contentreplica.ErrPlacement
	}
	clientTransport, origin, err := p.remoteTransport(member)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(object)
	if err != nil || len(raw) > 8192 {
		return nil, contentreplica.ErrInvalid
	}
	// A caller-owned seekable file may be reused for the next replica. HTTP
	// owns only this reader wrapper, not the underlying file's Close method.
	var body io.Reader
	if source != nil {
		body = struct{ io.Reader }{source}
	}
	request, err := http.NewRequestWithContext(ctx, method, origin.String()+clusterContentPath, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set(contentObjectHeader, base64.RawURLEncoding.EncodeToString(raw))
	request.Header.Set("X-Steve-Coordinator-Epoch", strconv.FormatUint(state.Coordinator.Epoch, 10))
	request.Header.Set("X-Steve-Writer-Generation", strconv.FormatUint(state.WriterGeneration, 10))
	if method == http.MethodPut {
		request.ContentLength = object.Blob.Size
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	client := &http.Client{Transport: clientTransport, Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure)
		switch failure.Code {
		case "placement":
			return nil, contentreplica.ErrPlacement
		case "invalid":
			return nil, contentreplica.ErrInvalid
		case "too_large":
			return nil, contentreplica.ErrTooLarge
		case "quota":
			return nil, checkpoint.ErrQuota
		case "integrity":
			return nil, contentreplica.ErrIntegrity
		case "missing":
			return nil, contentreplica.ErrIncomplete
		}
		if response.StatusCode == http.StatusForbidden {
			return nil, contentreplica.ErrPlacement
		}
		return nil, fmt.Errorf("content replica unavailable: HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (transport peerContentTransport) Put(ctx context.Context, nodeID string, object contentreplica.Object, source io.Reader) (contentreplica.Receipt, error) {
	response, err := transport.request(ctx, http.MethodPut, nodeID, object, source)
	if err != nil {
		return contentreplica.Receipt{}, err
	}
	defer response.Body.Close()
	var receipt contentreplica.Receipt
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8193))
	if err := decoder.Decode(&receipt); err != nil {
		return contentreplica.Receipt{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return contentreplica.Receipt{}, contentreplica.ErrIntegrity
	}
	where, err := (peerContentPolicy{peer: transport.peer}).CheckpointPlacement(ctx, object.Scope, nodeID)
	if err != nil {
		return contentreplica.Receipt{}, err
	}
	if receipt.NodeID != nodeID || receipt.ObjectID != object.ID() || receipt.FailureDomain != where.FailureDomain || receipt.StoredAt.IsZero() {
		return contentreplica.Receipt{}, contentreplica.ErrIntegrity
	}
	return receipt, nil
}

func (transport peerContentTransport) Get(ctx context.Context, nodeID string, object contentreplica.Object, into io.Writer) error {
	response, err := transport.request(ctx, http.MethodGet, nodeID, object, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.ContentLength != object.Blob.Size {
		return contentreplica.ErrIntegrity
	}
	written, err := io.Copy(into, io.LimitReader(response.Body, object.Blob.Size+1))
	if err != nil {
		return err
	}
	if written != object.Blob.Size {
		return contentreplica.ErrIntegrity
	}
	return ctx.Err()
}

func (p *clusterPeer) contentAuthority(r *http.Request) error {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return contentreplica.ErrPlacement
	}
	identity, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil || identity.ClusterID != p.config.ClusterID {
		return contentreplica.ErrPlacement
	}
	runtime := p.runtime.Load()
	if runtime == nil {
		return cluster.ErrInactive
	}
	state, err := runtime.ReadState(r.Context())
	if err != nil {
		return err
	}
	epoch, epochErr := strconv.ParseUint(r.Header.Get("X-Steve-Coordinator-Epoch"), 10, 64)
	writer, writerErr := strconv.ParseUint(r.Header.Get("X-Steve-Writer-Generation"), 10, 64)
	if epochErr != nil || writerErr != nil || state.Coordinator.NodeID != identity.NodeID || state.Coordinator.Epoch != epoch || state.WriterGeneration != writer || writer == 0 || state.Removing[identity.NodeID] {
		return contentreplica.ErrPlacement
	}
	if _, ok := state.Members[identity.NodeID]; !ok {
		return contentreplica.ErrPlacement
	}
	return nil
}

func (p *clusterPeer) serveContent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	// A context deadline alone does not interrupt a blocked HTTP body read.
	// Bound the underlying connection so a vanished peer cannot hold a quota
	// reservation or the store's shutdown wait indefinitely.
	deadline, _ := ctx.Deadline()
	control := http.NewResponseController(w)
	if err := control.SetReadDeadline(deadline); err != nil {
		http.Error(w, "content stream deadline unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := control.SetWriteDeadline(deadline); err != nil {
		http.Error(w, "content stream deadline unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPut && r.Method != http.MethodGet {
		http.Error(w, "GET or PUT required", http.StatusMethodNotAllowed)
		return
	}
	if err := p.contentAuthority(r); err != nil {
		http.Error(w, "content authority denied", http.StatusForbidden)
		return
	}
	header := r.Header.Get(contentObjectHeader)
	if len(header) > 11000 {
		http.Error(w, "content descriptor too large", http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		http.Error(w, "invalid content descriptor", http.StatusBadRequest)
		return
	}
	var object contentreplica.Object
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&object); err != nil {
		http.Error(w, "invalid content descriptor", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || object.Blob.Size < 0 || object.Blob.Size > contentreplica.DefaultMaxObjectBytes {
		http.Error(w, "invalid content size", http.StatusBadRequest)
		return
	}
	if err := p.contentRequestPlacement(r, object); err != nil {
		http.Error(w, "content placement denied", http.StatusForbidden)
		return
	}
	store, release, err := p.acquireContent()
	if err != nil {
		http.Error(w, "content store unavailable", http.StatusServiceUnavailable)
		return
	}
	defer release()
	if r.Method == http.MethodPut {
		if r.ContentLength != object.Blob.Size {
			http.Error(w, "content length differs", http.StatusBadRequest)
			return
		}
		receipt, err := store.Put(r.Context(), object, http.MaxBytesReader(w, r.Body, object.Blob.Size+1))
		if err != nil {
			writeContentError(w, err)
			return
		}
		if err := p.contentAuthority(r); err != nil {
			http.Error(w, "content authority changed", http.StatusForbidden)
			return
		}
		if err := p.contentRequestPlacement(r, object); err != nil {
			http.Error(w, "content placement changed", http.StatusForbidden)
			return
		}
		writePeerJSON(w, receipt)
		return
	}
	file, err := os.CreateTemp("", "steve-content-download-*")
	if err != nil {
		http.Error(w, "content staging unavailable", http.StatusServiceUnavailable)
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := store.Get(r.Context(), object, file); err != nil {
		writeContentError(w, err)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "content staging unavailable", http.StatusInternalServerError)
		return
	}
	if err := p.contentAuthority(r); err != nil {
		http.Error(w, "content authority changed", http.StatusForbidden)
		return
	}
	if err := p.contentRequestPlacement(r, object); err != nil {
		http.Error(w, "content placement changed", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(object.Blob.Size, 10))
	io.Copy(w, file)
}

func writeContentError(w http.ResponseWriter, err error) {
	code, status := "unavailable", http.StatusServiceUnavailable
	switch {
	case errors.Is(err, contentreplica.ErrPlacement), errors.Is(err, checkpoint.ErrPlacement):
		code, status = "placement", http.StatusForbidden
	case errors.Is(err, contentreplica.ErrInvalid), errors.Is(err, checkpoint.ErrInvalid):
		code, status = "invalid", http.StatusBadRequest
	case errors.Is(err, contentreplica.ErrTooLarge):
		code, status = "too_large", http.StatusRequestEntityTooLarge
	case errors.Is(err, checkpoint.ErrQuota):
		code, status = "quota", http.StatusInsufficientStorage
	case errors.Is(err, contentreplica.ErrIntegrity), errors.Is(err, checkpoint.ErrIntegrity):
		code, status = "integrity", http.StatusUnprocessableEntity
	case errors.Is(err, contentreplica.ErrIncomplete), errors.Is(err, checkpoint.ErrIncomplete), errors.Is(err, os.ErrNotExist):
		code, status = "missing", http.StatusNotFound
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code string `json:"code"`
	}{Code: code})
}

func (p *clusterPeer) contentRequestPlacement(r *http.Request, object contentreplica.Object) error {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return contentreplica.ErrPlacement
	}
	identity, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		return contentreplica.ErrPlacement
	}
	policy := peerContentPolicy{peer: p}
	if _, err := policy.CheckpointPlacement(r.Context(), object.Scope, identity.NodeID); err != nil {
		return err
	}
	_, err = policy.CheckpointPlacement(r.Context(), object.Scope, p.config.NodeID)
	return err
}
