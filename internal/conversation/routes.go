package conversation

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

const RouteKind = "conversation-route"
const MessageRouteKind = "conversation-message-route"

type Route struct {
	Conversation string `json:"conversation"`
	Channel      string `json:"channel"`
}

// BindRouteTx is committed with adapter admission. Opaque IDs and their
// prefixes cannot override the durable channel to which a conversation belongs.
func BindRouteTx(tx *ledger.Tx, id, channel string) error {
	if id == "" || channel == "" {
		return errors.New("conversation route needs an identity and channel")
	}
	old, found, err := durable.Get[Route](tx, RouteKind, id)
	if err != nil {
		return err
	}
	if found {
		if old.Channel != channel || old.Conversation != id {
			return ledger.ErrConflict
		}
		return nil
	}
	return tx.PutBinding(RouteKind, id, Route{Conversation: id, Channel: channel})
}
func Channel(ctx context.Context, book *ledger.Ledger, id string) (string, error) {
	route, found, err := durable.Read[Route](ctx, book, RouteKind, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("conversation has no admitted channel route")
	}
	return route.Channel, nil
}

func BindMessageTx(tx *ledger.Tx, channel, message, id string) error {
	key := durable.Key(channel, message)
	old, found, err := durable.Get[Route](tx, MessageRouteKind, key)
	if err != nil {
		return err
	}
	if found && (old.Channel != channel || old.Conversation != id) {
		return ledger.ErrConflict
	}
	return tx.PutBinding(MessageRouteKind, key, Route{Conversation: id, Channel: channel})
}

func MessageConversation(ctx context.Context, book *ledger.Ledger, channel, message string) (string, error) {
	r, found, err := durable.Read[Route](ctx, book, MessageRouteKind, durable.Key(channel, message))
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("native reply anchor has no conversation route")
	}
	return r.Conversation, nil
}
