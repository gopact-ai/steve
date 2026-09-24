package agentexec

import (
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/view"
)

var offline = Diagnosis{Attempted: i18n.ExecTriedReachStepNode, Problem: i18n.ExecProblemStepUnsafe, Recommendation: i18n.ExecAdviceRestoreNode}

func TestBlockedAsksInTheCatalogLanguage(t *testing.T) {
	record := attempt.Record{Spec: attempt.Spec{ID: "att-1", TaskID: "task-1"}}
	for _, tc := range []struct {
		text          i18n.Catalog
		title, retry  string
		attempted     string
		stopUncertain string
	}{
		{i18n.New(i18n.LocaleZH), "继续执行需要你的处理", "重新检查原执行", "已尝试：连接原步骤的节点并核对已接受命令。", "原执行状态尚未完整确认，不能重新发送原任务。"},
		{i18n.New(i18n.LocaleEN), "Continuing this execution needs your decision", "Recheck the original execution", "Tried: Reach the original step's node and check the accepted command.", "The original execution's state is not fully confirmed, so its task cannot be sent again."},
	} {
		q := BlockedIn(tc.text, record, "offline", offline, nil).Question
		if q.RequestID != "execution-recovery/att-1/offline" || q.Kind != "recovery" || q.Title != tc.title || !q.Required || !q.AllowFreeText {
			t.Fatalf("question = %+v", q)
		}
		if !strings.HasPrefix(q.Message, tc.attempted) || !strings.Contains(q.Message, tc.stopUncertain) || !strings.Contains(q.Message, tc.text.T(offline.Problem)) || !strings.HasSuffix(q.Message, tc.text.T(offline.Recommendation)) {
			t.Fatalf("message = %q", q.Message)
		}
		if len(q.Choices) != 2 || q.Choices[0].Value != "retry" || q.Choices[0].Label != tc.retry || q.Choices[1].Value != "wait" {
			t.Fatalf("choices = %+v", q.Choices)
		}
	}
}

// The execution layer has no request of its own to take a language from, so
// it asks in English; the exchange it reaches asks it again in its own.
func TestBlockedIsEnglishUntilAskedInAnotherLanguage(t *testing.T) {
	record := attempt.Record{Spec: attempt.Spec{ID: "att-1", TaskID: "task-1"}}
	cause := errors.New("node unreachable")
	blocked := Blocked(record, "offline", offline, cause)
	if want := BlockedIn(i18n.New(i18n.LocaleEN), record, "offline", offline, cause).Question; blocked.Question.Title != want.Title || blocked.Question.Message != want.Message || containsHan(blocked.Question.Message) {
		t.Fatalf("Blocked = %+v, want %+v", blocked.Question, want)
	}
	zh := blocked.In(i18n.New(i18n.LocaleZH))
	if want := BlockedIn(i18n.New(i18n.LocaleZH), record, "offline", offline, cause); zh.Question.Message != want.Question.Message || zh.Question.RequestID != blocked.Question.RequestID {
		t.Fatalf("In(zh) = %+v, want %+v", zh.Question, want.Question)
	}
	if zh.AttemptID != "att-1" || zh.TaskID != "task-1" || !errors.Is(zh, cause) {
		t.Fatalf("In lost the block's attempt or cause: %+v", zh)
	}
	own := &RecoveryBlocked{Question: RecoveryQuestion("recovery/x", "t", "m", view.Choice{}, view.Choice{})}
	if own.In(i18n.New(i18n.LocaleZH)) != own {
		t.Fatal("a block without a diagnosis was rebuilt")
	}
}
