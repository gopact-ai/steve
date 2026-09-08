package turn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/protocol"
)

func TestProfileFollowupWritesSharedIdentityWithoutScanningLocalHistory(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "rejected"}[fail], func(t *testing.T) {
			workspace := t.TempDir()
			if err := home.Bootstrap(workspace, "owner"); err != nil {
				t.Fatal(err)
			}
			coordinator, _, runner := homeCoordinator(t, workspace, "owner")
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			shared := memory.NewLedgerStore(book)
			if _, err := shared.BootstrapHome(t.Context(), home.DefaultFiles(home.LocaleZH), memory.Actor{By: "onboard"}); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("shared identity write refused")
			loader := home.EditableReader{Reader: home.Reader{ReadFiles: func() (map[string]string, error) { return shared.HomeFiles(t.Context()) }}, SaveIdentity: func(ctx context.Context, soul, user string) error {
				if fail {
					return failure
				}
				return shared.WriteIdentity(ctx, soul, user, memory.Actor{By: "onboard"})
			}}
			coordinator.SetIdentity("owner", loader)
			coordinator.assembler.SetHome(loader)
			history := filepath.Join(coordinator.scanHome, ".codex", "sessions")
			if err := os.MkdirAll(history, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(history, "private.jsonl"), []byte(`{"role":"user","text":"private-history-must-not-be-scanned"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(workspace, home.FileUser))
			if err != nil {
				t.Fatal(err)
			}
			runner.reply = "已记下。\n===SOUL.md===\n# Soul\n你是用户的可靠个人助手，帮助维护项目。\n===USER.md===\n# User\n- 称呼：李工\n- 时区：Asia/Shanghai\n"
			result, err := coordinator.Handle(t.Context(), Request{ConversationID: "dm", Input: "叫我李工，时区上海", SenderOpenID: "owner", ChatType: protocol.ChatP2P})
			if fail {
				if !errors.Is(err, failure) || strings.Contains(result.Text, "已记下") {
					t.Fatalf("failed shared save reported success: %+v %v", result, err)
				}
			} else if err != nil || result.Text != "已记下。" {
				t.Fatalf("shared profile result: %+v %v", result, err)
			}
			for _, prompt := range runner.seen() {
				if strings.Contains(prompt, "private-history-must-not-be-scanned") {
					t.Fatal("shared profile scanned local sessions")
				}
			}
			after, err := os.ReadFile(filepath.Join(workspace, home.FileUser))
			if err != nil || string(before) != string(after) {
				t.Fatal("shared profile wrote local identity files")
			}
			files, err := memory.NewLedgerStore(book).HomeFiles(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if fail && !home.IsTemplate(files[home.FileUser]) {
				t.Fatal("rejected identity save was published")
			}
			if !fail && !strings.Contains(files[home.FileUser], "李工") {
				t.Fatal("generated profile did not reach shared ledger")
			}
			if !fail {
				runner.reply = "可以一起维护项目。"
				next, err := coordinator.Handle(t.Context(), Request{ConversationID: "dm", Input: "你能做什么", SenderOpenID: "owner", ChatType: protocol.ChatP2P})
				if err != nil {
					t.Fatalf("shared identity followup: %v", err)
				}
				if next.Injected.NewSession || next.Injected.Session != result.Injected.Session || !next.Injected.InstructionsSent || !strings.Contains(next.Injected.Instructions, "李工") {
					t.Fatal("shared identity did not refresh the existing conversation")
				}
			}
		})
	}
}
