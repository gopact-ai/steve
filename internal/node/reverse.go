package node

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// listenMCP belongs to the server, not a hub connection. Its port is part of
// the ACP session fingerprint, including throughout a network interruption.
func (s *Server) listenMCP() (net.Listener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mcpListener != nil {
		return s.mcpListener, nil
	}
	var listener net.Listener
	var err error
	if s.mcpPort > 0 {
		listener, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(s.mcpPort))
	}
	if listener == nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	s.mcpPort = listener.Addr().(*net.TCPAddr).Port
	s.mcpListener = listener
	s.rememberPort(s.mcpPort)
	go s.forwardMCP(listener)
	return listener, nil
}

func (s *Server) closeMCP() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mcpListener != nil {
		_ = s.mcpListener.Close()
	}
}

// hubGrace is how long a request waits for the first hub to attach. A
// node that has only just come up is not unreachable, and the agent
// asking has no way to tell the difference. An outage is a different
// thing: once a hub has been here, its absence is reported at once, since
// it reconnects on its own and the caller is told to retry.
const hubGrace = 5 * time.Second

// awaitHub returns the hub connection, waiting briefly for the first one
// to attach. The wait exists because nothing else announces readiness:
// the reverse channel is wired by a goroutine, and the first tool call
// can arrive before it has run.
func (s *Server) awaitHub(ctx context.Context) (*nodewire.Mux, error) {
	s.mu.Lock()
	mux := s.hubMux
	if mux == nil && !s.hubSeen {
		if s.hubWaiters == nil {
			s.hubWaiters = make(chan struct{})
		}
		waiters := s.hubWaiters
		s.mu.Unlock()
		wait, cancel := context.WithTimeout(ctx, hubGrace)
		defer cancel()
		select {
		case <-waiters:
		case <-wait.Done():
			return nil, fmt.Errorf("hub unreachable")
		}
		s.mu.Lock()
		mux = s.hubMux
	}
	s.mu.Unlock()
	if mux == nil {
		return nil, fmt.Errorf("hub unreachable")
	}
	return mux, nil
}

func (s *Server) forwardMCP(listener net.Listener) {
	transport := &http.Transport{
		// Each request gets one stream. In particular, failed writes are
		// never retried on a reused HTTP connection: tools may have effects.
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 15 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			mux, err := s.awaitHub(ctx)
			if err != nil {
				return nil, err
			}
			select {
			case <-mux.Done():
				return nil, fmt.Errorf("hub unreachable")
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			stream, err := mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamMCP})
			if err != nil {
				return nil, err
			}
			return streamConn{Stream: stream}, nil
		},
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: "hub"})
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"hub unreachable, retry later"}}`))
		},
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	defer server.Close()
	defer transport.CloseIdleConnections()
	_ = server.Serve(listener)
}

// HTTP owns request cancellation and the header timer. Mux bounds socket
// writes; a stream has no independent socket deadline to overwrite.
type streamConn struct{ *nodewire.Stream }

func (streamConn) LocalAddr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (streamConn) RemoteAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (streamConn) SetDeadline(time.Time) error      { return nil }
func (streamConn) SetReadDeadline(time.Time) error  { return nil }
func (streamConn) SetWriteDeadline(time.Time) error { return nil }
