package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

const GrantKind = "service-identity-grant"
const credentialKind = "service-identity-credential"

var ErrDenied = errors.New("identity: authority is absent or revoked")

// Grant is an explicit delegation from the server owner to one connection.
// Provider/client IDs never share the native project-grant namespace.
type Grant struct {
	Key              string   `json:"idempotency_key"`
	EnrollmentDigest string   `json:"enrollment_digest"`
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	Principal        string   `json:"principal"`
	Enabled          bool     `json:"enabled"`
	EventTypes       []string `json:"event_types,omitempty"`
	// Work binds a conversation to its creation-time project. Empty authority
	// grants no intelligence; strings matching native actor IDs grant nothing.
	Work         map[string]string `json:"work,omitempty"`
	Capabilities []CapabilityGrant `json:"capabilities,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
}

type CapabilityGrant struct {
	Client       string `json:"client"`
	Capability   string `json:"capability"`
	Conversation string `json:"conversation"`
	Approval     bool   `json:"approval"`
}

type Enrollment struct {
	Grant Grant  `json:"grant"`
	Token string `json:"token"`
}

// Principal is constructed by authentication, never decoded from a request.
type Principal struct {
	GrantID string
	Owner   bool
}

type Service struct {
	Book    *ledger.Ledger
	OwnerID string
}

func RandomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func ValidName(s string) bool {
	if len(s) < 1 || len(s) > 128 || s != strings.TrimSpace(s) {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}

func (s *Service) Enroll(ctx context.Context, requested Grant) (Enrollment, error) {
	requested.Capabilities = append([]CapabilityGrant(nil), requested.Capabilities...)
	if len(requested.Capabilities) > 32 || len(requested.EventTypes) > 64 || len(requested.Work) > 32 {
		return Enrollment{}, errors.New("connection scope exceeds its limit")
	}
	if s.OwnerID == "" {
		return Enrollment{}, ErrDenied
	}
	if requested.Key == "" || len(requested.Key) > 512 {
		return Enrollment{}, errors.New("connection enrollment requires a stable idempotency_key")
	}
	if strings.TrimSpace(requested.Name) == "" || len(requested.Name) > 128 || requested.Kind != "provider" && requested.Kind != "client" {
		return Enrollment{}, errors.New("identity: name and provider/client kind are required")
	}
	if requested.Kind == "client" && len(requested.EventTypes) > 0 {
		return Enrollment{}, errors.New("clients cannot publish provider events")
	}
	for _, eventType := range requested.EventTypes {
		if !ValidName(strings.TrimSuffix(eventType, ".*")) {
			return Enrollment{}, errors.New("invalid event type scope")
		}
	}
	for conversation, project := range requested.Work {
		if (conversation == "" || len(conversation) > 512) || !ValidName(project) {
			return Enrollment{}, errors.New("work scope requires an explicit Console conversation and project")
		}
	}
	for _, c := range requested.Capabilities {
		if !(ValidName(c.Client) || requested.Kind == "client" && c.Client == "") || !ValidName(c.Capability) || (c.Conversation == "" || len(c.Conversation) > 512) {
			return Enrollment{}, errors.New("invalid client capability grant")
		}
	}
	raw, err := json.Marshal(struct {
		Name, Kind   string
		Events       []string
		Work         map[string]string
		Capabilities []CapabilityGrant
	}{requested.Name, requested.Kind, requested.EventTypes, requested.Work, requested.Capabilities})
	if err != nil {
		return Enrollment{}, err
	}
	requested.EnrollmentDigest = durable.Key(string(raw))
	requested.ID = durable.Key(s.OwnerID, requested.Key)
	requested.Principal = "connection:" + requested.ID
	if requested.Kind == "client" {
		for i := range requested.Capabilities {
			if requested.Capabilities[i].Client == "" {
				requested.Capabilities[i].Client = requested.ID
			} else {
				return Enrollment{}, errors.New("a client grant can only expose its own capabilities")
			}
		}
	}
	requested.Enabled = true
	requested.CreatedAt = time.Now().UTC()
	token := "svc_" + RandomID()
	replay := false
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		old, found, err := durable.Get[Grant](tx, GrantKind, requested.ID)
		if err != nil {
			return err
		}
		if found {
			if old.EnrollmentDigest != requested.EnrollmentDigest {
				return ledger.ErrConflict
			}
			requested = old
			replay = true
			return nil
		}
		if err := tx.PutBinding(GrantKind, requested.ID, requested); err != nil {
			return err
		}
		return tx.PutBinding(credentialKind, durable.Key(token), requested.ID)
	})
	if replay {
		token = ""
	}
	return Enrollment{Grant: requested, Token: token}, err
}

func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	var p Principal
	err := s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		id, found, err := durable.Get[string](tx, credentialKind, durable.Key(token))
		if err != nil {
			return err
		}
		if !found || token == "" {
			return ErrDenied
		}
		g, found, err := durable.Get[Grant](tx, GrantKind, id)
		if err != nil {
			return err
		}
		if !found || !g.Enabled {
			return ErrDenied
		}
		p.GrantID = id
		return nil
	})
	return p, err
}

// Check rereads live authority inside the same transaction as a protected write.
func (s *Service) Check(r ledger.Reader, p Principal) (Grant, error) {
	if p.Owner && p.GrantID == "" && s.OwnerID != "" {
		return Grant{ID: s.OwnerID, Principal: s.OwnerID, Kind: "owner", Enabled: true}, nil
	}
	if p.Owner || p.GrantID == "" {
		return Grant{}, ErrDenied
	}
	g, found, err := durable.Get[Grant](r, GrantKind, p.GrantID)
	if err != nil {
		return Grant{}, err
	}
	if !found || !g.Enabled {
		return Grant{}, ErrDenied
	}
	return g, nil
}

func (s *Service) Revoke(ctx context.Context, id string) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		g, found, err := durable.Get[Grant](tx, GrantKind, id)
		if err != nil {
			return err
		}
		if !found {
			return ErrDenied
		}
		g.Enabled = false
		return tx.PutBinding(GrantKind, id, g)
	})
}

func Matches(pattern, name string) bool {
	return pattern == name || strings.HasSuffix(pattern, ".*") && strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
}

func (g Grant) AllowsEvent(eventType string) bool {
	if g.Kind != "provider" {
		return false
	}
	for _, pattern := range g.EventTypes {
		if Matches(pattern, eventType) {
			return true
		}
	}
	return false
}

func (g Grant) AllowsWork(conversation, project string) bool {
	return g.Kind == "owner" || project != "" && g.Work[conversation] == project
}
