package console

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

// Remap changes only structured local identifiers. Material content/provenance
// and all user/agent prose remain immutable facts from the source hub. The
// source is copied through JSON so it stays untouched; a transfer that does
// not survive that round trip is reported rather than remapped in part.
func (in ProjectTransfer) Remap(taskID, conversation, key func(string) string) (ProjectTransfer, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return ProjectTransfer{}, fmt.Errorf("copy console transfer: %w", err)
	}
	var out ProjectTransfer
	if err := json.Unmarshal(raw, &out); err != nil {
		return ProjectTransfer{}, fmt.Errorf("copy console transfer: %w", err)
	}
	apply := func(fn func(string) string, id string) string {
		if fn == nil || id == "" {
			return id
		}
		return fn(id)
	}
	reply := func(r consoleapi.Reply) consoleapi.Reply {
		r.Conversation = apply(conversation, r.Conversation)
		if r.Process != nil {
			for i := range r.Process.Steps {
				step := &r.Process.Steps[i]
				if id, ok := strings.CutPrefix(step.ID, "#"); ok {
					step.ID = "#" + apply(taskID, id)
				}
				for j, ref := range step.Refs {
					step.Refs[j] = ledger.RemapTaskReference(ref, taskID)
				}
			}
		}
		return r
	}
	replies := out.Replies
	out.Replies = map[string][]consoleapi.Reply{}
	for id, list := range replies {
		for _, r := range list {
			mapped := apply(conversation, id)
			out.Replies[mapped] = append(out.Replies[mapped], reply(r))
		}
	}
	meta := out.Meta
	out.Meta = map[string]Meta{}
	for id, m := range meta {
		out.Meta[apply(conversation, id)] = m
	}
	exchanges := out.Exchanges
	out.Exchanges = map[string][]TransferExchange{}
	for id, list := range exchanges {
		for _, e := range list {
			e.Conversation = apply(conversation, e.Conversation)
			e.Key = apply(key, e.Key)
			e.Origin = apply(key, e.Origin)
			aliases := map[string]string{}
			for i := range e.Quotes {
				old := e.Quotes[i].Conversation
				renamed := apply(conversation, old)
				original := e.QuoteAliases[old]
				if original == "" {
					original = old
				}
				e.Quotes[i].Conversation = renamed
				if renamed != original {
					aliases[renamed] = original
				}
			}
			if len(aliases) > 0 {
				e.QuoteAliases = aliases
			} else {
				e.QuoteAliases = nil
			}
			if e.Receipt != nil {
				r := reply(*e.Receipt)
				e.Receipt = &r
			}
			mapped := apply(conversation, id)
			out.Exchanges[mapped] = append(out.Exchanges[mapped], e)
		}
	}
	for id, q := range out.Questions {
		q.Conversation = apply(conversation, q.Conversation)
		q.TaskID = apply(taskID, q.TaskID)
		out.Questions[id] = q
	}
	for i, id := range out.Conversations {
		out.Conversations[i] = apply(conversation, id)
	}
	return out, nil
}
