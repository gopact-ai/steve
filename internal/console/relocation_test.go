package console

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type approvedPlanDriver struct {
	recoveryDriver
	plan turn.RelocationPlan
}

func (d *approvedPlanDriver) PlanRelocation(context.Context, string, turn.Request) (turn.RelocationPlan, error) {
	return d.plan, nil
}
func (d *approvedPlanDriver) RelocateChat(_ context.Context, id, choice string, req turn.Request) (turn.Result, error) {
	if choice != "confirm-stopped-and-retry:"+id {
		panic("replacement used another decision")
	}
	return turn.Result{Text: "continued original task", Attempt: "replacement"}, nil
}

func TestRestartConsumesOnlyExactPersistedRelocationApproval(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-plan", true: "changed-plan"}[changed], func(t *testing.T) {
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			doc := recoveryDocument()
			s := New(&echo{}, "owner", nil)
			s.EnableRetainedRecovery(lifetime)
			if err := s.Persist(doc); err != nil {
				t.Fatal(err)
			}
			binding := consoleapi.PendingQuestion{Conversation: "console:main", ExchangeID: "e1", Project: "p", TaskID: "task-1", AttemptID: "attempt-1"}
			question := view.Question{RequestID: "relocate-original", Title: "恢复原任务", Message: "已检查快照与目标；确认停止后继续。", Required: true, Choices: []view.Choice{{Value: "confirm-stopped-and-retry:relocate-original", Label: "按此方案重试"}}}
			answered := make(chan error, 1)
			go func() { _, err := s.RequestRecovery(lifetime, binding, question); answered <- err }()
			var pending consoleapi.PendingQuestion
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
				list := s.Questions("main")
				if len(list) > 0 {
					pending = list[0]
					break
				}
				time.Sleep(time.Millisecond)
			}
			if pending.ID == "" {
				t.Fatal("plan question missing")
			}
			if _, err := s.AnswerQuestion(t.Context(), pending.ID, consoleapi.QuestionAnswer{CommandID: "accept-exact-plan", Decision: "accept", Choice: question.Choices[0].Value}); err != nil {
				t.Fatal(err)
			}
			if err := <-answered; err != nil {
				t.Fatal(err)
			}
			cancel()
			lifetime, stop := context.WithCancel(t.Context())
			defer stop()
			restarted := New(&echo{}, "owner", nil)
			restarted.EnableRetainedRecovery(lifetime)
			if err := restarted.Persist(doc); err != nil {
				t.Fatal(err)
			}
			if changed {
				question.RequestID = "relocate-changed"
				question.Choices[0].Value = "confirm-stopped-and-retry:" + question.RequestID
			}
			driver := &approvedPlanDriver{plan: turn.RelocationPlan{ID: question.RequestID, Question: question}, recoveryDriver: recoveryDriver{candidates: []turn.RetainedChat{{AttemptID: "attempt-1", TaskID: "task-1", Conversation: "console:main", MessageID: "web-e1", AgentID: "worker", ProjectID: "p"}}, resume: func(context.Context, string, turn.Request) (turn.Result, error) {
				return turn.Result{}, &turn.RecoveryBlocked{Question: view.Question{Message: "source unavailable"}}
			}}}
			if err := restarted.RecoverChats(lifetime, driver); err != nil {
				t.Fatal(err)
			}
			if !changed {
				if e := awaitExchange(t, restarted, "e1"); e.State != "done" {
					t.Fatalf("persisted exact approval not consumed: %+v", e)
				}
				if len(restarted.Questions("main")) != 1 {
					t.Fatal("asked the same plan again after restart")
				}
			} else {
				var next consoleapi.PendingQuestion
				for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
					for _, q := range restarted.Questions("main") {
						if q.State == "pending" {
							next = q
						}
					}
					if next.ID != "" {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if next.RequestID != question.RequestID {
					t.Fatalf("changed plan used stale authority: %+v", next)
				}
			}
			stop()
			cleanup, finish := context.WithTimeout(t.Context(), 2*time.Second)
			defer finish()
			if err := restarted.Shutdown(cleanup); err != nil {
				t.Fatal(err)
			}
		})
	}
}
