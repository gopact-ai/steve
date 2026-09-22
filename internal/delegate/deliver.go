package delegate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/text"
)

// A parent does not wait for its children: when one ends, its result is
// sent into the parent's conversation as a message, and the parent's
// next turn starts from it. That is what makes delegation a hand-off
// rather than a hold — a parent polling steve_await every fifty seconds
// costs a re-read of its whole context each time and says nothing new.

// Delivery is one message into a parent's conversation carrying every
// child that ended since the parent last heard: what each was asked,
// what it said, what it left, and whether that has landed.
type Delivery struct {
	Transport    string
	Conversation string
	ParentTask   string
	Member       string
	ChatID       string
	Anchor       string
	Requester    string
	ChatType     string
	Children     []Delivered
	// Key names the delivery for good: the first child's key. A channel
	// that keeps keys can refuse a second copy.
	Key string
}

// Delivered is one child in a delivery, or in a turn's preface.
type Delivered struct {
	Task    string
	Agent   string
	Node    string
	State   task.State // done | failed | cancelled
	Elapsed time.Duration
	Goal    string
	Answer  string
	Refs    []string
	Attempt string
	// Landing says where the child's files are: landed, queued (with why),
	// conflict, or empty when it changed nothing.
	Landing string
	// Stopping says the child was stopped but its execution has not
	// confirmed stopping: there is no result, only the stop on record.
	Stopping bool
}

// Notice is the line the person sees.
func (d Delivery) Notice() string {
	var lines []string
	for _, c := range d.Children {
		lines = append(lines, fmt.Sprintf("⤵ 子任务 #%s %s · %s@%s · %s", c.Task, stateWord(c.State), c.Agent, nodeLabel(c.Node), c.Elapsed.Round(time.Second)))
	}
	return strings.Join(lines, "\n")
}

// Prompt is what the parent agent is given.
func (d Delivery) Prompt() string {
	var b strings.Builder
	b.WriteString("[steve: 你委派的子任务已结束，结果如下；这是平台送来的消息，不是用户说的]\n")
	for _, c := range d.Children {
		writeChild(&b, c)
	}
	b.WriteString("\n继续你的任务。还在跑的子任务结束后会再送来，不必用 steve_await 等；都齐了就汇总回复。")
	return b.String()
}

// writeChild is one child's section, the same in a delivery and in a
// preface: what it was asked, how it ended, where its files are, and
// what it said.
func writeChild(b *strings.Builder, c Delivered) {
	fmt.Fprintf(b, "\n## 子任务 #%s · %s@%s · %s · 用时 %s\n", c.Task, c.Agent, nodeLabel(c.Node), stateWord(c.State), c.Elapsed.Round(time.Second))
	if c.Stopping {
		b.WriteString("停止已记录，但执行端还没有确认停下；没有结果。\n")
	}
	if g := strings.TrimSpace(c.Goal); g != "" {
		fmt.Fprintf(b, "目标：%s\n", text.Clip(g, 300))
	}
	if c.Landing != "" {
		fmt.Fprintf(b, "改动：%s\n", c.Landing)
	}
	if len(c.Refs) > 0 {
		fmt.Fprintf(b, "refs：%s\n", strings.Join(c.Refs, "；"))
	}
	if a := strings.TrimSpace(c.Answer); a != "" {
		fmt.Fprintf(b, "回答：\n%s\n", text.Clip(a, 4000))
	}
}

func stateWord(state task.State) string {
	switch state {
	case task.StateFailed:
		return "失败"
	case task.StateCancelled:
		return "已取消"
	}
	return "完成"
}

// SetDeliverer installs the channel that carries deliveries: the console
// or the chat, chosen by the conversation. Nil means results are only
// available through steve_await, as before.
func (s *Service) SetDeliverer(fn func(context.Context, Delivery) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliver = fn
}

// SetReplaySafeDelivery identifies channels whose durable ingress deduplicates
// delivery keys even across restarts. Other channels require a receipt.
func (s *Service) SetReplaySafeDelivery(check func(task.Task) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replaySafeDelivery = check
}

// SetDeliveryReceipt installs a read-only durable receipt lookup. Receipts
// remain meaningful after the parent has paused or finished. Lookups must be
// repeatable; observing a receipt never consumes or deletes it.
func (s *Service) SetDeliveryReceipt(check func(task.Task, string) (bool, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveryReceipt = check
}

// collect marks a child's result as read by its parent in-turn: it will
// not be delivered again. Only a terminal result counts.
func (s *Service) collect(taskID string, result agentmcp.DelegateResult) {
	if result.State != task.StateDone && result.State != task.StateFailed && result.State != task.StateCancelled {
		return
	}
	if err := s.tasks.SetDelivery(taskID, task.DeliveryDelivered); err != nil && !strings.Contains(err.Error(), "not found") {
		slog.Error(fmt.Sprintf("delegate: mark task #%s collected: %v", taskID, err), "task", taskID)
	}
}

// flushIfIdle delivers what a parent has waiting, unless a turn of the
// parent is running: that turn's end delivers instead, so a child that
// ends mid-turn is never announced twice.
func (s *Service) flushIfIdle(ctx context.Context, parentID string) {
	if s.attempts != nil {
		if _, live := s.attempts.LiveAttemptOf(ctx, parentID); live {
			return
		}
	}
	s.Flush(ctx, parentID)
}

// Flush sends the parent every finished child it has not been told
// about, in one message. Called when a parent's turn ends and when a
// child ends while no turn runs. Safe to call twice: a child is
// delivered once.
func (s *Service) Flush(ctx context.Context, parentID string) {
	s.flush(ctx, parentID, time.Now(), nil)
}

func (s *Service) flush(ctx context.Context, parentID string, due time.Time, waiting []task.Task) {
	s.mu.Lock()
	deliver, replaySafe, receipt := s.deliver, s.replaySafeDelivery, s.deliveryReceipt
	s.mu.Unlock()
	if deliver == nil {
		return
	}
	s.deliverMu.Lock()
	defer s.deliverMu.Unlock()
	if waiting == nil {
		waiting = s.tasks.Undelivered()[parentID]
	}
	// Re-read only the candidate IDs under the dispatch reservation. A receipt
	// or inline collection may have completed since the shared snapshot.
	waiting = s.currentWaiting(waiting)
	parent, ok := s.tasks.Get(parentID)
	if ok && receipt != nil {
		waiting = s.checkDeliveryReceipts(parent, waiting, receipt)
	}
	if len(waiting) == 0 {
		return
	}
	// A caller already waiting gets the first chance to consume its result.
	// Registration precedes execution, so closing done cannot race an inline
	// response into an additional automatic continuation.
	s.mu.Lock()
	ready := waiting[:0]
	for _, tracked := range waiting {
		if d := tracked.Delivery; d != nil && (d.State == task.DeliveryUncertain || (!due.IsZero() && d.NextAttemptAt.After(due))) {
			continue
		}
		if current := s.pending[tracked.ID]; current != nil && current.waiters > 0 {
			continue
		}
		ready = append(ready, tracked)
	}
	s.mu.Unlock()
	waiting = ready
	if len(waiting) == 0 {
		return
	}
	if !ok || parent.State.Terminal() {
		// Nobody to continue: the results stay on the children's records;
		// the listing shows them. Mark them so they are not retried.
		for _, c := range waiting {
			if err := s.tasks.SetDelivery(c.ID, task.DeliverySuppressed); err != nil {
				slog.Error(fmt.Sprintf("delegate: suppress delivery for task #%s: %v", c.ID, err), "task", c.ID, "parent", parentID)
			}
		}
		return
	}
	if parent.State != task.StateRunning {
		return
	}
	if parent.Held() {
		// The user stopped the task: a delivery would start its next turn
		// on its own. What the children left waits for the user's next
		// turn, which opens with it (Preface).
		return
	}
	ids := make([]string, 0, len(waiting))
	for _, child := range waiting {
		ids = append(ids, child.ID)
	}
	batches, err := s.tasks.PrepareDeliveries(parentID, ids)
	if err != nil {
		slog.Error("delegate: prepare result delivery", "parent", parentID, "error", err)
		return
	}
	landing := s.landFor(ctx, parent, nil)
	for _, batch := range batches {
		if ctx.Err() != nil {
			return
		}
		s.deliverBatch(ctx, deliver, parent, batch, landing, replaySafe != nil && replaySafe(parent))
	}
}

func (s *Service) deliverBatch(ctx context.Context, deliver func(context.Context, Delivery) error, parent task.Task, waiting []task.Task, landing func(task.Task) string, replaySafe bool) {
	d := Delivery{Transport: parent.Transport, Conversation: parent.Channel, ParentTask: parent.ID, Member: parent.Member, ChatID: parent.ChatID,
		Anchor: parent.AnchorMessage, Requester: parent.Requester, ChatType: parent.ChatType, Key: waiting[0].Delivery.Key}
	ids := make([]string, 0, len(waiting))
	for _, c := range waiting {
		d.Children = append(d.Children, Delivered{Task: c.ID, Agent: c.Member, Node: c.Node, State: c.State,
			Elapsed: c.UpdatedAt.Sub(c.CreatedAt), Goal: c.Goal, Answer: c.Result.Answer,
			Refs: withoutLandingTalk(c.Result.Refs), Attempt: c.Result.Attempt, Landing: landing(c)})
		ids = append(ids, c.ID)
	}
	if err := s.tasks.StartDelivery(ids, replaySafe); err != nil {
		slog.Error("delegate: record delivery start", "parent", parent.ID, "error", err)
		return
	}
	state, detail := task.DeliveryDelivered, ""
	if err := deliver(ctx, d); err != nil {
		state, detail = task.DeliveryPending, err.Error()
		if errors.Is(err, channel.ErrDeliveryQueued) {
			state, detail = task.DeliveryQueued, ""
		} else if errors.Is(err, channel.ErrOutcomeUnknown) {
			state = task.DeliveryUncertain
		}
		if state != task.DeliveryQueued {
			slog.Error("delegate: result delivery failed", "parent", parent.ID, "conversation", parent.Channel, "error", err)
		}
	}
	if err := s.tasks.RecordDelivery(ids, state, detail); err != nil {
		slog.Error("delegate: record result delivery", "parent", parent.ID, "error", err)
	} else if state == task.DeliveryDelivered {
		slog.Info(fmt.Sprintf("delegate: delivered %d child result(s) into %s for task #%s", len(d.Children), parent.Channel, parent.ID), "parent", parent.ID, "conversation", parent.Channel)
	}
}

// landFor lands what the project has queued — the parent holds no lock
// between turns, so this is the moment; a turn composing its prompt lends
// its own lease — and answers, per child, where its files are. The
// message must not say "landed" for a child whose landing is still queued
// behind someone else's lock.
func (s *Service) landFor(ctx context.Context, parent task.Task, lease *ledger.Lease) func(task.Task) string {
	byArtifact := map[string]string{}
	held := ""
	if s.artifacts != nil && parent.ProjectID != "" {
		if p, found, err := s.artifacts.Project(ctx, parent.ProjectID); err == nil && found {
			var landed []artifact.Landing
			var lerr error
			if lease != nil {
				landed, lerr = s.artifacts.LandPendingUnder(ctx, p, *lease)
			} else {
				landed, lerr = s.artifacts.LandPending(ctx, p)
			}
			for _, l := range landed {
				switch l.State {
				case "committed":
					byArtifact[l.Artifact] = fmt.Sprintf("已落地主目录，%d 个路径", len(l.Paths))
				case "conflict":
					byArtifact[l.Artifact] = "落地冲突：" + l.Error
				default:
					byArtifact[l.Artifact] = l.State
					if l.Error != "" {
						byArtifact[l.Artifact] += "：" + l.Error
					}
				}
			}
			if lerr != nil {
				if errors.Is(lerr, ledger.ErrHeld) {
					held = "排队中：主目录正被别的回合占用，空出来就落地"
				} else {
					held = "排队中：" + lerr.Error()
				}
			}
		}
	}
	return func(c task.Task) string {
		if c.Result == nil {
			return ""
		}
		artifact := ""
		for _, r := range c.Result.Refs {
			if rest, ok := strings.CutPrefix(r, "artifact "); ok {
				artifact = strings.TrimSpace(rest)
			}
			if strings.HasPrefix(r, "landed into your working directory") {
				return "已落地主目录（在父回合内）"
			}
		}
		if artifact == "" {
			return ""
		}
		if state, ok := byArtifact[artifact]; ok {
			return state
		}
		if held != "" {
			return held
		}
		if c.State == task.StateCancelled {
			// A stop revokes the child's permission to land; what it
			// published stays an artifact.
			return "未落地：任务已停止，落地授权已撤销"
		}
		return "已在此前落地或无改动"
	}
}

// RedeliverPending is the start-up pass: children that ended before the
// process died, whose parents were never told.
func (s *Service) RedeliverPending(ctx context.Context) {
	s.reconcileDeliveries(ctx, time.Now())
}

// ReconcileDeliveries retries durable pending messages while the application is
// running. It does not wake paused/failed parents or replay uncertain sends.
func (s *Service) ReconcileDeliveries(ctx context.Context) error {
	s.reconcileDeliveries(ctx, time.Now())
	return ctx.Err()
}

func (s *Service) reconcileDeliveries(ctx context.Context, now time.Time) {
	for parentID, waiting := range s.tasks.Undelivered() {
		if ctx.Err() != nil {
			return
		}
		if s.attempts != nil {
			if _, live := s.attempts.LiveAttemptOf(ctx, parentID); live {
				continue
			}
		}
		s.flush(ctx, parentID, now, waiting)
	}
}

// withoutLandingTalk drops the refs that said, at the child's end, where
// its files were about to go: the delivery says where they are now.
func withoutLandingTalk(refs []string) []string {
	var out []string
	for _, r := range refs {
		if strings.HasPrefix(r, "queued to land") || strings.HasPrefix(r, "not landed yet") || strings.HasPrefix(r, "landed into your working directory") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ConfirmDelivery records an asynchronous channel receipt without holding the
// dispatch lock. A parent turn may itself flush newly finished children.
func (s *Service) ConfirmDelivery(d Delivery, err error) {
	ids := make([]string, 0, len(d.Children))
	for _, c := range d.Children {
		ids = append(ids, c.Task)
	}
	state, detail := task.DeliveryDelivered, ""
	if err != nil {
		state, detail = task.DeliveryUncertain, err.Error()
	}
	if err := s.tasks.RecordDelivery(ids, state, detail); err != nil {
		slog.Error("delegate: record continuation receipt", "parent", d.ParentTask, "error", err)
	}
}
