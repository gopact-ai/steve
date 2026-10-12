package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

const ConnectionAuditKind = "service-identity-audit"
const credentialRotationKind = "service-identity-rotation"

type credential struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}

// Credentials issued before lifecycle support stored a grant ID string.
// Their generation is zero; normal authentication remains valid until rotation.
func readCredential(r ledger.Reader, digest string) (out credential, found bool, err error) {
	raw, found, err := durable.Get[json.RawMessage](r, credentialKind, digest)
	if err != nil || !found {
		return out, found, err
	}
	if len(raw) > 0 && raw[0] == '"' {
		err = json.Unmarshal(raw, &out.ID)
	} else {
		err = json.Unmarshal(raw, &out)
	}
	return out, found, err
}

type ScopeUpdate struct {
	Revision     uint64            `json:"revision"`
	Name         string            `json:"name"`
	EventTypes   []string          `json:"event_types,omitempty"`
	Work         map[string]string `json:"work,omitempty"`
	Capabilities []CapabilityGrant `json:"capabilities,omitempty"`
}

type RotateRequest struct {
	Revision uint64 `json:"revision"`
	Key      string `json:"idempotency_key"`
}

type ConnectionChange struct {
	ID        string    `json:"id"`
	Operation string    `json:"operation"`
	Before    Grant     `json:"before"`
	After     Grant     `json:"after"`
	At        time.Time `json:"at"`
}

func auditConnection(tx *ledger.Tx, operation string, before, after Grant) error {
	change := ConnectionChange{
		ID:        durable.Key(after.ID, operation, fmt.Sprint(after.ScopeRevision), fmt.Sprint(after.CredentialRevision)),
		Operation: operation, Before: before, After: after, At: time.Now().UTC(),
	}
	return tx.PutBinding(ConnectionAuditKind, change.ID, change)
}

func (s *Service) Audit(ctx context.Context, after string) (durable.Page[ConnectionChange], error) {
	return durable.PageOf[ConnectionChange](ctx, s.Book, ConnectionAuditKind, after)
}

func validateUpdatedScope(g Grant) error {
	if strings.TrimSpace(g.Name) == "" || len(g.Name) > 128 || len(g.EventTypes) > 64 || len(g.Work) > 32 || len(g.Capabilities) > 32 {
		return errors.New("connection requires a name and bounded scopes")
	}
	if g.Kind == "client" && len(g.EventTypes) != 0 {
		return errors.New("clients cannot publish provider events")
	}
	for _, eventType := range g.EventTypes {
		if !ValidName(strings.TrimSuffix(eventType, ".*")) {
			return errors.New("invalid event type scope")
		}
	}
	for conversation, project := range g.Work {
		if conversation == "" || len(conversation) > 512 || !ValidName(project) {
			return errors.New("work scope requires an explicit conversation and project")
		}
	}
	seenCapabilities := map[string]bool{}
	for _, c := range g.Capabilities {
		if !ValidName(c.Client) || !ValidName(c.Capability) || c.Conversation == "" || len(c.Conversation) > 512 || g.Kind == "client" && c.Client != g.ID {
			return errors.New("invalid client capability grant")
		}
		key := durable.Key(c.Client, c.Capability, c.Conversation)
		if seenCapabilities[key] {
			return errors.New("duplicate client capability grant")
		}
		seenCapabilities[key] = true
	}
	return nil
}

// UpdateScope preserves the enrollment and source identity. A changed scope is
// not a reinstallation, and credential rotation does not expand that scope.
func (s *Service) UpdateScope(ctx context.Context, id string, input ScopeUpdate) (out Grant, err error) {
	if s.OwnerID == "" {
		return out, ErrDenied
	}
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		before, found, err := durable.Get[Grant](tx, GrantKind, id)
		if err != nil {
			return err
		}
		if !found || !before.Enabled {
			return ErrDenied
		}
		out = before
		out.Name, out.EventTypes, out.Work = input.Name, append([]string(nil), input.EventTypes...), input.Work
		out.Capabilities = append([]CapabilityGrant(nil), input.Capabilities...)
		if out.Kind == "client" {
			for i := range out.Capabilities {
				if out.Capabilities[i].Client == "" {
					out.Capabilities[i].Client = out.ID
				}
			}
		}
		if err := validateUpdatedScope(out); err != nil {
			return err
		}
		if before.ScopeRevision == input.Revision+1 && reflect.DeepEqual(before, out) {
			out = before
			return nil
		}
		if before.ScopeRevision != input.Revision {
			return ledger.ErrConflict
		}
		out.ScopeRevision++
		if err := tx.PutBinding(GrantKind, id, out); err != nil {
			return err
		}
		return auditConnection(tx, "scope_updated", before, out)
	})
	return
}

type rotationReceipt struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}

// RotateCredential returns a secret once. A lost response can be recovered by
// observing the new revision and rotating again with a new key, without changing
// the principal, subscriptions, capabilities or accepted effect identities.
func (s *Service) RotateCredential(ctx context.Context, id string, input RotateRequest) (out Enrollment, err error) {
	if s.OwnerID == "" {
		return out, ErrDenied
	}
	if input.Key == "" || len(input.Key) > 512 {
		return out, errors.New("credential rotation requires a stable idempotency_key")
	}
	token := "svc_" + RandomID()
	replay := false
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		before, found, err := durable.Get[Grant](tx, GrantKind, id)
		if err != nil {
			return err
		}
		if !found || !before.Enabled {
			return ErrDenied
		}
		receiptID := durable.Key(id, input.Key)
		old, found, err := durable.Get[rotationReceipt](tx, credentialRotationKind, receiptID)
		if err != nil {
			return err
		}
		if found {
			if old.ID != id || old.Revision != input.Revision {
				return ledger.ErrConflict
			}
			out.Grant = before
			replay = true
			return nil
		}
		if before.CredentialRevision != input.Revision {
			return ledger.ErrConflict
		}
		out.Grant = before
		out.Grant.CredentialRevision++
		// Both old string credentials and generation-bearing credentials are
		// removed. Audits contain revisions and scopes, never bearer secrets.
		if _, err := tx.Exec(`DELETE FROM bindings WHERE kind=? AND CASE json_type(data) WHEN 'text' THEN json_extract(data,'$') ELSE json_extract(data,'$.id') END=?`, credentialKind, id); err != nil {
			return err
		}
		if err := tx.PutBinding(credentialKind, durable.Key(token), credential{ID: id, Revision: out.Grant.CredentialRevision}); err != nil {
			return err
		}
		if err := tx.PutBinding(GrantKind, id, out.Grant); err != nil {
			return err
		}
		if err := tx.PutBinding(credentialRotationKind, receiptID, rotationReceipt{ID: id, Revision: input.Revision}); err != nil {
			return err
		}
		return auditConnection(tx, "credential_rotated", before, out.Grant)
	})
	if err == nil && !replay {
		out.Token = token
	}
	return
}
