package execution

import (
	"errors"
	"fmt"
	"strings"
)

// RetainedObserverDetached identifies a node-owned execution that lost only
// this process's observer. It does not prove native process exit. Task Stop
// still reports this unresolved execution; global shutdown may finish joining
// an observer whose entire service lifetime has ended.
type RetainedObserverDetached struct {
	AttemptID string
	NodeID    string
	SessionID string
	Cause     error
}

// NodePreparationObserverDetached identifies an attempted node-owned open
// whose reply was lost before the coordinator learned a native session ID.
// It is not a local writer and does not prove that a node process stopped.
type NodePreparationObserverDetached struct {
	AttemptID     string
	NodeID        string
	OpenCommandID string
	Cause         error
}

func (e *NodePreparationObserverDetached) Error() string {
	return fmt.Sprintf("observer detached while opening execution %s on %s", e.AttemptID, e.NodeID)
}
func (e *NodePreparationObserverDetached) Unwrap() error { return e.Cause }

func (e *RetainedObserverDetached) Error() string {
	return fmt.Sprintf("observer detached from retained execution %s on %s", e.AttemptID, e.NodeID)
}
func (e *RetainedObserverDetached) Unwrap() error { return e.Cause }

func (s *Scope) retainedDetached() bool {
	var detached *RetainedObserverDetached
	if errors.As(s.err, &detached) && detached.AttemptID == s.key.AttemptID && detached.NodeID != "" && strings.HasPrefix(detached.SessionID, "ns_") {
		return true
	}
	var preparation *NodePreparationObserverDetached
	return errors.As(s.err, &preparation) && preparation.AttemptID != "" && preparation.AttemptID == s.key.AttemptID && preparation.NodeID != "" && preparation.OpenCommandID != ""
}

// AdoptRetained replaces ended observers of the exact same attempt after its
// node binding and original command were revalidated. The new scope stays
// active, so this never reports that the native execution has stopped.
func (s *Scope) AdoptRetained() {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	select {
	case <-s.done:
		return
	default:
	}
	if s.key.AttemptID == "" {
		return
	}
	for previous := range s.registry.entries {
		if previous == s || previous.key.AttemptID != s.key.AttemptID {
			continue
		}
		select {
		case <-previous.done:
			delete(s.registry.entries, previous)
		default:
		}
	}
}
