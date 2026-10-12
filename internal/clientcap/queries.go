package clientcap

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"time"
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
		// Filter authority and availability before the discovery limit. Retained
		// offline registrations must not hide newer live authorized clients.
		query := `SELECT c.data FROM bindings AS c JOIN bindings AS g ON g.kind=? AND g.id=c.id WHERE c.kind=? AND json_extract(g.data,'$.enabled')=1 AND json_extract(g.data,'$.kind')='client' AND julianday(json_extract(c.data,'$.until'))>julianday(?) AND EXISTS(SELECT 1 FROM json_each(g.data,'$.capabilities') AS scope JOIN json_each(c.data,'$.capabilities') AS advertised ON json_extract(advertised.value,'$.name')=json_extract(scope.value,'$.capability') WHERE json_extract(scope.value,'$.conversation')=? AND json_extract(scope.value,'$.client')=c.id`
		args := []any{identity.GrantKind, ClientKind, time.Now().UTC().Format(time.RFC3339Nano), conversation}
		if !p.Owner {
			query += ` AND EXISTS(SELECT 1 FROM json_each(?,'$.capabilities') AS caller WHERE json_extract(caller.value,'$.conversation')=? AND json_extract(caller.value,'$.client')=c.id AND json_extract(caller.value,'$.capability')=json_extract(advertised.value,'$.name'))`
			raw, _ := json.Marshal(grant)
			args = append(args, string(raw), conversation)
		}
		query += `) ORDER BY c.id LIMIT 200`
		rows, err := tx.Query(query, args...)
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
