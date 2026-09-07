package console

import (
	"context"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/schedule"
)

// EnqueueScheduled durably accepts one occurrence on the original console
// channel. Its fixed execution metadata and deduplication key commit together.
func (s *Service) EnqueueScheduled(ctx context.Context, firing schedule.Firing) (Exchange, error) {
	if firing.Channel != "console" {
		return Exchange{}, fmt.Errorf("schedule %s is not a console schedule", firing.ID)
	}
	if firing.Key == "" || firing.Member == "" || firing.ProjectID == "" || firing.Requester == "" || !strings.HasPrefix(firing.ConversationID, Prefix) {
		return Exchange{}, fmt.Errorf("schedule %s has incomplete console context", firing.ID)
	}
	s.mu.Lock()
	previous, err := s.submittedLocked(firing.ConversationID, firing.Key, "")
	if previous != nil {
		exchange := copyExchange(previous.Exchange)
		s.mu.Unlock()
		return exchange, nil
	}
	s.mu.Unlock()
	if err != nil {
		return Exchange{}, err
	}
	if firing.Requester != s.owner {
		return Exchange{}, fmt.Errorf("schedule %s belongs to another owner", firing.ID)
	}
	if s.inspector == nil {
		return Exchange{}, fmt.Errorf("schedule %s project cannot be checked", firing.ID)
	}
	if current := s.inspector.ProjectOf(ctx, firing.ConversationID); current != firing.ProjectID {
		return Exchange{}, fmt.Errorf("scheduled project is %s, but this conversation is now bound to %s; scheduled work was not queued", firing.ProjectID, current)
	}
	_, exchange, err := s.enqueue(ctx, firing.ConversationID, "定时任务 #"+firing.ID+" · "+firing.Prompt, nil, enqueueOptions{
		Key: firing.Key, Prompt: "@" + firing.Member + " " + firing.Prompt,
		Origin: "schedule:" + firing.ID, Requester: firing.Requester, ExpectedProject: firing.ProjectID,
	})
	return exchange, err
}
