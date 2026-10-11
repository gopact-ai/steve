package gateway

import (
	"context"
	"encoding/json"
	"github.com/gopact-ai/steve/internal/conversation"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
)

// RestoreRoutes upgrades authenticated original admissions, including known
// topic and automation receipts. It performs no remote calls or dispatches.
func (g *Gateway) RestoreRoutes(ctx context.Context, book *ledger.Ledger) error {
	after := ""
	for {
		rows, err := book.DB().QueryContext(ctx, `SELECT id FROM commands WHERE kind IN ('gateway-input','gateway-recovery-input') AND finished_at IS NOT NULL AND error='' AND id>? ORDER BY id LIMIT 100`, after)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			r, _, err := book.CommandReceipt(ctx, id)
			if err != nil {
				return err
			}
			if r.Kind == gatewayInputKind {
				input, err := decodeGatewayInput(r)
				if err != nil || inputChannel(input) != g.name {
					continue
				}
				err = book.Update(ctx, func(tx *ledger.Tx) error {
					msg := input.Message
					if err := conversation.BindRouteTx(tx, conversationID(msg), g.name); err != nil {
						return err
					}
					if err := conversation.BindMessageTx(tx, g.name, msg.MessageID, conversationID(msg)); err != nil {
						return err
					}
					if msg.ChatType == protocol.ChatP2P && msg.Origin == "" && g.authority.IsOwnerTx(tx, identity.Account{Channel: g.name, ID: msg.ActorID}) {
						key := durable.Key(g.name, conversationID(msg))
						_, found, err := durable.Get[nativeWorkAnchor](tx, nativeAnchorKind, key)
						if err != nil {
							return err
						}
						if !found {
							if err := tx.PutBinding(nativeAnchorKind, key, nativeWorkAnchor{Message: msg}); err != nil {
								return err
							}
						}
					}
					if _, topic := topicTask(msg); topic {
						route, found, err := ledger.CommandReceiptTx(tx, id+"/topic")
						if err != nil {
							return err
						}
						if found {
							routed, err := topicMessage(input, route, id)
							if err == nil {
								if err := conversation.BindRouteTx(tx, conversationID(routed), g.name); err != nil {
									return err
								}
								if err := conversation.BindMessageTx(tx, g.name, routed.MessageID, conversationID(routed)); err != nil {
									return err
								}
							}
						}
					}
					return nil
				})
				if err != nil {
					return err
				}
			} else {
				var input recoveryInput
				if json.Unmarshal(r.Result, &input) != nil {
					continue
				}
				name := input.Transport
				if name == "" {
					name = "feishu"
				}
				if name != g.name || input.ConversationID == "" || input.MessageID == "" || input.Requester != r.Actor {
					continue
				}
				if err := book.Update(ctx, func(tx *ledger.Tx) error {
					if err := conversation.BindRouteTx(tx, input.ConversationID, g.name); err != nil {
						return err
					}
					return conversation.BindMessageTx(tx, g.name, input.MessageID, input.ConversationID)
				}); err != nil {
					return err
				}
			}
		}
		after = ids[len(ids)-1]
	}
}
