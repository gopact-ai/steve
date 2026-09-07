package turn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

func configureChannelOwner(t *testing.T, c *Coordinator, channel, owner string) {
	t.Helper()
	if err := c.SetChannelOwner(channel, owner); err != nil {
		t.Fatal(err)
	}
}

func TestChannelOwnerControlsProjectPermissionsWithoutCrossChannelPrivilege(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), nil, time.Minute)
	c.SetIdentity("console-owner", nil)
	configureChannelOwner(t, c, "feishu", "ou_im_owner")
	for _, test := range []struct {
		channel, sender string
		allowed         bool
	}{{"feishu", "ou_im_owner", true}, {"console", "console-owner", true}, {"feishu", "console-owner", false}, {"console", "ou_im_owner", false}, {"unknown", "console-owner", false}} {
		req := Request{Channel: test.channel, ConversationID: fmt.Sprintf("%s-%s", test.channel, test.sender), SenderOpenID: test.sender, Input: "/grant worker target write", ChatType: protocol.ChatP2P}
		_, err := c.Handle(t.Context(), req)
		if (err == nil) != test.allowed {
			t.Errorf("channel=%s sender=%s allowed=%t err=%v", test.channel, test.sender, test.allowed, err)
		}
		if req.SenderOpenID != test.sender {
			t.Fatal("native sender identity was rewritten")
		}
	}
	if c.ownerOpenID != "console-owner" {
		t.Fatal("channel request changed the baseline owner")
	}
}

func TestChannelOwnerHomeUsesNativeIdentityAndSharedMCPMode(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "console-owner"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileMemory), []byte("private owner memory"), 0600); err != nil {
		t.Fatal(err)
	}
	c, _, runner := homeCoordinator(t, dir, "console-owner")
	configureChannelOwner(t, c, "feishu", "ou_im_owner")
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTasks(tasks, "")
	result, err := c.Handle(t.Context(), Request{Channel: "feishu", ConversationID: "oc-native", SenderOpenID: "ou_im_owner", ChatType: protocol.ChatP2P, Input: "hello", MessageID: "om-native", ChatID: "oc-native"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Injected == nil || !strings.Contains(runner.seen()[0], "private owner memory") || !strings.Contains(runner.seen()[0], "speaker=ou_im_owner owner=true") {
		t.Fatalf("wrong owner context: %+v / %v", result.Injected, runner.seen())
	}
	if c.modeOf("oc-native") != home.ModeOwner {
		t.Fatal("MCP lost the channel owner's private mode")
	}
	if got := tasks.List("oc-native"); len(got) != 1 || got[0].Requester != "ou_im_owner" || got[0].AnchorMessage != "om-native" {
		t.Fatalf("task lost native identity: %+v", got)
	}
	if record, err := c.attempts.Get(t.Context(), result.Attempt); err != nil || record.By != "ou_im_owner" {
		t.Fatalf("attempt lost native actor: %+v %v", record, err)
	}
	_, err = c.Handle(t.Context(), Request{Channel: "feishu", ConversationID: "oc-group", SenderOpenID: "ou_im_owner", ChatType: protocol.ChatGroup, Input: "/use codex"})
	if err != nil {
		t.Fatal(err)
	}
	if c.modeOf("oc-group") != home.ModeGuest {
		t.Fatal("group exposed private owner mode")
	}
}

func TestChannelOwnerRegistrationIsExplicitAndFacadeSnapshotIsStable(t *testing.T) {
	c := New(nil, nil, nil, nil, time.Minute)
	c.SetIdentity("console-owner", nil)
	if _, err := c.forChannel("feishu"); err == nil {
		t.Fatal("unregistered channel borrowed console identity")
	}
	if err := c.SetChannelOwner("console", "override"); err == nil {
		t.Fatal("channel registration overwrote Console owner")
	}
	configureChannelOwner(t, c, "feishu", "ou_first")
	first, err := c.forChannel("feishu")
	if err != nil {
		t.Fatal(err)
	}
	configureChannelOwner(t, c, "feishu", "ou_next")
	second, err := c.forChannel("feishu")
	if err != nil {
		t.Fatal(err)
	}
	if first.localized(i18n.LocaleEN).ownerOpenID != "ou_first" || second.ownerOpenID != "ou_next" || c.ownerOpenID != "console-owner" {
		t.Fatal("registration changed an in-flight view or Console baseline")
	}
}

func TestNativeChannelOwnerKeepsACPApprovalAndQuestionCallbacks(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mockagent build: %v %s", err, output)
	}
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager, err := harness.NewManager(map[string]harness.Config{"mock": {Command: bin, Permission: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, 10*time.Second)
	c.SetIdentity("console-owner", nil)
	configureChannelOwner(t, c, "feishu", "ou_im_owner")
	asks := 0
	result, err := c.Handle(t.Context(), Request{Channel: "feishu", ConversationID: "oc-native", SenderOpenID: "ou_im_owner", ChatType: protocol.ChatP2P, Input: "perm askme", MessageID: "om-native", ChatID: "oc-native",
		OnAsk: func(_ context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
			asks++
			return permission.Choose(true, ask.Options), nil
		},
		OnAskUser: func(_ context.Context, q view.Question) (view.Answer, error) {
			asks++
			return view.Answer{Value: "Blue"}, nil
		},
	})
	if err != nil || asks != 2 || !strings.Contains(result.Text, "[permission: selected/allow]") || !strings.Contains(result.Text, "[answer: accept:Blue]") || !strings.Contains(result.Text, "speaker=ou_im_owner owner=true") {
		t.Fatalf("native ACP callback path: %+v calls=%d err=%v", result, asks, err)
	}
}

func TestConcurrentChannelOwnersAndLocalesRemainRequestLocal(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), nil, time.Minute)
	c.SetIdentity("console-owner", nil)
	configureChannelOwner(t, c, "feishu", "ou_im_owner")
	var wg sync.WaitGroup
	for n := range 24 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			channel, owner, locale := "console", "console-owner", "zh"
			if n%2 == 1 {
				channel, owner, locale = "feishu", "ou_im_owner", "en"
			}
			conversation := fmt.Sprintf("channel-%d", n)
			_, err := c.Handle(t.Context(), Request{Channel: channel, Locale: locale, ConversationID: conversation, SenderOpenID: owner, ChatType: protocol.ChatP2P, Input: fmt.Sprintf("/grant worker reader-%d read", n)})
			if err != nil {
				t.Error(err)
			}
			if c.modeOf(conversation) != home.ModeOwner {
				t.Errorf("%s lost its private mode", conversation)
			}
		}(n)
	}
	wg.Wait()
	if c.ownerOpenID != "console-owner" || c.text.Locale() != i18n.LocaleZH {
		t.Fatal("request modified baseline identity or catalog")
	}
	role, err := c.projects.Access(t.Context(), "worker", "console-owner", c.ownerOpenID)
	if err != nil || role != project.RoleAdmin {
		t.Fatalf("baseline owner changed %s %v", role, err)
	}
}
