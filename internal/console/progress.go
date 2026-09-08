package console

import (
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/view"
)

// progressEvery bounds token repaints, including the last chunk before a
// pause. Status changes and the first visible content bypass this interval.
const progressEvery = 100 * time.Millisecond

// progressStream owns both the turn's current process and its pending repaint.
// Publishing under mu makes Close a barrier: no progress can follow the reply.
type progressStream struct {
	mu         sync.Mutex
	work       *process
	publish    func(consoleapi.Progress)
	latest     consoleapi.Progress
	published  consoleapi.Progress
	last       time.Time
	timer      *time.Timer
	generation uint64
	pending    bool
	closed     bool
}

func (s *Service) progress(conversation, exchangeID string, work *process) *progressStream {
	return &progressStream{work: work, publish: func(p consoleapi.Progress) {
		if s.model != nil {
			s.model.Publish(readmodel.Event{Kind: "console.progress", Conversation: conversation, ExchangeID: exchangeID, Progress: &p})
		}
	}}
}

func (s *progressStream) Update(p view.Progress) {
	cut := readmodel.FromProgress(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	cut.Phase = s.latest.Phase
	s.latest = cut
	s.work.turn(cut)
	s.pending = true
	if s.last.IsZero() || time.Since(s.last) >= progressEvery || progressStatusChanged(s.published, cut) {
		s.flush()
		return
	}
	if s.timer == nil {
		s.generation++
		generation := s.generation
		s.timer = time.AfterFunc(time.Until(s.last.Add(progressEvery)), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if !s.closed && generation == s.generation {
				s.flush()
			}
		})
	}
}

// Phase flushes pending text before reporting a lifecycle transition, so
// finalization never hides the agent's last words behind result persistence.
func (s *progressStream) Phase(phase view.Phase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.latest.Phase == string(phase) {
		return
	}
	s.latest.Phase = string(phase)
	s.pending = true
	s.flush()
}

func (s *progressStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.flush()
	s.closed = true
}

// flush requires mu. Generation also invalidates a stopped timer whose
// callback already started and is waiting for this mutex.
func (s *progressStream) flush() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.generation++
	if !s.pending {
		return
	}
	s.pending = false
	s.published = s.latest
	s.last = time.Now()
	s.publish(s.latest)
}

func progressStatusChanged(prev, next consoleapi.Progress) bool {
	if (prev.Answer == "" && next.Answer != "") || (prev.Reasoning == "" && next.Reasoning != "") || (len(prev.Timeline) == 0 && len(next.Timeline) > 0) || len(prev.Tools) != len(next.Tools) || len(prev.Plan) != len(next.Plan) {
		return true
	}
	for i, tool := range next.Tools {
		if tool.ID != prev.Tools[i].ID || tool.Status != prev.Tools[i].Status {
			return true
		}
	}
	for i, step := range next.Plan {
		if step.Status != prev.Plan[i].Status {
			return true
		}
	}
	return false
}
