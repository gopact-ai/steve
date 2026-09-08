package delegate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
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

// Delivered is one child in a delivery.
type Delivered struct {
	Task    string
	Agent   string
	Node    string
	State   task.State // done | failed
	Elapsed time.Duration
	Goal    string
	Answer  string
	Refs    []string
	Attempt string
	// Landing says where the child's files are: landed, queued (with why),
	// conflict, or empty when it changed nothing.
	Landing string
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
		fmt.Fprintf(&b, "\n## 子任务 #%s · %s@%s · %s · 用时 %s\n", c.Task, c.Agent, nodeLabel(c.Node), stateWord(c.State), c.Elapsed.Round(time.Second))
		if g := strings.TrimSpace(c.Goal); g != "" {
			fmt.Fprintf(&b, "目标：%s\n", clipRunes(g, 300))
		}
		if c.Landing != "" {
			fmt.Fprintf(&b, "改动：%s\n", c.Landing)
		}
		if len(c.Refs) > 0 {
			fmt.Fprintf(&b, "refs：%s\n", strings.Join(c.Refs, "；"))
		}
		if a := strings.TrimSpace(c.Answer); a != "" {
			fmt.Fprintf(&b, "回答：\n%s\n", clipRunes(a, 4000))
		}
	}
	b.WriteString("\n继续你的任务。还在跑的子任务结束后会再送来，不必用 steve_await 等；都齐了就汇总回复。")
	return b.String()
}

func stateWord(state task.State) string {
	if state == task.StateFailed {
		return "失败"
	}
	return "完成"
}

func clipRunes(text string, limit int) string {
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	return string(r[:limit]) + "…"
}

// SetDeliverer installs the channel that carries deliveries: the console
// or the chat, chosen by the conversation. Nil means results are only
// available through steve_await, as before.
func (s *Service) SetDeliverer(fn func(context.Context, Delivery) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliver = fn
}

// collect marks a child's result as read by its parent in-turn: it will
// not be delivered again. Only a terminal result counts.
func (s *Service) collect(taskID string, result agentmcp.DelegateResult) {
	if result.State != task.StateDone && result.State != task.StateFailed {
		return
	}
	if err := s.tasks.SetDelivery(taskID, task.DeliveryDelivered); err != nil && !strings.Contains(err.Error(), "not found") {
		log.Printf("delegate: mark task #%s collected: %v", taskID, err)
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
	s.mu.Lock()
	deliver := s.deliver
	s.mu.Unlock()
	if deliver == nil {
		return
	}
	s.deliverMu.Lock()
	defer s.deliverMu.Unlock()
	waiting := s.tasks.Undelivered()[parentID]
	// A caller already waiting gets the first chance to consume its result.
	// Registration precedes execution, so closing done cannot race an inline
	// response into an additional automatic continuation.
	s.mu.Lock()
	ready := waiting[:0]
	for _, tracked := range waiting {
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
	parent, ok := s.tasks.Get(parentID)
	if !ok || parent.Finished() || parent.State == task.StatePaused {
		// Nobody to continue: the results stay on the children's records;
		// the listing shows them. Mark them so they are not retried.
		for _, c := range waiting {
			_ = s.tasks.SetDelivery(c.ID, task.DeliveryDelivered)
		}
		return
	}
	landing := s.landFor(ctx, parent)
	d := Delivery{Conversation: parent.Channel, ParentTask: parent.ID, Member: parent.Member, ChatID: parent.ChatID,
		Anchor: parent.AnchorMessage, Requester: parent.Requester, ChatType: parent.ChatType}
	for _, c := range waiting {
		elapsed := c.UpdatedAt.Sub(c.CreatedAt)
		dc := Delivered{Task: c.ID, Agent: c.Member, Node: c.Node, State: c.State, Elapsed: elapsed, Goal: c.Goal,
			Answer: c.Result.Answer, Refs: withoutLandingTalk(c.Result.Refs), Attempt: c.Result.Attempt}
		if c.State == task.StateDone {
			dc.State = task.StateDone
		} else {
			dc.State = task.StateFailed
		}
		dc.Landing = landing(c)
		d.Children = append(d.Children, dc)
		if err := s.tasks.SetDelivery(c.ID, task.DeliveryPending); err != nil {
			log.Printf("delegate: mark task #%s pending delivery: %v", c.ID, err)
		}
	}
	d.Key = task.DeliveryKey(waiting[0].ID)
	if err := deliver(ctx, d); err != nil {
		log.Printf("delegate: deliver %d child result(s) to task #%s: %v", len(d.Children), parentID, err)
		return
	}
	for _, c := range waiting {
		if err := s.tasks.SetDelivery(c.ID, task.DeliveryDelivered); err != nil {
			log.Printf("delegate: mark task #%s delivered: %v", c.ID, err)
		}
	}
	log.Printf("delegate: delivered %d child result(s) into %s for task #%s", len(d.Children), parent.Channel, parentID)
}

// landFor lands what the project has queued — the parent holds no lock
// between turns, so this is the moment — and answers, per child, where
// its files are. The message must not say "landed" for a child whose
// landing is still queued behind someone else's lock.
func (s *Service) landFor(ctx context.Context, parent task.Task) func(task.Task) string {
	byArtifact := map[string]string{}
	held := ""
	if s.artifacts != nil && parent.ProjectID != "" {
		if p, found, err := s.artifacts.Project(ctx, parent.ProjectID); err == nil && found {
			landed, lerr := s.artifacts.LandPending(ctx, p)
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
		return "已在此前落地或无改动"
	}
}

// RedeliverPending is the start-up pass: children that ended before the
// process died, whose parents were never told.
func (s *Service) RedeliverPending(ctx context.Context) {
	for parentID := range s.tasks.Undelivered() {
		s.flushIfIdle(ctx, parentID)
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
