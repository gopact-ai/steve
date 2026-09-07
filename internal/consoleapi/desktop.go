package consoleapi

import "context"

type DesktopStatus struct {
	Enabled       bool   `json:"enabled"`
	NodeID        string `json:"node_id,omitempty"`
	SetupRequired bool   `json:"setup_required"`
	AgentCount    int    `json:"agent_count"`
	DefaultAgent  string `json:"default_agent,omitempty"`
}

type DesktopAgentCandidate struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Harness    string   `json:"harness"`
	Executable string   `json:"executable,omitempty"`
	Installed  bool     `json:"installed"`
	Requires   []string `json:"requires,omitempty"`
	Registered bool     `json:"registered"`
}

type DesktopDiscovery struct {
	Agents []DesktopAgentCandidate `json:"agents"`
}

type DesktopEnrollRequest struct {
	AgentIDs []string `json:"agent_ids"`
}

type DesktopService interface {
	DesktopStatus(context.Context) (DesktopStatus, error)
	DesktopDiscover(context.Context) (DesktopDiscovery, error)
	DesktopEnroll(context.Context, DesktopEnrollRequest) (DesktopStatus, error)
}
