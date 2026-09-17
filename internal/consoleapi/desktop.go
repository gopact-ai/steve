package consoleapi

import "context"

type DesktopStatus struct {
	Enabled       bool   `json:"enabled"`
	NodeID        string `json:"node_id,omitempty"`
	SetupRequired bool   `json:"setup_required"`
	AgentCount    int    `json:"agent_count"`
	// LocalAgentCount is how many of those agents run on this computer.
	// It is zero on a machine that only drives agents elsewhere, which is
	// a supported way to run the desktop App.
	LocalAgentCount int    `json:"local_agent_count"`
	DefaultAgent    string `json:"default_agent,omitempty"`
	// WorkspacePath is the default project's directory on this computer:
	// where conversations work unless a project says otherwise.
	WorkspacePath string `json:"workspace_path,omitempty"`
	// WorkspaceManaged is set while that directory is still inside the
	// desktop's own state directory, where a fresh installation keeps its
	// first project until the owner chooses somewhere.
	WorkspaceManaged bool `json:"workspace_managed,omitempty"`
	// Setup is where the first-run guide opens next. It is empty when the
	// desktop is not managed here.
	Setup *DesktopSetup `json:"setup,omitempty"`
}

type DesktopSetup struct {
	Step string `json:"step"`
	Done bool   `json:"done"`
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

// DesktopEnrollAgent registers one discovered tool. AgentID is the name the
// agent answers to in chat, About says what it is good for so the planner
// can pick it, and Default makes it the agent a conversation starts with.
type DesktopEnrollAgent struct {
	CandidateID string `json:"candidate_id"`
	AgentID     string `json:"agent_id,omitempty"`
	About       string `json:"about,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

// DesktopEnrollRequest carries the owner's choice. Agents is what the guide
// sends; AgentIDs is the same choice without names, kept so an older client
// still registers its tools under their own IDs.
type DesktopEnrollRequest struct {
	AgentIDs []string             `json:"agent_ids,omitempty"`
	Agents   []DesktopEnrollAgent `json:"agents,omitempty"`
}

// DesktopSetupRequest records the guide page to open next; Done closes the
// guide for good.
type DesktopSetupRequest struct {
	Step string `json:"step"`
	Done bool   `json:"done,omitempty"`
}

// DesktopWorkspaceRequest moves the default project's directory.
type DesktopWorkspaceRequest struct {
	Path string `json:"path"`
}

type DesktopService interface {
	DesktopStatus(context.Context) (DesktopStatus, error)
	DesktopDiscover(context.Context) (DesktopDiscovery, error)
	DesktopEnroll(context.Context, DesktopEnrollRequest) (DesktopStatus, error)
	DesktopSetup(context.Context, DesktopSetupRequest) (DesktopStatus, error)
	DesktopWorkspace(context.Context, DesktopWorkspaceRequest) (DesktopStatus, error)
}
