package console

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/turn"
)

type relocationDriver interface {
	PlanRelocation(context.Context, string, turn.Request) (turn.RelocationPlan, error)
	RelocateChat(context.Context, string, string, turn.Request) (turn.Result, error)
}

// A crash after persisting the exact plan choice must not ask for the same
// authorization again. General recovery answers and free-form advice carry
// no replacement authority and are deliberately excluded.
func (s *Service) approvedRelocation(binding consoleapi.PendingQuestion, planID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	choice := "confirm-stopped-and-retry:" + planID
	for _, q := range s.questions {
		if q.Kind != "recovery" || q.State != "answered" || q.RequestID != planID || q.Principal != s.owner || q.Conversation != binding.Conversation || q.ExchangeID != binding.ExchangeID || q.Project != binding.Project || q.TaskID != binding.TaskID || q.AttemptID != binding.AttemptID {
			continue
		}
		if q.Answer != nil && q.Answer.Decision == "accept" && q.Answer.Choice == choice && q.Answer.Text == "" {
			return choice
		}
	}
	return ""
}

// relocationInput carries only the original frozen input and earlier replies
// from this same project. A later user edit or an unrelated project is not
// silently folded into an already approved recovery plan.
func (s *Service) relocationInput(ctx context.Context, e Exchange, projectID, principal string) (*turn.RelocationContext, []harness.Media, error) {
	if projectID == "" || (e.ExpectedProject != "" && e.ExpectedProject != projectID) {
		return nil, nil, errors.New("original project binding is unavailable")
	}
	if e.ExpectedProject == "" {
		e.ExpectedProject = projectID
	}
	materials, media, err := s.executionMaterials(ctx, e, principal)
	if err != nil {
		return nil, nil, err
	}
	quotes, err := s.quoteBlock(ctx, e.Conversation, e.Quotes)
	if err != nil {
		return nil, nil, err
	}
	input := e.Input
	if e.Prompt != "" {
		input = e.Prompt
	}
	s.mu.Lock()
	var list []recoveryLine
	for _, reply := range s.replies[e.Conversation] {
		if reply.ExchangeID == e.ID {
			break
		}
		belongs := reply.ProjectID == projectID || reply.Injected != nil && reply.Injected.Project == projectID
		if !belongs {
			continue
		}
		if reply.Kind == "reply" && reply.ExchangeID != "" {
			for _, previous := range s.exchanges[e.Conversation] {
				if previous.ID == reply.ExchangeID && previous.Input != "" {
					list = append(list, recoveryLine{Kind: "原用户输入", Text: previous.Input})
					break
				}
			}
		}
		if reply.Input != "" {
			list = append(list, recoveryLine{Kind: "用户", Text: reply.Input})
		}
		if reply.Text != "" {
			list = append(list, recoveryLine{Kind: "执行记录", Text: reply.Text})
		}
	}
	s.mu.Unlock()
	var history strings.Builder
	for _, line := range list {
		if history.Len()+len(line.Text) > 192<<10 {
			return nil, nil, errors.New("原会话历史超过恢复上下文上限，需要先提供可引用的摘要")
		}
		fmt.Fprintf(&history, "%s：\n%s\n\n", line.Kind, line.Text)
	}
	return &turn.RelocationContext{Input: quotes + materials + input, History: history.String()}, media, nil
}

type recoveryLine struct{ Kind, Text string }
