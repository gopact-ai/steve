package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

type TransferExchange = DurableExchange

func transferExchange(e *queuedExchange) TransferExchange {
	out := durableExchange(e)
	out.Exchange = copyExchange(out.Exchange)
	return out
}

// ProjectTransfer is a durable domain snapshot, without process contexts or
// live callback waiters. Callers must stop the service before importing it.
type ProjectTransfer struct {
	Schema        int                                   `json:"schema"`
	Project       string                                `json:"project"`
	Conversations []string                              `json:"conversations"`
	Replies       map[string][]consoleapi.Reply         `json:"replies"`
	Meta          map[string]Meta                       `json:"meta"`
	Exchanges     map[string][]TransferExchange         `json:"exchanges"`
	Questions     map[string]consoleapi.PendingQuestion `json:"questions"`
}

func loadTranscript(doc ledger.Doc) (transcript, error) {
	out := transcript{Replies: map[string][]consoleapi.Reply{}, Meta: map[string]Meta{}, Exchanges: map[string][]*queuedExchange{}, Questions: map[string]consoleapi.PendingQuestion{}}
	raw, ok, err := doc.Load()
	if err != nil {
		return out, err
	}
	if !ok || len(raw) == 0 {
		return out, nil
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	if out.Replies == nil {
		return out, errors.New("console transcript has no attributable records")
	}
	if out.Meta == nil {
		out.Meta = map[string]Meta{}
	}
	if out.Exchanges == nil {
		out.Exchanges = map[string][]*queuedExchange{}
	}
	if out.Questions == nil {
		out.Questions = map[string]consoleapi.PendingQuestion{}
	}
	return out, nil
}

func ExportProject(book *ledger.Ledger, project string, conversations []string) (ProjectTransfer, error) {
	state, err := LoadState(book)
	if err != nil {
		return ProjectTransfer{}, err
	}
	return exportProject(state.transcript(), project, conversations)
}

// ExportProjectDocument is an explicit file/test adapter, not ledger authority.
func ExportProjectDocument(doc ledger.Doc, project string, conversations []string) (ProjectTransfer, error) {
	saved, err := loadTranscript(doc)
	if err != nil {
		return ProjectTransfer{}, err
	}
	return exportProject(saved, project, conversations)
}

func exportProject(saved transcript, project string, conversations []string) (ProjectTransfer, error) {
	out := ProjectTransfer{Schema: 1, Project: project, Replies: map[string][]consoleapi.Reply{}, Meta: map[string]Meta{}, Exchanges: map[string][]TransferExchange{}, Questions: map[string]consoleapi.PendingQuestion{}}
	if project == "" {
		return out, errors.New("project is required")
	}
	selected := map[string]bool{}
	for _, id := range conversations {
		selected[id] = true
	}
	// Historical records still belong to this project after a conversation
	// changes its current binding; include their provenance, not that binding.
	for id, list := range saved.Exchanges {
		for _, e := range list {
			if e.ExpectedProject == project {
				selected[id] = true
			}
		}
	}
	for id, list := range saved.Replies {
		for _, r := range list {
			if r.ProjectID == project {
				selected[id] = true
			}
		}
	}
	for id := range selected {
		if !strings.HasPrefix(id, Prefix) {
			return out, fmt.Errorf("invalid console conversation %q", id)
		}
		foreign := false
		byExchange := map[string]string{}
		for _, e := range saved.Exchanges[id] {
			if e.ExpectedProject == "" {
				return out, fmt.Errorf("exchange %s has no original project", e.ID)
			}
			byExchange[e.ID] = e.ExpectedProject
			if e.ExpectedProject != project {
				foreign = true
				continue
			}
			out.Exchanges[id] = append(out.Exchanges[id], transferExchange(e))
		}
		for _, r := range saved.Replies[id] {
			if r.ProjectID == "" {
				r.ProjectID = byExchange[r.ExchangeID]
			}
			if r.ProjectID == "" && r.Injected != nil {
				r.ProjectID = r.Injected.Project
			}
			if r.ProjectID == "" {
				return out, fmt.Errorf("reply %s has no original project", r.ID)
			}
			if r.ProjectID != project {
				foreign = true
				continue
			}
			out.Replies[id] = append(out.Replies[id], r)
		}
		if len(out.Replies[id]) > 0 || len(out.Exchanges[id]) > 0 {
			out.Conversations = append(out.Conversations, id)
			if m, ok := saved.Meta[id]; ok && !foreign {
				out.Meta[id] = m
			}
		}
	}
	listed := map[string]bool{}
	for _, id := range out.Conversations {
		listed[id] = true
	}
	for id, q := range saved.Questions {
		if q.Project == project {
			out.Questions[id] = copyQuestion(q)
			if !listed[q.Conversation] {
				out.Conversations = append(out.Conversations, q.Conversation)
				listed[q.Conversation] = true
			}
		}
	}
	sort.Strings(out.Conversations)
	return out, nil
}

// ImportProjectDocument is the explicit file/test counterpart of ImportProjectTx.
func ImportProjectDocument(doc ledger.Doc, in ProjectTransfer) error {
	saved, err := loadTranscript(doc)
	if err != nil {
		return err
	}
	if err := mergeProject(&saved, in); err != nil {
		return err
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	return doc.Save(raw)
}

func mergeProject(saved *transcript, in ProjectTransfer) error {
	if in.Schema != 1 || in.Project == "" {
		return errors.New("unsupported console project transfer")
	}
	allowed := map[string]bool{}
	for _, id := range in.Conversations {
		if !strings.HasPrefix(id, Prefix) || allowed[id] {
			return errors.New("invalid imported conversation")
		}
		allowed[id] = true
	}
	existingReplies := map[string]consoleapi.Reply{}
	for _, list := range saved.Replies {
		for _, r := range list {
			existingReplies[r.ID] = r
		}
	}
	existingExchanges := map[string]*queuedExchange{}
	for _, list := range saved.Exchanges {
		for _, e := range list {
			existingExchanges[e.ID] = e
		}
	}
	for conversation, list := range in.Replies {
		for _, r := range list {
			if !allowed[conversation] || r.ID == "" || r.Conversation != conversation || r.ProjectID != in.Project {
				return errors.New("invalid imported reply provenance")
			}
			if old, ok := existingReplies[r.ID]; ok {
				if !reflect.DeepEqual(old, r) {
					return fmt.Errorf("reply %s conflicts", r.ID)
				}
				continue
			}
			saved.Replies[conversation] = append(saved.Replies[conversation], r)
			existingReplies[r.ID] = r
		}
	}
	for conversation, list := range in.Exchanges {
		for _, e := range list {
			if !allowed[conversation] || e.ID == "" || e.Conversation != conversation || e.ExpectedProject != in.Project {
				return errors.New("invalid imported exchange provenance")
			}
			if e.Receipt != nil && (e.Receipt.Conversation != conversation || e.Receipt.ExchangeID != e.ID || (e.Receipt.ProjectID != "" && e.Receipt.ProjectID != in.Project)) {
				return errors.New("invalid imported receipt")
			}
			for _, f := range e.Materials {
				if f.Material.Project != in.Project {
					return errors.New("imported material crosses project boundary")
				}
			}
			if e.State == consoleapi.ExchangeRunning {
				e.State = consoleapi.ExchangeFailed
				if e.Receipt == nil {
					e.ReplyID = "r-transfer-" + e.ID
					e.Receipt = &consoleapi.Reply{ID: e.ReplyID, At: e.StartedAt, Conversation: conversation, ProjectID: in.Project, ExchangeID: e.ID, Kind: "reply", Text: "execution interrupted by transfer", Error: "execution interrupted by transfer"}
					if old, ok := existingReplies[e.ReplyID]; ok && !reflect.DeepEqual(old, *e.Receipt) {
						return fmt.Errorf("reply %s conflicts", e.ReplyID)
					} else if !ok {
						saved.Replies[conversation] = append(saved.Replies[conversation], *e.Receipt)
						existingReplies[e.ReplyID] = *e.Receipt
					}
				}
			}
			if old, ok := existingExchanges[e.ID]; ok {
				got := transferExchange(old)
				if !reflect.DeepEqual(got, e) {
					return fmt.Errorf("exchange %s conflicts", e.ID)
				}
				continue
			}
			for _, old := range saved.Exchanges[conversation] {
				if e.Key != "" && e.Key == old.Key {
					return fmt.Errorf("command key %s conflicts", e.Key)
				}
			}
			q := e.queued()
			saved.Exchanges[conversation] = append(saved.Exchanges[conversation], q)
			existingExchanges[e.ID] = q
		}
	}
	for id, q := range in.Questions {
		if id == "" || q.ID != id || q.Project != in.Project || !allowed[q.Conversation] {
			return errors.New("invalid imported question provenance")
		}
		if q.State == "pending" {
			q.State = "interrupted"
		}
		if old, ok := saved.Questions[id]; ok && !reflect.DeepEqual(old, q) {
			return fmt.Errorf("question %s conflicts", id)
		}
		saved.Questions[id] = q
	}
	for conversation, m := range in.Meta {
		if !allowed[conversation] {
			return errors.New("invalid imported metadata")
		}
		if old, ok := saved.Meta[conversation]; ok && !reflect.DeepEqual(old, m) {
			return fmt.Errorf("conversation %s metadata conflicts", conversation)
		}
		saved.Meta[conversation] = m
	}
	for id, list := range saved.Replies {
		sort.SliceStable(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
		saved.Replies[id] = list
	}
	return nil
}

// Validation and application share the caller's transaction with every other
// transfer owner. Recompute collision checks at commit, never import a stale
// pre-merged full console document.
func ValidateProjectImportTx(tx *ledger.Tx, in ProjectTransfer) error {
	_, _, err := projectConsoleChangesTx(tx, in)
	return err
}

func ImportProjectTx(tx *ledger.Tx, in ProjectTransfer) error {
	before, changes, err := projectConsoleChangesTx(tx, in)
	if err != nil {
		return err
	}
	return writeConsoleChangesTx(tx, before.revision, changes)
}

func projectConsoleChangesTx(tx *ledger.Tx, in ProjectTransfer) (consoleRecords, []consoleChange, error) {
	before, err := loadConsoleRecordsTx(tx)
	if err != nil {
		return before, nil, err
	}
	state, err := before.state()
	if err != nil {
		return before, nil, err
	}
	next := state.transcript()
	if err := mergeProject(&next, in); err != nil {
		return before, nil, err
	}
	changes, err := consoleChanges(before, next)
	return before, changes, err
}
