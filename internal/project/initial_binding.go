package project

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
)

// BindInitial creates a conversation's first binding. A retry to the same
// project preserves its version; an existing different binding is never moved.
func (s *Store) BindInitial(ctx context.Context, conversationID, projectID, by string) (Binding, error) {
	if _, ok, err := s.Get(ctx, projectID); err != nil {
		return Binding{}, err
	} else if !ok {
		return Binding{}, fmt.Errorf("%w: %s", ErrUnknown, projectID)
	}
	current, exists, err := s.Binding(ctx, conversationID)
	if err != nil {
		return Binding{}, err
	}
	if !exists {
		created := Binding{ConversationID: conversationID, ProjectID: projectID, By: by, At: s.now().UTC()}
		err = s.l.Update(ctx, func(tx *ledger.Tx) error {
			version, err := tx.CompareAndSetName(bindingNameBase+conversationID+"/project", 0, projectID)
			if err != nil {
				return err
			}
			created.Version = version
			return tx.PutBinding(kindBinding, conversationID, created)
		})
		if err == nil {
			return created, nil
		}
		if !errors.Is(err, ledger.ErrConflict) {
			return Binding{}, err
		}
		// Another initializer may have committed this same binding while
		// we waited. Read its receipt rather than advancing the version.
		current, exists, err = s.Binding(ctx, conversationID)
		if err != nil {
			return Binding{}, err
		}
	}
	if exists && current.ProjectID == projectID {
		return current, nil
	}
	return Binding{}, fmt.Errorf("%w: conversation %q is already bound to project %q", ledger.ErrConflict, conversationID, current.ProjectID)
}
