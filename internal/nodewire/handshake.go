package nodewire

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/ability"
	"io"
	"strings"
	"time"
)

// ProtocolVersion is bumped when a frame or handshake field changes meaning.
// ProtocolMin is the oldest version this build still speaks; a peer whose
// range does not overlap ours is refused with both ranges in the reason.
const (
	ProtocolVersion = 1
	ProtocolMin     = 1
)

// Negotiate picks the newest version both sides speak, or 0 when the
// ranges do not overlap. A peer that names no range speaks exactly its
// Version.
func Negotiate(peerMin, peerMax int) int {
	if peerMax == 0 {
		return 0
	}
	if peerMin == 0 {
		peerMin = peerMax
	}
	chosen := min(peerMax, ProtocolVersion)
	if chosen < peerMin || chosen < ProtocolMin {
		return 0
	}
	return chosen
}

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
	// ProtocolMin and ProtocolMax are the range the hub speaks; the node
	// answers with the newest both sides share as Advert.Version. A hub
	// that sends only Version speaks that one version.
	ProtocolMin int `json:"protocol_min,omitempty"`
	ProtocolMax int `json:"protocol_max,omitempty"`
	// Features are the protocol extensions the hub supports.
	Features []string `json:"features,omitempty"`
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
	// Snapshot is everything the machine can do, as the ability domain
	// defines it: evidence, availability, coverage, a digest. An advert
	// without one is from an older node; Synthesize fills in from the
	// old fields and says so. Features are the protocol extensions the
	// sender supports.
	Snapshot *ability.Snapshot `json:"snapshot,omitempty"`
	Features []string          `json:"features,omitempty"`
	// SessionGraceMS is the node's process reconnect window, in milliseconds.
	SessionGraceMS int64 `json:"session_grace_ms,omitempty"`
	// OwnSkills are the skills the machine's AI tools have of their own,
	// outside Steve — under ~/.codex/skills and the like — so the owner
	// can see them from the hub and load one. A machine that predates
	// the field sends none.
	OwnSkills []OwnSkill `json:"own_skills,omitempty"`
	// OwnMCP are the MCP servers the machine's coding agents have of
	// their own, outside Steve — shape only: values of environment
	// variables and headers never leave the machine.
	OwnMCP []OwnMCP `json:"own_mcp,omitempty"`
	// Capabilities are free-form facts a step can require: "gpu",
	// "prod-cred", "internal-net".
	Capabilities  []string `json:"capabilities,omitempty"`
	WorkspaceRoot string   `json:"workspace_root,omitempty"`
	// StateDir is the node's own directory: blobs, shadow repositories.
	StateDir string `json:"state_dir,omitempty"`
	// Skills is the hash of the skill bundle the node has materialized
	// into its harness homes; empty means none. The hub compares it with
	// the bundle it would send and sends only on a difference.
	Skills string `json:"skills,omitempty"`
	// Git is the node's git version, empty when git is not on its PATH.
	// A node without git cannot hold a workspace that is not its own.
	Git        string `json:"git,omitempty"`
	GitMinimum string `json:"git_minimum,omitempty"`
	GitWarning string `json:"git_warning,omitempty"`
	// MCPPort is the node's loopback port for this hub's messaging server.
	// The node listens there and tunnels back, so an agent on this machine
	// still only ever talks to 127.0.0.1 — the security property survives
	// the move to another host instead of being traded for an open port.
	MCPPort int `json:"mcp_port,omitempty"`
	// Health is the machine's room to work, refreshed with every advert.
	Health *Health `json:"health,omitempty"`
	// Refused is set instead of the rest when the node turns the hub away.
	Refused string `json:"refused,omitempty"`
}

// Health is capacity rather than capability: free disk where workspaces
// live, the one-minute load, and the worktrees the machine holds. A
// machine that is full is not a machine to place work on, whatever its
// snapshot says it can do.
type Health struct {
	DiskFree  uint64    `json:"disk_free"`
	DiskTotal uint64    `json:"disk_total"`
	Load1     float64   `json:"load1"`
	Worktrees int       `json:"worktrees"`
	At        time.Time `json:"at"`
}

// Dial performs the hub side of the handshake on an established connection.
func Dial(conn io.ReadWriter, hello Hello) (Advert, error) {
	hello.Version = ProtocolVersion
	hello.ProtocolMin, hello.ProtocolMax = ProtocolMin, ProtocolVersion
	if err := writeJSON(conn, hello); err != nil {
		return Advert{}, fmt.Errorf("send hello: %w", err)
	}
	var advert Advert
	if err := readJSON(conn, &advert); err != nil {
		return Advert{}, fmt.Errorf("read advert: %w", err)
	}
	if advert.Refused != "" {
		// The node says why in words; the error says it in kind, so a
		// hub can tell a wrong token from a node another hub already holds.
		cause := ErrRefused
		switch {
		case advert.Refused == "token rejected":
			cause = ErrBadToken
		case strings.HasPrefix(advert.Refused, "hub speaks v"):
			cause = ErrVersionMismatch
		}
		return Advert{}, fmt.Errorf("%w: %s", cause, advert.Refused)
	}
	if advert.Version < ProtocolMin || advert.Version > ProtocolVersion {
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
	return AcceptClaim(conn, valid, nil, advert)
}

// ErrRefused is a handshake the node declined for a reason of its own —
// another hub already holds it, say — after the token checked out.
var ErrRefused = errors.New("nodewire: refused")

// AcceptClaim is AcceptWith with a claim step between the token check and
// the advert: the node may decline a hub it will not serve, and the hub
// learns why instead of receiving an advert and then losing the link.
func AcceptClaim(conn io.ReadWriter, valid func(token string) bool, claim func(Hello) error, advert Advert) (Hello, error) {
	var hello Hello
	if err := readJSON(conn, &hello); err != nil {
		return Hello{}, fmt.Errorf("read hello: %w", err)
	}
	peerMin, peerMax := hello.ProtocolMin, hello.ProtocolMax
	if peerMax == 0 {
		peerMin, peerMax = hello.Version, hello.Version
	}
	chosen := Negotiate(peerMin, peerMax)
	if chosen == 0 {
		_ = writeJSON(conn, Advert{Version: ProtocolVersion,
			Refused: fmt.Sprintf("hub speaks v%d–v%d, node speaks v%d–v%d", peerMin, peerMax, ProtocolMin, ProtocolVersion)})
		return Hello{}, ErrVersionMismatch
	}
	hello.Version = chosen
	advert.Version = chosen
	if !valid(hello.Token) {
		_ = writeJSON(conn, Advert{Version: ProtocolVersion, Refused: "token rejected"})
		return Hello{}, ErrBadToken
	}
	if claim != nil {
		if err := claim(hello); err != nil {
			_ = writeJSON(conn, Advert{Version: ProtocolVersion, Refused: err.Error()})
			return Hello{}, fmt.Errorf("%w: %s", ErrRefused, err)
		}
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

// OwnSkill is one skill a machine's AI tools have of their own.
type OwnSkill struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// OwnMCP is one MCP server a coding agent on the machine has configured.
type OwnMCP struct {
	Name       string   `json:"name"`
	Source     string   `json:"source"`
	Scope      string   `json:"scope,omitempty"`
	Type       string   `json:"type"`
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	URL        string   `json:"url,omitempty"`
	EnvKeys    []string `json:"env_keys,omitempty"`
	HeaderKeys []string `json:"header_keys,omitempty"`
}
