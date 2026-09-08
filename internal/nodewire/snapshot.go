package nodewire

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
)

// Repo is one git repository inside a project's directory, as the machine
// that holds it sees it: which branch is checked out, the last commit,
// whether the tree is dirty, whether an instructions file is there.
type Repo struct {
	// Path is relative to the project directory; "." is the directory itself.
	Path     string    `json:"path"`
	Branch   string    `json:"branch,omitempty"`
	Head     string    `json:"head,omitempty"`
	Subject  string    `json:"subject,omitempty"`
	At       time.Time `json:"at,omitzero"`
	Dirty    bool      `json:"dirty"`
	Remote   string    `json:"remote,omitempty"`
	AgentsMD bool      `json:"agents_md"`
	// Missing says the directory itself does not exist on the machine.
	Missing bool `json:"missing,omitempty"`
}

// InspectReply answers StreamInspect.
type InspectReply struct {
	Repos []Repo `json:"repos"`
	Error string `json:"error,omitempty"`
}

// MCPTool is one tool an MCP server offers, as the probe saw it.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// MCPProbeReply answers StreamMCPProbe.
type MCPProbeReply struct {
	ServerName    string    `json:"server_name,omitempty"`
	ServerVersion string    `json:"server_version,omitempty"`
	Protocol      string    `json:"protocol,omitempty"`
	Tools         []MCPTool `json:"tools"`
	Digest        string    `json:"digest,omitempty"`
	ElapsedMS     int64     `json:"elapsed_ms,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// Settings is what a machine offers, as its operator writes it: the AI
// tools it can start, the commands to look for, the MCP servers it can
// bind, and what is only declared. It is the editable part of node.json,
// and of the hub's own configuration for the hub machine.
type Settings struct {
	Revision     string                    `json:"revision"`
	Harnesses    map[string]HarnessSetting `json:"harnesses"`
	Tools        []string                  `json:"tools"`
	MCPServers   map[string]MCPSetting     `json:"mcp_servers"`
	Declares     []string                  `json:"declares"`
	Capabilities []string                  `json:"capabilities"`
	// ExternalBroker says the MCP servers belong to a broker process of
	// its own and cannot be edited here.
	ExternalBroker bool `json:"external_broker,omitempty"`
}

// HarnessSetting is one AI tool: how to start it.
type HarnessSetting struct {
	Adapter    *string  `json:"adapter,omitempty"`
	Slots      *int     `json:"slots,omitempty"`
	Permission *string  `json:"permission,omitempty"`
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	Env        []string `json:"env"`
	ProcessDir string   `json:"process_dir,omitempty"`
	Models     []string `json:"models,omitempty"`
}

// MCPSetting is one MCP server as the machine can start or reach it. Env
// and headers are secrets; they travel hub→node inside the connection and
// are written to the node's own file, never into an advert.
type MCPSetting struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers"`
}

// ConfigReply answers StreamConfig: the settings in force, or why not.
type ConfigReply struct {
	Settings  Settings `json:"settings"`
	ErrorCode string   `json:"error_code,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// AdmitRequest asks a node for its final word on the part of a requirement
// it owns. The hub sends the tree, not text, so there is no second parser
// to disagree with the first; Generation and Sequence say which snapshot
// the hub placed on, so the reply can be read against it.
type AdmitRequest struct {
	Attempt     string              `json:"attempt"`
	Harness     string              `json:"harness"`
	Requirement ability.Requirement `json:"requirement"`
	Generation  int64               `json:"generation,omitempty"`
	Sequence    int64               `json:"sequence,omitempty"`
	// Uses names the MCP servers the session will use: the node binds
	// each one for the attempt and returns a launcher; one it cannot bind
	// refuses the admission.
	Uses []string `json:"uses,omitempty"`
	// Nonce is echoed in the reply, so a reply is known to answer this
	// request and no other.
	Nonce string `json:"nonce,omitempty"`
}

// AdmitReply is the node's verdict, on the snapshot it just took. Error is
// set when the node could not evaluate at all.
type AdmitReply struct {
	Admission ability.Admission `json:"admission"`
	Bindings  []ability.Binding `json:"bindings,omitempty"`
	Nonce     string            `json:"nonce,omitempty"`
	Error     string            `json:"error,omitempty"`
}

// Features a node or hub may support beyond the base protocol. The
// handshake exchanges them; a hub opens a stream only to a node that
// lists the feature it needs, and treats a node without one as older,
// never as broken.
const (
	FeatureJournal   = "process_journal.v1"
	FeatureManifest  = "manifest.v1"
	FeatureAdmission = "execution_admission.v1"
	// FeatureSkills says the node takes skill bundles: a content-addressed
	// tar the hub puts in its blob directory and asks it to materialize
	// into every harness home it isolates.
	FeatureSkills = "skill_bundle.v1"
	// FeatureMCP says the node binds its own MCP servers at admission and
	// hands back a secret-free launcher for each, instead of the hub
	// shipping commands and env in session/new.
	FeatureMCP = "node_mcp_binding.v1"
	// FeatureConfig says the node takes its offers from the hub over
	// StreamConfig and writes them to its own config file.
	FeatureConfig         = "node_config.v1"
	FeatureConfigRevision = "node_config_revision.v1"
	// FeatureInspect says the node answers StreamInspect.
	FeatureInspect = "inspect.v1"
	// FeatureMCPProbe says the node answers StreamMCPProbe and reports
	// its coding agents' own MCP servers in its advert.
	FeatureMCPProbe = "mcp_probe.v1"
	// FeatureOwnSkills says the node reports its coding agents' own
	// skills in its advert, so an empty list means none, not "too old".
	FeatureOwnSkills = "own_skills.v1"
	FeatureArtifact  = "artifact_ops.v1"
	FeatureFiles     = "file_ops.v1"
)

// Features is what this build supports.
func Features() []string {
	return []string{FeatureManifest, FeatureAdmission, FeatureSkills, FeatureMCP, FeatureConfig, FeatureConfigRevision, FeatureInspect, FeatureMCPProbe, FeatureOwnSkills, FeatureJournal, FeatureArtifact, FeatureFiles}
}

// HasFeature says whether a list names a feature.
func HasFeature(list []string, feature string) bool {
	for _, f := range list {
		if f == feature {
			return true
		}
	}
	return false
}

// Synthesize builds a snapshot for an advert that carries none — an
// older node. It knows only what the old fields say: harnesses and tag
// words. Coverage is partial for everything else, so a requirement on a
// tool is unknown there, not absent; the source says legacy.
func Synthesize(adv Advert, now time.Time) *ability.Snapshot {
	if adv.Snapshot != nil {
		return adv.Snapshot
	}
	s := &ability.Snapshot{
		Schema: ability.Schema, Node: adv.Node, GeneratedAt: now, ReceivedAt: now, Source: "legacy",
		Coverage: map[ability.Kind]ability.Coverage{ability.Harness: ability.Complete, ability.Tag: ability.Complete},
	}
	for _, h := range adv.Harnesses {
		c := ability.Capability{Kind: ability.Harness, ID: h.ID, Assurance: ability.Existence,
			Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "advert", OK: h.Missing == "", Result: h.Missing, At: now}}}
		if h.Missing != "" {
			c.Detail = h.Missing
		}
		s.Offers = append(s.Offers, c)
	}
	for _, tag := range adv.Capabilities {
		s.Offers = append(s.Offers, ability.Capability{Kind: ability.Tag, ID: tag, Evidence: []ability.Evidence{{Kind: ability.Declared, Method: "config", OK: true}}})
	}
	if s.Node == "" {
		s.Node = "hub"
	}
	if err := ability.Validate(s); err != nil {
		slog.Warn(fmt.Sprintf("nodewire: snapshot from %s advert invalid: %v", s.Node, err), "node", s.Node)
	}
	return s
}
