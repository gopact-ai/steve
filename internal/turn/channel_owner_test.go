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
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
)

func TestChannelOwnerControlsProjectPermissionsWithoutCrossChannelPrivilege(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), nil, time.Minute, withOwner("console-owner"), withChannelOwner("feishu", "ou_im_owner"))
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
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	c, _, runner := homeCoordinator(t, dir, "console-owner", onLedger(book), withChannelOwner("feishu", "ou_im_owner"))
	tasks := c.tasks
	c.artifacts.SetExecution(c.executions)
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

func TestChannelOwnerRegistrationIsExplicitAndKeepsTheConsoleBaseline(t *testing.T) {
	owners := map[string]string{"feishu": "ou_first"}
	c := buildCoordinator(t, withDeps(func(d *Deps) { d.Timeout, d.ChannelOwners = time.Minute, owners }), withOwner("console-owner"))
	if _, err := c.forChannel("slack"); err == nil {
		t.Fatal("unregistered channel borrowed console identity")
	}
	owners["feishu"] = "ou_next" // the caller's map is not the coordinator's
	native, err := c.forChannel("feishu")
	if err != nil {
		t.Fatal(err)
	}
	if native.localized(i18n.LocaleEN).ownerOpenID != "ou_first" || c.ownerOpenID != "console-owner" {
		t.Fatal("registration changed after New or replaced the Console baseline")
	}
}

func TestNativeChannelOwnerKeepsACPApprovalAndQuestionCallbacks(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mockagent build: %v %s", err, output)
	}
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	manager, err := harness.NewManager(map[string]harness.Config{"mock": {Command: bin, Permission: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, 10*time.Second, withOwner("console-owner"), withChannelOwner("feishu", "ou_im_owner"))
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
	store, _ := state.OpenLedger(testLedger(t))
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), nil, time.Minute, withOwner("console-owner"), withChannelOwner("feishu", "ou_im_owner"))
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

func TestChannelOwnersResolveBaselineAndRegisteredChannels(t *testing.T) {
	owners, err := newChannelOwners("console-owner", map[string]string{"feishu": "ou_im_owner"})
	if err != nil {
		t.Fatal(err)
	}
	for channel, want := range map[string]string{"": "console-owner", "console": "console-owner", "feishu": "ou_im_owner"} {
		if got, err := owners.of(channel); err != nil || got != want {
			t.Errorf("owner of %q = %q, %v; want %q", channel, got, err, want)
		}
	}
	if _, err := owners.of("slack"); err == nil {
		t.Error("an unregistered channel resolved to an owner")
	}
	for _, channel := range []string{"", "console", " feishu"} {
		if _, err := newChannelOwners("console-owner", map[string]string{channel: "someone"}); err == nil {
			t.Errorf("registered an owner for channel %q", channel)
		}
	}
}
