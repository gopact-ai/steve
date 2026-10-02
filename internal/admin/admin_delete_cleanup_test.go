package admin

import (
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
)

func TestPendingCleanupDeletionRefusalSaysToWaitForTheOriginalExecution(t *testing.T) {
	for _, locale := range i18n.Locales() {
		text := i18n.New(locale)
		err := deleteRefusal(text, task.ErrRetirementPending)
		if !errors.Is(err, task.ErrRetirementPending) {
			t.Fatal("cleanup refusal lost its cause")
		}
		if locale == i18n.LocaleEN {
			if !strings.Contains(err.Error(), "cleanup") || !strings.Contains(err.Error(), "delete") {
				t.Fatalf("cleanup refusal is not actionable: %v", err)
			}
		} else if !strings.Contains(err.Error(), "原执行") || !strings.Contains(err.Error(), "删除") {
			t.Fatalf("cleanup refusal is not actionable: %v", err)
		}
	}
}
