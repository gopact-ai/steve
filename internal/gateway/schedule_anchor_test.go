package gateway

import (
	"testing"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestScheduleControlsPreserveChannelAnchor(t *testing.T) {
	for _, durable := range []bool{false, true} {
		for _, input := range []string{"/schedules", "@builder /every 30m inspect", "/at 1h report"} {
			g := New(fakeProcessor{})
			g.BindChannel(&reply{text: make(chan string, 4)})
			gate := &recordingGate{calls: make(chan string, 4)}
			g.SetAgentGate(gate)
			msg := feishu.InboundMessage{ConversationID: "chat", ChatID: "chat", MessageID: "control", Text: input, SenderOpenID: "owner"}
			if durable {
				book, err := ledger.Open(t.TempDir(), ledger.Options{})
				if err != nil {
					t.Fatal(err)
				}
				_, ui, err := g.dispatchInput(t.Context(), book, "schedule-control", gatewayInput{Message: msg}, msg, "", nil)
				if ui != nil {
					ui.closeProgress()
				}
				book.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else if err := g.process(msg); err != nil {
				t.Fatal(err)
			}
			select {
			case changed := <-gate.calls:
				t.Fatalf("durable=%v %s replaced active anchor: %s", durable, input, changed)
			default:
			}
		}
	}
}
