package agentmcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/intent"
)

var errOutcomeUnknown = channel.ErrOutcomeUnknown

type messageArgs struct {
	Channel   string `json:"channel,omitempty"`
	Content   string `json:"content,omitempty"`
	Format    string `json:"format,omitempty"`
	Mention   bool   `json:"mention,omitempty"`
	Progress  string `json:"progress,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

type messageCall struct {
	args      messageArgs
	address   channel.Address
	messenger channel.Messenger
	epoch     uint64
	record    sentMsg
	taskID    string
}

func channelArgument() map[string]any {
	return map[string]any{"type": "string", "description": "Optional channel ID. Defaults to the channel bound to this conversation. An explicit value must match that binding (or the message receipt for update/recall); it cannot select another recipient."}
}

func (s *Server) channelCall(ctx context.Context, bind binding, tool string, raw json.RawMessage) (string, error) {
	var args messageArgs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return "", fmt.Errorf("bad %s arguments: %w", tool, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("bad %s arguments: expected one object", tool)
	}
	if args.Mention {
		return "", errors.New("mention is not allowed: mentions are reserved for the platform's final answer")
	}
	args.Channel = strings.TrimSpace(args.Channel)
	args.Content = truncateRunes(args.Content, maxContentRunes)
	args.Progress = truncateRunes(strings.Join(strings.Fields(args.Progress), ""), 16)
	if tool != "channel_recall" && strings.TrimSpace(args.Content) == "" {
		return "", errors.New("content is required")
	}
	if tool == "channel_send" {
		if args.MessageID != "" {
			return "", errors.New("channel_send does not accept message_id")
		}
		if args.Format == "" {
			args.Format = "markdown"
		}
		if args.Format != "text" && args.Format != "markdown" {
			return "", fmt.Errorf("unknown format %q (use markdown or text)", args.Format)
		}
	} else if args.MessageID == "" {
		return "", errors.New("message_id is required")
	}
	if tool == "channel_recall" && (args.Content != "" || args.Progress != "" || args.Format != "") {
		return "", errors.New("channel_recall accepts only message_id and channel")
	}
	if tool == "channel_update" && args.Format != "" {
		return "", errors.New("channel_update preserves the original message format")
	}

	s.mu.Lock()
	if err := s.loadConversationLocked(ctx, bind.conversationID, &bind); err != nil {
		s.mu.Unlock()
		return "", err
	}
	a := s.anchors[bind.conversationID]
	if a == nil || a.address.Message == "" || a.address.Conversation == "" {
		s.mu.Unlock()
		return "", errors.New("no active conversation to deliver to")
	}
	prepared := messageCall{args: args, address: a.address, epoch: a.epoch}
	if tool != "channel_send" {
		st := s.sent[bind]
		if st == nil || st.epoch != a.epoch {
			s.mu.Unlock()
			return "", errors.New("can only update or recall a message this agent sent in the current turn")
		}
		record, owned := st.ids[args.MessageID]
		if !owned {
			s.mu.Unlock()
			return "", errors.New("can only update or recall a message this agent sent in the current turn")
		}
		if record.busy {
			if err := s.reconcileMessageLocked(ctx, bind, args.MessageID, st, record); err != nil {
				s.mu.Unlock()
				return "", err
			}
			var exists bool
			record, exists = st.ids[args.MessageID]
			if !exists {
				s.mu.Unlock()
				return "", errors.New("the message was already recalled")
			}
		}
		prepared.record = record
		prepared.address = record.address
		prepared.messenger = record.messenger
		prepared.args.Format = record.format
		if tool == "channel_update" && prepared.args.Progress == "" {
			prepared.args.Progress = record.progress
		}
	}
	if args.Channel != "" && args.Channel != prepared.address.Channel {
		s.mu.Unlock()
		return "", fmt.Errorf("channel %q is not bound to this conversation/message", args.Channel)
	}
	if prepared.messenger == nil {
		prepared.messenger = s.channels[prepared.address.Channel]
	}
	if prepared.messenger == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("channel %q is unavailable", prepared.address.Channel)
	}
	prepared.args.Channel = prepared.address.Channel
	informer := s.informer
	hasIntents := s.intents != nil
	s.mu.Unlock()

	effectBind := bind
	if fixed, ok := ctx.Value(grantContextKey{}).(grantContext); ok && fixed.Grant.Scope != nil {
		effectBind.taskID = fixed.Grant.Scope.TaskID
	}
	if hasIntents && effectBind.taskID == "" {
		if informer == nil {
			return "", errors.New("no active task available for message effects")
		}
		info, err := informer.Context(ctx, bind.conversationID, bind.agentID)
		if err != nil {
			return "", fmt.Errorf("resolve message task: %w", err)
		}
		if info.Task == "" {
			return "", errors.New("no active task available for message effects")
		}
		effectBind.taskID = info.Task
	}
	prepared.taskID = effectBind.taskID

	// Normalize routing/defaults before claiming the effect. Inbound anchor IDs
	// change on resume; the same uncertain send to the same conversation must
	// remain blocked across attempts even if its new turn has another anchor.
	canonical := struct {
		messageArgs
		Conversation string `json:"conversation"`
	}{prepared.args, prepared.address.Conversation}
	normalized, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return s.effect(ctx, effectBind, tool, normalized, func(callCtx context.Context) (string, channel.Address, error) {
		return s.dispatchMessage(callCtx, bind, tool, prepared)
	})
}

func (s *Server) dispatchMessage(ctx context.Context, bind binding, tool string, call messageCall) (string, channel.Address, error) {
	empty := channel.Address{}
	if err := ctx.Err(); err != nil {
		return "", empty, err
	}
	s.mu.Lock()
	a := s.anchors[bind.conversationID]
	if a == nil || a.epoch != call.epoch {
		s.mu.Unlock()
		return "", empty, errors.New("the current turn changed before dispatch")
	}
	st := s.sent[bind]
	previous := st
	var before sentState
	if st != nil {
		before = *st
		before.ids = make(map[string]sentMsg, len(st.ids))
		for id, record := range st.ids {
			before.ids[id] = record
		}
	}
	if st == nil || st.epoch != call.epoch {
		st = &sentState{epoch: call.epoch, ids: map[string]sentMsg{}}
	}
	var record sentMsg
	if tool == "channel_send" {
		if st.count >= maxSendsPerTurn {
			s.mu.Unlock()
			return "", empty, fmt.Errorf("send limit reached (%d per turn); save the rest for the final answer", maxSendsPerTurn)
		}
		st.count++
		attribution := s.styles[bind.conversationID]
		if bind.delegatedBy != "" {
			attribution = bind.agentID + " · 受 " + bind.delegatedBy + " 委派"
		}
		record = sentMsg{format: call.args.Format, seq: st.count, progress: call.args.Progress, attribution: attribution, messenger: call.messenger}
	} else {
		var ok bool
		record, ok = st.ids[call.args.MessageID]
		if !ok || record.busy || record.version != call.record.version {
			s.mu.Unlock()
			return "", empty, errors.New("message is no longer available or another operation is in progress")
		}
		if tool == "channel_update" {
			if st.updates >= maxUpdatesPerTurn {
				s.mu.Unlock()
				return "", empty, fmt.Errorf("update limit reached (%d per turn)", maxUpdatesPerTurn)
			}
			st.updates++
		}
		record.busy = true
		record.intentID, _ = ctx.Value(intentContextKey{}).(string)
		record.pendingTool = tool
		record.pendingProgress = call.args.Progress
		st.ids[call.args.MessageID] = record
	}
	s.sent[bind] = st
	if err := s.saveMessagesLocked(ctx, bind, true); err != nil {
		if previous != nil {
			*previous = before
		}
		s.sent[bind] = previous
		s.mu.Unlock()
		return "", empty, err
	}
	s.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	msg := channel.Message{Content: call.args.Content, Format: record.format, Attribution: milestoneTail(record.attribution, bind.agentID, record.seq, call.args.Progress)}
	receipt := call.address
	var id string
	var err error
	switch tool {
	case "channel_send":
		var random [16]byte
		if _, err = rand.Read(random[:]); err == nil {
			id, err = call.messenger.Send(callCtx, call.address, msg)
		}
		if err == nil && id == "" {
			err = fmt.Errorf("%w: channel returned an empty message receipt", errOutcomeUnknown)
		}
		receipt.Message = id
		handle := "msg_" + hex.EncodeToString(random[:])
		s.mu.Lock()
		stillCurrent := s.anchors[bind.conversationID] != nil && s.anchors[bind.conversationID].epoch == call.epoch && s.sent[bind] == st
		if err == nil && stillCurrent {
			record.address = receipt
			st.ids[handle] = record
		} else if err != nil && stillCurrent && st.count > 0 && !errors.Is(err, channel.ErrOutcomeUnknown) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			st.count--
		}
		journal := s.journal
		if stillCurrent {
			if saveErr := s.saveMessagesLocked(context.Background(), bind, false); saveErr != nil {
				err = fmt.Errorf("%w: message receipt could not be saved: %v", errOutcomeUnknown, saveErr)
			}
		}
		s.mu.Unlock()
		if err != nil {
			return "", receipt, fmt.Errorf("send failed: %w", err)
		}
		if journal != nil {
			journal(bind.conversationID, bind.agentID, call.taskID, receipt)
		}
		return "sent message_id=" + handle, receipt, nil
	case "channel_update":
		err = call.messenger.Update(callCtx, receipt, msg)
	case "channel_recall":
		err = call.messenger.Recall(callCtx, receipt)
	}
	s.mu.Lock()
	if st.epoch == call.epoch && s.sent[bind] == st {
		record.busy = errors.Is(err, channel.ErrOutcomeUnknown) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		if !record.busy {
			record.intentID, record.pendingTool, record.pendingProgress = "", "", ""
		}
		if err == nil && tool == "channel_recall" {
			delete(st.ids, call.args.MessageID)
		} else {
			if err == nil && tool == "channel_update" {
				record.progress = call.args.Progress
				record.version++
			}
			st.ids[call.args.MessageID] = record
		}
		if saveErr := s.saveMessagesLocked(context.Background(), bind, false); saveErr != nil {
			err = fmt.Errorf("%w: message receipt could not be saved: %v", errOutcomeUnknown, saveErr)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return "", receipt, fmt.Errorf("message operation failed: %w", err)
	}
	if tool == "channel_update" {
		return "updated " + call.args.MessageID, receipt, nil
	}
	return "recalled " + call.args.MessageID, receipt, nil
}

func (s *Server) reconcileMessageLocked(ctx context.Context, bind binding, id string, st *sentState, record sentMsg) error {
	reader, ok := s.intents.(interface {
		Outcome(context.Context, string) (string, error)
	})
	if !ok || record.intentID == "" {
		return errors.New("message operation is blocked pending reconciliation")
	}
	outcome, err := reader.Outcome(ctx, record.intentID)
	if err != nil {
		return fmt.Errorf("read message reconciliation: %w", err)
	}
	if outcome != string(intent.Succeeded) && outcome != string(intent.Failed) {
		return fmt.Errorf("message operation is blocked pending reconciliation of intent %s", record.intentID)
	}
	before := st.ids[id]
	if outcome == string(intent.Succeeded) && record.pendingTool == "channel_recall" {
		delete(st.ids, id)
	} else {
		if outcome == string(intent.Succeeded) && record.pendingTool == "channel_update" {
			record.version++
			record.progress = record.pendingProgress
		}
		record.busy = false
		record.intentID, record.pendingTool, record.pendingProgress = "", "", ""
		st.ids[id] = record
	}
	if err := s.saveMessagesLocked(ctx, bind, true); err != nil {
		st.ids[id] = before
		return err
	}
	return nil
}
