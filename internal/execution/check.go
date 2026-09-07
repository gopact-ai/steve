package execution

import (
	"context"
	"errors"
)

// CheckExecution verifies the original scope's task token without starting
// work or upgrading its epoch. A read-only probe carries no such token and
// cannot establish that an admitted execution was explicitly stopped.
func CheckExecution(ctx context.Context) error {
	s, ok := ctx.Value(scopeKey{}).(*Scope)
	if !ok || s == nil || s.registry.tasks == nil {
		return errors.New("execution authorization requires an admitted scope and task store")
	}
	token := s.Token()
	if token == nil {
		return errors.New("execution scope has no task token")
	}
	return s.registry.tasks.CheckExecution(*token)
}
