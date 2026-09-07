package turn

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
)

func sealCoordinator(t *testing.T, c *Coordinator) (func(), error) {
	t.Helper()
	return c.SealIdle()
}

func TestMaintenanceRejectsAllChannelsAndCommandsUntilReleased(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"mock": {reply: "ok"}}}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	c.SetIdentity("console-owner", nil)
	configureChannelOwner(t, c, "feishu", "ou_owner")
	release, err := sealCoordinator(t, c)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, channel := range []string{"console", "feishu"} {
		for _, input := range []string{"hello", "/new", "/tasks pause 1", "/project use worker", "/grant worker guest read", "/use worker"} {
			owner := "console-owner"
			if channel == "feishu" {
				owner = "ou_owner"
			}
			conversation := channel + input
			_, err := c.Handle(t.Context(), Request{Channel: channel, Locale: "en", ConversationID: conversation, SenderOpenID: owner, ChatType: protocol.ChatP2P, Input: input})
			var userError UserError
			if !errors.As(err, &userError) {
				t.Errorf("maintenance allowed %s %s: %v", channel, input, err)
			}
			if userError.Text != i18n.New(i18n.LocaleEN).T(i18n.HubMaintenance) {
				t.Errorf("maintenance error ignored request locale: %q", userError.Text)
			}
			if store.Conversation(conversation).ActiveAgent != "" {
				t.Errorf("blocked request changed conversation %s", conversation)
			}
		}
	}
	if again, err := sealCoordinator(t, c); err == nil {
		again()
		t.Error("second maintenance owner bypassed active seal")
	}
	release()
	second, err := c.SealIdle()
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := c.Handle(t.Context(), Request{Channel: "console", ConversationID: "still-sealed", Input: "/use worker"}); err == nil {
		t.Fatal("old release opened a newer seal")
	}
	second()
	if _, err := c.Handle(t.Context(), Request{Channel: "console", ConversationID: "after", Input: "/use worker"}); err != nil {
		t.Fatalf("release did not restore admission: %v", err)
	}
}

type resetBarrier struct {
	*fakeManager
	started chan struct{}
	proceed chan struct{}
}

func (m *resetBarrier) CloseSession(ctx context.Context, _ harness.Placement, _ string) error {
	close(m.started)
	select {
	case <-m.proceed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSealIdleRejectsCommandBeforeExecutionRegistryAdmission(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &resetBarrier{fakeManager: &fakeManager{}, started: make(chan struct{}), proceed: make(chan struct{})}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	done := make(chan error, 1)
	go func() {
		_, err := c.Handle(t.Context(), Request{Channel: "console", ConversationID: "reset", Input: "/new"})
		done <- err
	}()
	select {
	case <-manager.started:
	case <-time.After(time.Second):
		t.Fatal("reset command never reached the runtime")
	}
	release, err := sealCoordinator(t, c)
	if err == nil {
		release()
		t.Error("maintenance ignored an in-flight slash command outside execution registry")
	}
	close(manager.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	release, err = sealCoordinator(t, c)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
