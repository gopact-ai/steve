package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nativehistory"
)

// AbandonSource describes exactly the slot read inside the core decision's
// transaction. Only digests are persisted; credentials and context contents are
// never copied into an execution record.
type AbandonSource struct {
	State             string
	Fingerprint       string
	ImportFingerprint string
	Session           Session
}

func NativeImportFingerprint(ref *nativehistory.Reference) string {
	if ref == nil {
		return ""
	}
	raw, _ := json.Marshal(ref)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func sessionFingerprint(session Session) string {
	raw, _ := json.Marshal(session)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func AbandonSourceTx(tx *ledger.Tx, conversation, agent string) (AbandonSource, error) {
	raw, found, err := tx.LoadDocument("state")
	if err != nil {
		return AbandonSource{}, err
	}
	if !found {
		return AbandonSource{State: "absent"}, nil
	}
	var snapshot storedData
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return AbandonSource{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return AbandonSource{}, errors.New("session state is not one document")
	}
	session, present := snapshot.Conversations[conversation].Sessions[agent]
	if !present {
		return AbandonSource{State: "absent"}, nil
	}
	return AbandonSource{State: "present", Fingerprint: sessionFingerprint(session), ImportFingerprint: NativeImportFingerprint(session.NativeImport), Session: session}, nil
}

func importedContextKey(conversation, agent, node, harness, fingerprint string) string {
	return "import\x00" + conversation + "\x00" + agent + "\x00" + node + "\x00" + harness + "\x00" + fingerprint
}

func retiredContext(d data, session Session) bool {
	return session.UpstreamID != "" && d.RetiredContexts[nativeContextKey(session.NodeID, session.HarnessID, session.UpstreamID)] != "" || session.NativeImport != nil && d.RetiredContexts[importedContextKey(session.ConversationID, session.AgentID, session.NodeID, session.HarnessID, NativeImportFingerprint(session.NativeImport))] != ""
}

// CheckSessionContext also protects the open path before a late save would be
// refused. Retiring one imported snapshot does not prohibit a different import.
func (s *Store) CheckSessionContext(session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if retiredContext(s.data, session) {
		return ErrAbandonedContext
	}
	return nil
}
