package console

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

// FreezeProjectDocument is the explicit file/test adapter. Stable receipts
// keep retries from resurrecting dispatchable work.
func FreezeProjectDocument(doc ledger.Doc, project string) error {
	saved, err := loadTranscript(doc)
	if err != nil {
		return err
	}
	freezeConsole(&saved, project)
	raw, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("freeze project console: %w", err)
	}
	return doc.Save(raw)
}

func freezeConsole(saved *transcript, project string) {
	now := time.Now().UTC()
	for conversation, list := range saved.Exchanges {
		for _, e := range list {
			if e.ExpectedProject != project || e.State.Terminal() {
				continue
			}
			reply := consoleapi.Reply{ID: "moved-" + e.ID, ExchangeID: e.ID, Conversation: conversation, ProjectID: project, At: now, Kind: "reply", Text: "project migrated from this hub", Error: "project migrated from this hub"}
			e.State = consoleapi.ExchangeFailed
			e.ReplyID = reply.ID
			if e.Key != "" {
				e.Receipt = &reply
			}
			saved.Replies[conversation] = append(saved.Replies[conversation], reply)
		}
	}
	for id, q := range saved.Questions {
		if q.Project == project && q.State == "pending" {
			q.State = "interrupted"
			q.UpdatedAt = now
			saved.Questions[id] = q
		}
	}
}

// FreezeProjectTx seals source dispatch and receipt evidence in the same
// transaction releasing task records and project ownership.
func FreezeProjectTx(tx *ledger.Tx, exported ProjectTransfer) error {
	before, err := loadConsoleRecordsTx(tx)
	if err != nil {
		return err
	}
	state, err := before.state()
	if err != nil {
		return err
	}
	next := state.transcript()
	current, err := exportProject(next, exported.Project, exported.Conversations)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, exported) {
		return fmt.Errorf("%w: console changed during export", ledger.ErrConflict)
	}
	freezeConsole(&next, exported.Project)
	changes, err := consoleChanges(before, next)
	if err != nil {
		return err
	}
	return writeConsoleChangesTx(tx, before.revision, changes)
}
