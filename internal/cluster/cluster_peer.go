package cluster

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/steve/internal/adapter"
	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	webassets "github.com/gopact-ai/steve/internal/readmodel/web"
	runtimestore "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/sameorigin"
	"github.com/gopact-ai/steve/internal/sshconnect"
	"github.com/gopact-ai/steve/internal/stableport"
	"github.com/hashicorp/raft"
)

const clusterApplicationPath = "/cluster/application"

const clusterWorkerPath = "/cluster/worker"

const clusterContentPath = "/cluster/content"

type PeerApplicationEndpoint struct {
	URL, Token string
	Generation uint64
	Admin      *adminsvc.Service
}

type PeerWorkerDescriptor struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Token   string `json:"token"`
}

type ApplicationServer interface {
	URL() string
}

type PeerOptions struct {
	StartApplication func(context.Context, ApplicationHost, Activation, func(PeerApplicationEndpoint) error) (Deactivate, error)
	SSHHandler       func(SSHControl, string, string) (http.Handler, error)
	ConfigPath       string
	ClusterPath      string
	// SSHLaunch starts the sessions behind this node's links; nil means
	// the platform ssh client. Tests supply sessions without a network.
	SSHLaunch sshconnect.Launcher
	// Activate is injectable for integration tests and alternate application
	// runners. The endpoint must be loopback and stop must join every user of
	// the activation's ledger before returning.
	Activate             func(context.Context, Activation, func(PeerApplicationEndpoint) error) (Deactivate, error)
	ConfigureApplication func(*config.Config, Activation) error
	ApplicationReady     func(*adminsvc.Service, ApplicationServer, Activation) error
	RaftConfig           *raft.Config
	PollInterval         time.Duration
	AllowAutoFailover    bool
	// Only local process tests supply virtual independent failure domains.
	TestFailureDomain     func() (string, error)
	ContentRepairInterval time.Duration
	// Listen binds the Raft, peer and UI addresses; net.Listen when nil.
	// A listener it returns belongs to the peer, which closes it.
	Listen func(network, address string) (net.Listener, error)
}

type Peer struct {
	localSSH       *sshconnect.Service
	Options        PeerOptions
	Config         PeerConfig
	Runtime        atomic.Pointer[Runtime]
	client         *coordination.Client
	routes         *coordination.RouteTable
	links          map[string]*sshconnect.Link
	identity       coordination.TLSOptions
	OwnerToken     string
	UIToken        string
	UiURL          string
	peerServer     *http.Server
	uiServer       *http.Server
	localTransport *http.Transport
	Mu             sync.RWMutex
	Application    *PeerApplicationEndpoint
	peerTransports map[string]peerTransport
	ctx            context.Context
	cancel         context.CancelFunc
	// Links live on their own context: they are closed after the
	// runtime, so its last words to tunnelled members still get through.
	linkCtx           context.Context
	linkCancel        context.CancelFunc
	closeOnce         sync.Once
	closeErr          error
	unpublish         func()
	unlock            func()
	Errors            chan error
	worker            *node.Server
	workerDone        chan error
	WorkerDescriptor  PeerWorkerDescriptor
	workerListener    net.Listener
	tunnels           sync.WaitGroup
	closing           bool
	workerPrincipals  map[string]*workerPrincipal
	raftAdvertisement atomic.Value
	enrollmentMu      sync.Mutex
	content           *contentreplica.Store
	contentOnce       sync.Once
	contentErr        error
	contentOps        sync.WaitGroup
	// readContentState, when set, stands in for the runtime's read of the
	// committed state in content checks; tests use it to put a replica
	// behind that state or to make the read fail.
	readContentState atomic.Pointer[contentStateReader]
	contentRefusals  contentRefusals
	// duplexWarning reports once that the server's response writer cannot
	// read a request while answering it; each proxied request would say so.
	duplexWarning sync.Once
}

func OpenPeer(parent context.Context, options PeerOptions) (peer *Peer, runErr error) {
	if options.Listen == nil {
		options.Listen = net.Listen
	}
	if options.ClusterPath == "" {
		options.ClusterPath = DefaultClusterConfigPath(options.ConfigPath)
	}
	settings, err := LoadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		return nil, err
	}
	if err := confirmPeerIdentity(options, &settings); err != nil {
		return nil, err
	}

	application, err := config.Load(options.ConfigPath)
	if err != nil {
		return nil, err
	}
	if err := requireClusterLoopback(settings.UIAddress); err != nil {
		return nil, err
	}
	if len(application.Gateway.ReadModelToken) < 32 {
		return nil, errors.New("cluster UI requires a private access token of at least 32 characters")
	}
	owner, err := ReadClusterPrivate(settings.OwnerTokenFile)
	if err != nil {
		return nil, err
	}
	if len(owner) < 32 || strings.ContainsAny(string(owner), "\r\n\t ") {
		return nil, errors.New("cluster owner token is invalid")
	}
	unlock, err := runtimestore.AcquireLock(filepath.Join(settings.DataDir, "peer-process"))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	linkCtx, linkCancel := context.WithCancel(parent)
	p := &Peer{Options: options, Config: settings, OwnerToken: string(owner), UIToken: application.Gateway.ReadModelToken, ctx: ctx, cancel: cancel, linkCtx: linkCtx, linkCancel: linkCancel, unlock: unlock, Errors: make(chan error, 1), peerTransports: map[string]peerTransport{}, localTransport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, IdleConnTimeout: 30 * time.Second}}
	p.workerPrincipals = map[string]*workerPrincipal{}
	defer func() {
		if runErr != nil {
			p.Close()
		}
	}()
	if err := p.startWorker(application.LocalWorkspaceRoot()); err != nil {
		return nil, err
	}
	raftListener, peerListener, err := bindPeerListeners(settings, options.Listen)
	if err != nil {
		return nil, err
	}
	defer func() {
		if runErr != nil {
			raftListener.Close()
			peerListener.Close()
		}
	}()
	uiListener, err := stableport.Listen(options.Listen, "tcp", settings.UIAddress)
	if err != nil {
		return nil, err
	}
	defer func() {
		if runErr != nil {
			uiListener.Close()
		}
	}()
	p.recordBoundAddresses(raftListener, peerListener, uiListener)
	if err := SaveClusterJSON(options.ClusterPath, p.Config, false); err != nil {
		return nil, err
	}
	if err := desktop.PinAddress(options.ConfigPath, application, p.UiURL); err != nil {
		return nil, err
	}
	identity, err := p.Config.TlsOptions()
	if err != nil {
		return nil, err
	}
	identity.AuthorizePeer = p.authorizedRaftPeer
	p.identity = identity
	seeds := append([]coordination.Member(nil), p.Config.Seeds...)
	seeds = append(seeds, coordination.Member{NodeID: p.Config.NodeID, Address: p.Config.RaftAddress, APIAddress: p.Config.PeerURL, Name: p.Config.Name})
	selfHost, _, _ := net.SplitHostPort(peerListener.Addr().String())
	p.routes = coordination.NewRouteTable(p.Config.Routes)
	p.links = map[string]*sshconnect.Link{}
	for nodeID, link := range p.Config.Links {
		p.openLink(nodeID, link)
	}
	p.client, err = coordination.NewClient(coordination.ClientConfig{TLS: identity, Members: seeds, SelfHost: selfHost, Routes: p.routes, ControlHeaders: func(context.Context, string) (http.Header, error) {
		return http.Header{"Authorization": []string{"Bearer " + p.OwnerToken}}, nil
	}})
	if err != nil {
		return nil, err
	}
	stream, err := coordination.NewTLSStreamLayer(&advertisedPeerListener{Listener: raftListener, address: &p.raftAdvertisement}, identity, p.resolveRaftPeer, p.routes)
	if err != nil {
		return nil, err
	}
	runtime, err := Open(Config{LedgerDir: filepath.Dir(application.Gateway.StatePath), Coordination: coordination.Config{ClusterID: p.Config.ClusterID, NodeID: p.Config.NodeID, Build: nodewire.Version(), FailureDomain: p.Config.FailureDomain, StorageLevel: p.Config.StorageLevel, DataDir: filepath.Join(p.Config.DataDir, "raft"), APIAddress: p.Config.PeerURL, Name: p.Config.Name, Bootstrap: p.Config.Bootstrap, StreamLayer: stream, Probe: p.client.Probe, ValidateJoin: p.validateJoiningNetwork, AuthorizeReplica: p.authorizeLedgerReplica, RaftConfig: options.RaftConfig, LogOutput: raftLogOutput(options)}, Client: p.client, Activate: p.activate, PollInterval: options.PollInterval})
	if err != nil {
		return nil, err
	}
	p.Runtime.Store(runtime)
	if err := p.startPeerServers(runtime, peerListener, uiListener); err != nil {
		return nil, err
	}
	go p.watchRuntime(runtime)

	p.unpublish, err = desktop.PublishEndpoint(options.ConfigPath, p.UiURL)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func confirmPeerIdentity(options PeerOptions, settings *PeerConfig) error {
	identitySource := physicalFailureDomain
	if options.TestFailureDomain != nil {
		identitySource = options.TestFailureDomain
	}
	failureDomain, identityErr := identitySource()
	if identityErr != nil && settings.FailureDomain != "" {
		return errors.New("无法确认已登记节点的物理身份")
	}
	if settings.FailureDomain != "" && failureDomain != settings.FailureDomain {
		return errors.New("节点数据的物理身份已改变，请重新确认接入身份")
	}
	settings.FailureDomain = failureDomain
	return nil
}

// bindPeerListeners opens the node's Raft and peer listeners.
func bindPeerListeners(settings PeerConfig, listen func(network, address string) (net.Listener, error)) (raft, peer net.Listener, err error) {
	raft, err = stableport.Listen(listen, "tcp", settings.RaftBindAddress)
	if err != nil {
		return nil, nil, err
	}
	peer, err = stableport.Listen(listen, "tcp", settings.PeerBindAddress)
	if err != nil {
		raft.Close()
		return nil, nil, err
	}
	return raft, peer, nil
}

func (p *Peer) recordBoundAddresses(raftListener, peerListener, uiListener net.Listener) {
	p.Config.RaftBindAddress = raftListener.Addr().String()
	p.Config.PeerBindAddress = peerListener.Addr().String()
	p.Config.RaftAddress = boundAdvertiseAddress(p.Config.RaftAddress, raftListener.Addr().String())
	p.Config.PeerAddress = boundAdvertiseAddress(p.Config.PeerAddress, peerListener.Addr().String())
	p.Config.UIAddress = uiListener.Addr().String()
	if p.Config.PeerURL == "" {
		p.Config.PeerURL = "https://" + p.Config.PeerAddress
	} else if advertised, err := url.Parse(p.Config.PeerURL); err == nil && advertised.Port() == "0" {
		_, port, _ := net.SplitHostPort(p.Config.PeerAddress)
		advertised.Host = net.JoinHostPort(advertised.Hostname(), port)
		p.Config.PeerURL = advertised.String()
	}
	p.raftAdvertisement.Store(p.Config.RaftAddress)
	p.UiURL = "http://" + p.Config.UIAddress
}

func (p *Peer) startPeerServers(runtime *Runtime, peerListener, uiListener net.Listener) error {
	serverTLS, err := p.identity.ServerConfig()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(coordination.RPCPath, runtime.RPCHandler(coordination.RPCOptions{AuthorizeControl: p.authorizeControl}))
	mux.HandleFunc(clusterApplicationPath+"/", p.servePeerApplication)
	mux.HandleFunc(clusterWorkerPath, p.serveWorkerTunnel)
	mux.HandleFunc(clusterWorkerPath+"/descriptor", p.serveWorkerDescriptor)
	mux.HandleFunc("/cluster/network/check", p.serveNetworkCheck)
	mux.HandleFunc("/cluster/enrollment/", p.servePeerEnrollment)
	mux.HandleFunc(clusterContentPath, p.serveContent)
	p.peerServer = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, TLSConfig: serverTLS, BaseContext: func(net.Listener) context.Context { return p.ctx }}
	p.uiServer = &http.Server{Handler: http.HandlerFunc(p.serveUI), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return p.ctx }}
	go p.serve(p.peerServer, tls.NewListener(peerListener, serverTLS))
	go p.serve(p.uiServer, uiListener)
	return nil
}

// watchRuntime reports a runtime that stopped on its own. Without it the
// process that owns this peer would keep waiting on a service that is no
// longer there — including the restart that asks to continue as a newly
// installed program.
func (p *Peer) watchRuntime(runtime *Runtime) {
	select {
	case <-p.ctx.Done():
	case <-runtime.Done():
		if err := runtime.Failure(); err != nil && p.ctx.Err() == nil {
			select {
			case p.Errors <- err:
			default:
			}
		}
	}
}

func (p *Peer) serve(server *http.Server, listener net.Listener) {
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		select {
		case p.Errors <- err:
		default:
		}
	}
}

func (p *Peer) Close() error {
	p.closeOnce.Do(func() {
		p.Mu.Lock()
		p.closing = true
		var workerConnections []*authenticatedWorkerConnection
		for _, principal := range p.workerPrincipals {
			workerConnections = append(workerConnections, principal.Connection)
		}
		p.Mu.Unlock()
		p.cancel()
		for _, connection := range workerConnections {
			connection.Close()
		}
		if p.uiServer != nil {
			p.closeErr = errors.Join(p.closeErr, p.uiServer.Close())
		}
		p.Mu.Lock()
		sshService := p.localSSH
		p.localSSH = nil
		p.Mu.Unlock()
		if sshService != nil {
			p.closeErr = errors.Join(p.closeErr, sshService.Close())
		}
		if p.peerServer != nil {
			p.closeErr = errors.Join(p.closeErr, p.peerServer.Close())
		}
		if runtime := p.Runtime.Load(); runtime != nil {
			p.closeErr = errors.Join(p.closeErr, runtime.Close())
		}
		p.linkCancel()
		p.Mu.Lock()
		links := p.links
		p.links = nil
		p.Mu.Unlock()
		for _, link := range links {
			link.Close()
		}
		if p.workerDone != nil {
			if p.workerListener != nil {
				p.workerListener.Close()
			}
			p.closeErr = errors.Join(p.closeErr, <-p.workerDone)
		}
		p.tunnels.Wait()
		p.closeErr = errors.Join(p.closeErr, p.closeContent())
		if p.client != nil {
			p.client.Close()
		}
		p.localTransport.CloseIdleConnections()
		p.Mu.Lock()
		for _, pooled := range p.peerTransports {
			pooled.transport.CloseIdleConnections()
		}
		p.Mu.Unlock()
		if p.unpublish != nil {
			p.unpublish()
		}
		if p.unlock != nil {
			p.unlock()
		}
	})
	return p.closeErr
}

func (p *Peer) authorizedRaftPeer(identity coordination.Identity) bool {
	if identity.ClusterID != p.Config.ClusterID {
		return false
	}
	if runtime := p.Runtime.Load(); runtime != nil {
		peers := runtime.TransportPeers()
		if len(peers) > 0 {
			_, ok := peers[identity.NodeID]
			return ok
		}
	}
	if identity.NodeID == p.Config.NodeID {
		return true
	}
	for _, seed := range p.Config.Seeds {
		if seed.NodeID == identity.NodeID {
			return true
		}
	}
	return false
}

func (p *Peer) resolveRaftPeer(address raft.ServerAddress) string {
	if runtime := p.Runtime.Load(); runtime != nil {
		for id, peerAddress := range runtime.TransportPeers() {
			if peerAddress == string(address) {
				return id
			}
		}
	}
	return p.client.PeerID(address)
}

func (p *Peer) authorizeControl(r *http.Request, identity coordination.Identity, _ string) (string, error) {
	if identity.ClusterID != p.Config.ClusterID || !ConstantToken(r.Header.Get("Authorization"), p.OwnerToken) {
		return "", errors.New("owner authorization denied")
	}
	return "owner", nil
}

func ConstantToken(header, want string) bool {
	prefix := "Bearer "
	return strings.HasPrefix(header, prefix) && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), []byte(want)) == 1
}

func (p *Peer) activate(ctx context.Context, activation Activation) (Deactivate, error) {
	ready := func(endpoint PeerApplicationEndpoint) error {
		parsed, err := url.Parse(endpoint.URL)
		if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Path != "" || len(endpoint.Token) < 32 {
			return errors.New("business application must expose a private loopback endpoint")
		}
		if err := requireClusterLoopback(parsed.Host); err != nil {
			return err
		}
		endpoint.Generation = activation.Generation
		p.Mu.Lock()
		p.Application = &endpoint
		p.Mu.Unlock()
		return nil
	}
	var stop Deactivate
	var err error
	if p.Options.Activate != nil {
		stop, err = p.Options.Activate(ctx, activation, ready)
	} else {
		stop, err = p.Options.StartApplication(ctx, p, activation, ready)
	}
	return func(stopCtx context.Context) error {
		p.Mu.Lock()
		if p.Application != nil && p.Application.Generation == activation.Generation {
			p.Application = nil
		}
		p.Mu.Unlock()
		if stop != nil {
			return stop(stopCtx)
		}
		return nil
	}, err
}

func (p *Peer) ApplicationStoreFailure(active Activation, cause error) {
	if cause != nil {
		// RequestRebuild only refuses (ErrInactive) when this generation
		// has already ended, and then there is nothing left to rebuild;
		// the cause is logged by the runtime.
		_ = active.Runtime.RequestRebuild(active.Generation, cause)
	}
}

func ApplicationAuthorityError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrInactive) || errors.Is(err, coordination.ErrUnavailable) || errors.Is(err, coordination.ErrNotLeader) || errors.Is(err, coordination.ErrNotCoordinator) || errors.Is(err, coordination.ErrConflict) || errors.Is(err, coordination.ErrStaleEpoch) || errors.Is(err, coordination.ErrStaleWriter)
}

func (p *Peer) serveUI(w http.ResponseWriter, r *http.Request) {
	// The listener is loopback-only (requireClusterLoopback); what remains is
	// the owner's browser acting for another site. Proxied hops drop Origin.
	if err := sameorigin.Check(r, sameorigin.Loopback); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/cluster/") || strings.HasPrefix(r.URL.Path, coordination.RPCPath) {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/assets/") {
		p.staticPage(w, r)
		return
	}
	if !ConstantToken(r.Header.Get("Authorization"), p.UIToken) && subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(p.UIToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/console/versions" && r.Method == http.MethodGet {
		WriteJSON(w, consoleapi.Versions{Hub: nodewire.Version(), HubID: p.Config.NodeID, ProtocolMin: nodewire.ProtocolMin, ProtocolMax: nodewire.ProtocolVersion, Nodes: []consoleapi.VersionNode{}, Peers: []consoleapi.HubPeerInfo{}})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/console/coordination") {
		p.serveCoordination(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/console/ssh/") {
		p.serveSSHLocal(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/console/desktop") {
		p.serveDesktopLocal(w, r)
		return
	}
	if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/console/") && r.URL.Path != "/state" && r.URL.Path != "/usage" && r.URL.Path != "/events" && r.URL.Path != "/history" && !strings.HasPrefix(r.URL.Path, "/bootstrap/") && !strings.HasPrefix(r.URL.Path, "/dist/") {
		p.staticPage(w, r)
		return
	}
	runtime := p.Runtime.Load()
	if runtime == nil {
		http.Error(w, "coordination is starting", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	state, err := runtime.ReadState(ctx)
	cancel()
	if err != nil {
		HTTPError(w, err)
		return
	}
	if state.Coordinator.NodeID == p.Config.NodeID {
		p.proxyLocalApplication(w, r, state.Coordinator.Epoch)
		return
	}
	member, ok := state.Members[state.Coordinator.NodeID]
	if !ok {
		HTTPError(w, coordination.ErrUnavailable)
		return
	}
	transport, origin, err := p.remoteTransport(member)
	if err != nil {
		HTTPError(w, err)
		return
	}
	p.proxy(w, r, origin, clusterApplicationPath, p.OwnerToken, state.Coordinator.Epoch, transport)
}

func (p *Peer) servePeerApplication(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "mutual TLS is required", http.StatusUnauthorized)
		return
	}
	identity, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		http.Error(w, "invalid peer identity", http.StatusUnauthorized)
		return
	}
	if _, err := p.authorizeControl(r, identity, "application"); err != nil {
		http.Error(w, "owner authorization denied", http.StatusForbidden)
		return
	}
	epoch, err := strconv.ParseUint(r.Header.Get("X-Steve-Coordinator-Epoch"), 10, 64)
	if err != nil {
		http.Error(w, "coordinator epoch is required", http.StatusBadRequest)
		return
	}
	request := r.Clone(r.Context())
	request.URL.Path = strings.TrimPrefix(request.URL.Path, clusterApplicationPath)
	request.URL.RawPath = ""
	p.proxyLocalApplication(w, request, epoch)
}

func (p *Peer) proxyLocalApplication(w http.ResponseWriter, r *http.Request, epoch uint64) {
	runtime := p.Runtime.Load()
	if runtime == nil {
		HTTPError(w, coordination.ErrUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	active, err := runtime.WaitReady(ctx)
	cancel()
	if err != nil {
		HTTPError(w, err)
		return
	}
	if active.Assignment.Epoch != epoch {
		HTTPError(w, coordination.ErrStaleEpoch)
		return
	}
	p.Mu.RLock()
	var target PeerApplicationEndpoint
	if p.Application != nil {
		target = *p.Application
	}
	p.Mu.RUnlock()
	if target.URL == "" || target.Generation != active.Generation {
		HTTPError(w, coordination.ErrUnavailable)
		return
	}
	origin, _ := url.Parse(target.URL)
	p.proxy(w, r, origin, "", target.Token, epoch, p.localTransport)
}

func (p *Peer) proxy(w http.ResponseWriter, r *http.Request, origin *url.URL, prefix, token string, epoch uint64, transport http.RoundTripper) {
	// The application may answer before the forwarded request body is fully
	// copied. Without full duplex, starting the answer makes this server take
	// the rest of the inbound body for itself and close it, and the transport
	// still forwarding that body then tears down the upstream connection: the
	// console is left with a 200 and half of its answer. HTTP/2 streams are
	// full duplex already and refuse the call, which is not worth a warning.
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil && r.ProtoMajor == 1 {
		p.duplexWarning.Do(func() {
			slog.Warn(fmt.Sprintf("cluster: proxied answers can be cut short: %v", err))
		})
	}
	proxy := httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(request *httputil.ProxyRequest) {
		query := request.In.URL.Query()
		query.Del("token")
		request.SetURL(origin)
		request.Out.URL.Path = prefix + request.In.URL.Path
		request.Out.URL.RawPath = ""
		request.Out.URL.RawQuery = query.Encode()
		request.Out.Header.Del("Cookie")
		request.Out.Header.Del("Referer")
		request.Out.Header.Del("Origin")
		request.Out.Header.Set("Authorization", "Bearer "+token)
		request.Out.Header.Set("X-Steve-Coordinator-Epoch", strconv.FormatUint(epoch, 10))
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "协调节点暂时不可用，请稍后重试。", http.StatusServiceUnavailable)
	}, ModifyResponse: func(response *http.Response) error {
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return errors.New("application redirects are not forwarded")
		}
		response.Header.Del("Set-Cookie")
		return nil
	}}
	proxy.ServeHTTP(w, r)
}

func (p *Peer) remoteTransport(member coordination.Member) (*http.Transport, *url.URL, error) {
	origin, err := url.Parse(member.APIAddress)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, nil, errors.New("coordinator has no valid HTTPS peer endpoint")
	}
	route, _ := p.routes.Lookup(member.NodeID)
	p.Mu.Lock()
	defer p.Mu.Unlock()
	// One transport per node. Mutual TLS proves whatever answers is that
	// node, so identity is not the concern; idle connections are. They may
	// sit on a tunnel that has since gone or on an address the node left,
	// so when the address or route changes the transport is rebuilt.
	pooled, ok := p.peerTransports[member.NodeID]
	if ok && (pooled.address != member.APIAddress || pooled.route != route.API) {
		pooled.transport.CloseIdleConnections()
		delete(p.peerTransports, member.NodeID)
		ok = false
	}
	if !ok {
		tlsConfig, err := p.identity.ClientConfig(member.NodeID)
		if err != nil {
			return nil, nil, err
		}
		transport := &http.Transport{TLSClientConfig: tlsConfig, DialContext: p.peerDial(member.NodeID, false, 5*time.Second), TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second}
		pooled = peerTransport{transport: transport, address: member.APIAddress, route: route.API}
		p.peerTransports[member.NodeID] = pooled
	}
	return pooled.transport, origin, nil
}

// peerTransport is the transport kept for one other node and the address
// and route it was built to reach.
type peerTransport struct {
	transport *http.Transport
	address   string
	route     string
}

// peerDial connects to another node. The address it is handed is what the
// node advertises; this node's route to it wins when there is one (an SSH
// tunnel ending at a loopback port here). This node's own advertised
// address is often one only other machines can route to (a VPN tunnel
// address), so calls aimed at itself go to the host the matching listener
// is bound to (the peer API listener, or the Raft listener for raft). The
// mutual TLS identity check still proves the port serves the node meant.
func (p *Peer) peerDial(nodeID string, raft bool, timeout time.Duration) coordination.DialFunc {
	dial := (&net.Dialer{Timeout: timeout}).DialContext
	if nodeID != p.Config.NodeID {
		if raft {
			return p.routes.RaftDial(nodeID, dial)
		}
		return p.routes.APIDial(nodeID, dial)
	}
	// Bind addresses are fixed when the listeners open, before anything
	// dials; remoteTransport calls this while holding p.Mu.
	bound := p.Config.PeerBindAddress
	if raft {
		bound = p.Config.RaftBindAddress
	}
	bindHost, _, _ := net.SplitHostPort(bound)
	return coordination.SelfDial(bindHost, dial)
}

func (p *Peer) staticPage(w http.ResponseWriter, r *http.Request) {
	root, err := webassets.Files()
	if err != nil {
		http.Error(w, "console not built", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	file, err := root.Open(name)
	if err != nil {
		name = "index.html"
		file, err = root.Open(name)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file.Close()
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	}
	http.ServeFileFS(w, r, root, name)
}

func (p *Peer) Join(ctx context.Context, request coordination.JoinRequest) (coordination.Result, error) {
	return p.Runtime.Load().Join(ctx, request)
}

func (p *Peer) Coordination(ctx context.Context) (consoleapi.CoordinationView, error) {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return consoleapi.CoordinationView{}, coordination.ErrUnavailable
	}
	state, err := runtime.ReadState(ctx)
	authoritative := err == nil
	if err != nil {
		state = runtime.Status().State
	}
	view := consoleapi.CoordinationView{Enabled: true, ClusterID: state.ClusterID, NodeID: p.Config.NodeID, CoordinatorID: state.Coordinator.NodeID, Epoch: state.Coordinator.Epoch, Revision: state.Revision, Authoritative: authoritative, ObservedAt: time.Now().UTC(), AutoFailover: state.AutoFailover, Nodes: []consoleapi.CoordinatorNode{}, Events: []consoleapi.CoordinatorEvent{}}
	if !authoritative {
		view.Reason = "暂时无法与多数节点确认状态，显示本机最后同步的记录。"
	}
	view.Nodes = coordinationNodes(ctx, state, p.Config.NodeID, runtime.Status(), p.client.Probe)
	live := 0
	eligibleTarget := false
	for _, node := range view.Nodes {
		if node.Voter && node.Online {
			live++
		}
		if node.ID != state.Coordinator.NodeID && node.AutoEligible && node.Ready {
			eligibleTarget = true
		}
	}
	view.Ready = authoritative && state.CanAutoFailover() && live >= len(state.Voters)/2+1 && eligibleTarget
	if !p.Options.AllowAutoFailover {
		view.Ready = false
		if view.Reason == "" {
			view.Reason = "任务续跑准备尚未完成，自动容灾暂不可用。"
		}
	}
	for _, record := range state.Audit {
		view.Events = append(view.Events, consoleapi.CoordinatorEvent{ID: record.CommandID, At: record.Time, Kind: record.Kind, Actor: record.Actor, From: record.From, To: record.To, Reason: record.Reason})
	}
	return view, nil
}

func (p *Peer) TransferCoordinator(ctx context.Context, request consoleapi.CoordinatorTransfer) (consoleapi.CoordinationView, error) {
	_, err := p.Runtime.Load().Transfer(ctx, coordination.TransferRequest{ID: request.CommandID, Actor: "owner", ExpectedEpoch: request.ExpectedEpoch, TargetNodeID: request.TargetNodeID, Reason: "user_request"})
	if err != nil {
		return consoleapi.CoordinationView{}, err
	}
	return p.Coordination(ctx)
}

func (p *Peer) SetAutoFailover(ctx context.Context, request consoleapi.CoordinatorPolicy) (consoleapi.CoordinationView, error) {
	if request.Enabled && !p.Options.AllowAutoFailover {
		return consoleapi.CoordinationView{}, fmt.Errorf("%w: 任务续跑准备尚未完成", coordination.ErrNotReady)
	}
	_, err := p.Runtime.Load().SetAutoFailover(ctx, coordination.PolicyRequest{ID: request.CommandID, Actor: "owner", ExpectedRevision: request.ExpectedRevision, Enabled: request.Enabled})
	if err != nil {
		return consoleapi.CoordinationView{}, err
	}
	return p.Coordination(ctx)
}

func (p *Peer) startWorker(workspaceRoot string) error {
	var cfg node.ServerConfig
	raw, err := ReadClusterPrivate(p.Config.WorkerConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		token := ClusterRandomToken()
		cfg = node.ServerConfig{Name: p.Config.NodeID, Listen: "127.0.0.1:0", Token: token, Hubs: map[string]string{p.Config.ClusterID: token}, Harnesses: map[string]node.HarnessSpec{}, StateDir: filepath.Join(p.Config.DataDir, "node"), WorkspaceRoot: workspaceRoot}
		if err := SaveClusterJSON(p.Config.WorkerConfigFile, cfg, true); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg.Name != p.Config.NodeID || len(cfg.Token) < 32 {
		return errors.New("worker configuration does not match this physical node identity")
	}
	// Where this installation lives follows from the peer's own
	// configuration, so a worker file written by an older build still
	// reports the whole installation rather than its node directory.
	cfg.StateRoot = filepath.Dir(p.Config.DataDir)
	if err := requireClusterLoopback(cfg.Listen); err != nil {
		return err
	}
	listener, err := stableport.Listen(net.Listen, "tcp", cfg.Listen)
	if err != nil {
		return err
	}
	cfg.Listen = listener.Addr().String()
	if err := SaveClusterJSON(p.Config.WorkerConfigFile, cfg, false); err != nil {
		listener.Close()
		return err
	}
	if err := preparePeerAdapters(p.ctx, &cfg); err != nil {
		listener.Close()
		return err
	}
	cfg.Source = p.Config.WorkerConfigFile
	cfg.Listener = listener
	cfg.SessionAuthorizer = p
	cfg.PluginAuthorizer = p
	cfg.AuthenticatedPeer = p.authenticatedWorkerPeer
	restorePeerAdapters(&cfg)
	p.workerListener = listener
	p.worker = node.NewServer(cfg)
	p.workerDone = make(chan error, 1)
	go func() {
		err := p.worker.Serve(p.ctx)
		p.workerDone <- err
		if p.ctx.Err() == nil {
			if err == nil {
				err = errors.New("local worker stopped unexpectedly")
			}
			select {
			case p.Errors <- err:
			default:
			}
		}
	}()
	p.WorkerDescriptor = PeerWorkerDescriptor{Name: cfg.Name, Address: cfg.Listen, Token: cfg.Token}
	return nil
}

func (p *Peer) Worker() PeerWorkerDescriptor { return p.WorkerDescriptor }

func (p *Peer) ConfigureNodes(nodes map[string]node.Config) error {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return coordination.ErrUnavailable
	}
	state := runtime.Status()
	for id, cfg := range nodes {
		if _, ok := state.Members[id]; ok {
			cfg.DialContext = p.DialWorker
			nodes[id] = cfg
		}
	}
	return nil
}

// DialWorker establishes an authenticated stream to a peer's own loopback
// worker. The connection's lifetime is independent of the setup context.
func (p *Peer) DialWorker(parent context.Context, nodeID string) (net.Conn, error) {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return nil, coordination.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	state, err := runtime.ReadState(ctx)
	if err != nil {
		return nil, err
	}
	if state.Coordinator.NodeID != p.Config.NodeID {
		return nil, coordination.ErrNotCoordinator
	}
	member, ok := state.Members[nodeID]
	if !ok {
		return nil, coordination.ErrInvalid
	}
	if nodeID == p.Config.NodeID {
		connection, err := p.openLocalWorker(ctx, p.Config.NodeID)
		if err != nil {
			return nil, err
		}
		go p.watchWorkerAuthority(p.ctx, state.Coordinator, state.WriterGeneration, connection.(*authenticatedWorkerConnection).done, func() { connection.Close() })
		return connection, nil
	}
	origin, err := url.Parse(member.APIAddress)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil {
		return nil, coordination.ErrInvalid
	}
	tlsConfig, err := p.identity.ClientConfig(nodeID)
	if err != nil {
		return nil, err
	}
	plain, err := p.peerDial(nodeID, false, 5*time.Second)(ctx, "tcp", origin.Host)
	if err != nil {
		return nil, err
	}
	connection := tls.Client(plain, tlsConfig)
	if err := connection.HandshakeContext(ctx); err != nil {
		plain.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer func() {
		if !stop() || ctx.Err() != nil {
			connection.Close()
		}
	}()
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Path: clusterWorkerPath}, Host: origin.Host, Header: http.Header{"Authorization": []string{"Bearer " + p.OwnerToken}, "X-Steve-Coordinator-Epoch": []string{strconv.FormatUint(state.Coordinator.Epoch, 10)}}}
	request.Header.Set("X-Steve-Writer-Generation", strconv.FormatUint(state.WriterGeneration, 10))
	if err := request.Write(connection); err != nil {
		connection.Close()
		return nil, err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		connection.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		connection.Close()
		return nil, fmt.Errorf("worker tunnel rejected: HTTP %d", response.StatusCode)
	}
	if err := ctx.Err(); err != nil {
		connection.Close()
		return nil, err
	}
	return &bufferedWorkerConnection{Conn: connection, reader: reader}, nil
}

type bufferedWorkerConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedWorkerConnection) Read(buffer []byte) (int, error) { return c.reader.Read(buffer) }

func (p *Peer) serveWorkerTunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "mutual TLS required", http.StatusUnauthorized)
		return
	}
	identity, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		http.Error(w, "invalid peer identity", http.StatusUnauthorized)
		return
	}
	if _, err := p.authorizeControl(r, identity, "worker"); err != nil {
		http.Error(w, "owner authorization denied", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	state, err := p.Runtime.Load().ReadState(ctx)
	cancel()
	if err != nil {
		HTTPError(w, err)
		return
	}
	epoch, err := strconv.ParseUint(r.Header.Get("X-Steve-Coordinator-Epoch"), 10, 64)
	if err != nil || state.Coordinator.NodeID != identity.NodeID || state.Coordinator.Epoch != epoch {
		HTTPError(w, coordination.ErrStaleEpoch)
		return
	}
	writer, err := strconv.ParseUint(r.Header.Get("X-Steve-Writer-Generation"), 10, 64)
	if err != nil || state.WriterGeneration != writer {
		HTTPError(w, coordination.ErrStaleEpoch)
		return
	}
	worker, err := p.openLocalWorker(r.Context(), identity.NodeID)
	if err != nil {
		HTTPError(w, coordination.ErrUnavailable)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		worker.Close()
		http.Error(w, "connection hijacking unavailable", http.StatusInternalServerError)
		return
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		worker.Close()
		return
	}
	p.Mu.Lock()
	if p.closing {
		p.Mu.Unlock()
		connection.Close()
		worker.Close()
		return
	}
	p.tunnels.Add(1)
	p.Mu.Unlock()
	defer p.tunnels.Done()
	defer connection.Close()
	defer worker.Close()
	stop := context.AfterFunc(p.ctx, func() { connection.Close(); worker.Close() })
	defer stop()
	watchCtx, watchCancel := context.WithCancel(p.ctx)
	defer watchCancel()
	go p.watchWorkerAuthority(watchCtx, state.Coordinator, writer, nil, func() { connection.Close(); worker.Close() })
	if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffer.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { io.Copy(connection, worker); connection.Close(); worker.Close(); close(done) }()
	io.Copy(worker, buffer)
	connection.Close()
	worker.Close()
	<-done
}

func restorePeerAdapters(cfg *node.ServerConfig) {
	installer := &adapter.Installer{Dir: filepath.Join(cfg.StateDir, "adapters")}
	for id, spec := range cfg.Harnesses {
		if spec.Adapter != "" {
			spec.Command = ""
			if installed, ok := installer.Resolve(spec.Adapter); ok {
				spec.Command = installed.Command
			}
			cfg.Harnesses[id] = spec
		}
	}
}

type workerPrincipal struct {
	NodeID     string
	Connection *authenticatedWorkerConnection
}

type authenticatedWorkerConnection struct {
	net.Conn
	once   sync.Once
	closed func()
	err    error
	done   chan struct{}
}

func (c *authenticatedWorkerConnection) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close(); close(c.done); c.closed() })
	return c.err
}

func (p *Peer) openLocalWorker(ctx context.Context, nodeID string) (net.Conn, error) {
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", p.WorkerDescriptor.Address)
	if err != nil {
		return nil, err
	}
	key := connection.LocalAddr().String()
	principal := &workerPrincipal{NodeID: nodeID}
	wrapped := &authenticatedWorkerConnection{Conn: connection, done: make(chan struct{}), closed: func() {
		p.Mu.Lock()
		if p.workerPrincipals[key] == principal {
			delete(p.workerPrincipals, key)
		}
		p.Mu.Unlock()
	}}
	principal.Connection = wrapped
	p.Mu.Lock()
	if p.closing {
		p.Mu.Unlock()
		connection.Close()
		return nil, net.ErrClosed
	}
	p.workerPrincipals[key] = principal
	p.Mu.Unlock()
	return wrapped, nil
}

func (p *Peer) authenticatedWorkerPeer(connection net.Conn) (string, bool) {
	p.Mu.RLock()
	defer p.Mu.RUnlock()
	principal, ok := p.workerPrincipals[connection.RemoteAddr().String()]
	if !ok {
		return "", false
	}
	return principal.NodeID, true
}

func (p *Peer) watchWorkerAuthority(ctx context.Context, assignment coordination.Assignment, writer uint64, done <-chan struct{}, closeConnection func()) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastQuorum := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
		}
		runtime := p.Runtime.Load()
		if runtime == nil {
			closeConnection()
			return
		}
		local := runtime.Status()
		if !local.Healthy || local.Coordinator != assignment || local.WriterGeneration != writer {
			closeConnection()
			return
		}
		if time.Since(lastQuorum) < time.Second {
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		state, err := runtime.ReadState(readCtx)
		cancel()
		if err != nil || state.Coordinator != assignment || state.WriterGeneration != writer {
			closeConnection()
			return
		}
		lastQuorum = time.Now()
	}
}

func (p *Peer) serveWorkerDescriptor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "mutual TLS required", http.StatusUnauthorized)
		return
	}
	identity, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		http.Error(w, "invalid peer identity", http.StatusUnauthorized)
		return
	}
	if _, err := p.authorizeControl(r, identity, "worker-descriptor"); err != nil {
		http.Error(w, "owner authorization denied", http.StatusForbidden)
		return
	}
	WriteJSON(w, p.Worker())
}

func (p *Peer) FetchWorker(ctx context.Context, member coordination.Member) (PeerWorkerDescriptor, error) {
	transport, origin, err := p.remoteTransport(member)
	if err != nil {
		return PeerWorkerDescriptor{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.String()+clusterWorkerPath+"/descriptor", nil)
	if err != nil {
		return PeerWorkerDescriptor{}, err
	}
	request.Header.Set("Authorization", "Bearer "+p.OwnerToken)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return PeerWorkerDescriptor{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return PeerWorkerDescriptor{}, fmt.Errorf("worker descriptor unavailable: HTTP %d", response.StatusCode)
	}
	var worker PeerWorkerDescriptor
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&worker); err != nil {
		return worker, err
	}
	if worker.Name != member.NodeID || len(worker.Token) < 32 {
		return PeerWorkerDescriptor{}, errors.New("worker descriptor identity differs from authenticated peer")
	}
	if err := requireClusterLoopback(worker.Address); err != nil {
		return PeerWorkerDescriptor{}, err
	}
	return worker, nil
}

func (p *Peer) SetCoordinatorEligibility(ctx context.Context, request consoleapi.CoordinatorEligibility) (consoleapi.CoordinationView, error) {
	_, err := p.Runtime.Load().SetEligibility(ctx, coordination.EligibilityRequest{ID: request.CommandID, Actor: "owner", ExpectedRevision: request.ExpectedRevision, NodeID: request.NodeID, Eligible: request.Eligible})
	if err != nil {
		return consoleapi.CoordinationView{}, err
	}
	return p.Coordination(ctx)
}

// SetNodeVoting gives a machine a vote in the cluster, or takes it back.
// This is separate from manually choosing the coordinator when automatic
// failover is disabled.
func (p *Peer) SetNodeVoting(ctx context.Context, request consoleapi.CoordinatorVoting) (consoleapi.CoordinationView, error) {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return consoleapi.CoordinationView{}, coordination.ErrUnavailable
	}
	_, err := runtime.SetVoting(ctx, coordination.VotingRequest{ID: request.CommandID, Actor: "owner", ExpectedRevision: request.ExpectedRevision, NodeID: request.NodeID, Voting: request.Voting})
	if err != nil {
		return consoleapi.CoordinationView{}, err
	}
	return p.Coordination(ctx)
}

func (p *Peer) RenameNode(ctx context.Context, request consoleapi.CoordinatorRename) (consoleapi.CoordinationView, error) {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return consoleapi.CoordinationView{}, coordination.ErrUnavailable
	}
	_, err := runtime.Rename(ctx, coordination.RenameRequest{ID: request.CommandID, Actor: "owner", ExpectedRevision: request.ExpectedRevision, NodeID: request.NodeID, Name: request.Name})
	if err != nil {
		return consoleapi.CoordinationView{}, err
	}
	return p.Coordination(ctx)
}

// MemberNames reads the display names this replica last synchronized. A
// replica that is not caught up shows a name at most one rename behind.
func (p *Peer) MemberNames() map[string]string {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return nil
	}
	return runtime.MemberNames()
}

func (p *Peer) serveCoordination(w http.ResponseWriter, r *http.Request) {
	var view consoleapi.CoordinationView
	var err error
	decode := func(target any) bool {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(target) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return false
		}
		return true
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/console/coordination":
		view, err = p.Coordination(r.Context())
	case r.Method == http.MethodPost && r.URL.Path == "/console/coordination/transfer":
		var request consoleapi.CoordinatorTransfer
		if !decode(&request) {
			return
		}
		view, err = p.TransferCoordinator(r.Context(), request)
	case r.Method == http.MethodPut && r.URL.Path == "/console/coordination/policy":
		var request consoleapi.CoordinatorPolicy
		if !decode(&request) {
			return
		}
		view, err = p.SetAutoFailover(r.Context(), request)
	case r.Method == http.MethodPut && r.URL.Path == "/console/coordination/eligibility":
		var request consoleapi.CoordinatorEligibility
		if !decode(&request) {
			return
		}
		view, err = p.SetCoordinatorEligibility(r.Context(), request)
	case r.Method == http.MethodPut && r.URL.Path == "/console/coordination/voting":
		var request consoleapi.CoordinatorVoting
		if !decode(&request) {
			return
		}
		view, err = p.SetNodeVoting(r.Context(), request)
	case r.Method == http.MethodPut && r.URL.Path == "/console/coordination/name":
		var request consoleapi.CoordinatorRename
		if !decode(&request) {
			return
		}
		view, err = p.RenameNode(r.Context(), request)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		HTTPError(w, err)
		return
	}
	WriteJSON(w, view)
}

func WriteJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(value)
}

func HTTPError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, coordination.ErrConflict) || errors.Is(err, coordination.ErrStaleEpoch) || errors.Is(err, coordination.ErrCommandConflict) {
		status = http.StatusConflict
	}
	if errors.Is(err, coordination.ErrInvalid) {
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// raftLogOutput sends Raft's own warnings and errors to the process log,
// which is where a lost leader or a stalled replica has to be visible. Tests
// pin their own Raft settings and keep the transcript quiet.
func raftLogOutput(options PeerOptions) io.Writer {
	if options.RaftConfig != nil {
		return io.Discard
	}
	return os.Stderr
}
