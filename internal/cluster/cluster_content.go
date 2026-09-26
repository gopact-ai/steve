package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/project"
)

const contentObjectHeader = "X-Steve-Content-Object"
const contentUploadHeader = "X-Steve-Content-Upload"

// Each content operation rechecks the committed project declaration and local
// replica position. An incoming object's classification is never authoritative.
type peerContentPolicy struct{ peer *Peer }

type contentPlacementState struct {
	state       coordination.State
	declaration platformconfig.Declaration
	project     project.Project
}

type contentStateReader func(context.Context) (coordination.State, error)

var (
	// errContentLagging reports a replica that did not apply what the
	// committed state says exists within the time a content check waits.
	errContentLagging = fmt.Errorf("%w: replica is behind the committed state", contentreplica.ErrUnavailable)
	// errContentPeerUnchecked is a peer's answer that it could not check a
	// content request for now: its replica behind or its committed state
	// out of reach. It refuses nothing.
	errContentPeerUnchecked = errors.New("the peer could not check the request for now")
	// errContentAuthority refuses a caller the committed state does not
	// admit to content at all: no cluster identity, not a member, removed.
	errContentAuthority = errors.New("content caller has no authority")
	// errContentStale refuses a caller whose coordinator epoch or writer
	// generation is not the committed one.
	errContentStale = errors.New("content caller's coordinator epoch or writer generation is not current")
	// errContentMethod refuses a request that is not a content operation.
	errContentMethod = errors.New("content request method must be GET, PUT or POST")
)

type contentReadsKey struct{}

// contentReads is the committed state one content request judges by. It
// is read once, through the leader, and every check the request makes
// after that reuses it.
type contentReads struct {
	mu    sync.Mutex
	state *coordination.State
}

// withContentReads starts a request's reads of the committed state afresh:
// the checks made under the returned context share one read.
func withContentReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, contentReadsKey{}, &contentReads{})
}

// committedState is the coordination state a content check judges by, as
// the runtime reads it through the consensus leader — once per request
// when the request shares its reads.
func (p *Peer) committedState(ctx context.Context, runtime *Runtime) (coordination.State, error) {
	read := runtime.ReadState
	if stand := p.readContentState.Load(); stand != nil {
		read = *stand
	}
	// A read that fails refuses nothing: the check could not be made.
	read = unavailableRead(read)
	reads, _ := ctx.Value(contentReadsKey{}).(*contentReads)
	if reads == nil {
		return read(ctx)
	}
	reads.mu.Lock()
	defer reads.mu.Unlock()
	if reads.state == nil {
		state, err := read(ctx)
		if err != nil {
			return coordination.State{}, err
		}
		reads.state = &state
	}
	return *reads.state, nil
}

func unavailableRead(read contentStateReader) contentStateReader {
	return func(ctx context.Context) (coordination.State, error) {
		state, err := read(ctx)
		if err != nil {
			return coordination.State{}, fmt.Errorf("%w: read committed state: %w", contentreplica.ErrUnavailable, err)
		}
		return state, nil
	}
}

// awaitContentReplica waits for this replica to apply the committed state's
// application version, as a write waits for its replica to catch up: for at
// most ApplyTimeout (5s by default), and not at all once the replica has
// stopped applying. It returns the version the replica has.
func (p *Peer) awaitContentReplica(ctx context.Context, runtime *Runtime, version uint64) (uint64, error) {
	local, err := runtime.awaitApplied(ctx, 0, version)
	if err != nil {
		return 0, contentCatchUpError(ctx, err)
	}
	return local, nil
}

// contentCatchUpError is what a content check makes of err, the end of its
// wait for this replica to catch up. Past the bound the replica is lagging,
// and the caller may try again here or elsewhere. A replica that stopped
// applying, a runtime that stopped, or the caller's own end leaves the check
// unmade. None of them refuses anything.
func contentCatchUpError(ctx context.Context, err error) error {
	if ctx.Err() == nil && errors.Is(err, coordination.ErrUnavailable) {
		return fmt.Errorf("%w: %v", errContentLagging, err)
	}
	return fmt.Errorf("%w: replica catch-up: %w", contentreplica.ErrUnavailable, err)
}

// contentState is what the committed state says of a project's content. A
// placement is refused only for what that state says: no platform
// configuration, a project it does not declare or place, or one this hub
// does not own. A read that fails or a project declaration still catching up
// with the platform configuration refuses nothing: the check could not be
// made yet.
func (p *Peer) contentState(ctx context.Context, projectID string) (contentPlacementState, error) {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return contentPlacementState{}, fmt.Errorf("%w: %w", contentreplica.ErrUnavailable, ErrInactive)
	}
	if projectID == "" {
		return contentPlacementState{}, fmt.Errorf("%w: no project", contentreplica.ErrInvalid)
	}
	state, err := p.committedState(ctx, runtime)
	if err != nil {
		return contentPlacementState{}, err
	}
	version, err := p.awaitContentReplica(ctx, runtime, state.AppVersion)
	if err != nil {
		return contentPlacementState{}, err
	}
	unchecked := func(what string, err error) error {
		return fmt.Errorf("%w: %s: %w", contentreplica.ErrUnavailable, what, err)
	}
	for {
		d, ok, err := platformconfig.New(runtime.Ledger()).Load()
		if err != nil {
			return contentPlacementState{}, unchecked("read platform configuration", err)
		}
		if !ok {
			return contentPlacementState{}, fmt.Errorf("%w: no platform configuration", contentreplica.ErrPlacement)
		}
		cfg := &config.Config{}
		if err := d.Apply(cfg); err != nil {
			return contentPlacementState{}, unchecked("apply platform configuration", err)
		}
		_, declarationHash, err := configbuild.ProjectDeclarations(cfg)
		if err != nil {
			return contentPlacementState{}, unchecked("project declarations", err)
		}
		projects := project.Open(runtime.Ledger())
		projects.SetHubID(p.Config.ClusterID)
		projects.RequireDeclaration(declarationHash)
		item, found, lookupErr := projects.Get(ctx, projectID)
		after, err := runtime.Ledger().ReplicaVersion()
		if err != nil {
			return contentPlacementState{}, unchecked("read local replica version", err)
		}
		if after != version {
			// The replica applied more while it was read: read it again.
			version = after
			continue
		}
		switch {
		case errors.Is(lookupErr, project.ErrNotOwner):
			return contentPlacementState{}, errors.Join(contentreplica.ErrPlacement, lookupErr)
		case lookupErr != nil:
			// ErrDeclarationPending among others: the project declaration
			// has not caught up with the platform configuration yet.
			return contentPlacementState{}, unchecked("read project "+projectID, lookupErr)
		case !found:
			return contentPlacementState{}, fmt.Errorf("%w: project %s is not declared", contentreplica.ErrPlacement, projectID)
		case item.Home.Node == "" || !item.Level.OrDefault().Valid():
			return contentPlacementState{}, fmt.Errorf("%w: project %s has no home or level", contentreplica.ErrPlacement, projectID)
		}
		return contentPlacementState{state: state, declaration: d, project: item}, nil
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
	level := datalevel.Level(assigned.Level).OrDefault()
	if !level.Valid() || !current.project.Level.OrDefault().Admits(level) || scope.Level == "sealed" && nodeID != scope.HomeNodeID {
		return contentreplica.Placement{}, contentreplica.ErrPlacement
	}
	domain := member.FailureDomain
	if domain == "" {
		// An offline first installation can store one local copy even when
		// the OS has no machine identifier. It cannot claim independent copies.
		if len(current.state.Members) != 1 || nodeID != policy.peer.Config.NodeID {
			return contentreplica.Placement{}, contentreplica.ErrPlacement
		}
		domain = "single-node:" + nodeID
	}
	return contentreplica.Placement{FailureDomain: domain}, nil
}

func (p *Peer) ContentReplicator(active Activation) (contentreplica.Replicator, error) {
	if active.NodeID != p.Config.NodeID || active.Runtime == nil {
		return nil, contentreplica.ErrPlacement
	}
	client, err := contentreplica.New(contentreplica.Config{
		Ledger: active.Ledger,
		NodeID: p.Config.NodeID, Local: peerLocalContent{peer: p}, Remote: peerContentTransport{peer: p, active: active}, Policy: peerContentPolicy{peer: p},
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
	active Activation
}

func (c generationContent) MaxObjectBytes() int64 { return c.client.MaxObjectBytes() }

func contentGenerationState(ctx context.Context, active Activation) (coordination.State, error) {
	if active.Context == nil || active.Context.Err() != nil || active.Runtime == nil {
		return coordination.State{}, ErrInactive
	}
	state, err := active.Runtime.ReadState(ctx)
	if err != nil {
		return coordination.State{}, err
	}
	if state.Coordinator != active.Assignment || state.WriterGeneration != active.WriterGeneration {
		return coordination.State{}, ErrInactive
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

func (c generationContent) PrepareBundle(parent context.Context, projectID, commit, base string, ref contentreplica.BlobRef, source io.ReadSeeker) (contentreplica.Manifest, error) {
	ctx, cancel, err := c.context(parent)
	if err != nil {
		return contentreplica.Manifest{}, err
	}
	defer cancel()
	manifest, err := c.client.PrepareBundle(ctx, projectID, commit, base, ref, source)
	if err != nil {
		return contentreplica.Manifest{}, err
	}
	if _, err := contentGenerationState(ctx, c.active); err != nil {
		return contentreplica.Manifest{}, err
	}
	return manifest, nil
}

func (p *Peer) acquireContent() (*contentreplica.Store, func(), error) {
	p.Mu.Lock()
	if p.closing {
		p.Mu.Unlock()
		return nil, nil, ErrInactive
	}
	p.contentOps.Add(1)
	p.Mu.Unlock()
	p.contentOnce.Do(func() {
		p.content, p.contentErr = contentreplica.Open(contentreplica.StoreConfig{Ledger: p.Runtime.Load().Ledger(), Dir: filepath.Join(p.Config.DataDir, "content"), NodeID: p.Config.NodeID, Policy: peerContentPolicy{peer: p}})
	})
	if p.contentErr != nil {
		p.contentOps.Done()
		return nil, nil, p.contentErr
	}
	return p.content, p.contentOps.Done, nil
}

func (p *Peer) closeContent() error {
	p.contentOps.Wait()
	if p.content != nil {
		return p.content.Close()
	}
	return nil
}

type peerLocalContent struct{ peer *Peer }

func (local peerLocalContent) Put(ctx context.Context, upload contentreplica.Upload, source io.Reader) (contentreplica.Receipt, error) {
	store, release, err := local.peer.acquireContent()
	if err != nil {
		return contentreplica.Receipt{}, err
	}
	defer release()
	return store.Put(ctx, upload, source)
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
	peer   *Peer
	active Activation
}

func (transport peerContentTransport) request(ctx context.Context, method, nodeID string, object contentreplica.Object, uploadID string, source io.Reader) (*http.Response, error) {
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
	if state.Coordinator.NodeID != p.Config.NodeID {
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
	if method == http.MethodPut {
		request.Header.Set(contentUploadHeader, uploadID)
	}
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
		return nil, contentReplyError(&p.contentReplies, nodeID, response)
	}
	return response, nil
}

// contentReplyError is what a peer's refusal of a content request means
// here, read by its code. A reply without a code this node knows — a proxy's
// page, a peer that did not say — is what its status says and no more: a
// server error may pass when asked again, anything else is a plain failure
// that neither refuses a placement nor passes by itself. The caller names
// the node.
func contentReplyError(logs *contentRefusals, nodeID string, response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	var failure struct {
		Code string `json:"code"`
	}
	readErr := json.Unmarshal(raw, &failure)
	switch failure.Code {
	case "placement":
		return contentreplica.ErrPlacement
	case "authority":
		return fmt.Errorf("%w: %w", contentreplica.ErrPlacement, errContentAuthority)
	case "stale":
		// The peer read the committed state after this generation did:
		// this generation is no longer the one writing.
		return fmt.Errorf("%w: %w: %w", ErrInactive, contentreplica.ErrSuperseded, errContentStale)
	case "lagging":
		return fmt.Errorf("%w: %w", errContentPeerUnchecked, errContentLagging)
	case "unavailable":
		return fmt.Errorf("%w: %w: HTTP %d", errContentPeerUnchecked, contentreplica.ErrUnavailable, response.StatusCode)
	case "invalid", "method":
		return contentreplica.ErrInvalid
	case "too_large":
		return contentreplica.ErrTooLarge
	case "quota":
		return checkpoint.ErrQuota
	case "integrity":
		return contentreplica.ErrIntegrity
	case "missing":
		return contentreplica.ErrIncomplete
	}
	body := raw[:min(len(raw), 64)]
	if readErr == nil {
		readErr = fmt.Errorf("code %q", failure.Code)
	}
	// Only the status is left to go by, which is worth knowing about.
	if logged, suppressed := logs.admit(nodeID+"\x00"+strconv.Itoa(response.StatusCode), time.Now()); logged {
		slog.Warn(fmt.Sprintf("cluster: content reply from %s: HTTP %d without a code: body %q: %v", nodeID, response.StatusCode, body, readErr), "node", nodeID, "status", response.StatusCode, "suppressed", suppressed)
	}
	if response.StatusCode >= 500 {
		return fmt.Errorf("%w: %w: HTTP %d without a code: %q", errContentPeerUnchecked, contentreplica.ErrUnavailable, response.StatusCode, body)
	}
	return fmt.Errorf("content reply HTTP %d without a code: %q", response.StatusCode, body)
}

func (transport peerContentTransport) Put(ctx context.Context, nodeID string, upload contentreplica.Upload, source io.Reader) (contentreplica.Receipt, error) {
	object := upload.Object
	response, err := transport.request(ctx, http.MethodPut, nodeID, object, upload.ID, source)
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
	if receipt.UploadID != upload.ID || receipt.NodeID != nodeID || receipt.ObjectID != object.ID() || receipt.FailureDomain != where.FailureDomain || receipt.StoredAt.IsZero() {
		return contentreplica.Receipt{}, contentreplica.ErrIntegrity
	}
	return receipt, nil
}

func (transport peerContentTransport) Get(ctx context.Context, nodeID string, object contentreplica.Object, into io.Writer) error {
	response, err := transport.request(ctx, http.MethodGet, nodeID, object, "", nil)
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

func (p *Peer) contentAuthority(r *http.Request) error {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return fmt.Errorf("%w: no verified client certificate", errContentAuthority)
	}
	identity, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		return fmt.Errorf("%w: %w", errContentAuthority, err)
	}
	if identity.ClusterID != p.Config.ClusterID {
		return fmt.Errorf("%w: certificate of cluster %s", errContentAuthority, identity.ClusterID)
	}
	runtime := p.Runtime.Load()
	if runtime == nil {
		return fmt.Errorf("%w: %w", contentreplica.ErrUnavailable, ErrInactive)
	}
	state, err := p.committedState(r.Context(), runtime)
	if err != nil {
		return err
	}
	epoch, epochErr := strconv.ParseUint(r.Header.Get("X-Steve-Coordinator-Epoch"), 10, 64)
	writer, writerErr := strconv.ParseUint(r.Header.Get("X-Steve-Writer-Generation"), 10, 64)
	// A caller whose generation has ended hears that first: it stops, and
	// nothing it held is judged by a membership it no longer writes for.
	switch {
	case epochErr != nil || writerErr != nil || writer == 0:
		return fmt.Errorf("%w: no coordinator epoch and writer generation", errContentAuthority)
	case state.Coordinator.NodeID != identity.NodeID:
		return fmt.Errorf("%w: %s is not the coordinator, %s is", errContentStale, identity.NodeID, state.Coordinator.NodeID)
	case state.Coordinator.Epoch != epoch || state.WriterGeneration != writer:
		return fmt.Errorf("%w: epoch %d, writer %d; committed epoch %d, writer %d", errContentStale, epoch, writer, state.Coordinator.Epoch, state.WriterGeneration)
	case state.Removing[identity.NodeID]:
		return fmt.Errorf("%w: %s is being removed", errContentAuthority, identity.NodeID)
	}
	if _, ok := state.Members[identity.NodeID]; !ok {
		return fmt.Errorf("%w: %s is not a member", errContentAuthority, identity.NodeID)
	}
	return nil
}

func (p *Peer) serveContent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	// Admitting the request reads the committed state once; the checks
	// after the transfer read it again, once, to see what changed.
	r = r.WithContext(withContentReads(ctx))
	recheck := r.WithContext(withContentReads(ctx))
	// A context deadline alone does not interrupt a blocked HTTP body read.
	// Bound the underlying connection so a vanished peer cannot hold a quota
	// reservation or the store's shutdown wait indefinitely.
	deadline, _ := ctx.Deadline()
	control := http.NewResponseController(w)
	if err := control.SetReadDeadline(deadline); err != nil {
		p.refuseContent(w, r, fmt.Errorf("%w: stream deadline: %w", contentreplica.ErrUnavailable, err))
		return
	}
	if err := control.SetWriteDeadline(deadline); err != nil {
		p.refuseContent(w, r, fmt.Errorf("%w: stream deadline: %w", contentreplica.ErrUnavailable, err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPut && r.Method != http.MethodGet && r.Method != http.MethodPost {
		p.refuseContent(w, r, fmt.Errorf("%w: %s", errContentMethod, r.Method))
		return
	}
	if err := p.contentAuthority(r); err != nil {
		p.refuseContent(w, r, err)
		return
	}
	if r.Method == http.MethodPost {
		p.serveContentMaintenance(w, r)
		return
	}
	header := r.Header.Get(contentObjectHeader)
	if len(header) > 11000 {
		p.refuseContent(w, r, fmt.Errorf("%w: descriptor of %d bytes", contentreplica.ErrInvalid, len(header)))
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		p.refuseContent(w, r, fmt.Errorf("%w: descriptor: %w", contentreplica.ErrInvalid, err))
		return
	}
	var object contentreplica.Object
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&object); err != nil {
		p.refuseContent(w, r, fmt.Errorf("%w: descriptor: %w", contentreplica.ErrInvalid, err))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || object.Blob.Size < 0 || object.Blob.Size > contentreplica.DefaultMaxObjectBytes {
		p.refuseContent(w, r, fmt.Errorf("%w: descriptor size %d", contentreplica.ErrInvalid, object.Blob.Size))
		return
	}
	if err := p.contentRequestPlacement(r, object); err != nil {
		p.refuseContent(w, r, err)
		return
	}
	store, release, err := p.acquireContent()
	if err != nil {
		p.refuseContent(w, r, fmt.Errorf("%w: store: %w", contentreplica.ErrUnavailable, err))
		return
	}
	defer release()
	if r.Method == http.MethodPut {
		if r.ContentLength != object.Blob.Size {
			p.refuseContent(w, r, fmt.Errorf("%w: body of %d bytes for content of %d", contentreplica.ErrInvalid, r.ContentLength, object.Blob.Size))
			return
		}
		upload := contentreplica.Upload{ID: r.Header.Get(contentUploadHeader), Object: object}
		receipt, err := store.Put(r.Context(), upload, http.MaxBytesReader(w, r.Body, object.Blob.Size+1))
		if err != nil {
			p.refuseContent(w, r, err)
			return
		}
		if err := p.contentAuthority(recheck); err != nil {
			p.refuseContent(w, r, err)
			return
		}
		if err := p.contentRequestPlacement(recheck, object); err != nil {
			p.refuseContent(w, r, err)
			return
		}
		WriteJSON(w, receipt)
		return
	}
	file, err := os.CreateTemp("", "steve-content-download-*")
	if err != nil {
		p.refuseContent(w, r, fmt.Errorf("%w: staging: %w", contentreplica.ErrUnavailable, err))
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := store.Get(r.Context(), object, file); err != nil {
		p.refuseContent(w, r, err)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		p.refuseContent(w, r, fmt.Errorf("%w: staging: %w", contentreplica.ErrUnavailable, err))
		return
	}
	if err := p.contentAuthority(recheck); err != nil {
		p.refuseContent(w, r, err)
		return
	}
	if err := p.contentRequestPlacement(recheck, object); err != nil {
		p.refuseContent(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(object.Blob.Size, 10))
	io.Copy(w, file)
}

// contentRefusal is how a content request refused for err is answered: 403
// for a caller or a placement the committed state does not admit, 503 for
// a check that could not be made and may succeed when asked again.
func contentRefusal(err error) (status int, code string) {
	switch {
	case errors.Is(err, errContentMethod):
		return http.StatusMethodNotAllowed, "method"
	case errors.Is(err, errContentAuthority):
		return http.StatusForbidden, "authority"
	case errors.Is(err, errContentStale):
		return http.StatusForbidden, "stale"
	case errors.Is(err, errContentLagging):
		return http.StatusServiceUnavailable, "lagging"
	case errors.Is(err, contentreplica.ErrUnavailable):
		// A check that could not be made refuses nothing, whatever else
		// the error carries.
		return http.StatusServiceUnavailable, "unavailable"
	case errors.Is(err, contentreplica.ErrPlacement), errors.Is(err, checkpoint.ErrPlacement):
		return http.StatusForbidden, "placement"
	case errors.Is(err, contentreplica.ErrInvalid), errors.Is(err, checkpoint.ErrInvalid):
		return http.StatusBadRequest, "invalid"
	case errors.Is(err, contentreplica.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large"
	case errors.Is(err, checkpoint.ErrQuota):
		return http.StatusInsufficientStorage, "quota"
	case errors.Is(err, contentreplica.ErrIntegrity), errors.Is(err, checkpoint.ErrIntegrity):
		return http.StatusUnprocessableEntity, "integrity"
	case errors.Is(err, contentreplica.ErrIncomplete), errors.Is(err, checkpoint.ErrIncomplete), errors.Is(err, os.ErrNotExist):
		return http.StatusNotFound, "missing"
	}
	return http.StatusServiceUnavailable, "unavailable"
}

// contentRefusalLogEvery is how often a peer logs the refusals of one
// caller with one code: the first at once, the ones after it counted into
// the next line logged.
const contentRefusalLogEvery = time.Minute

// contentRefusals limits how often a peer logs refusals, per caller and code.
type contentRefusals struct {
	mu   sync.Mutex
	seen map[string]*contentRefusalCount
}

type contentRefusalCount struct {
	logged     time.Time
	suppressed int
}

// admit says whether a refusal under key is logged now, and how many were
// not logged since the last one that was.
func (l *contentRefusals) admit(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]*contentRefusalCount{}
	}
	count, found := l.seen[key]
	if found && now.Sub(count.logged) < contentRefusalLogEvery {
		count.suppressed++
		return false, 0
	}
	if !found {
		if len(l.seen) >= 256 {
			for seen, old := range l.seen {
				if now.Sub(old.logged) >= contentRefusalLogEvery {
					delete(l.seen, seen)
				}
			}
		}
		count = &contentRefusalCount{}
		l.seen[key] = count
	}
	suppressed := count.suppressed
	count.logged, count.suppressed = now, 0
	return true, suppressed
}

// refuseContent answers a content request refused for err, and says why in
// this peer's log: who asked, with which coordinator epoch and writer
// generation, and the reason.
func (p *Peer) refuseContent(w http.ResponseWriter, r *http.Request, err error) {
	status, code := contentRefusal(err)
	caller := "unknown"
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		if identity, idErr := coordination.CertificateIdentity(r.TLS.PeerCertificates[0]); idErr == nil {
			caller = identity.NodeID
		}
	}
	if logged, suppressed := p.contentRefusals.admit(caller+"\x00"+code, time.Now()); logged {
		slog.Warn(fmt.Sprintf("cluster: content refused %s %s from %s: HTTP %d: %v", code, r.Method, caller, status, err), "caller", caller, "epoch", r.Header.Get("X-Steve-Coordinator-Epoch"), "writer", r.Header.Get("X-Steve-Writer-Generation"), "code", code, "status", status, "suppressed", suppressed)
	}
	writeContentReply(w, status, code)
}

// writeContentReply sends a content refusal: the status and a JSON code.
func writeContentReply(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		Code string `json:"code"`
	}{Code: code}); err != nil {
		// The status has gone out; the peer falls back to it and logs the
		// missing code on its side, so this is only a hint that it left.
		slog.Warn(fmt.Sprintf("cluster: content reply %s: %v", code, err))
	}
}

func (p *Peer) contentRequestPlacement(r *http.Request, object contentreplica.Object) error {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return fmt.Errorf("%w: no client certificate", errContentAuthority)
	}
	identity, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		return fmt.Errorf("%w: %w", errContentAuthority, err)
	}
	policy := peerContentPolicy{peer: p}
	if _, err := policy.CheckpointPlacement(r.Context(), object.Scope, identity.NodeID); err != nil {
		return err
	}
	_, err = policy.CheckpointPlacement(r.Context(), object.Scope, p.Config.NodeID)
	return err
}
