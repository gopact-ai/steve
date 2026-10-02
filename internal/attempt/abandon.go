package attempt

import "time"

// Abandoned records a decision to stop accounting for an execution without
// claiming that its process stopped. ProjectedAt records only session and
// endpoint-capacity retirement. DeliveryDoneAt separately records the receiver's
// durable result; neither releases the physical writer before exit is proved.
type Abandoned struct {
	At                time.Time       `json:"at"`
	By                string          `json:"by"`
	ForceStopRevision uint64          `json:"force_stop_revision"`
	Reason            string          `json:"reason"`
	MessageID         string          `json:"message_id,omitempty"`
	SlotState         string          `json:"slot_state"`
	SlotFingerprint   string          `json:"slot_fingerprint,omitempty"`
	ImportFingerprint string          `json:"import_fingerprint,omitempty"`
	Conversation      string          `json:"conversation"`
	Session           string          `json:"session,omitempty"`
	ProjectedAt       time.Time       `json:"projected_at,omitzero"`
	DeliveryDoneAt    time.Time       `json:"delivery_done_at,omitzero"`
	DeliveryResult    AbandonDelivery `json:"delivery_result,omitempty"`
}

// AbandonContext is the non-secret slot identity captured by the session owner.
type AbandonContext struct {
	State             string
	Fingerprint       string
	ImportFingerprint string
}
