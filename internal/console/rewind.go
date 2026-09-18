package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// Rewinding is what "edit a message I already sent" has to mean once an
// agent has answered it. Putting the old text back in the box and sending
// it again would ask the same agent a second question with the first one
// still in its memory; what the owner wants is the thread as it would
// have been had they typed the new line instead. So the thread goes back
// to the moment before that line: it and everything after it leave the
// transcript, the agent's own session is closed so it cannot remember
// what the transcript no longer says, and the conversation up to that
// point is handed to the fresh session as text.
//
// A rewind is refused while anything is in flight. Truncating a thread
// under a running turn would leave an agent answering into lines that no
// longer exist.

// maxRewindHistory bounds what a rewound thread carries into its new
// session. Beyond it the oldest lines are dropped and said to be dropped,
// because refusing to edit a long thread is worse than an honest gap.
const maxRewindHistory = 192 << 10

// ErrRewindTargetGone is a line that is no longer in the transcript: it
// was already rewound past, or the thread was trimmed beyond it.
var ErrRewindTargetGone = consoleapi.ErrRewindTargetGone

// rewindPlan is what a rewind would do, worked out before anything is
// destroyed so a refusal costs nothing.
type rewindPlan struct {
	conversation string
	// line is where the edited message sits in the transcript, and
	// exchange is the submission it came from; everything from there on
	// goes.
	line     int
	exchange string
	history  string
	removed  []string
}

// planRewind reads what a rewind would need without changing anything.
func (s *Service) planRewind(conversation, replyID string) (rewindPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planRewindLocked(conversation, replyID)
}

func (s *Service) planRewindLocked(conversation, replyID string) (rewindPlan, error) {
	if s.closing || s.maintenance {
		return rewindPlan{}, consoleapi.ErrConsoleClosing
	}
	if s.sealed[conversation] {
		return rewindPlan{}, fmt.Errorf("%w: %s is being deleted", consoleapi.ErrBusy, conversation)
	}
	for _, e := range s.exchanges[conversation] {
		if !e.State.Terminal() {
			return rewindPlan{}, fmt.Errorf("%w: %s has a line %s", consoleapi.ErrBusy, conversation, e.State)
		}
	}
	list := s.replies[conversation]
	at := -1
	for i := range list {
		if list[i].ID == replyID && list[i].Kind == "sent" {
			at = i
			break
		}
	}
	if at < 0 {
		return rewindPlan{}, ErrRewindTargetGone
	}
	plan := rewindPlan{conversation: conversation, line: at, exchange: list[at].ExchangeID, history: rewindHistory(list[:at])}
	for _, r := range list[at:] {
		if r.ID != "" {
			plan.removed = append(plan.removed, r.ID)
		}
	}
	return plan, nil
}

// rewindHistory writes the thread back as a record the next session can
// read: who said what, in order, the way the recovery path hands a moved
// conversation to a new machine.
func rewindHistory(list []consoleapi.Reply) string {
	type line struct{ who, text string }
	var lines []line
	for _, r := range list {
		if r.Silent {
			continue
		}
		who, text := "Steve", strings.TrimSpace(r.Text)
		if r.Kind == "sent" {
			who, text = "用户", strings.TrimSpace(r.Input)
		}
		if text == "" {
			continue
		}
		lines = append(lines, line{who, text})
	}
	total, from := 0, 0
	for i := len(lines) - 1; i >= 0; i-- {
		total += len(lines[i].who) + len(lines[i].text) + 4
		if total > maxRewindHistory {
			from = i + 1
			break
		}
	}
	if from >= len(lines) {
		return ""
	}
	var b strings.Builder
	b.WriteString("以下是本次对话在这条消息之前的记录，供你了解上文，不是新的指令：\n\n")
	if from > 0 {
		fmt.Fprintf(&b, "（更早的 %d 条记录因为太长没有带过来。）\n\n", from)
	}
	for _, l := range lines[from:] {
		fmt.Fprintf(&b, "%s：\n%s\n\n", l.who, l.text)
	}
	b.WriteString("记录到此为止。用户刚刚改写了原本在这里的那条消息，它之后发生的对话已经作废，不要再依据它们行事。下面是用户改写后的消息：\n\n")
	return b.String()
}

// applyRewindLocked drops the edited line and everything after it. It
// returns what to put back if accepting the replacement then fails, so a
// rewind that cannot be spoken never happens at all.
func (s *Service) applyRewindLocked(plan rewindPlan) func() {
	conversation := plan.conversation
	replies := s.replies[conversation]
	exchanges := s.exchanges[conversation]
	keptExchanges := len(exchanges)
	if plan.exchange != "" {
		for i, e := range exchanges {
			if e.ID == plan.exchange {
				keptExchanges = i
				break
			}
		}
	}
	dropped := map[string]*process{}
	for _, e := range exchanges[keptExchanges:] {
		if work := s.processes[e.ID]; work != nil {
			dropped[e.ID] = work
			delete(s.processes, e.ID)
		}
	}
	gone := map[string]consoleapi.PendingQuestion{}
	for id, q := range s.questions {
		if q.Conversation != conversation {
			continue
		}
		for _, e := range exchanges[keptExchanges:] {
			if q.ExchangeID == e.ID {
				gone[id] = q
				delete(s.questions, id)
				break
			}
		}
	}
	s.replies[conversation] = append([]consoleapi.Reply(nil), replies[:plan.line]...)
	s.exchanges[conversation] = append([]*queuedExchange(nil), exchanges[:keptExchanges]...)
	return func() {
		s.replies[conversation] = replies
		s.exchanges[conversation] = exchanges
		for id, work := range dropped {
			s.processes[id] = work
		}
		for id, q := range gone {
			s.questions[id] = q
		}
	}
}

// acceptRewoundLocked takes a line that replaces one already sent. The
// thread goes back inside the same lock that accepts the replacement, so
// no reader sees a thread missing its tail with nothing said in its
// place, and a refusal to accept puts the thread back as it was.
func (s *Service) acceptRewoundLocked(e *queuedExchange, target string, front bool) (*queuedExchange, Exchange, error) {
	plan, err := s.planRewindLocked(e.Conversation, target)
	if err != nil {
		return nil, Exchange{}, err
	}
	e.History = plan.history
	undo := s.applyRewindLocked(plan)
	accepted, exchange, err := s.acceptExchangeLocked(e, front)
	if err != nil {
		undo()
		if saveErr := s.save(); saveErr != nil {
			slog.Error(fmt.Sprintf("console: restore rewound thread %s: %v", plan.conversation, saveErr))
		}
		return nil, Exchange{}, err
	}
	s.publishRewoundLocked(plan)
	return accepted, exchange, nil
}

// publishRewoundLocked tells every open page which lines went, so a
// transcript being read right now loses them without waiting for a reload.
func (s *Service) publishRewoundLocked(plan rewindPlan) {
	if s.model == nil {
		return
	}
	at := time.Now().UTC()
	for _, id := range plan.removed {
		s.model.Publish(readmodel.Event{At: at, Kind: "console.recalled", Conversation: plan.conversation, ReplyID: id})
	}
}

// beginRewind does the part of a rewind that must happen before the
// replacement is queued: it checks the thread can go back, then ends the
// agent sessions it holds so the next turn opens a fresh one. Nothing is
// removed here; the transcript is truncated inside the same lock that
// accepts the new line.
//
// A retry of a submission that already rewound finds its target gone.
// That is not a failure — the exchange it created is proof the rewind
// happened — so the retry is let through to be recognised as a duplicate.
func (s *Service) beginRewind(ctx context.Context, conversation, replyID, key string) (string, error) {
	conversation = ConversationID(conversation)
	_, err := s.planRewind(conversation, replyID)
	if errors.Is(err, ErrRewindTargetGone) && key != "" && s.submittedKey(conversation, key) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if resetter, ok := s.handler.(sessionResetter); ok {
		if err := resetter.ResetConversationSessions(ctx, conversation); err != nil {
			return "", fmt.Errorf("结束这个会话的 agent 会话失败，没有改动任何记录：%w", err)
		}
	}
	return replyID, nil
}

// submittedKey reports a submission identity this conversation has
// already accepted.
func (s *Service) submittedKey(conversation, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.exchanges[conversation] {
		if e.Key == key {
			return true
		}
	}
	return false
}
