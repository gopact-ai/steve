package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
)

type ordinaryDisclosureProcessor struct{ *ordinaryCompletionProbe }

func (p ordinaryDisclosureProcessor) ResumeRetainedChat(ctx context.Context, id string, req turn.Request) (turn.Result, error) {
	return p.coordinator.ResumeRetainedChat(ctx, id, req)
}

type ordinaryDisclosureChannel struct {
	mu    sync.Mutex
	texts []string
}

func (c *ordinaryDisclosureChannel) Reply(context.Context, string, string) error {
	return errors.New("non-receipted reply is not supported")
}
func (c *ordinaryDisclosureChannel) ReplyText(_ context.Context, _ string, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, text)
	return fmt.Sprintf("receipt-%d", len(c.texts)), nil
}

// Only native Handle is replaced, using the existing completed-attempt
// fixture. Real coordinator retained delivery, disclosure, accounting, owner
// admission and gateway durable delivery all run against one actual ledger.
func TestOrdinarySealedDisclosureNotificationKeepsExecutionIdentity(t *testing.T) {
	for _, mode := range []string{"public", "sealed-owner", "sealed-guest"} {
		t.Run(mode, func(t *testing.T) {
			f := openCrashProbe(t, t.TempDir())
			defer f.close(t)
			r := f.seed(t, true, "", "feishu")
			projects := project.Open(f.book)
			level := project.LevelPublic
			if strings.HasPrefix(mode, "sealed") {
				level = project.LevelSealed
			}
			if err := projects.Declare(f.ctx, []project.Project{{ID: "p", Level: level,
				Home: project.Home{Path: t.TempDir()}, DefaultRole: project.RoleWrite}}); err != nil {
				t.Fatal(err)
			}
			if _, err := projects.Bind(f.ctx, crashConversation, "p", "owner"); err != nil {
				t.Fatal(err)
			}
			f.c.SetProjects(projects, "p", "")
			if mode == "sealed-guest" {
				if err := f.c.SetChannelOwner("feishu", "different-security-owner"); err != nil {
					t.Fatal(err)
				}
			}
			p := &ordinaryCompletionProbe{coordinator: f.c, task: r.TaskID, attempt: r.ID}
			ch := &ordinaryDisclosureChannel{}
			g := gateway.New(ordinaryDisclosureProcessor{p})
			g.BindChannel(ch)
			g.SetRecoveryLedger(f.book)
			workers := &reconciliationWorkers{}
			g.SetIngressLifetime(f.ctx, workers)
			if err := g.HandleMessage(feishu.InboundMessage{
				ConversationID: crashConversation, ChatID: "console", MessageID: "web-original",
				SenderOpenID: "owner", Text: "original goal", Mentioned: true, ChatType: protocol.ChatP2P,
			}); err != nil {
				t.Fatal(err)
			}
			workers.Close()
			for range 2 {
				if err := g.RecoverQueued(f.ctx, f.book, f.c, nil); err != nil {
					t.Errorf("retained recovery: %v", err)
				}
			}
			pending, err := f.book.PendingCommands(f.ctx, "gateway-input")
			if err != nil {
				t.Fatal(err)
			}
			disclosures, err := projects.PendingDisclosures(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("native Handle=%d delivered=%d pending inputs=%d pending disclosures=%d",
				p.calls.Load(), len(ch.texts), len(pending), len(disclosures))
			if p.calls.Load() != 1 || len(ch.texts) != 1 || len(pending) != 0 {
				t.Fatalf("legitimate coordinator output was stranded as mismatched attempt: Handle=%d replies=%d pending=%d disclosures=%d",
					p.calls.Load(), len(ch.texts), len(pending), len(disclosures))
			}
			dispatch, found, err := f.book.CommandReceipt(f.ctx, "gateway-input/web-original/dispatch")
			var output struct {
				Result  turn.Result `json:"result"`
				Recover bool        `json:"recover"`
			}
			if err != nil || !found || dispatch.FinishedAt == nil || dispatch.Error != "" || json.Unmarshal(dispatch.Result, &output) != nil || output.Recover || output.Result.Attempt != r.ID {
				t.Fatalf("disclosure changed committed result identity: %+v output=%+v err=%v", dispatch, output, err)
			}
			tracked, found := f.tasks.Get(r.TaskID)
			if !found || tracked.Budget.Turns != 1 || tracked.Budget.Tokens.Total != 18 || len(tracked.Attempts) != 1 || tracked.Attempts[0].Open() {
				t.Fatalf("disclosure replay changed exact accounting: %+v", tracked)
			}
			if mode == "sealed-guest" && (strings.Contains(ch.texts[0], crashAnswer) || !strings.Contains(ch.texts[0], "/approve") || len(disclosures) != 1) {
				t.Fatalf("sealed result did not preserve exactly one disclosure notification: %q disclosures=%d", ch.texts[0], len(disclosures))
			}
			if mode != "sealed-guest" && (ch.texts[0] != crashAnswer || len(disclosures) != 0) {
				t.Fatalf("ordinary allowed result was changed: %q disclosures=%d", ch.texts[0], len(disclosures))
			}
		})
	}
}
