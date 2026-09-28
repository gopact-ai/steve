package admin

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/sshconnect"
	"github.com/gopact-ai/steve/internal/task"
)

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// A console change that cannot happen says why in the language of the
// request it came in.
func TestRefusalsAreInTheRequestLanguage(t *testing.T) {
	admin := nodeAdminFixture(t)
	for _, tc := range []struct {
		locale i18n.Locale
		han    bool
	}{{i18n.LocaleEN, false}, {i18n.LocaleZH, true}} {
		ctx := i18n.WithLocale(t.Context(), tc.locale)
		refusals := []error{}
		_, err := admin.AddNode(ctx, consoleapi.AddNodeRequest{Name: "Bad Name", Addr: "127.0.0.1:1"})
		refusals = append(refusals, err)
		refusals = append(refusals, admin.RemoveNode(ctx, "node-missing"))
		refusals = append(refusals, admin.RemoveNode(ctx, admin.NodeName))
		_, err = CheckProjectDir(textFor(ctx), "/abs/dir")
		refusals = append(refusals, err)
		for i, err := range refusals {
			if err == nil || containsHan(err.Error()) != tc.han {
				t.Fatalf("%s refusal %d = %v", tc.locale, i, err)
			}
		}
	}
}

// A refusal said in the reader's language still carries the error it
// stands for.
func TestRefusalsKeepTheirCause(t *testing.T) {
	en := i18n.New(i18n.LocaleEN)
	refused := deleteRefusal(en, fmt.Errorf("wrapped: %w", task.ErrExecuting))
	if !errors.Is(refused, task.ErrExecuting) || containsHan(refused.Error()) {
		t.Fatalf("delete refusal = %v", refused)
	}
	preparing := saidError{en.T(i18n.AdminCopyInProgress, "api", "node-b"), ErrWorkspacePreparing}
	if !errors.Is(preparing, ErrWorkspacePreparing) || containsHan(preparing.Error()) || !strings.Contains(preparing.Error(), "node-b") {
		t.Fatalf("preparing = %v", preparing)
	}
}

// An installer that cannot be sent blocks the SSH plan with a reason in
// the language of the request that asked for the plan.
func TestSSHBinaryRefusalIsInTheRequestLanguage(t *testing.T) {
	admin := nodeAdminFixture(t)
	admin.cfg().Gateway.NodeBinary = filepath.Join(t.TempDir(), "missing")
	req := sshconnect.InstallRequest{Name: "remote", Addr: "127.0.0.1:1"}
	for _, tc := range []struct {
		locale i18n.Locale
		han    bool
	}{{i18n.LocaleEN, false}, {i18n.LocaleZH, true}} {
		plan, err := (sshNodeBackend{admin: admin}).Preview(i18n.WithLocale(t.Context(), tc.locale), req, SshCheckFixture())
		if err != nil {
			t.Fatal(err)
		}
		var step *sshconnect.Step
		for i := range plan.Steps {
			if plan.Steps[i].ID == "binary" {
				step = &plan.Steps[i]
			}
		}
		if step == nil || step.Status != "blocked" || step.Message == "" || containsHan(step.Message) != tc.han {
			t.Fatalf("%s binary step = %#v", tc.locale, step)
		}
	}
}
