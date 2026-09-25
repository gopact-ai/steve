package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

func TestConfirmedAttemptDeliveryTxRequiresFullOriginalProof(t *testing.T) {
	for _, mutation := range []string{
		"", `DELETE FROM commands WHERE id='input/reply'`,
		`UPDATE commands SET finished_at=NULL WHERE id='input/reply'`,
		`UPDATE commands SET error='delivery failed' WHERE id='input/reply'`,
		`UPDATE commands SET result='{"command_id":"another-input","receipt":"confirmed"}' WHERE id='input/reply'`,
		`UPDATE commands SET result='{"command_id":"input","receipt":""}' WHERE id='input/reply'`,
		`UPDATE commands SET actor='other' WHERE id='input/reply'`,
		`UPDATE commands SET kind='another-owner-reply' WHERE id='input/reply'`,
		`UPDATE commands SET result='{}' WHERE id='input/attempt'`,
		`UPDATE commands SET result='{}' WHERE id='input'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			_, book := attemptOwnerBook(t)
			raw, err := json.Marshal(ledger.CommandProof{CommandID: "input", Receipt: "confirmed"})
			if err != nil {
				t.Fatal(err)
			}
			if err := book.RecordCommand(t.Context(), "input/reply", "gateway-input-reply", "owner", raw); err != nil {
				t.Fatal(err)
			}
			if mutation != "" {
				if _, err := book.DB().Exec(mutation); err != nil {
					t.Fatal(err)
				}
			}
			var receipt string
			var confirmed bool
			err = book.Read(t.Context(), func(tx *ledger.ReadTx) error {
				var err error
				receipt, confirmed, err = ConfirmedAttemptDeliveryTx(tx, "original-attempt", "original-task", "conversation", "input-message", "owner")
				return err
			})
			if mutation == "" {
				if err != nil || !confirmed || receipt != "confirmed" {
					t.Fatalf("exact proof failed: %q %t %v", receipt, confirmed, err)
				}
			} else if confirmed || receipt != "" {
				t.Fatalf("incomplete/unrelated proof confirmed delivery: %q %t %v", receipt, confirmed, err)
			}
		})
	}
}

type suppressedReceiptProbe struct{ turntest.IdleCoordinator }

func (suppressedReceiptProbe) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	req.OnTurnReady("original-task", "original-attempt")
	return turn.Result{Attempt: "original-attempt"}, nil
}

func TestConfirmedDeliveryReadsActualIngressAndRecoveryOwners(t *testing.T) {
	for _, mode := range []string{"ordinary", "topic", "recovery", "suppressed"} {
		t.Run(mode, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			p := &durableInputProbe{}
			g := New(p)
			g.BindChannel(&ingressTopicChannel{})
			g.SetRecoveryLedger(book)
			conversation, anchor, taskID, attemptID := "conversation", "input-message", "original-task", "original-attempt"
			key := "gateway-input/input-message"
			switch mode {
			case "recovery":
				g = New(&recoveryProbe{})
				g.BindChannel(&recoveryChannel{})
				key, anchor, taskID, attemptID = "recovery-input", "notice-receipt", "parent", "new-attempt"
				if err := g.QueueRecovery(t.Context(), book, key, revivalFixture(), ""); err != nil {
					t.Fatal(err)
				}
				if err := g.recoverQueuedFixture(t.Context(), book, &recoveryProbe{}, func(string, string) error { return nil }); err != nil {
					t.Fatal(err)
				}
			default:
				msg := inboundFixture()
				var driver RecoveryDriver = p
				if mode == "topic" {
					msg.ConversationID, msg.Text = msg.ChatID, "/t original"
					conversation, anchor = "topic-thread", "topic-anchor"
				}
				if mode == "suppressed" {
					g = New(suppressedReceiptProbe{})
					g.BindChannel(&recoveryChannel{})
					g.SetRecoveryLedger(book)
					msg.ChatType, msg.Mentioned = protocol.ChatGroup, false
					driver = nil
				}
				var workers recoveryTestWorkers
				g.SetIngressLifetime(t.Context(), &workers, driver)
				if err := g.HandleMessage(msg); err != nil {
					t.Fatal(err)
				}
				workers.Wait()
			}
			read := func(want bool) {
				t.Helper()
				err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
					id, confirmed, err := ConfirmedAttemptDeliveryTx(tx, attemptID, taskID, conversation, anchor, "owner")
					if confirmed != want || want && (err != nil || id == "") || !want && id != "" {
						t.Fatalf("owner %s delivery: %q %t %v", mode, id, confirmed, err)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			read(mode != "suppressed")
			// A real route/notice is necessary even with an exact /reply row.
			if mode == "topic" || mode == "recovery" {
				suffix := "/topic"
				if mode == "recovery" {
					suffix = "/notice"
				}
				if _, err := book.DB().Exec(`UPDATE commands SET finished_at=NULL WHERE id=?`, key+suffix); err != nil {
					t.Fatal(err)
				}
				read(false)
			}
		})
	}
}
