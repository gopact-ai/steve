package provider

import (
	"context"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
)

// Discovery includes enrolled producers/consumers without a registration.
// Their health is unknown, not offline. Filter before pagination so client
// grants cannot hide providers or make an empty page imply no connections.
func (s *Service) Registrations(ctx context.Context, after string) (page durable.Page[Registration], err error) {
	page.Items = []Registration{}
	err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		rows, err := tx.Query(`SELECT id,data FROM bindings WHERE kind=? AND id>? AND (CASE WHEN json_valid(data) THEN json_extract(data,'$.kind') END)='provider' ORDER BY id LIMIT 101`, identity.GrantKind, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		last := ""
		for rows.Next() {
			var id string
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				return err
			}
			if len(page.Items) == 100 {
				page.Next = last
				break
			}
			grant, err := durable.Decode[identity.Grant](tx, raw, identity.GrantKind, id)
			if err != nil {
				return err
			}
			item, _, err := durable.Get[Registration](tx, RegistrationKind, id)
			if err != nil {
				return err
			}
			health, err := HealthTx(tx, id)
			if err != nil {
				return err
			}
			item.ID, item.Principal, item.Name, item.State = grant.ID, grant.Principal, grant.Name, health.State
			page.Items = append(page.Items, item)
			last = id
		}
		return rows.Err()
	})
	return
}
