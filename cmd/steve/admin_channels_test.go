package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func channelValue[T any](v T) *T { return &v }

func channelsAdminFixture(t *testing.T) (*fleetAdmin, *hubChannelsService) {
	t.Helper()
	a := agentAdminFixture(t)
	a.cfg.Gateway.OwnerID, a.cfg.Gateway.DefaultChannel = "console-owner", "feishu"
	a.cfg.Gateway.StatePath = filepath.Join(t.TempDir(), "state.json")
	a.cfg.Projects = map[string]config.Project{"work": {Home: config.ProjectHome{Path: t.TempDir()}}}
	a.cfg.Feishu = config.Feishu{AppID: "app", AppSecret: "original-private-secret", OwnerOpenID: "im-owner"}
	if err := config.Save(a.path, a.cfg); err != nil {
		t.Fatal(err)
	}
	return a, newHubChannelsService(a, a.cfg)
}

func assertNoChannelSecrets(t *testing.T, view consoleapi.ChannelsView) {
	t.Helper()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-secret") || strings.Contains(string(raw), `"app_secret":`) {
		t.Fatal("channels response exposed a secret")
	}
}

func TestChannelsRotationCASAndEffectiveSettings(t *testing.T) {
	a, s := channelsAdminFixture(t)
	before, err := s.Channels(t.Context())
	if err != nil || before.Revision == "" || before.PendingRestart || before.ApplyMode != "restart" {
		t.Fatalf("initial view=%+v err=%v", before, err)
	}
	assertNoChannelSecrets(t, before)
	update := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: config.ChannelPatch{Feishu: &config.FeishuChannelPatch{AppSecret: &config.ChannelSecret{Action: "replace", Value: channelValue("rotated-private-secret")}}}}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := s.UpdateChannels(t.Context(), update); results <- err })
	}
	wg.Wait()
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
		t.Fatalf("winners=%d conflicts=%d", wins, conflicts)
	}
	after, _ := s.Channels(t.Context())
	if !after.PendingRestart || !reflect.DeepEqual(before.Effective, after.Effective) || !reflect.DeepEqual(after.Desired, after.Effective) {
		t.Fatal("secret rotation did not remain pending behind redacted settings")
	}
	assertNoChannelSecrets(t, after)
	reloaded, err := config.Load(a.path)
	if err != nil || reloaded.Feishu.AppSecret != "rotated-private-secret" {
		t.Fatal("secret rotation not persisted", err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{Channels: config.ChannelPatch{}}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatal("missing revision accepted", err)
	}
	policies := newHubSettingsService(a, a.cfg)
	settings, _ := policies.Settings(t.Context())
	if _, err := policies.UpdateSettings(t.Context(), consoleapi.SettingsUpdate{BaseRevision: settings.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":3}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: after.Revision}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatal("unrelated settings save did not invalidate channel revision", err)
	}
}

func TestChannelsDisablePreservesConsoleIdentityAndSecrets(t *testing.T) {
	a, _ := channelsAdminFixture(t)
	a.cfg.Gateway.OwnerID = ""
	if err := config.Save(a.path, a.cfg); err != nil {
		t.Fatal(err)
	}
	s := newHubChannelsService(a, a.cfg)
	before, _ := s.Channels(t.Context())
	update := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: config.ChannelPatch{DefaultChannel: channelValue("console"), Feishu: &config.FeishuChannelPatch{Enabled: channelValue(false), OwnerOpenID: channelValue("different-im-owner")}}}
	after, err := s.UpdateChannels(t.Context(), update)
	if err != nil {
		t.Fatal(err)
	}
	if after.Desired.Console.OwnerID != "im-owner" || after.Desired.Feishu.OwnerOpenID != "different-im-owner" || after.Desired.Feishu.Enabled || !after.Desired.Feishu.AppSecretConfigured || !after.PendingRestart || !after.Effective.Feishu.Enabled {
		t.Fatal("disable changed identity or falsely updated effective channel")
	}
	assertNoChannelSecrets(t, after)
	if a.cfg.Feishu.AppSecret != "original-private-secret" {
		t.Fatal("disabled channel lost omitted secret")
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: after.Revision, Channels: config.ChannelPatch{Feishu: &config.FeishuChannelPatch{AppSecret: &config.ChannelSecret{Action: "clear"}}}}); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Channels(t.Context())
	if after.Desired.Feishu.AppSecretConfigured || a.cfg.Feishu.AppSecret != "" || !after.Effective.Feishu.AppSecretConfigured {
		t.Fatal("explicit clear did not preserve effective snapshot")
	}
}

func TestChannelsSaveFailuresAndExternalEditRespectCommitBoundary(t *testing.T) {
	a, s := channelsAdminFixture(t)
	before, _ := s.Channels(t.Context())
	raw, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	update := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: config.ChannelPatch{Feishu: &config.FeishuChannelPatch{AppSecret: &config.ChannelSecret{Action: "replace", Value: channelValue("rotated-private-secret")}}}}
	a.writeConfig = func(string, *config.Config) error {
		return errors.New("write failed original-private-secret rotated-private-secret")
	}
	if _, err := s.UpdateChannels(t.Context(), update); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("write failure leaked a secret or reported success")
	}
	after, _ := s.Channels(t.Context())
	stored, _ := os.ReadFile(a.path)
	if !reflect.DeepEqual(before, after) || string(stored) != string(raw) || a.cfg.Feishu.AppSecret != "original-private-secret" {
		t.Fatal("failed save published candidate")
	}
	a.writeConfig = func(path string, c *config.Config) error {
		if err := config.Save(path, c); err != nil {
			return err
		}
		return &config.CommittedError{Err: fmt.Errorf("directory sync failed for %s", c.Feishu.AppSecret)}
	}
	applied, err := s.UpdateChannels(t.Context(), update)
	if err != nil || applied.Warning == "" || !applied.PendingRestart || a.cfg.Feishu.AppSecret != "rotated-private-secret" {
		t.Fatal("committed warning rolled back candidate", err)
	}
	assertNoChannelSecrets(t, applied)
	if err := os.WriteFile(a.path, []byte("external edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: applied.Revision}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatal("external file edit was overwritten", err)
	}
	stored, _ = os.ReadFile(a.path)
	if string(stored) != "external edit" {
		t.Fatal("external edit was lost")
	}
}

func TestChannelsViewsDoNotAliasAppliedConfiguration(t *testing.T) {
	a, _ := channelsAdminFixture(t)
	a.cfg.Feishu.AllowedSenders = []string{"allowed"}
	s := newHubChannelsService(a, a.cfg)
	view, _ := s.Channels(t.Context())
	view.Desired.Feishu.AllowedSenders[0] = "changed"
	view.Effective.Feishu.AllowedSenders[0] = "changed"
	next, _ := s.Channels(t.Context())
	if next.Desired.Feishu.AllowedSenders[0] != "allowed" || next.Effective.Feishu.AllowedSenders[0] != "allowed" || next.PendingRestart {
		t.Fatal("public response mutated runtime snapshot or declaration")
	}
}
