package console

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/view"
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

func TestQuestionTextAnswerResumesOriginalRequestAndSurvivesRestart(t *testing.T) {
	for _, withOptions := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "text-or-choice"}[withOptions], func(t *testing.T) {
			dir := t.TempDir()
			book, err := ledger.Open(dir, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { book.Close() })
			s := New(&echo{}, "owner", nil)
			if err := s.Persist(book.Document("console")); err != nil {
				t.Fatal(err)
			}
			question := view.Question{Title: "How should I continue?", Message: "The service is unavailable. I tried reconnecting. You can restore it or choose another machine.", SessionID: "session-1", Generation: 3, AllowFreeText: true}
			if withOptions {
				question.Choices = []view.Choice{{Value: "wait", Label: "Wait for the service"}}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type result struct {
				answer view.Answer
				err    error
			}
			done := make(chan result, 1)
			go func() {
				answer, err := s.askUser(ctx, consoleapi.PendingQuestion{Conversation: "console:original", ExchangeID: "e-original", Project: "scratch", TaskID: "task-1", AttemptID: "attempt-1"}, question)
				done <- result{answer, err}
			}()
			q := pendingForTest(t, s)
			if !q.AllowFreeText || q.Message != question.Message || q.SessionID != question.SessionID || q.Generation != question.Generation {
				t.Fatalf("question lost agent context: %+v", q)
			}
			answer := consoleapi.QuestionAnswer{CommandID: "reply-1", Decision: "accept", Text: "Use the staging machine.\nKeep the existing workspace."}
			if _, err := book.DB().Exec("PRAGMA query_only = ON"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AnswerQuestion(t.Context(), q.ID, answer); err == nil {
				t.Fatal("accepted text without durable storage")
			}
			if got := s.Questions("")[0]; got.State != "pending" || got.Answer != nil {
				t.Fatalf("failed save consumed question: %+v", got)
			}
			select {
			case got := <-done:
				t.Fatalf("failed save resumed request: %+v", got)
			default:
			}
			if _, err := book.DB().Exec("PRAGMA query_only = OFF"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				got, err := s.AnswerQuestion(t.Context(), q.ID, answer)
				if err != nil || got.State != "answered" || got.Answer == nil || *got.Answer != answer || got.ExchangeID != q.ExchangeID || got.TaskID != q.TaskID || got.AttemptID != q.AttemptID {
					t.Fatalf("text answer = %+v, %v", got, err)
				}
			}
			select {
			case got := <-done:
				if got.err != nil || got.answer.Text != answer.Text || got.answer.Value != "" {
					t.Fatalf("callback = %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("answer did not resume original callback")
			}
			if len(s.exchanges) != 0 || len(s.Replies("original")) != 0 {
				t.Fatal("answer created a new submission or transcript reply")
			}
			book.Close()
			book, err = ledger.Open(dir, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			restored := New(&echo{}, "owner", nil)
			if err := restored.Persist(book.Document("console")); err != nil {
				t.Fatal(err)
			}
			if got, err := restored.AnswerQuestion(t.Context(), q.ID, answer); err != nil || got.Answer == nil || *got.Answer != answer {
				t.Fatalf("durable retry = %+v, %v", got, err)
			}
			conflict := answer
			conflict.Text = "Use another machine instead."
			if _, err := restored.AnswerQuestion(t.Context(), q.ID, conflict); !errors.Is(err, consoleapi.ErrQuestionConflict) {
				t.Fatalf("changed answer retry = %v", err)
			}
		})
	}
}

func TestQuestionTextValidationKeepsPendingDecision(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_, _ = s.awaitQuestion(ctx, consoleapi.PendingQuestion{Kind: "question", Project: "scratch", AllowFreeText: true, Options: []consoleapi.QuestionOption{{ID: "wait", Label: "Wait"}}})
	}()
	q := pendingForTest(t, s)
	for _, bad := range []consoleapi.QuestionAnswer{
		{Decision: "accept", Text: "missing command id"},
		{CommandID: "empty", Decision: "accept"},
		{CommandID: "blank", Decision: "accept", Text: " \n\t"},
		{CommandID: "too-long", Decision: "accept", Text: strings.Repeat("x", (64<<10)+1)},
		{CommandID: "both", Decision: "accept", Choice: "wait", Text: "run now"},
		{CommandID: "forged", Decision: "accept", Choice: "not-offered"},
		{CommandID: "decline-text", Decision: "decline", Text: "run now"},
		{CommandID: "cancel-text", Decision: "cancel", Text: "run now"},
		{CommandID: "unknown", Decision: "unknown", Text: "run now"},
	} {
		if _, err := s.AnswerQuestion(t.Context(), q.ID, bad); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
			t.Errorf("%s error = %v", bad.CommandID, err)
		}
		if got := s.Questions("")[0]; got.State != "pending" || got.Answer != nil {
			t.Fatalf("invalid reply consumed the question: %+v", got)
		}
	}
	answer := consoleapi.QuestionAnswer{CommandID: "wait", Decision: "accept", Choice: "wait"}
	if _, err := s.AnswerQuestion(t.Context(), q.ID, answer); err != nil {
		t.Fatal(err)
	}
	if len(s.exchanges) != 0 {
		t.Fatal("selecting the agent's wait option created another submission")
	}
}

func TestPermissionAndChoiceOnlyQuestionRejectText(t *testing.T) {
	for _, kind := range []string{"permission", "question"} {
		t.Run(kind, func(t *testing.T) {
			s := New(&echo{}, "owner", nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() {
				_, _ = s.askUser(ctx, consoleapi.PendingQuestion{Project: "scratch"}, view.Question{Kind: kind, AllowFreeText: kind == "permission", Choices: []view.Choice{{Value: "allow", Label: "Allow once"}}})
			}()
			q := pendingForTest(t, s)
			if q.AllowFreeText {
				t.Fatal("permission request exposed text authorization")
			}
			if _, err := s.AnswerQuestion(t.Context(), q.ID, consoleapi.QuestionAnswer{CommandID: "text", Decision: "accept", Text: "allow"}); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
				t.Fatalf("text authorization returned %v", err)
			}
			if _, err := s.AnswerQuestion(t.Context(), q.ID, consoleapi.QuestionAnswer{CommandID: "choice", Decision: "accept", Choice: "allow"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQuestionWithoutLiveCallbackDoesNotAcceptAnAnswer(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	s.questions["orphan"] = consoleapi.PendingQuestion{ID: "orphan", Principal: "owner", State: "pending", Kind: "question", AllowFreeText: true, Deadline: time.Now().Add(time.Hour)}
	if _, err := s.AnswerQuestion(t.Context(), "orphan", consoleapi.QuestionAnswer{CommandID: "reply", Decision: "accept", Text: "Continue"}); !errors.Is(err, consoleapi.ErrQuestionConflict) {
		t.Fatalf("answer without live callback = %v", err)
	}
	if got := s.Questions("")[0]; got.State != "interrupted" || got.Answer != nil {
		t.Fatalf("orphan question = %+v", got)
	}
	if _, err := s.AnswerQuestion(t.Context(), "unknown", consoleapi.QuestionAnswer{CommandID: "reply", Decision: "accept", Text: "Continue"}); !errors.Is(err, consoleapi.ErrQuestionNotFound) {
		t.Fatalf("unknown question = %v", err)
	}
}

func TestRecoveryQuestionWaitsUntilTheUserDecides(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	s.questionTimeout = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan consoleapi.PendingQuestion, 1)
	go func() {
		q, _ := s.awaitQuestion(ctx, consoleapi.PendingQuestion{Kind: "recovery", Project: "p", Options: []consoleapi.QuestionOption{{ID: "retry"}}})
		done <- q
	}()
	q := pendingForTest(t, s)
	if !q.Deadline.IsZero() {
		t.Fatalf("recovery question has an automatic deadline: %v", q.Deadline)
	}
	time.Sleep(20 * time.Millisecond)
	if got := s.Questions("")[0]; got.State != "pending" {
		t.Fatalf("recovery expired without a decision: %+v", got)
	}
	if _, err := s.AnswerQuestion(ctx, q.ID, consoleapi.QuestionAnswer{CommandID: "retry", Decision: "accept", Choice: "retry"}); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.State != "answered" {
		t.Fatalf("recovery decision: %+v", got)
	}
}

func TestNodeOwnedQuestionReplaysDurableAnswerInsteadOfAskingAgain(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := New(&echo{}, "owner", nil)
	if err := s.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	base := consoleapi.PendingQuestion{Conversation: "console:original", ExchangeID: "e1", Project: "p", TaskID: "task-1", AttemptID: "attempt-1"}
	question := view.Question{SessionID: "ns_" + strings.Repeat("a", 64), RequestID: "nq_" + strings.Repeat("b", 64), Title: "Recovery", Message: "How should I continue?", AllowFreeText: true}
	done := make(chan view.Answer, 1)
	go func() { answer, _ := s.askUser(t.Context(), base, question); done <- answer }()
	q := pendingForTest(t, s)
	if !q.Deadline.IsZero() {
		t.Fatalf("node-owned question can expire while its execution waits: %v", q.Deadline)
	}
	if _, err := s.AnswerQuestion(t.Context(), q.ID, consoleapi.QuestionAnswer{CommandID: "answer-1", Decision: "accept", Text: "Use the staging machine."}); err != nil {
		t.Fatal(err)
	}
	if answer := <-done; answer.Text != "Use the staging machine." {
		t.Fatal(answer)
	}
	restored := New(&echo{}, "owner", nil)
	if err := restored.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	question.Generation = 99
	answer, err := restored.askUser(ctx, base, question)
	if err != nil || answer.Text != "Use the staging machine." || len(restored.Questions("")) != 1 {
		t.Fatalf("durable answer was asked again: %+v %v", answer, err)
	}
	question.Message = "Changed operation requiring another decision"
	if _, err := restored.askUser(ctx, base, question); !errors.Is(err, consoleapi.ErrQuestionConflict) {
		t.Fatalf("changed question reused approval: %v", err)
	}
}

func TestNodeOwnedQuestionRebindsTheSameUnansweredQuestionID(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	base := consoleapi.PendingQuestion{Conversation: "console:original", ExchangeID: "e1", Project: "p", TaskID: "task-1", AttemptID: "attempt-1"}
	question := view.Question{SessionID: "ns_" + strings.Repeat("a", 64), RequestID: "nq_" + strings.Repeat("c", 64), Message: "Choose a machine", Choices: []view.Choice{{Value: "staging", Label: "Staging"}}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _, _ = s.askUser(ctx, base, question) }()
	first := pendingForTest(t, s)
	cancel()
	<-done
	restored := New(&echo{}, "owner", nil)
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	resumed := make(chan view.Answer, 1)
	go func() { answer, _ := restored.askUser(t.Context(), base, question); resumed <- answer }()
	second := pendingForTest(t, restored)
	if second.ID != first.ID || second.RequestID != question.RequestID {
		t.Fatalf("unanswered node question duplicated: old=%s new=%+v", first.ID, second)
	}
	if _, err := restored.AnswerQuestion(t.Context(), second.ID, consoleapi.QuestionAnswer{CommandID: "answer", Decision: "accept", Choice: "staging"}); err != nil {
		t.Fatal(err)
	}
	if answer := <-resumed; answer.Value != "staging" {
		t.Fatal(answer)
	}
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
