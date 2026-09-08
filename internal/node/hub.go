package node

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// closeStream ends a stream with the reason the hub reads as its result.
// The reason is the reply, so failing to send it is worth a line; a
// connection that already went away reports nil here, and the hub loop
// logs the disconnect instead.
func closeStream(stream *nodewire.Stream, reason string) {
	if err := stream.CloseWithReason(reason); err != nil {
		slog.Error(fmt.Sprintf("steve-node: close %v stream: %v", stream.Request().Kind, err), "kind", stream.Request().Kind)
	}
}

// hubClaim is what a handshake took on this node, kept where the deferred
// release can see it: a claim may have succeeded even when writing the
// advert afterwards failed.
type hubClaim struct {
	hub     string
	claimed bool
	// clean records that the hub left gracefully with its processes
	// stopped, which the persisted owner record passes on as evidence.
	clean bool
}

// handle serves one accepted connection: the handshake, then either the
// single blob a peer was granted or the hub's stream loop.
func (s *Server) handle(ctx context.Context, socket net.Conn) {
	defer socket.Close()
	// Closing on cancellation only wakes the reads below; the deferred
	// close reports nothing either.
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	claim := &hubClaim{}
	defer func() {
		if claim.claimed {
			s.release(claim.hub, claim.clean)
		}
	}()
	hello, ok := s.handshake(socket, claim)
	if !ok {
		return
	}
	sessionPrincipal := hello.Hub
	if authenticate := s.conf().AuthenticatedPeer; authenticate != nil {
		sessionPrincipal, ok = authenticate(socket)
		if !ok || sessionPrincipal == "" {
			slog.Warn("steve-node: rejected connection without authenticated peer identity")
			return
		}
	}
	if name, ok := s.grantedName(hello.Token); ok {
		// A peer, not the hub: it may take the one blob it was granted and
		// nothing else, and the grant is spent by the connection.
		s.servePeer(ctx, socket, hello, name)
		return
	}
	slog.Info(fmt.Sprintf("steve-node: hub %q connected from %s", hello.Hub, socket.RemoteAddr()), "hub", hello.Hub)
	claim.clean = s.serveHub(ctx, socket, hello.Hub, sessionPrincipal)
}

// handshake exchanges hello and advert under the handshake deadline. A hub
// presenting a bound token must call itself by the bound name, and claims
// the node before the advert goes out; a granted peer claims nothing.
func (s *Server) handshake(socket net.Conn, claim *hubClaim) (nodewire.Hello, bool) {
	// The deadline only bounds the handshake; the connection is long-lived
	// afterwards and its streams carry their own timeouts.
	if err := socket.SetDeadline(time.Now().Add(nodewire.HandshakeTimeout)); err != nil {
		slog.Error(fmt.Sprintf("steve-node: handshake from %s: %v", socket.RemoteAddr(), err))
		return nodewire.Hello{}, false
	}
	// The reverse listener is bound before the advert so its port can be
	// reported in the same breath: the hub bakes that URL into the session
	// fingerprint, so it has to be known before any session opens.
	mcp, err := s.listenMCP()
	if err != nil {
		slog.Error(fmt.Sprintf("steve-node: reverse MCP listener: %v — agents here lose the send primitive", err))
	}
	advert := s.advert()
	if mcp != nil {
		advert.MCPPort = mcp.Addr().(*net.TCPAddr).Port
	}
	hello, err := nodewire.AcceptClaim(socket, s.validToken, func(h nodewire.Hello) error {
		if _, peer := s.grantedName(h.Token); peer {
			return nil
		}
		if bound, ok := s.hubOf(h.Token); ok && bound != h.Hub {
			return fmt.Errorf("this token belongs to hub %q, not %q", bound, h.Hub)
		}
		if err := s.claim(h.Hub); err != nil {
			return err
		}
		claim.claimed = true
		claim.hub = h.Hub
		return nil
	}, advert)
	if err != nil {
		slog.Error(fmt.Sprintf("steve-node: handshake from %s: %v", socket.RemoteAddr(), err))
		return hello, false
	}
	// Clearing a deadline on a live socket cannot fail in a way the
	// stream loop would not notice on its first read.
	_ = socket.SetDeadline(time.Time{})
	return hello, true
}

// serveHub multiplexes the hub's streams until it leaves and reports
// whether it left cleanly: a graceful close while it was still the hub
// being served, which also stops the processes it owned here.
func (s *Server) serveHub(ctx context.Context, socket net.Conn, hub, principal string) (clean bool) {
	mux := nodewire.NewMux(socket, false)
	defer mux.Close()
	s.attachHub(mux)
	defer func() {
		clean = s.detachHub(mux)
		if clean {
			s.stopProcesses(hub)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			// The socket is closed by handle's AfterFunc; closing the mux
			// here only wakes Accept sooner.
			_ = mux.Close()
		case <-mux.Done():
		}
	}()
	for {
		stream, err := mux.Accept(ctx)
		if err != nil {
			slog.Warn(fmt.Sprintf("steve-node: hub %q disconnected", hub), "hub", hub)
			return
		}
		s.dispatch(ctx, mux, hub, principal, stream)
	}
}

// attachHub makes mux the served hub and wakes whoever was waiting for
// one to attach.
func (s *Server) attachHub(mux *nodewire.Mux) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hubMux = mux
	s.hubSeen = true
	if s.hubWaiters != nil {
		close(s.hubWaiters)
		s.hubWaiters = nil
	}
}

// detachHub forgets mux if it is still the served hub and reports whether
// it closed gracefully while it was.
func (s *Server) detachHub(mux *nodewire.Mux) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	clean := mux.Graceful() && s.hubMux == mux
	if s.hubMux == mux {
		s.hubMux = nil
	}
	return clean
}

// readOnlyStream is a request that observes the node without changing it,
// which a restarting node still answers.
func readOnlyStream(req nodewire.OpenRequest) bool {
	return req.Kind == nodewire.StreamAdvert || req.Kind == nodewire.StreamInspect || (req.Kind == nodewire.StreamConfig && (req.Command == "get" || req.Command == "discover-agents"))
}

// dispatch hands one accepted stream to its handler on the request
// group. Work that changes the node is counted against a pending restart
// and refused once one is draining.
func (s *Server) dispatch(ctx context.Context, mux *nodewire.Mux, hub, principal string, stream *nodewire.Stream) {
	req := stream.Request()
	if req.Kind == nodewire.StreamRestart {
		s.requestWG.Go(func() { s.restartStream(hub, stream) })
		return
	}
	var done func()
	if !readOnlyStream(req) {
		var err error
		done, err = s.beginWork()
		if err != nil {
			closeStream(stream, err.Error())
			return
		}
	}
	s.requestWG.Go(func() {
		if done != nil {
			defer done()
		}
		s.serveStream(ctx, mux, principal, stream)
	})
}

// serveStream routes a stream by kind; anything unnamed is an agent
// process stream.
func (s *Server) serveStream(ctx context.Context, mux *nodewire.Mux, principal string, stream *nodewire.Stream) {
	switch stream.Request().Kind {
	case nodewire.StreamNodeSessions:
		s.sessionStream(ctx, principal, stream)
	case nodewire.StreamExec:
		s.runCommand(ctx, stream)
	case nodewire.StreamArtifact:
		s.runArtifact(ctx, stream)
	case nodewire.StreamFiles:
		s.runFiles(ctx, stream)
	case nodewire.StreamAdvert:
		s.sendAdvert(stream)
	case nodewire.StreamBlob:
		s.transferBlob(ctx, stream)
	case nodewire.StreamGrant:
		s.grant(stream)
	case nodewire.StreamFetch:
		s.fetch(ctx, stream)
	case nodewire.StreamAdmit:
		s.admit(stream)
	case nodewire.StreamSkills:
		s.applySkills(stream)
	case nodewire.StreamRelease:
		s.releaseAttempt(stream)
	case nodewire.StreamConfig:
		s.configure(stream)
	case nodewire.StreamInspect:
		s.inspect(ctx, stream)
	case nodewire.StreamMCPProbe:
		s.mcpProbe(ctx, stream)
	default:
		if stream.Request().Kind == nodewire.StreamACP {
			s.injectDrop(ctx, mux)
		}
		s.runAgent(ctx, stream)
	}
}

// peerGrant admits one peer connection for one blob until it expires.
type peerGrant struct {
	name    string
	expires time.Time
}

func (s *Server) validToken(token string) bool {
	if s.conf().Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.conf().Token)) == 1 {
		return true
	}
	if hub, _ := s.hubOf(token); hub != "" {
		return true
	}
	_, ok := s.grantedName(token)
	return ok
}

// hubOf is the hub name a token vouches for, from the hubs table; "" when
// the token is the shared one or unknown.
func (s *Server) hubOf(token string) (string, bool) {
	for name, t := range s.conf().Hubs {
		if t != "" && subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1 {
			return name, true
		}
	}
	return "", false
}

func (s *Server) grantedName(token string) (string, bool) {
	s.grantsMu.Lock()
	defer s.grantsMu.Unlock()
	g, ok := s.grants[token]
	if !ok {
		return "", false
	}
	if time.Now().After(g.expires) {
		delete(s.grants, token)
		return "", false
	}
	return g.name, true
}

// grant registers a one-time peer token: "<token> <name> <seconds>".
func (s *Server) grant(stream *nodewire.Stream) {
	fields := strings.Fields(stream.Request().Command)
	if len(fields) != 3 || fields[2] == "" {
		closeStream(stream, nodewire.ExitPrefix+"2")
		return
	}
	seconds, err := strconv.Atoi(fields[2])
	if err != nil || seconds <= 0 || fields[1] != filepath.Base(fields[1]) {
		closeStream(stream, nodewire.ExitPrefix+"2")
		return
	}
	s.grantsMu.Lock()
	if s.grants == nil {
		s.grants = map[string]peerGrant{}
	}
	s.grants[fields[0]] = peerGrant{name: fields[1], expires: time.Now().Add(time.Duration(seconds) * time.Second)}
	s.grantsMu.Unlock()
	slog.Info(fmt.Sprintf("steve-node: granted a peer %s for %ds", fields[1], seconds), "peer", fields[1])
	closeStream(stream, nodewire.ExitPrefix+"0")
}

// servePeer answers exactly one "get <name>" for the granted name.
func (s *Server) servePeer(ctx context.Context, socket net.Conn, hello nodewire.Hello, name string) {
	s.grantsMu.Lock()
	delete(s.grants, hello.Token)
	s.grantsMu.Unlock()
	slog.Info(fmt.Sprintf("steve-node: peer %q connected from %s for %s", hello.Hub, socket.RemoteAddr(), name), "peer", hello.Hub, "blob", name)
	mux := nodewire.NewMux(socket, false)
	defer mux.Close()
	stream, err := mux.Accept(ctx)
	if err != nil {
		return
	}
	req := stream.Request()
	if req.Kind != nodewire.StreamBlob || req.Command != "get "+name {
		closeStream(stream, nodewire.ExitPrefix+"2")
		return
	}
	done, err := s.beginWork()
	if err != nil {
		closeStream(stream, err.Error())
		return
	}
	defer done()
	s.transferBlob(ctx, stream)
}
