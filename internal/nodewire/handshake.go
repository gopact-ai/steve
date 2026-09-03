package nodewire

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// ProtocolVersion is bumped when a frame or handshake field changes meaning.
const ProtocolVersion = 1

// HandshakeTimeout bounds the hello/advert exchange so a wrong port does not
// hang a dial forever.
const HandshakeTimeout = 15 * time.Second

var (
	ErrBadToken        = errors.New("nodewire: node rejected the token")
	ErrVersionMismatch = errors.New("nodewire: protocol version mismatch")
)

// Hello is what the hub sends first. The token authenticates the hub to the
// node independently of whatever the network layer does — the two are
// deliberately not one chain.
type Hello struct {
	Version int    `json:"version"`
	Token   string `json:"token"`
	Hub     string `json:"hub,omitempty"`
}

// Harness is one agent runtime the node can actually start, with the models
// it offers. This is the node's own truth, discovered locally: the hub cannot
// know which binaries exist or which endpoints are reachable from there.
type Harness struct {
	ID      string   `json:"id"`
	Command string   `json:"command"`
	Version string   `json:"version,omitempty"`
	Models  []string `json:"models,omitempty"`
	// Slots is how many sessions this harness may run here at once; zero
	// means the node did not say, and the hub treats it as unlimited.
	Slots int `json:"slots,omitempty"`
	// Missing records why a configured harness is unusable here — a binary
	// that is not on the node's PATH, say. An honest roster reports what is
	// broken instead of omitting it.
	Missing string `json:"missing,omitempty"`
}

// Advert is the node's reply: what it is and what it can run. It is the only
// authority on this node's capabilities; a config line claiming otherwise is
// wrong, and the hub refuses placements the advert does not support.
type Advert struct {
	Version int    `json:"version"`
	Node    string `json:"node"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	// Hostname and IPs say which machine this is, independent of the
	// name the hub's config gave it; BuildVersion is the steve build
	// running there (Version above is the protocol's).
	BuildVersion string    `json:"build_version,omitempty"`
	Hostname     string    `json:"hostname,omitempty"`
	IPs          []string  `json:"ips,omitempty"`
	Harnesses    []Harness `json:"harnesses"`
	// Capabilities are free-form facts a step can require: "gpu",
	// "prod-cred", "internal-net".
	Capabilities  []string `json:"capabilities,omitempty"`
	WorkspaceRoot string   `json:"workspace_root,omitempty"`
	// StateDir is the node's own directory: blobs, shadow repositories.
	StateDir string `json:"state_dir,omitempty"`
	// Git is the node's git version, empty when git is not on its PATH.
	// A node without git cannot hold a workspace that is not its own.
	Git string `json:"git,omitempty"`
	// MCPPort is the node's loopback port for this hub's messaging server.
	// The node listens there and tunnels back, so an agent on this machine
	// still only ever talks to 127.0.0.1 — the security property survives
	// the move to another host instead of being traded for an open port.
	MCPPort int `json:"mcp_port,omitempty"`
	// Refused is set instead of the rest when the node turns the hub away.
	Refused string `json:"refused,omitempty"`
}

// Dial performs the hub side of the handshake on an established connection.
func Dial(conn io.ReadWriter, hello Hello) (Advert, error) {
	hello.Version = ProtocolVersion
	if err := writeJSON(conn, hello); err != nil {
		return Advert{}, fmt.Errorf("send hello: %w", err)
	}
	var advert Advert
	if err := readJSON(conn, &advert); err != nil {
		return Advert{}, fmt.Errorf("read advert: %w", err)
	}
	if advert.Refused != "" {
		return Advert{}, fmt.Errorf("%w: %s", ErrBadToken, advert.Refused)
	}
	if advert.Version != ProtocolVersion {
		return Advert{}, fmt.Errorf("%w: node speaks v%d, hub speaks v%d",
			ErrVersionMismatch, advert.Version, ProtocolVersion)
	}
	return advert, nil
}

// Accept performs the node side. It answers with advert on success, and with
// a refusal the hub can report rather than a bare closed connection.
func Accept(conn io.ReadWriter, token string, advert Advert) (Hello, error) {
	return AcceptWith(conn, func(offered string) bool {
		// Constant time: the token is a shared secret, and a timing oracle
		// here would let a prober recover it byte by byte.
		return subtle.ConstantTimeCompare([]byte(offered), []byte(token)) == 1
	}, advert)
}

// AcceptWith is Accept with the caller deciding which tokens are good: the
// hub's, or a one-time grant a peer node was given for one transfer.
func AcceptWith(conn io.ReadWriter, valid func(token string) bool, advert Advert) (Hello, error) {
	var hello Hello
	if err := readJSON(conn, &hello); err != nil {
		return Hello{}, fmt.Errorf("read hello: %w", err)
	}
	if hello.Version != ProtocolVersion {
		_ = writeJSON(conn, Advert{Version: ProtocolVersion,
			Refused: fmt.Sprintf("hub speaks v%d, node speaks v%d", hello.Version, ProtocolVersion)})
		return Hello{}, ErrVersionMismatch
	}
	if !valid(hello.Token) {
		_ = writeJSON(conn, Advert{Version: ProtocolVersion, Refused: "token rejected"})
		return Hello{}, ErrBadToken
	}
	advert.Version = ProtocolVersion
	if err := writeJSON(conn, advert); err != nil {
		return Hello{}, fmt.Errorf("send advert: %w", err)
	}
	return hello, nil
}

// The handshake rides the same framing as everything else so a reader never
// has to switch modes mid-connection.

func writeJSON(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return WriteFrame(w, Frame{Kind: KindOpen, Payload: payload})
}

func readJSON(r io.Reader, into any) error {
	f, err := ReadFrame(r)
	if err != nil {
		return err
	}
	if f.Kind != KindOpen {
		return fmt.Errorf("nodewire: expected handshake frame, got kind %d", f.Kind)
	}
	return json.Unmarshal(f.Payload, into)
}
