package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/channelsettings"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/project"
)

func TestSharedProjectRegistrationResolvesCoordinatorToPhysicalNode(t *testing.T) {
	for _, requestedNode := range []string{"", "hub", "node-a"} {
		t.Run("node="+requestedNode, func(t *testing.T) {
			a, _, book, _ := sharedSettingsFixture(t)
			a.Projects = project.Open(book)
			if err := (configbuild.ProjectController{Store: a.Projects}).Reconcile(t.Context(), configOf(a)); err != nil {
				t.Fatal(err)
			}
			req := consoleapi.AddProjectRequest{ID: "new", Node: requestedNode, Path: "new-service", Level: "internal", Repo: "inplace"}
			// The directory named is relative; the machine that holds the
			// project resolves it under its own workspace.
			suffix := filepath.Join("projects", "new-service")
			for range 2 {
				if err := a.AddProject(t.Context(), req); err != nil {
					t.Fatalf("register/retry project on coordinator: %v", err)
				}
			}
			stored, found, err := platformconfig.New(book).Load()
			if err != nil || !found {
				t.Fatalf("load shared configuration: found=%v err=%v", found, err)
			}
			if home := stored.Projects[req.ID].Home; home.Node != "node-a" || !filepath.IsAbs(home.Path) || !strings.HasSuffix(home.Path, suffix) {
				t.Fatalf("persisted project lost physical home: %+v", home)
			}
			p, found, err := a.Projects.Get(t.Context(), req.ID)
			if err != nil || !found || p.Home.Node != "node-a" || p.Home.Path != stored.Projects[req.ID].Home.Path {
				t.Fatalf("project projection disagrees with shared declaration: found=%v home=%+v err=%v", found, p.Home, err)
			}
		})
	}
}

func sharedSettingsFixture(t *testing.T) (*adminsvc.Service, *applicationConfiguration, *ledger.Ledger, config.Config) {
	t.Helper()
	a, _ := ChannelsAdminFixture(t)
	configOf(a).Gateway.HomePath = t.TempDir()
	configOf(a).Gateway.TaskMaxTurns = 3
	configOf(a).Gateway.Planner = "primary"
	configOf(a).Gateway.DirectTransfer = true
	configOf(a).Gateway.OfflineReminderAfter = config.Duration(23 * time.Minute)
	configOf(a).MCPServers = map[string]config.MCPServer{"external": {URL: "http://external", Headers: map[string]string{"Authorization": "external-mcp-private"}}}
	if err := config.Save(a.Path, configOf(a)); err != nil {
		t.Fatal(err)
	}
	original := *configOf(a)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	shared := &applicationConfiguration{ctx: t.Context(), store: platformconfig.New(book), local: platformconfig.LocalNode{ID: "node-a", Config: config.Node{Addr: "worker-a", Token: "node-private", Level: "restricted"}}}
	if err := shared.Configure(configOf(a)); err != nil {
		t.Fatal(err)
	}
	a.ClusterMode = true
	a.NodeName = shared.local.ID
	a.WriteConfig = shared.Save
	a.WriteConfigContext = shared.SaveContext
	a.ConfigRevision = shared.Revision
	return a, shared, book, original
}

func TestSharedApplicationSettingsCASAndReconstruction(t *testing.T) {
	a, _, book, original := sharedSettingsFixture(t)
	s := adminsvc.NewSettings(a, configOf(a))
	channels := adminsvc.NewChannels(a, configOf(a))
	before, _ := s.Settings(t.Context())
	channelBefore, _ := channels.Channels(t.Context())
	fileBefore := configOf(a).FileRevision()
	diskBefore, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	request := consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":42,"task_max_elapsed":"8h","prompt_timeout":"30m"},"policies":{"execution":{"step_timeout":"35m"},"snapshot":{"max_files":123}}}`)}
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() { _, err := s.UpdateSettings(t.Context(), request); results <- err })
	}
	wait.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, consoleapi.ErrSettingsConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("shared CAS winners=%d conflicts=%d", wins, conflicts)
	}
	after, _ := s.Settings(t.Context())
	if after.Revision == before.Revision || !after.PendingRestart || !reflect.DeepEqual(after.Effective, before.Effective) {
		t.Fatal("shared save did not advance desired revision only")
	}
	if _, err := channels.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: channelBefore.Revision}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatalf("shared settings did not invalidate channel revision: %v", err)
	}
	diskAfter, err := os.ReadFile(a.Path)
	if err != nil || string(diskBefore) != string(diskAfter) || configOf(a).FileRevision() != fileBefore {
		t.Fatal("shared save rewrote local file version")
	}
	fresh := original
	fresh.Gateway.StatePath = filepath.Join(t.TempDir(), "other-state")
	fresh.Gateway.ReadModelToken = "other-local-secret"
	fresh.Gateway.ReadModelAddr = "127.0.0.1:8888"
	fresh.Gateway.HubID = "local-infra"
	reloaded := &applicationConfiguration{ctx: t.Context(), store: platformconfig.New(book), local: platformconfig.LocalNode{ID: "node-a", Config: config.Node{Addr: "worker-a", Token: "node-private", Level: "restricted"}}}
	if err := reloaded.Configure(&fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Gateway.TaskMaxTurns != 42 || fresh.Gateway.TaskMaxElapsed != config.Duration(8*time.Hour) || fresh.Gateway.PromptTimeout != config.Duration(30*time.Minute) || fresh.Policies.Execution.StepTimeout != config.Duration(35*time.Minute) || fresh.Policies.Snapshot.MaxFiles != 123 {
		t.Fatalf("saved shared policy was lost: %+v", fresh.SettingsValues())
	}
	if fresh.Gateway.ReadModelToken != "other-local-secret" || fresh.Gateway.ReadModelAddr != "127.0.0.1:8888" || fresh.Gateway.HubID != "local-infra" || fresh.Gateway.StatePath == original.Gateway.StatePath {
		t.Fatal("shared settings overwrote local infrastructure")
	}
	if fresh.Gateway.Planner != "primary" || !fresh.Gateway.DirectTransfer || fresh.Gateway.OfflineReminderAfter != config.Duration(23*time.Minute) {
		t.Fatal("shared work policy did not survive")
	}
	if reloaded.Revision() != after.Revision {
		t.Fatal("reconstruction changed shared revision")
	}
	if err := os.WriteFile(a.Path, []byte("external local edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(t.Context(), consoleapi.SettingsUpdate{BaseRevision: after.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":43}}`)}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatalf("external local edit ignored: %v", err)
	}
}

func TestSharedApplicationChannelSecretSurvivesReplicaAndStaysPrivate(t *testing.T) {
	a, _, book, _ := sharedSettingsFixture(t)
	channels := adminsvc.NewChannels(a, configOf(a))
	before, _ := channels.Channels(t.Context())
	request := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: channelsettings.Patch{DefaultChannel: ChannelValue("feishu"), Feishu: &channelsettings.FeishuPatch{AppSecret: &channelsettings.Secret{Action: "replace", Value: ChannelValue("rotated-private-secret")}, Domain: ChannelValue(config.DomainLark), AllowedSenders: ChannelValue([]string{"allowed-owner"}), GroupPolicy: ChannelValue(config.GroupPolicyAllowlist)}}}
	after, err := channels.UpdateChannels(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision == after.Revision || !after.PendingRestart {
		t.Fatal("channel rotation did not advance shared revision")
	}
	AssertNoChannelSecrets(t, after)
	if _, err := channels.UpdateChannels(t.Context(), request); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatalf("stale channel save accepted: %v", err)
	}
	snapshot, err := book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	replica, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := replica.AttachReplication(applicationMCPReplicator{replica}); err != nil {
		t.Fatal(err)
	}
	newLocal := &config.Config{Gateway: config.Gateway{OwnerID: "other", HomePath: t.TempDir(), StatePath: "other-local-state", ReadModelToken: "other-local-ui"}}
	next := &applicationConfiguration{ctx: t.Context(), store: platformconfig.New(replica), local: platformconfig.LocalNode{ID: "node-b", Config: config.Node{Addr: "worker-b", Token: "other-worker-private", Level: "restricted"}}}
	if err := next.Configure(newLocal); err != nil {
		t.Fatal(err)
	}
	if newLocal.Feishu.AppSecret != "rotated-private-secret" || newLocal.Feishu.Domain != config.DomainLark || newLocal.Feishu.OwnerOpenID != "im-owner" || !newLocal.FeishuEnabled() || !reflect.DeepEqual(newLocal.Feishu.AllowedSenders, []string{"allowed-owner"}) || newLocal.Gateway.OwnerID != "console-owner" {
		t.Fatal("new coordinator lost private channel connection or policy")
	}
	newAdmin := &adminsvc.Service{ConfigStore: adminsvc.NewConfigStore(newLocal), ConfigRevision: next.Revision}
	newView, err := adminsvc.NewChannels(newAdmin, newLocal).Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	AssertNoChannelSecrets(t, newView)
	if newView.PendingRestart || newView.Revision != after.Revision {
		t.Fatal("new coordinator did not apply shared channel version")
	}
	declaration, ok, err := platformconfig.New(replica).Load()
	if err != nil || !ok {
		t.Fatal(err)
	}
	raw, err := json.Marshal(declaration)
	if err != nil {
		t.Fatal(err)
	}
	for _, excluded := range []string{"external-mcp-private", "other-local-ui", "other-local-state"} {
		if strings.Contains(string(raw), excluded) {
			t.Fatal("machine-only secret or path entered shared declaration")
		}
	}
}

func TestSharedApplicationRejectsAuthoritativeRevisionConflict(t *testing.T) {
	a, _, book, _ := sharedSettingsFixture(t)
	staleConfiguration := &applicationConfiguration{ctx: t.Context(), store: platformconfig.New(book), local: platformconfig.LocalNode{ID: "node-a", Config: config.Node{Addr: "worker-a", Token: "node-private", Level: "restricted"}}}
	staleConfig := *configOf(a)
	if err := staleConfiguration.Configure(&staleConfig); err != nil {
		t.Fatal(err)
	}
	staleAdmin := &adminsvc.Service{ConfigStore: adminsvc.NewConfigStore(&staleConfig), Path: a.Path, ConfigRevision: staleConfiguration.Revision, WriteConfigContext: staleConfiguration.SaveContext}
	stale := adminsvc.NewSettings(staleAdmin, &staleConfig)
	old, _ := stale.Settings(t.Context())
	current := adminsvc.NewSettings(a, configOf(a))
	before, _ := current.Settings(t.Context())
	if _, err := current.UpdateSettings(t.Context(), consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":77}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.UpdateSettings(t.Context(), consoleapi.SettingsUpdate{BaseRevision: old.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":99}}`)}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatalf("stale activation overwrote committed settings: %v", err)
	}
	d, ok, err := platformconfig.New(book).Load()
	if err != nil || !ok || d.Settings.Gateway.TaskMaxTurns != 77 || staleConfig.Gateway.TaskMaxTurns != 3 {
		t.Fatalf("rejected save published state: turns=%d stale=%d err=%v", d.Settings.Gateway.TaskMaxTurns, staleConfig.Gateway.TaskMaxTurns, err)
	}
}
