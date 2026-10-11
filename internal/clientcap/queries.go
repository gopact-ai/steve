package clientcap

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
)

func (b *Broker) Invoke(ctx context.Context, p identity.Principal, input Request) (Invocation, error) {
	return b.Submit(ctx, p, input, nil, nil)
}

func (b *Broker) Status(ctx context.Context, p identity.Principal, id string) (out Invocation, err error) {
	err = b.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		g, err := b.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		var found bool
		out, found, err = durable.Get[Invocation](tx, CallKind, id)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("client invocation not found")
		}
		if !p.Owner && out.GrantID != g.ID {
			return identity.ErrDenied
		}
		return nil
	})
	out.Token = ""
	return
}

// Available returns only currently advertised capabilities with explicit grants
// for this conversation. Registration can never broaden these permissions.
func (b *Broker) Available(ctx context.Context, p identity.Principal, conversation string) (out []Registration, err error) {
	out = []Registration{}
	err = b.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		grant, err := b.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT data FROM bindings WHERE kind=? ORDER BY id LIMIT 200`, ClientKind)
		if err != nil {
			return err
		}
		defer rows.Close()
		// Collect before further queries so no transport owns a second read snapshot.
		var clients []Registration
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var c Registration
			if err := json.Unmarshal(raw, &c); err != nil {
				return err
			}
			clients = append(clients, c)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, client := range clients {
			filtered := client
			filtered.Capabilities = []Capability{}
			for _, cap := range client.Capabilities {
				_, _, err := b.capability(tx, Request{Client: client.ID, Capability: cap.Name, Conversation: conversation})
				if err != nil {
					continue
				}
				allowed := p.Owner
				for _, scope := range grant.Capabilities {
					if scope.Client == client.ID && scope.Capability == cap.Name && scope.Conversation == conversation {
						allowed = true
					}
				}
				if allowed {
					filtered.Capabilities = append(filtered.Capabilities, cap)
				}
			}
			if len(filtered.Capabilities) > 0 {
				out = append(out, filtered)
			}
		}
		return nil
	})
	return
}
