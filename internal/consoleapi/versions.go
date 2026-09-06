package consoleapi

import (
	"context"
	"time"

	"github.com/gopact-ai/steve/internal/project"
)

// ReleaseManifest is a publisher's immutable description, independent of how
// releases are discovered. No binary runs merely because a manifest is found.
type ReleaseManifest struct {
	Version     string    `json:"version"`
	Revision    string    `json:"revision"`
	Component   string    `json:"component"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	URL         string    `json:"url"`
	SHA256      string    `json:"sha256"`
	ProtocolMin int       `json:"protocol_min"`
	ProtocolMax int       `json:"protocol_max"`
	PublishedAt time.Time `json:"published_at"`
}

// ReleaseProvider can be supplied by a future authenticated release service.
// Discovery and installation remain distinct operations.
type ReleaseProvider interface {
	Latest(context.Context, string, string, string) (ReleaseManifest, error)
}
type VersionNode struct {
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	Online     bool     `json:"online"`
	MatchesHub bool     `json:"matches_hub"`
	Protocol   int      `json:"protocol"`
	Features   []string `json:"features"`
}
type Versions struct {
	Projects            []project.Ownership `json:"projects"`
	Peers               []HubPeerInfo       `json:"peers"`
	Hub                 string              `json:"hub"`
	HubID               string              `json:"hub_id"`
	ProtocolMin         int                 `json:"protocol_min"`
	ProtocolMax         int                 `json:"protocol_max"`
	Nodes               []VersionNode       `json:"nodes"`
	DiscoveryConfigured bool                `json:"discovery_configured"`
	Automatic           bool                `json:"automatic"`
	Latest              []ReleaseManifest   `json:"latest,omitempty"`
}
type HubPeerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	URL  string `json:"url"`
}
type VersionService interface {
	Versions(context.Context) (Versions, error)
}
