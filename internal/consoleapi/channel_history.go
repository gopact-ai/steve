package consoleapi

import (
	"context"
	"errors"
)

var (
	ErrChannelConversationNotFound = errors.New("channel conversation not found")
	ErrChannelHistoryCursor        = errors.New("invalid channel history cursor")
)

// ExchangeIdentity resolves the authoritative owner before queue controls run.
type ExchangeIdentity interface {
	ExchangeConversation(id string) (string, bool)
}

// ChannelHistory is a read-only projection of accepted channel inputs.
// IDs remain opaque transport identities, never console-prefixed aliases.
type ChannelHistory interface {
	List(context.Context, string, int) (ChannelConversationPage, error)
	Read(context.Context, string, string, int) (ChannelConversationHistory, error)
	Contains(context.Context, string) (bool, error)
}

type ChannelConversationPage struct {
	Conversations []Conversation `json:"conversations"`
	NextCursor    string         `json:"next_cursor,omitempty"`
}

type ChannelConversationHistory struct {
	Conversation Conversation `json:"conversation"`
	Replies      []Reply      `json:"replies"`
	NextCursor   string       `json:"next_cursor,omitempty"`
}
