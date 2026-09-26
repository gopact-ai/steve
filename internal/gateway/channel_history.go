package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
)

type channelHistory struct{ book *ledger.Ledger }

// NewChannelHistory reads original accepted channel inputs and retained dispatch
// evidence. It never reserves commands, recovers attempts or sends messages.
// List traverses opaque IDs in lexical order. Read takes the latest turns first,
// returns each page oldest-to-newest, and binds older-page cursors to that exact
// conversation. Limits count turns (at most two lines each), up to 200.
func NewChannelHistory(book *ledger.Ledger) consoleapi.ChannelHistory {
	return &channelHistory{book: book}
}

type channelHistoryCursor struct {
	Version      int    `json:"v"`
	Kind         string `json:"kind"`
	Conversation string `json:"conversation,omitempty"`
	At           string `json:"at,omitempty"`
	ID           string `json:"id"`
}

func decodeChannelHistoryCursor(raw, kind, conversation string) (channelHistoryCursor, error) {
	c := channelHistoryCursor{Version: 1, Kind: kind, Conversation: conversation}
	if raw == "" {
		return c, nil
	}
	if len(raw) > 1<<20 {
		return c, consoleapi.ErrChannelHistoryCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return c, consoleapi.ErrChannelHistoryCursor
	}
	c = channelHistoryCursor{}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF || c.Version != 1 || c.Kind != kind || c.Conversation != conversation || c.ID == "" {
		return c, consoleapi.ErrChannelHistoryCursor
	}
	if kind == "read" {
		if _, err := time.Parse(time.RFC3339Nano, c.At+"Z"); err != nil {
			return c, consoleapi.ErrChannelHistoryCursor
		}
	} else if c.At != "" {
		return c, consoleapi.ErrChannelHistoryCursor
	}
	return c, nil
}
func encodeChannelHistoryCursor(c channelHistoryCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}
func channelHistoryLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return min(limit, 200)
}

func (h *channelHistory) Contains(ctx context.Context, rawID string) (bool, error) {
	var found bool
	err := h.book.Read(ctx, func(tx *ledger.ReadTx) error {
		var err error
		found, err = channelContainsTx(tx, rawID)
		return err
	})
	return found, err
}
func channelContainsTx(tx *ledger.ReadTx, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var key string
	err := tx.QueryRow(`SELECT id FROM (`+channelTurnsSQL("")+`) LIMIT 1`, id, id).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
func channelConversationTx(tx *ledger.ReadTx, id string) (consoleapi.Conversation, error) {
	c := consoleapi.Conversation{ID: id, Transport: "feishu", ReadOnly: true}
	var at, title, project sql.NullString
	if err := tx.QueryRow(channelSummarySQL(), id, id).Scan(&c.Count, &at, &title, &project); err != nil {
		return c, err
	}
	if c.Count == 0 {
		return c, consoleapi.ErrChannelConversationNotFound
	}
	var err error
	c.LastAt, err = time.Parse(time.RFC3339Nano, at.String+"Z")
	c.Title = title.String
	c.Project = project.String
	return c, err
}
func (h *channelHistory) List(ctx context.Context, cursor string, limit int) (consoleapi.ChannelConversationPage, error) {
	page := consoleapi.ChannelConversationPage{Conversations: []consoleapi.Conversation{}}
	c, err := decodeChannelHistoryCursor(cursor, "list", "")
	if err != nil {
		return page, err
	}
	limit = channelHistoryLimit(limit)
	err = h.book.Read(ctx, func(tx *ledger.ReadTx) error {
		after := c.ID
		for len(page.Conversations) <= limit {
			rows, err := tx.Query(channelCandidatesSQL(), after, limit+1, after, limit+1, limit+1)
			if err != nil {
				return err
			}
			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				break
			}
			for _, id := range ids {
				after = id
				exists, err := channelContainsTx(tx, id)
				if err != nil {
					return err
				}
				if !exists {
					continue
				}
				conversation, err := channelConversationTx(tx, id)
				if err != nil {
					return err
				}
				page.Conversations = append(page.Conversations, conversation)
				if len(page.Conversations) > limit {
					break
				}
			}
			if len(ids) < limit+1 {
				break
			}
		}
		return nil
	})
	if err != nil {
		return consoleapi.ChannelConversationPage{}, err
	}
	if len(page.Conversations) > limit {
		page.Conversations = page.Conversations[:limit]
		c.ID = page.Conversations[limit-1].ID
		page.NextCursor = encodeChannelHistoryCursor(c)
	}
	return page, nil
}

type channelTurnPosition struct{ ID, At string }

func channelPositionsTx(tx *ledger.ReadTx, conversation string, c channelHistoryCursor, limit int) ([]channelTurnPosition, error) {
	read := func(boundary string, args ...any) ([]channelTurnPosition, error) {
		rows, err := tx.Query(`SELECT id,at FROM (`+channelTurnsSQL(boundary)+`) ORDER BY at DESC,id DESC LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []channelTurnPosition
		for rows.Next() {
			var p channelTurnPosition
			if err := rows.Scan(&p.ID, &p.At); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, rows.Err()
	}
	if c.ID == "" {
		return read("", conversation, conversation, limit)
	}
	// Disjoint seeks retain index range access even with equal timestamps.
	out, err := read(` AND rtrim(i.received_at,'Z')=? AND i.id<?`, conversation, c.At, c.ID, conversation, c.At, c.ID, limit)
	if err != nil || len(out) == limit {
		return out, err
	}
	older, err := read(` AND rtrim(i.received_at,'Z')<?`, conversation, c.At, conversation, c.At, limit-len(out))
	return append(out, older...), err
}
func (h *channelHistory) Read(ctx context.Context, conversation, cursor string, limit int) (consoleapi.ChannelConversationHistory, error) {
	page := consoleapi.ChannelConversationHistory{Replies: []consoleapi.Reply{}}
	c, err := decodeChannelHistoryCursor(cursor, "read", conversation)
	if err != nil {
		return page, err
	}
	limit = channelHistoryLimit(limit)
	err = h.book.Read(ctx, func(tx *ledger.ReadTx) error {
		var err error
		page.Conversation, err = channelConversationTx(tx, conversation)
		if err != nil {
			return err
		}
		positions, err := channelPositionsTx(tx, conversation, c, limit+1)
		if err != nil {
			return err
		}
		if len(positions) > limit {
			positions = positions[:limit]
			last := positions[limit-1]
			c.At, c.ID = last.At, last.ID
			page.NextCursor = encodeChannelHistoryCursor(c)
		}
		slices.Reverse(positions)
		for _, p := range positions {
			replies, err := channelRepliesTx(ctx, tx, p.ID, conversation)
			if err != nil {
				return err
			}
			page.Replies = append(page.Replies, replies...)
		}
		return nil
	})
	if err != nil {
		return consoleapi.ChannelConversationHistory{}, err
	}
	return page, nil
}

func channelRepliesTx(ctx context.Context, tx *ledger.ReadTx, id, conversation string) ([]consoleapi.Reply, error) {
	input, _, err := ledger.CommandReceiptTx(tx, id)
	if err != nil {
		return nil, err
	}
	sent := consoleapi.Reply{ID: id, ExchangeID: id, Conversation: conversation, At: input.ReceivedAt, Kind: "sent"}
	prefix := "gateway-input"
	routeSafe := true
	if input.Kind == gatewayInputKind {
		ordinary, err := decodeGatewayInput(input)
		if err != nil {
			return nil, err
		}
		// An eligible topic with a missing route cannot prove where its output
		// was sent. Keep only the original accepted text in the source group.
		topicText, isTopic := topicTask(ordinary.Message)
		routeSafe = !(isTopic && topicText != "" && conversationID(ordinary.Message) == ordinary.Message.ChatID)
		sent.Input = ordinary.Message.Text
		sent.Relayed = ordinary.Message.Origin != ""
		route, exists, err := ledger.CommandReceiptTx(tx, id+"/topic")
		if err != nil {
			return nil, err
		}
		if exists {
			routed, routeErr := topicMessage(ordinary, route, id)
			routeSafe = routeErr == nil && conversationID(routed) == conversation
		}
	} else {
		var recovery recoveryInput
		if err := json.Unmarshal(input.Result, &recovery); err != nil {
			return nil, err
		}
		sent.Input, sent.Relayed = recovery.Prompt, true
		prefix = "gateway-recovery"
	}
	out := []consoleapi.Reply{sent}
	if !routeSafe {
		return out, nil
	}
	reply := consoleapi.Reply{ID: id + "/dispatch", ExchangeID: id, Conversation: conversation, At: input.ReceivedAt, Kind: "reply", Delivery: "unavailable"}
	dispatch, _, err := ledger.CommandReceiptTx(tx, id+"/dispatch")
	if err != nil {
		return nil, err
	}
	var output recoveredOutput
	// A recovery-required dispatch is not the output eventually delivered.
	// A later reply proof confirms delivery only; it cannot confirm this body.
	hasOutput := channelSuccessful(dispatch, prefix+"-dispatch", input.Actor) && !bytes.Equal(bytes.TrimSpace(dispatch.Result), []byte("null")) && json.Unmarshal(dispatch.Result, &output) == nil && !output.Recover
	if hasOutput {
		reply.Text, reply.Title, reply.AttemptID = output.Result.Text, output.Result.Title, output.Result.Attempt
		// Match finalText's disclosure boundary: Error is internal diagnostic
		// evidence, not user-facing text. Only UserError is explicitly public.
		text := i18n.FromContext(ctx)
		switch {
		case output.Canceled:
			reply.Error = text.T(i18n.TurnCanceled)
		case output.UserError != "":
			reply.Error = output.UserError
		case output.Error != "":
			reply.Error = text.T(i18n.AgentFailed)
		}
		if reply.Error != "" {
			reply.Text = reply.Error
		}
		if injected := output.Result.Injected; injected != nil {
			reply.ProjectID = injected.Project
			reply.Injected = &consoleapi.Injected{Project: injected.Project, Agent: injected.Agent, Workspace: injected.Workspace, Node: injected.Node, Harness: injected.Harness, Model: injected.Model}
		}
		reply.At = *dispatch.FinishedAt
		reply.Delivery = "unconfirmed"
	}
	confirmedAt, confirmed, err := channelProofTx(tx, id+"/reply", prefix+"-reply", input)
	if err != nil {
		return nil, err
	}
	suppressed := false
	var suppressedAt time.Time
	if prefix == "gateway-input" {
		suppressedAt, suppressed, err = channelProofTx(tx, id+"/suppressed", "gateway-input-suppressed", input)
		if err != nil {
			return nil, err
		}
	}
	if confirmed {
		reply.Delivery = "confirmed"
	} else if suppressed {
		reply.Delivery = "suppressed"
	}
	if confirmedAt.After(reply.At) {
		reply.At = confirmedAt
	}
	if suppressedAt.After(reply.At) {
		reply.At = suppressedAt
	}
	if hasOutput || confirmed || suppressed {
		out = append(out, reply)
	}
	return out, nil
}
func channelSuccessful(r ledger.CommandRecord, kind, actor string) bool {
	return r.Kind == kind && r.Actor == actor && r.FinishedAt != nil && r.Error == ""
}
func channelProofTx(tx *ledger.ReadTx, id, kind string, input ledger.CommandRecord) (time.Time, bool, error) {
	r, _, err := ledger.CommandReceiptTx(tx, id)
	if err != nil {
		return time.Time{}, false, err
	}
	var proof ledger.CommandProof
	if channelSuccessful(r, kind, input.Actor) && json.Unmarshal(r.Result, &proof) == nil && proof.CommandID == input.ID && proof.Receipt != "" {
		return *r.FinishedAt, true, nil
	}
	return time.Time{}, false, nil
}
