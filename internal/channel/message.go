// Package channel defines the transport-neutral messages agents exchange
// with the conversation they are working for. Adapters own rendering and I/O.
package channel

import (
	"context"
	"errors"
)

// ErrOutcomeUnknown means a dispatched operation may have taken effect.
// Adapters wrap it for transport failures that cannot prove non-delivery.
var ErrOutcomeUnknown = errors.New("channel message outcome is unknown")

// Address is an already-authorized destination. Message is the inbound
// anchor when sending, or the provider receipt when updating/recalling.
type Address struct {
	Channel      string `json:"channel"`
	Conversation string `json:"conversation"`
	Message      string `json:"message"`
}

// Message carries content and its attribution without a provider card schema.
type Message struct {
	Content     string `json:"content"`
	Format      string `json:"format"`
	Attribution string `json:"attribution,omitempty"`
}

type Messenger interface {
	Send(context.Context, Address, Message) (string, error)
	Update(context.Context, Address, Message) error
	Recall(context.Context, Address) error
}
