package identity

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

const AccountKind = "identity-account"
const AccountAuditKind = "identity-account-audit"

// Account is authenticated by an adapter. Its ID never identifies an account
// on another adapter, even when the native strings happen to be equal.
type Account struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
}

type AccountBinding struct {
	Account    Account   `json:"account"`
	Principal  string    `json:"principal"`
	Revision   uint64    `json:"revision"`
	Enabled    bool      `json:"enabled"`
	Configured bool      `json:"configured,omitempty"`
	At         time.Time `json:"at"`
}

type AccountChange struct {
	ID     string         `json:"id"`
	Before AccountBinding `json:"before"`
	After  AccountBinding `json:"after"`
}

// Accounts is the durable namespace shared by native adapters and policy.
// Linking changes the account's principal, not historical tasks or attempts.
type Accounts struct {
	Book  *ledger.Ledger
	Owner string
}

func ValidChannel(name string) bool { return ValidName(name) && !strings.Contains(name, ":") }

func (a Account) Valid() bool {
	return ValidChannel(a.Channel) && a.ID != "" && len(a.ID) <= 512 && strings.TrimSpace(a.ID) == a.ID
}
func (a Account) Key() string       { return durable.Key(a.Channel, a.ID) }
func (a Account) Principal() string { return "user:" + a.Key() }

func ValidPrincipal(principal string) bool {
	if !strings.HasPrefix(principal, "user:") || len(principal) != 69 {
		return false
	}
	_, err := hex.DecodeString(principal[5:])
	return err == nil && strings.ToLower(principal) == principal
}

// LookupAccount resolves the administrative subject even when disabled.
// Disabled accounts still have grant metadata; they have no caller authority.
func LookupAccount(r ledger.Reader, a Account) (AccountBinding, error) {
	if !a.Valid() {
		return AccountBinding{}, ErrDenied
	}
	b, found, err := durable.Get[AccountBinding](r, AccountKind, a.Key())
	if err != nil {
		return b, err
	}
	if !found {
		return AccountBinding{Account: a, Principal: a.Principal(), Enabled: true}, nil
	}
	if b.Account != a || !ValidPrincipal(b.Principal) {
		return AccountBinding{}, ErrDenied
	}
	return b, nil
}

func ResolveAccount(r ledger.Reader, a Account) (AccountBinding, error) {
	b, err := LookupAccount(r, a)
	if err != nil {
		return b, err
	}
	if !b.Enabled {
		return AccountBinding{}, ErrDenied
	}
	return b, nil
}

func (s *Accounts) Resolve(ctx context.Context, a Account) (out AccountBinding, err error) {
	err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error { out, err = ResolveAccount(tx, a); return err })
	return
}

// Bootstrap links only explicitly configured owner accounts. Removed owner
// declarations return to their own namespace; they cannot retain owner rights.
func (s *Accounts) Bootstrap(ctx context.Context, config Configuration) error {
	s.Owner = ""
	if config.ConsoleOwner != "" {
		s.Owner = Account{Channel: "console", ID: config.ConsoleOwner}.Principal()
	}
	desired := map[string]Account{}
	add := func(a Account) {
		if a.ID != "" && s.Owner != "" {
			desired[a.Key()] = a
		}
	}
	add(Account{Channel: "console", ID: config.ConsoleOwner})
	for ch, id := range config.NativeOwners {
		add(Account{Channel: ch, ID: id})
	}
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		if err := tx.PutBinding(AuthorityKind, "owner", s.Owner); err != nil {
			return err
		}
		old, err := tx.Bindings(AccountKind)
		if err != nil {
			return err
		}
		for key := range old {
			b, _, err := durable.Get[AccountBinding](tx, AccountKind, key)
			if err != nil {
				return err
			}
			if _, keep := desired[key]; !keep && b.Configured {
				before := b
				b.Principal, b.Configured, b.Enabled = b.Account.Principal(), false, true
				if err := writeAccount(tx, before, b); err != nil {
					return err
				}
			}
		}
		for key, account := range desired {
			if !account.Valid() {
				return ErrDenied
			}
			before, found, err := durable.Get[AccountBinding](tx, AccountKind, key)
			if err != nil {
				return err
			}
			if found && before.Configured && before.Enabled && before.Principal == s.Owner {
				continue
			}
			after := before
			after.Account, after.Principal, after.Enabled, after.Configured = account, s.Owner, true, true
			if err := writeAccount(tx, before, after); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeAccount(tx *ledger.Tx, before, after AccountBinding) error {
	after.Revision = before.Revision + 1
	after.At = time.Now().UTC()
	if err := tx.PutBinding(AccountKind, after.Account.Key(), after); err != nil {
		return err
	}
	change := AccountChange{ID: durable.Key(after.Account.Key(), after.At.Format(time.RFC3339Nano)), Before: before, After: after}
	return tx.PutBinding(AccountAuditKind, change.ID, change)
}

// Bind is owner administration, with a revision fence. An empty principal
// unlinks the account. Account IDs and connection credentials cannot name users.
func (s *Accounts) Bind(ctx context.Context, account Account, principal string, revision uint64, enabled bool) (out AccountBinding, err error) {
	if !account.Valid() {
		return out, ErrDenied
	}
	if principal == "" {
		principal = account.Principal()
	}
	if !ValidPrincipal(principal) {
		return out, errors.New("account links require a user principal")
	}
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		before, found, err := durable.Get[AccountBinding](tx, AccountKind, account.Key())
		if err != nil {
			return err
		}
		if before.Revision != revision {
			return ledger.ErrConflict
		}
		if found && before.Configured {
			return errors.New("configured owner accounts are managed by configuration")
		}
		out = AccountBinding{Account: account, Principal: principal, Enabled: enabled}
		if err := writeAccount(tx, before, out); err != nil {
			return err
		}
		out, _, err = durable.Get[AccountBinding](tx, AccountKind, account.Key())
		return err
	})
	return
}

func (s *Accounts) Bindings(ctx context.Context, after string) (durable.Page[AccountBinding], error) {
	return durable.PageOf[AccountBinding](ctx, s.Book, AccountKind, after)
}

func (o Owners) Resolve(ctx context.Context, channel, id string) (principal, owner string, err error) {
	if channel == "" {
		channel = "console"
	}
	if o.config.Accounts == nil {
		owner, err = o.Owner(channel)
		return id, owner, err
	}
	binding, err := o.config.Accounts.Resolve(ctx, Account{Channel: channel, ID: id})
	return binding.Principal, o.config.Accounts.Owner, err
}

func (o Owners) IsOwner(ctx context.Context, channel, id string) bool {
	p, owner, err := o.Resolve(ctx, channel, id)
	return err == nil && owner != "" && p == owner
}

func (o Owners) IsOwnerTx(r ledger.Reader, a Account) bool {
	if o.config.Accounts == nil {
		owner, err := o.Owner(a.Channel)
		return err == nil && owner != "" && owner == a.ID
	}
	binding, err := ResolveAccount(r, a)
	return err == nil && o.config.Accounts.Owner != "" && binding.Principal == o.config.Accounts.Owner
}
