package consoleapi

import (
	"encoding/json"
	"testing"
)

func TestExchangeStateKeepsLegacyReceipts(t *testing.T) {
	for _, state := range []string{"queued", "running", "recovering", "awaiting-user", "done", "failed", "cancelled", "future-state", ""} {
		t.Run(state, func(t *testing.T) {
			var exchange Exchange
			if err := json.Unmarshal([]byte(`{"state":"`+state+`"}`), &exchange); err != nil {
				t.Fatal(err)
			}
			terminal := state == "done" || state == "failed" || state == "cancelled"
			if exchange.State.Terminal() != terminal {
				t.Fatalf("legacy receipt terminal classification changed: %q", state)
			}
			raw, err := json.Marshal(exchange)
			if err != nil {
				t.Fatal(err)
			}
			var legacy struct {
				State string `json:"state"`
			}
			if err := json.Unmarshal(raw, &legacy); err != nil || legacy.State != state {
				t.Fatalf("legacy exchange state changed: %s, %v", raw, err)
			}
		})
	}
}
