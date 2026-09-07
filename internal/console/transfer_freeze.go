package console

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

// FreezeProject seals only the source's dispatchable work after its original
// queue was exported. Stable receipts keep retries from resurrecting it.
func FreezeProject(doc ledger.Doc, project string) error {
	saved, err := loadTranscript(doc)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for conversation, list := range saved.Exchanges {
		for _, e := range list {
			if e.ExpectedProject != project || terminalExchange(e.State) {
				continue
			}
			reply := consoleapi.Reply{ID: "moved-" + e.ID, ExchangeID: e.ID, Conversation: conversation, ProjectID: project, At: now, Kind: "reply", Text: "project migrated from this hub", Error: "project migrated from this hub"}
			e.State = "failed"
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
	raw, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("freeze project console: %w", err)
	}
	return doc.Save(raw)
}
