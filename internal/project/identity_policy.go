package project

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
)

const GrantMigrationKind = "identity-project-grant-migration"

type GrantMigration struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	Original  Grant            `json:"original"`
	Account   identity.Account `json:"account"`
	Principal string           `json:"principal"`
	At        time.Time        `json:"at"`
}

// SetIdentityPolicy is called before admission starts. Legacy grants came from
// the single native adapter configured in the old server. The caller supplies
// that namespace explicitly; runtime account equality is never migration proof.
func (s *Store) SetIdentityPolicy(ctx context.Context, accounts *identity.Accounts, legacyChannel string) error {
	if accounts == nil || !identity.ValidName(legacyChannel) {
		return errors.New("project identity policy needs accounts and a legacy channel")
	}
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		for _, kind := range []string{kindGrant, kindConfigGrant} {
			rows, err := tx.Bindings(kind)
			if err != nil {
				return err
			}
			for id, raw := range rows {
				var original Grant
				if err := json.Unmarshal(raw, &original); err != nil {
					return err
				}
				principal, account, err := canonicalPrincipal(tx, original.Principal, legacyChannel)
				if err != nil {
					return err
				}
				if principal == original.Principal {
					continue
				}
				migrated := original
				migrated.Principal = principal
				key := migrated.Project + "/" + principal
				if other, found, err := durable.Get[Grant](tx, kind, key); err != nil {
					return err
				} else if found && other.Role != migrated.Role {
					// Conflicting old aliases do not become a stronger unified grant.
					migrated.Role = RoleNone
				}
				if err := tx.PutBinding(kind, key, migrated); err != nil {
					return err
				}
				if err := durable.Delete(tx, kind, id); err != nil {
					return err
				}
				migration := GrantMigration{ID: durable.Key(kind, id, string(raw)), Kind: kind, Original: original, Account: account, Principal: principal, At: time.Now().UTC()}
				if err := tx.PutBinding(GrantMigrationKind, migration.ID, migration); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		s.identityAccounts, s.legacyGrantChannel = accounts, legacyChannel
	}
	return err
}

func canonicalPrincipal(r ledger.Reader, value, channel string) (string, identity.Account, error) {
	if strings.HasPrefix(value, "user:") {
		if !identity.ValidPrincipal(value) {
			return "", identity.Account{}, identity.ErrDenied
		}
		return value, identity.Account{}, nil
	}
	if strings.HasPrefix(value, "connection:") {
		return "", identity.Account{}, identity.ErrDenied
	}
	if rest, explicit := strings.CutPrefix(value, "account:"); explicit {
		var ok bool
		channel, value, ok = strings.Cut(rest, ":")
		if !ok {
			return "", identity.Account{}, identity.ErrDenied
		}
	}
	account := identity.Account{Channel: channel, ID: value}
	binding, err := identity.LookupAccount(r, account)
	return binding.Principal, account, err
}

func (s *Store) Principal(ctx context.Context, value, channel string) (out string, err error) {
	if s.identityAccounts == nil {
		return value, nil
	}
	if channel == "" {
		channel = s.legacyGrantChannel
	}
	err = s.l.Read(ctx, func(tx *ledger.ReadTx) error { out, _, err = canonicalPrincipal(tx, value, channel); return err })
	return
}

func (s *Store) GrantMigrations(ctx context.Context, after string) (durable.Page[GrantMigration], error) {
	return durable.PageOf[GrantMigration](ctx, s.l, GrantMigrationKind, after)
}

// AccessPolicyTx is the policy decision used by fixed-execution tool grants.
// Placement and write leases still prove project ownership separately.
func AccessPolicyTx(r ledger.Reader, id, principal, owner string) (Role, error) {
	p, found, err := durable.Get[Project](r, kindProject, id)
	if err != nil {
		return RoleNone, err
	}
	if !found {
		return RoleNone, ErrUnknown
	}
	if principal != "" && owner != "" && principal == owner {
		return RoleAdmin, nil
	}
	for _, kind := range []string{kindConfigGrant, kindGrant} {
		grant, found, err := durable.Get[Grant](r, kind, id+"/"+principal)
		if err != nil {
			return RoleNone, err
		}
		if found {
			return grant.Role, nil
		}
	}
	if p.DefaultRole != "" {
		return p.DefaultRole, nil
	}
	if p.Level == "sealed" || p.Level == "restricted" {
		return RoleNone, nil
	}
	return RoleWrite, nil
}
