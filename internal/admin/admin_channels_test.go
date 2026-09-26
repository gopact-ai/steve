package admin

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
	"time"

	"github.com/gopact-ai/steve/internal/channelsettings"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func ChannelValue[T any](v T) *T { return &v }

func ChannelsAdminFixture(t *testing.T) (*Service, *hubChannelsService) {
	t.Helper()
	a := agentAdminFixture(t)
	a.cfg().Gateway.OwnerID, a.cfg().Gateway.DefaultChannel = "console-owner", "feishu"
	a.cfg().Gateway.StatePath = filepath.Join(t.TempDir(), "state.json")
	a.cfg().Projects = map[string]config.Project{"work": {Home: config.ProjectHome{Path: t.TempDir()}}}
	a.cfg().Feishu = config.Feishu{AppID: "app", AppSecret: "original-private-secret", OwnerOpenID: "im-owner"}
	if err := config.Save(a.Path, a.cfg()); err != nil {
		t.Fatal(err)
	}
	return a, NewChannels(a, a.cfg())
}

func AssertNoChannelSecrets(t *testing.T, view consoleapi.ChannelsView) {
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
	a, s := ChannelsAdminFixture(t)
	before, err := s.Channels(t.Context())
	if err != nil || before.Revision == "" || before.PendingRestart || before.ApplyMode != "restart" {
		t.Fatalf("initial view=%+v err=%v", before, err)
	}
	AssertNoChannelSecrets(t, before)
	update := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: channelsettings.Patch{Feishu: &channelsettings.FeishuPatch{AppSecret: &channelsettings.Secret{Action: "replace", Value: ChannelValue("rotated-private-secret")}}}}
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
	AssertNoChannelSecrets(t, after)
	reloaded, err := config.Load(a.Path)
	if err != nil || reloaded.Feishu.AppSecret != "rotated-private-secret" {
		t.Fatal("secret rotation not persisted", err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{Channels: channelsettings.Patch{}}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatal("missing revision accepted", err)
	}
	policies := NewSettings(a, a.cfg())
	settings, _ := policies.Settings(t.Context())
	if _, err := policies.UpdateSettings(t.Context(), consoleapi.SettingsUpdate{BaseRevision: settings.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":3}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: after.Revision}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatal("unrelated settings save did not invalidate channel revision", err)
	}
}

func TestChannelsDisablePreservesConsoleIdentityAndSecrets(t *testing.T) {
	a, _ := ChannelsAdminFixture(t)
	a.cfg().Gateway.OwnerID = ""
	if err := config.Save(a.Path, a.cfg()); err != nil {
		t.Fatal(err)
	}
	s := NewChannels(a, a.cfg())
	before, _ := s.Channels(t.Context())
	update := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: channelsettings.Patch{DefaultChannel: ChannelValue("console"), Feishu: &channelsettings.FeishuPatch{Enabled: ChannelValue(false), OwnerOpenID: ChannelValue("different-im-owner")}}}
	after, err := s.UpdateChannels(t.Context(), update)
	if err != nil {
		t.Fatal(err)
	}
	if after.Desired.Console.OwnerID != "im-owner" || after.Desired.Feishu.OwnerOpenID != "different-im-owner" || after.Desired.Feishu.Enabled || !after.Desired.Feishu.AppSecretConfigured || !after.PendingRestart || !after.Effective.Feishu.Enabled {
		t.Fatal("disable changed identity or falsely updated effective channel")
	}
	AssertNoChannelSecrets(t, after)
	if a.cfg().Feishu.AppSecret != "original-private-secret" {
		t.Fatal("disabled channel lost omitted secret")
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: after.Revision, Channels: channelsettings.Patch{Feishu: &channelsettings.FeishuPatch{AppSecret: &channelsettings.Secret{Action: "clear"}}}}); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Channels(t.Context())
	if after.Desired.Feishu.AppSecretConfigured || a.cfg().Feishu.AppSecret != "" || !after.Effective.Feishu.AppSecretConfigured {
		t.Fatal("explicit clear did not preserve effective snapshot")
	}
}

func TestChannelsSaveFailuresAndExternalEditRespectCommitBoundary(t *testing.T) {
	a, s := ChannelsAdminFixture(t)
	before, _ := s.Channels(t.Context())
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	update := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: channelsettings.Patch{Feishu: &channelsettings.FeishuPatch{AppSecret: &channelsettings.Secret{Action: "replace", Value: ChannelValue("rotated-private-secret")}}}}
	a.WriteConfig = func(string, *config.Config) error {
		return errors.New("write failed original-private-secret rotated-private-secret")
	}
	if _, err := s.UpdateChannels(t.Context(), update); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("write failure leaked a secret or reported success")
	}
	after, _ := s.Channels(t.Context())
	stored, _ := os.ReadFile(a.Path)
	if !reflect.DeepEqual(before, after) || string(stored) != string(raw) || a.cfg().Feishu.AppSecret != "original-private-secret" {
		t.Fatal("failed save published candidate")
	}
	a.WriteConfig = func(path string, c *config.Config) error {
		if err := config.Save(path, c); err != nil {
			return err
		}
		return &config.CommittedError{Err: fmt.Errorf("directory sync failed for %s", c.Feishu.AppSecret)}
	}
	applied, err := s.UpdateChannels(t.Context(), update)
	if err != nil || applied.Warning == "" || !applied.PendingRestart || a.cfg().Feishu.AppSecret != "rotated-private-secret" {
		t.Fatal("committed warning rolled back candidate", err)
	}
	AssertNoChannelSecrets(t, applied)
	if err := os.WriteFile(a.Path, []byte("external edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: applied.Revision}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatal("external file edit was overwritten", err)
	}
	stored, _ = os.ReadFile(a.Path)
	if string(stored) != "external edit" {
		t.Fatal("external edit was lost")
	}
}

func TestChannelsViewsDoNotAliasAppliedConfiguration(t *testing.T) {
	a, _ := ChannelsAdminFixture(t)
	a.cfg().Feishu.AllowedSenders = []string{"allowed"}
	s := NewChannels(a, a.cfg())
	view, _ := s.Channels(t.Context())
	view.Desired.Feishu.AllowedSenders[0] = "changed"
	view.Effective.Feishu.AllowedSenders[0] = "changed"
	next, _ := s.Channels(t.Context())
	if next.Desired.Feishu.AllowedSenders[0] != "allowed" || next.Effective.Feishu.AllowedSenders[0] != "allowed" || next.PendingRestart {
		t.Fatal("public response mutated runtime snapshot or declaration")
	}
}

// Reading the channels does not wait for a channel change to be saved.
func TestChannelsReadDuringAChannelSave(t *testing.T) {
	a, s := ChannelsAdminFixture(t)
	before, err := s.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var during consoleapi.ChannelsView
	finished := readsDuringSave(t, a, func() error {
		_, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: channelsettings.Patch{Feishu: &channelsettings.FeishuPatch{GroupPolicy: ChannelValue("open")}}})
		return err
	}, func() error {
		during, err = s.Channels(t.Context())
		return err
	})
	if !finished {
		t.Fatal("reading the channels waited for a channel save")
	}
	if !reflect.DeepEqual(during.Desired, before.Desired) {
		t.Fatal("a reader saw channels that were not saved yet")
	}
	if a.cfg().Feishu.GroupPolicy != "open" {
		t.Fatal("the saved channels were not published")
	}
}

// The running channel reports its state and binds its access policy while
// a configuration save is in flight, without waiting for it.
func TestChannelRuntimeStateDoesNotWaitForASave(t *testing.T) {
	a, s := ChannelsAdminFixture(t)
	var bound []config.Feishu
	finished := readsDuringSave(t, a, func() error {
		return a.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "new", Harness: "mock"})
	}, func() error {
		s.SetRuntimeError("connection failed")
		s.BindAccessUpdater(func(f config.Feishu) { bound = append(bound, f) })
		return nil
	})
	if !finished {
		t.Fatal("the channel's runtime state waited for a configuration save")
	}
	view, err := s.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if view.RuntimeError != "connection failed" || view.ApplyMode != "mixed" || len(bound) != 1 {
		t.Fatalf("runtime error %q, apply mode %q, %d bindings", view.RuntimeError, view.ApplyMode, len(bound))
	}
}

// A retrying channel startup shows how many attempts failed, when the next
// begins and why the last failed. It is replaced by a terminal runtime error
// and withdrawn once the channel starts.
func TestChannelsViewReportsAStartupRetry(t *testing.T) {
	_, s := ChannelsAdminFixture(t)
	next := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	retry := consoleapi.ChannelStartupRetry{Attempts: 3, NextAt: next, LastError: "dial tcp: network is unreachable"}
	reported := retry
	s.SetStartupRetry(&reported)
	reported.Attempts = 99
	view, err := s.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if view.StartupRetry == nil || *view.StartupRetry != retry || view.RuntimeError != "" {
		t.Fatalf("startup retry %+v, runtime error %q; want %+v alone", view.StartupRetry, view.RuntimeError, retry)
	}
	AssertNoChannelSecrets(t, view)

	s.SetRuntimeError("connection failed")
	if view, _ = s.Channels(t.Context()); view.StartupRetry != nil || view.RuntimeError != "connection failed" {
		t.Fatalf("after a terminal failure: startup retry %+v, runtime error %q", view.StartupRetry, view.RuntimeError)
	}
	s.SetStartupRetry(&retry)
	s.SetStartupRetry(nil)
	if view, _ = s.Channels(t.Context()); view.StartupRetry != nil {
		t.Fatalf("a started channel still reports a startup retry: %+v", view.StartupRetry)
	}
}
