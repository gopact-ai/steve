package console

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func pendingForTest(t *testing.T, s *Service) consoleapi.PendingQuestion {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		for _, q := range s.Questions("") {
			if q.State == "pending" {
				return q
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("question did not become pending")
	return consoleapi.PendingQuestion{}
}

func TestQuestionDecisionIsDurableValidatedAndSingleWinner(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	doc := &brokenQueueDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan consoleapi.PendingQuestion, 1)
	go func() {
		q, _ := s.awaitQuestion(ctx, consoleapi.PendingQuestion{Conversation: "console:a", ExchangeID: "e1", Project: "scratch", Options: []consoleapi.QuestionOption{{ID: "Blue", Label: "Blue"}}})
		result <- q
	}()
	q := pendingForTest(t, s)
	answer := consoleapi.QuestionAnswer{CommandID: "choice-1", Decision: "accept", Choice: "Blue"}
	if _, err := s.AnswerQuestionAs(t.Context(), "intruder", q.ID, answer); !errors.Is(err, consoleapi.ErrQuestionForbidden) {
		t.Fatal(err)
	}
	bad := answer
	bad.Choice = "forged"
	if _, err := s.AnswerQuestion(t.Context(), q.ID, bad); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
		t.Fatal(err)
	}
	doc.muErr.Lock()
	doc.err = errors.New("disk unavailable")
	doc.muErr.Unlock()
	if _, err := s.AnswerQuestion(t.Context(), q.ID, answer); err == nil {
		t.Fatal("answered without durability")
	}
	if s.Questions("")[0].State != "pending" {
		t.Fatal("failed save changed the decision")
	}
	doc.muErr.Lock()
	doc.err = nil
	doc.muErr.Unlock()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AnswerQuestion(t.Context(), q.ID, answer); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := <-result; got.State != "answered" || got.Answer.Choice != "Blue" {
		t.Fatalf("answer %+v", got)
	}
	conflict := answer
	conflict.CommandID = "choice-2"
	if _, err := s.AnswerQuestion(t.Context(), q.ID, conflict); !errors.Is(err, consoleapi.ErrQuestionConflict) {
		t.Fatal(err)
	}
	restored := New(&echo{}, "owner", nil)
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if got, err := restored.AnswerQuestion(t.Context(), q.ID, answer); err != nil || got.State != "answered" {
		t.Fatalf("replay %+v %v", got, err)
	}
}

func TestQuestionCancellationExpiryAndRestartCloseOldRequests(t *testing.T) {
	for _, mode := range []string{"cancel", "expire", "restart"} {
		t.Run(mode, func(t *testing.T) {
			s := New(&echo{}, "owner", nil)
			doc := &memDoc{}
			if err := s.Persist(doc); err != nil {
				t.Fatal(err)
			}
			if mode == "expire" {
				s.questionTimeout = 30 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = s.awaitQuestion(ctx, consoleapi.PendingQuestion{Conversation: "console:a", ExchangeID: "e1", Project: "scratch", Options: []consoleapi.QuestionOption{{ID: "yes"}}})
			}()
			q := pendingForTest(t, s)
			if mode == "restart" {
				restored := New(&echo{}, "owner", nil)
				if err := restored.Persist(doc); err != nil {
					t.Fatal(err)
				}
				if got := restored.Questions("")[0]; got.State != "interrupted" {
					t.Fatalf("restored %+v", got)
				}
				if _, err := restored.AnswerQuestion(t.Context(), q.ID, consoleapi.QuestionAnswer{CommandID: "late", Decision: "accept", Choice: "yes"}); !errors.Is(err, consoleapi.ErrQuestionConflict) {
					t.Fatal(err)
				}
				cancel()
			} else if mode == "cancel" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("question callback leaked")
			}
			want := "cancelled"
			if mode == "expire" {
				want = "expired"
			}
			if got := s.Questions("")[0].State; got != want {
				t.Fatalf("state %s", got)
			}
		})
	}
}
