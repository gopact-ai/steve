package channel

import "github.com/gopact-ai/steve/internal/protocol"

// Input is normalized by an authenticated adapter before durable acceptance.
// The JSON names retain the native input image used by historical receipts.
// Provider-specific enrichment keys are opaque to the conversation core.
type Input struct {
	ConversationID string
	ChatID         string
	ChatType       protocol.ChatType
	MessageID      string
	ActorID        string `json:"SenderOpenID"`
	Text           string
	Quote          string
	ParentID       string
	ImageKeys      []string
	Images         []Image
	Mentioned      bool
	Origin         string
}
type Image struct {
	MIME string
	Data []byte
}

// Action is a normalized interaction response, authenticated by the adapter.
// Its provider receipt and original actor are checked by the interaction owner.
type Action struct {
	ActorID   string `json:"OpenID"`
	MessageID string
	ChatID    string
	Action    string
	RequestID string
	Decision  string
}
type Feedback struct {
	Type    string
	Content string
}
