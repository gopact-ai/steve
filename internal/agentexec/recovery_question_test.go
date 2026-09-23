package agentexec

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
)

func TestBlockedAsksInTheCatalogLanguage(t *testing.T) {
	record := attempt.Record{Spec: attempt.Spec{ID: "att-1", TaskID: "task-1"}}
	for _, tc := range []struct {
		text          i18n.Catalog
		title, retry  string
		attempted     string
		stopUncertain string
	}{
		{i18n.Catalog{}, "继续执行需要你的处理", "重新检查原执行", "已尝试：联系原节点。", "原执行状态尚未完整确认，不能重新发送原任务。"},
		{i18n.New(i18n.LocaleEN), "Continuing this execution needs your decision", "Recheck the original execution", "Tried: 联系原节点.", "The original execution's state is not fully confirmed, so its task cannot be sent again."},
	} {
		q := BlockedIn(tc.text, record, "offline", "联系原节点", "原节点暂时离线。", "建议恢复节点后重新检查。", nil).Question
		if q.RequestID != "execution-recovery/att-1/offline" || q.Kind != "recovery" || q.Title != tc.title || !q.Required || !q.AllowFreeText {
			t.Fatalf("question = %+v", q)
		}
		if !strings.HasPrefix(q.Message, tc.attempted) || !strings.Contains(q.Message, tc.stopUncertain) {
			t.Fatalf("message = %q", q.Message)
		}
		if len(q.Choices) != 2 || q.Choices[0].Value != "retry" || q.Choices[0].Label != tc.retry || q.Choices[1].Value != "wait" {
			t.Fatalf("choices = %+v", q.Choices)
		}
	}
	// Blocked speaks the default language, as the execution layer has no
	// request of its own to take one from.
	if got, want := Blocked(record, "offline", "a", "b", "c", nil).Question, BlockedIn(i18n.Catalog{}, record, "offline", "a", "b", "c", nil).Question; got.Title != want.Title || got.Message != want.Message {
		t.Fatalf("Blocked = %+v, want %+v", got, want)
	}
}
