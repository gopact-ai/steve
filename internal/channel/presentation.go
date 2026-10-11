package channel

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/view"
)

var ErrUnsupported = errors.New("channel capability is unsupported")

// Presenter owns the provider wire schema. Core supplies semantic turn data;
// opaque bytes never become admission, interaction or delivery evidence.
type Presenter interface {
	Render(view.Turn, view.Copy) []byte
}

// Adapter is the minimum native channel contract. Rich presentation, topics
// and reactions are optional; plain text still has a durable provider receipt.
type Adapter interface {
	Messenger
	EnrichInput(context.Context, Input) Input
}

// Provider is a server plugin's native I/O port. Run authenticates and
// normalizes input, calls ready once delivery is available, and exits on ctx.
// It receives no harness, task store or owner credential.
type Provider interface {
	Adapter
	Run(context.Context, Handlers, func()) error
}

// ConversationTransport adds rich conversation I/O. Unsupported operations
// return ErrUnsupported, leaving uncertain effects behind original receipts.
type ConversationTransport interface {
	// Reply posts text without reporting the new message's id.
	Reply(ctx context.Context, messageID, text string) error
	// ReplyText posts text and returns the new message's id.
	ReplyText(ctx context.Context, messageID, text string) (string, error)
	// ReplyCard posts a card and returns its message id.
	ReplyCard(ctx context.Context, messageID string, payload []byte) (string, error)
	// PatchCard replaces a posted card in place.
	PatchCard(ctx context.Context, messageID string, payload []byte) error
	// ReplyThread starts a topic thread on a message and returns the anchor
	// message's id and the thread's id.
	ReplyThread(ctx context.Context, messageID, text string) (string, string, error)
	// EnrichInput reads an accepted message's optional remote context.
	EnrichInput(ctx context.Context, msg Input) Input
	// AddReaction reacts to a message and returns the reaction's id.
	AddReaction(ctx context.Context, messageID, emoji string) (string, error)
	// RemoveReaction removes a reaction AddReaction returned.
	RemoveReaction(ctx context.Context, messageID, reactionID string) error
	// DeleteMessage recalls a message the gateway posted.
	DeleteMessage(ctx context.Context, messageID string) error
}

// Handlers accepts only adapter-authenticated native input and actions.
type Handlers struct {
	Input  func(Input) error
	Action func(Action) Feedback
}
