package console

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func clientKey(id string) string {
	if id == "" {
		return ""
	}
	return "client:" + id
}

func submission(input, prompt string, quotes []QuoteRef) (string, string, []QuoteRef, string) {
	normalize := func(text string) string { return strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")) }
	input, prompt = normalize(input), normalize(prompt)
	if prompt == input {
		prompt = ""
	}
	refs := make([]QuoteRef, len(quotes))
	for i, q := range quotes {
		refs[i] = QuoteRef{Conversation: conversationID(q.Conversation), ReplyID: q.ReplyID}
	}
	raw, _ := json.Marshal(struct {
		Input, Prompt string
		Quotes        []QuoteRef
	}{input, prompt, refs})
	sum := sha256.Sum256(raw)
	return input, prompt, refs, hex.EncodeToString(sum[:])
}

func (s *Service) submittedLocked(conversation, key, hash string) (*queuedExchange, error) {
	if key == "" {
		return nil, nil
	}
	for _, e := range s.exchanges[conversation] {
		if e.Key == key {
			// Platform delivery keys retain their original first-write-wins
			// contract; client keys are bound to one immutable submission.
			if strings.HasPrefix(key, "client:") && e.PayloadHash != hash {
				return nil, consoleapi.ErrCommandConflict
			}
			return e, nil
		}
	}
	return nil, nil
}

func replyOutcome(reply consoleapi.Reply) outcome {
	out := outcome{reply: reply}
	if reply.Error != "" {
		out.err = errors.New(reply.Error)
	}
	return out
}
