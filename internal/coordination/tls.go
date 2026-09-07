package coordination

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

type Identity struct {
	ClusterID string
	NodeID    string
}

// IdentityURI is the URI SAN shared by Raft and peer HTTP certificates. Node
// certificates must be signed by the cluster CA and permit both server and
// client authentication.
func IdentityURI(clusterID, nodeID string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: "steve", Path: "/cluster/" + clusterID + "/node/" + nodeID, RawPath: "/cluster/" + url.PathEscape(clusterID) + "/node/" + url.PathEscape(nodeID)}
}

func CertificateIdentity(certificate *x509.Certificate) (Identity, error) {
	var found *Identity
	for _, uri := range certificate.URIs {
		if uri.Scheme != "spiffe" || uri.Host != "steve" {
			continue
		}
		parts := strings.Split(uri.EscapedPath(), "/")
		if len(parts) != 5 || parts[0] != "" || parts[1] != "cluster" || parts[3] != "node" || uri.RawQuery != "" || uri.Fragment != "" || uri.User != nil {
			return Identity{}, fmt.Errorf("%w: invalid node certificate identity", ErrInvalid)
		}
		cluster, err := url.PathUnescape(parts[2])
		if err != nil || cluster == "" {
			return Identity{}, fmt.Errorf("%w: invalid cluster certificate identity", ErrInvalid)
		}
		node, err := url.PathUnescape(parts[4])
		if err != nil || node == "" {
			return Identity{}, fmt.Errorf("%w: invalid node certificate identity", ErrInvalid)
		}
		if found != nil {
			return Identity{}, fmt.Errorf("%w: ambiguous node certificate identity", ErrInvalid)
		}
		found = &Identity{ClusterID: cluster, NodeID: node}
	}
	if found == nil {
		return Identity{}, fmt.Errorf("%w: node certificate lacks its identity URI", ErrInvalid)
	}
	return *found, nil
}

type TLSOptions struct {
	ClusterID        string
	NodeID           string
	Certificate      tls.Certificate
	RootCAs          *x509.CertPool
	HandshakeTimeout time.Duration
	// AuthorizePeer is mandatory for the Raft stream layer. It must consult the
	// current members or an explicit initial seed/prepare authorization. A
	// certificate signed by the cluster CA alone does not grant Raft access.
	AuthorizePeer func(Identity) bool
}

func (o TLSOptions) validate() error {
	if o.RootCAs == nil || len(o.Certificate.Certificate) == 0 || o.ClusterID == "" || o.NodeID == "" {
		return fmt.Errorf("%w: cluster CA and node certificate are required", ErrInvalid)
	}
	leaf, err := x509.ParseCertificate(o.Certificate.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse local certificate: %w", err)
	}
	identity, err := CertificateIdentity(leaf)
	if err != nil {
		return err
	}
	if identity != (Identity{ClusterID: o.ClusterID, NodeID: o.NodeID}) {
		return fmt.Errorf("%w: local certificate identity differs", ErrInvalid)
	}
	return nil
}

func (o TLSOptions) ServerConfig() (*tls.Config, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{o.Certificate}, ClientCAs: o.RootCAs, ClientAuth: tls.RequireAndVerifyClientCert, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
			return fmt.Errorf("%w: unverified peer certificate", ErrInvalid)
		}
		identity, err := CertificateIdentity(state.PeerCertificates[0])
		if err != nil {
			return err
		}
		if identity.ClusterID != o.ClusterID {
			return fmt.Errorf("%w: peer belongs to another cluster", ErrInvalid)
		}
		return nil
	}}, nil
}

func (o TLSOptions) ClientConfig(expectedNodeID string) (*tls.Config, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	if expectedNodeID == "" {
		return nil, fmt.Errorf("%w: expected peer node ID is required", ErrInvalid)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{o.Certificate}, RootCAs: o.RootCAs,
		// The address can change independently of node identity. VerifyConnection
		// performs the full CA/EKU/time validation and exact URI identity check
		// in place of DNS hostname validation; it also runs on resumed sessions.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("%w: missing peer certificate", ErrInvalid)
			}
			intermediates := x509.NewCertPool()
			for _, certificate := range state.PeerCertificates[1:] {
				intermediates.AddCert(certificate)
			}
			leaf := state.PeerCertificates[0]
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: o.RootCAs, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return fmt.Errorf("verify peer certificate: %w", err)
			}
			identity, err := CertificateIdentity(leaf)
			if err != nil {
				return err
			}
			if identity != (Identity{ClusterID: o.ClusterID, NodeID: expectedNodeID}) {
				return fmt.Errorf("%w: peer node identity differs", ErrInvalid)
			}
			return nil
		}}, nil
}

type TLSStreamLayer struct {
	listener    net.Listener
	options     TLSOptions
	server      *tls.Config
	peerID      func(raft.ServerAddress) string
	mu          sync.Mutex
	closed      bool
	connections map[*handshakeConnection]struct{}
}

// NewTLSStreamLayer owns listener on success. peerID resolves an advertised
// Raft address to its expected stable node ID from trusted seeds/membership.
func NewTLSStreamLayer(listener net.Listener, options TLSOptions, peerID func(raft.ServerAddress) string) (*TLSStreamLayer, error) {
	if listener == nil || peerID == nil || options.AuthorizePeer == nil {
		return nil, fmt.Errorf("%w: listener, peer identity resolver and Raft peer authorization are required", ErrInvalid)
	}
	server, err := options.ServerConfig()
	if err != nil {
		return nil, err
	}
	if options.HandshakeTimeout <= 0 {
		options.HandshakeTimeout = 5 * time.Second
	}
	verify := server.VerifyConnection
	server.VerifyConnection = func(state tls.ConnectionState) error {
		if err := verify(state); err != nil {
			return err
		}
		identity, err := CertificateIdentity(state.PeerCertificates[0])
		if err != nil {
			return err
		}
		if !options.AuthorizePeer(identity) {
			return fmt.Errorf("%w: peer has not been authorized for Raft membership", ErrInvalid)
		}
		return nil
	}
	return &TLSStreamLayer{listener: listener, options: options, server: server, peerID: peerID, connections: map[*handshakeConnection]struct{}{}}, nil
}

func (s *TLSStreamLayer) Accept() (net.Conn, error) {
	connection, err := s.listener.Accept()
	if err != nil {
		return nil, err
	}
	// NetworkTransport reads each accepted connection in its own goroutine. Do
	// the bounded handshake there so an unauthenticated slow client cannot
	// serialize all valid Raft heartbeats behind its handshake.
	return s.track(tls.Server(connection, s.server))
}

func (s *TLSStreamLayer) track(connection *tls.Conn) (net.Conn, error) {
	secure := &handshakeConnection{Conn: connection, timeout: s.options.HandshakeTimeout}
	secure.authorize = func() bool {
		certificates := secure.ConnectionState().PeerCertificates
		if len(certificates) == 0 {
			return false
		}
		identity, err := CertificateIdentity(certificates[0])
		return err == nil && s.options.AuthorizePeer(identity)
	}
	secure.onClose = func() { s.mu.Lock(); delete(s.connections, secure); s.mu.Unlock() }
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		connection.Close()
		return nil, net.ErrClosed
	}
	s.connections[secure] = struct{}{}
	s.mu.Unlock()
	return secure, nil
}

type handshakeConnection struct {
	*tls.Conn
	timeout   time.Duration
	once      sync.Once
	err       error
	onClose   func()
	closeOnce sync.Once
	closeErr  error
	authorize func() bool
}

func (c *handshakeConnection) handshake() error {
	c.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		c.err = c.Conn.HandshakeContext(ctx)
	})
	return c.err
}
func (c *handshakeConnection) Read(buffer []byte) (int, error) {
	if err := c.handshake(); err != nil {
		return 0, err
	}
	if c.authorize != nil && !c.authorize() {
		c.Close()
		return 0, net.ErrClosed
	}
	return c.Conn.Read(buffer)
}
func (c *handshakeConnection) Write(buffer []byte) (int, error) {
	if err := c.handshake(); err != nil {
		return 0, err
	}
	if c.authorize != nil && !c.authorize() {
		c.Close()
		return 0, net.ErrClosed
	}
	return c.Conn.Write(buffer)
}

func (c *handshakeConnection) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.Conn.Close()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.closeErr
}

func (s *TLSStreamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	peerID := s.peerID(address)
	if !s.options.AuthorizePeer(Identity{ClusterID: s.options.ClusterID, NodeID: peerID}) {
		return nil, fmt.Errorf("%w: peer is not authorized for Raft membership", ErrInvalid)
	}
	config, err := s.options.ClientConfig(peerID)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = s.options.HandshakeTimeout
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: config}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	connection, err := dialer.DialContext(ctx, "tcp", string(address))
	if err != nil {
		return nil, err
	}
	secure, ok := connection.(*tls.Conn)
	if !ok {
		connection.Close()
		return nil, fmt.Errorf("%w: expected TLS connection", ErrInvalid)
	}
	return s.track(secure)
}

// RevokeUnauthorized closes pooled inbound connections as soon as a committed
// membership change removes their permission. Requests already accepted while
// the member was authorized remain subject to Raft's normal trusted-peer model.
func (s *TLSStreamLayer) RevokeUnauthorized() {
	s.mu.Lock()
	connections := make([]*handshakeConnection, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.mu.Unlock()
	for _, connection := range connections {
		if connection.HandshakeComplete() && connection.authorize != nil && !connection.authorize() {
			connection.Close()
		}
	}
}

func (c *handshakeConnection) HandshakeComplete() bool { return c.ConnectionState().HandshakeComplete }

func (s *TLSStreamLayer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	connections := make([]*handshakeConnection, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.mu.Unlock()
	err := s.listener.Close()
	for _, connection := range connections {
		connection.Close()
	}
	return err
}
func (s *TLSStreamLayer) Addr() net.Addr { return s.listener.Addr() }
