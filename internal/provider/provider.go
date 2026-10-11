// Package provider owns discovery and health for externally managed event
// providers. Registration describes a connection; it never grants authority or
// starts plugin code, ACP sessions, or a second installation identity.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
)

const RegistrationKind = "event-provider-registration"
const ContractKind = "event-provider-contract"

var ErrContract = errors.New("provider event contract is absent or incompatible")

type Contract struct {
	Type        string          `json:"type"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Digest      string          `json:"digest"`
}

type Register struct {
	Generation uint64     `json:"generation"`
	PluginID   string     `json:"plugin_id"`
	Version    string     `json:"version"`
	Events     []Contract `json:"events"`
}

type Registration struct {
	ID         string     `json:"id"`
	Principal  string     `json:"principal"`
	Name       string     `json:"name"`
	Generation uint64     `json:"generation"`
	PluginID   string     `json:"plugin_id"`
	Version    string     `json:"version"`
	Events     []Contract `json:"events"`
	Until      time.Time  `json:"until"`
	State      string     `json:"state,omitempty"`
}

type Health struct {
	ID         string    `json:"id"`
	State      string    `json:"state"` // unregistered, online, offline, retired
	Generation uint64    `json:"generation"`
	Until      time.Time `json:"until,omitzero"`
}

type Service struct {
	Book     *ledger.Ledger
	Identity *identity.Service
}

// Withdrawn contract versions need only their immutable digest. Current
// declarations and accepted events retain the full schema for discovery and
// processing, so upgrades do not duplicate large schema histories here.
type contractReceipt struct {
	Digest string `json:"digest"`
}

func canonicalContracts(input []Contract) ([]Contract, error) {
	if len(input) > 64 {
		return nil, errors.New("a provider can declare at most 64 event contracts")
	}
	out := append([]Contract(nil), input...)
	seen := map[string]bool{}
	total := 0
	for i := range out {
		c := &out[i]
		total += len(c.Schema)
		if !identity.ValidName(c.Type) || !identity.ValidName(c.Version) || seen[c.Type] || len(c.Description) > 2048 || len(c.Schema) > 16<<10 || total > 256<<10 || !json.Valid(c.Schema) {
			return nil, errors.New("provider requires unique event types and bounded versioned contracts")
		}
		seen[c.Type] = true
		var schema map[string]any
		d := json.NewDecoder(bytes.NewReader(c.Schema))
		d.UseNumber()
		if d.Decode(&schema) != nil || schema == nil {
			return nil, errors.New("provider event schema must be a JSON object")
		}
		// Schemas are bounded discovery metadata, not executable validators.
		// Canonical JSON prevents formatting-only changes from altering a contract.
		canonical, err := json.Marshal(schema)
		if err != nil {
			return nil, err
		}
		c.Schema = canonical
		c.Digest = durable.Key(c.Type, c.Version, c.Description, string(canonical))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out, nil
}

func (s *Service) Register(ctx context.Context, p identity.Principal, input Register) (out Registration, err error) {
	if !identity.ValidName(input.PluginID) || !identity.ValidName(input.Version) {
		return out, errors.New("provider registration needs a plugin_id and version")
	}
	events, err := canonicalContracts(input.Events)
	if err != nil {
		return out, err
	}
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		g, err := s.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		if g.Kind != "provider" {
			return identity.ErrDenied
		}
		for _, event := range events {
			if !g.AllowsEvent(event.Type) {
				return identity.ErrDenied
			}
			// A type/version describes one immutable contract for this logical
			// provider, even if a later registration removes and reintroduces it.
			old, found, err := durable.Get[contractReceipt](tx, ContractKind, durable.Key(g.ID, event.Type, event.Version))
			if err != nil {
				return err
			}
			if found && old.Digest != event.Digest {
				return ledger.ErrConflict
			}
		}
		old, found, err := durable.Get[Registration](tx, RegistrationKind, g.ID)
		if err != nil {
			return err
		}
		if found && old.Generation == input.Generation+1 && old.PluginID == input.PluginID && old.Version == input.Version && reflect.DeepEqual(old.Events, events) {
			out = old
			return nil
		}
		if found && old.Generation != input.Generation || !found && input.Generation != 0 {
			return ledger.ErrConflict
		}
		out = Registration{ID: g.ID, Principal: g.Principal, Name: g.Name, Generation: input.Generation + 1, PluginID: input.PluginID, Version: input.Version, Events: events, Until: time.Now().UTC().Add(45 * time.Second)}
		for _, event := range events {
			if err := tx.PutBinding(ContractKind, durable.Key(g.ID, event.Type, event.Version), contractReceipt{Digest: event.Digest}); err != nil {
				return err
			}
		}
		return tx.PutBinding(RegistrationKind, g.ID, out)
	})
	return
}

func (s *Service) Heartbeat(ctx context.Context, p identity.Principal, generation uint64) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		g, err := s.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		if g.Kind != "provider" {
			return identity.ErrDenied
		}
		registration, found, err := durable.Get[Registration](tx, RegistrationKind, g.ID)
		if err != nil {
			return err
		}
		if !found || registration.Generation != generation {
			return ledger.ErrStale
		}
		registration.Until = time.Now().UTC().Add(45 * time.Second)
		return tx.PutBinding(RegistrationKind, g.ID, registration)
	})
}

func (s *Service) SelfRegistration(ctx context.Context, p identity.Principal) (out Registration, err error) {
	err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		g, err := s.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		if g.Kind != "provider" {
			return identity.ErrDenied
		}
		var found bool
		out, found, err = durable.Get[Registration](tx, RegistrationKind, g.ID)
		if err == nil && !found {
			return errors.New("provider is not registered")
		}
		if err == nil {
			health, e := HealthTx(tx, g.ID)
			out.State, err = health.State, e
		}
		return err
	})
	return
}

// Freeze returns the advertised contract at first acceptance. Unregistered
// legacy providers may keep their existing unversioned protocol. Callers must
// check immutable request replay before selecting the current declaration.
func Freeze(r ledger.Reader, grantID, eventType, requestedVersion string) (out Contract, declared bool, err error) {
	registration, found, err := durable.Get[Registration](r, RegistrationKind, grantID)
	if err != nil {
		return out, false, err
	}
	if !found {
		if requestedVersion != "" {
			return out, false, ErrContract
		}
		return out, false, nil
	}
	for _, event := range registration.Events {
		if event.Type == eventType && (requestedVersion == "" || requestedVersion == event.Version) {
			return event, true, nil
		}
	}
	return out, true, ErrContract
}

func HealthTx(r ledger.Reader, id string) (out Health, err error) {
	out.ID = id
	g, found, err := durable.Get[identity.Grant](r, identity.GrantKind, id)
	if err != nil {
		return out, err
	}
	if !found || !g.Enabled || g.Kind != "provider" {
		out.State = "retired"
		return out, nil
	}
	registration, found, err := durable.Get[Registration](r, RegistrationKind, id)
	if err != nil {
		return out, err
	}
	if !found {
		out.State = "unregistered"
		return out, nil
	}
	out.Generation, out.Until, out.State = registration.Generation, registration.Until, "online"
	if !registration.Until.After(time.Now()) {
		out.State = "offline"
	}
	return out, nil
}
