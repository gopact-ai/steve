package work

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/conversation"
	"github.com/gopact-ai/steve/internal/ledger"
	"maps"
)

// Routes dispatches controlled intelligence to the persisted conversation
// channel. Registering another adapter does not change Work or event routing.
type Routes struct {
	Book     *ledger.Ledger
	adapters map[string]Conversation
}

func NewRoutes(book *ledger.Ledger, adapters map[string]Conversation) *Routes {
	return &Routes{Book: book, adapters: maps.Clone(adapters)}
}
func (r *Routes) adapter(ctx context.Context, id string) (Conversation, error) {
	name, err := conversation.Channel(ctx, r.Book, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrAdmission, err)
	}
	a, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("%w: unavailable channel %s", ErrAdmission, name)
	}
	return a, nil
}
func (r *Routes) ValidateWork(ctx context.Context, id, project, input string) error {
	a, err := r.adapter(ctx, id)
	if err != nil {
		return err
	}
	return a.ValidateWork(ctx, id, project, input)
}
func (r *Routes) AcceptWork(ctx context.Context, w Record, guard func(*ledger.Tx, string) error) (string, error) {
	a, err := r.adapter(ctx, w.Request.Conversation)
	if err != nil {
		return "", err
	}
	return a.AcceptWork(ctx, w, guard)
}
func (r *Routes) WorkResult(ctx context.Context, id, exchange string) (Result, bool, error) {
	a, err := r.adapter(ctx, id)
	if err != nil {
		return Result{}, false, err
	}
	return a.WorkResult(ctx, id, exchange)
}
