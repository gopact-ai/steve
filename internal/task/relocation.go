package task

import "errors"

// RecoveryWorkspace is a task's isolated continuation location. It does not
// alter the project's canonical home or turn this directory into a copy.
type RecoveryWorkspace struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	NodeID    string `json:"node_id"`
	Path      string `json:"path"`
	Base      string `json:"base"`
	AgentID   string `json:"agent_id"`
	HarnessID string `json:"harness_id"`
	AttemptID string `json:"attempt_id"`
	PlanID    string `json:"plan_id"`
	TurnID    string `json:"turn_id"`
}

type RecoveryUsage struct {
	Tokens   Tokens
	Model    string
	Reported bool
}

func (s *Store) BindRecoveryWorkspace(token ExecutionToken, workspace RecoveryWorkspace, usage ...RecoveryUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkExecution(s.data.Tasks, token); err != nil {
		return err
	}
	if workspace.ID == "" || workspace.NodeID == "" || workspace.Path == "" || workspace.Base == "" || workspace.AttemptID == "" || workspace.PlanID == "" {
		return errors.New("recovery workspace identity is incomplete")
	}
	next := s.clone()
	tracked := next.Tasks[token.TaskID]
	if tracked.ProjectID != workspace.ProjectID || tracked.Member != workspace.AgentID {
		return errors.New("recovery workspace belongs to another task/project")
	}
	if tracked.RecoveryWorkspace == nil || tracked.RecoveryWorkspace.PlanID != workspace.PlanID {
		now := s.now()
		if previous := tracked.primaryAttempt(); previous != nil && previous.Open() {
			var sourceUsage RecoveryUsage
			if len(usage) > 0 {
				sourceUsage = usage[0]
			}
			if err := settleAccounting(next.Tasks, tracked, previous, now, OutcomeError, sourceUsage); err != nil {
				return err
			}
		}
		// This is another execution of the same already charged user turn.
		tracked.Attempts = append(tracked.Attempts, Attempt{ExecutionID: workspace.AttemptID, TurnID: workspace.TurnID, ExecutionEpoch: token.Epoch, Member: workspace.AgentID, Node: workspace.NodeID, StartedAt: now})
	}
	tracked.RecoveryWorkspace = &workspace
	tracked.Node = workspace.NodeID
	tracked.Workspace = workspace.Path
	tracked.UpdatedAt = s.now()
	return s.replaceLocked(next)
}

func (s *Store) RecoveryOn(channel, agentID, origin string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var newest *Task
	for _, t := range s.data.Tasks {
		if t.Channel == channel && t.Member == agentID && t.Origin == origin && t.RecoveryWorkspace != nil && t.State.Holds() && (newest == nil || t.UpdatedAt.After(newest.UpdatedAt)) {
			newest = t
		}
	}
	if newest == nil {
		return Task{}, false
	}
	return *newest.clone(), true
}

// KeepsRecoveryWorkspace retains the last completed continuation directory
// between turns. A recovery workspace is not an orphan after an attempt ends.
func (s *Store) KeepsRecoveryWorkspace(node, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.data.Tasks {
		if r := t.RecoveryWorkspace; r != nil && r.NodeID == node && r.Path == path {
			return true
		}
	}
	return false
}
