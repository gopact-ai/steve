package admin

import (
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channelsettings"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

func channelLivePolicy() config.Feishu {
	return config.Feishu{
		GroupPolicy: config.GroupPolicyAllowlist, AllowUnmentioned: true,
		AllowedSenders: []string{"allowed"}, BlockedSenders: []string{"blocked"},
	}
}

func channelLivePatch() channelsettings.Patch {
	f := channelLivePolicy()
	return channelsettings.Patch{Feishu: &channelsettings.FeishuPatch{
		GroupPolicy: &f.GroupPolicy, AllowUnmentioned: &f.AllowUnmentioned,
		AllowedSenders: &f.AllowedSenders, BlockedSenders: &f.BlockedSenders,
	}}
}

func TestChannelsLiveBindUsesAppliedStartupAndOwnsSlices(t *testing.T) {
	a, _ := ChannelsAdminFixture(t)
	a.Cfg.Feishu.AllowedSenders = []string{"startup"}
	a.Cfg.Feishu.BlockedSenders = []string{"startup-blocked"}
	s := NewChannels(a, a.Cfg)
	before, _ := s.Channels(t.Context())
	// A saved declaration before a runtime consumer is bound is not applied.
	pending, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: channelLivePatch()})
	if err != nil || !pending.PendingRestart || pending.ApplyMode != "restart" || len(pending.LiveFields) != 0 {
		t.Fatalf("unbound runtime claimed live application: view=%+v err=%v", pending, err)
	}
	var got config.Feishu
	s.BindAccessUpdater(func(f config.Feishu) { got = f })
	want := config.Feishu{GroupPolicy: before.Effective.Feishu.GroupPolicy, AllowedSenders: []string{"startup"}, BlockedSenders: []string{"startup-blocked"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("binding did not receive only the four applied startup fields")
	}
	got.AllowedSenders[0], got.BlockedSenders[0] = "mutated", "mutated"
	bound, _ := s.Channels(t.Context())
	if !reflect.DeepEqual(bound.Effective, before.Effective) || !bound.PendingRestart || bound.ApplyMode != "mixed" {
		t.Fatal("binding applied pending desired state or aliased startup state")
	}
	wantFields := []string{"feishu.group_policy", "feishu.allow_unmentioned", "feishu.allowed_senders", "feishu.blocked_senders"}
	if !reflect.DeepEqual(bound.LiveFields, wantFields) {
		t.Fatalf("live fields=%v", bound.LiveFields)
	}
	bound.LiveFields[0] = "mutated"
	next, _ := s.Channels(t.Context())
	if !reflect.DeepEqual(next.LiveFields, wantFields) {
		t.Fatal("view mutation changed live metadata")
	}
	s.BindAccessUpdater(nil)
	unbound, _ := s.Channels(t.Context())
	if unbound.ApplyMode != "restart" || len(unbound.LiveFields) != 0 {
		t.Fatal("missing runtime consumer still advertised live application")
	}
}

func TestChannelsLivePublishesOnlyAfterCommit(t *testing.T) {
	for _, committedWarning := range []bool{false, true} {
		name := "success"
		if committedWarning {
			name = "committed warning"
		}
		t.Run(name, func(t *testing.T) {
			a, s := ChannelsAdminFixture(t)
			var calls atomic.Int32
			var got config.Feishu
			s.BindAccessUpdater(func(f config.Feishu) {
				if calls.Add(1) > 1 {
					stored, err := config.Load(a.Path)
					if err != nil || stored.Feishu.GroupPolicy != f.GroupPolicy || a.Cfg.Feishu.GroupPolicy != f.GroupPolicy {
						t.Error("callback ran before durable commit and Cfg publication", err)
					}
				}
				got = f
			})
			before, _ := s.Channels(t.Context())
			entered, release := make(chan struct{}), make(chan struct{})
			releaseSave := sync.OnceFunc(func() { close(release) })
			defer releaseSave()
			a.WriteConfig = func(path string, c *config.Config) error {
				close(entered)
				<-release
				if err := config.Save(path, c); err != nil {
					return err
				}
				if committedWarning {
					return &config.CommittedError{Err: errors.New("directory sync failed")}
				}
				return nil
			}
			var after consoleapi.ChannelsView
			var updateErr error
			var wg sync.WaitGroup
			finished := make(chan struct{})
			wg.Go(func() {
				defer close(finished)
				after, updateErr = s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: channelLivePatch()})
			})
			select {
			case <-entered:
			case <-finished:
				t.Fatalf("update returned before persistence: %v", updateErr)
			case <-time.After(10 * time.Second):
				t.Fatal("update did not reach persistence")
			}
			premature := calls.Load() != 1
			releaseSave()
			wg.Wait()
			if premature || updateErr != nil || calls.Load() != 2 || after.PendingRestart || after.ApplyMode != "mixed" || (after.Warning != "") != committedWarning {
				t.Fatalf("invalid commit publication: premature=%v calls=%d view=%+v err=%v", premature, calls.Load(), after, updateErr)
			}
			if !reflect.DeepEqual(got, channelLivePolicy()) {
				t.Fatal("callback received fields beyond access policy or incorrect policy")
			}
			// The callback owns its slice payload, not the saved or applied state.
			got.AllowedSenders[0], got.BlockedSenders[0] = "mutated", "mutated"
			next, _ := s.Channels(t.Context())
			if next.Desired.Feishu.AllowedSenders[0] != "allowed" || next.Effective.Feishu.AllowedSenders[0] != "allowed" || next.Desired.Feishu.BlockedSenders[0] != "blocked" || next.Effective.Feishu.BlockedSenders[0] != "blocked" || next.PendingRestart {
				t.Fatal("callback payload aliases desired or effective state")
			}
		})
	}
}

func TestChannelsLiveFailureAndCASDoNotPublish(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"save failure", errors.New("write failed")},
		{"file CAS", config.ErrFileChanged},
		{"platform CAS", platformconfig.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, s := ChannelsAdminFixture(t)
			calls := 0
			s.BindAccessUpdater(func(config.Feishu) { calls++ })
			before, _ := s.Channels(t.Context())
			a.WriteConfig = func(string, *config.Config) error { return tc.err }
			_, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: channelLivePatch()})
			if !errors.Is(err, tc.err) || calls != 1 {
				t.Fatalf("failed save published: calls=%d err=%v", calls, err)
			}
			if tc.name != "save failure" && !errors.Is(err, consoleapi.ErrSettingsConflict) {
				t.Fatalf("CAS lost conflict classification: %v", err)
			}
			after, _ := s.Channels(t.Context())
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed save changed desired/effective state")
			}
		})
	}
	a, s := ChannelsAdminFixture(t)
	var calls atomic.Int32
	s.BindAccessUpdater(func(config.Feishu) { calls.Add(1) })
	before, _ := s.Channels(t.Context())
	req := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: channelLivePatch()}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := s.UpdateChannels(t.Context(), req); results <- err })
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, consoleapi.ErrSettingsConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 || calls.Load() != 2 {
		t.Fatalf("CAS: wins=%d conflicts=%d callbacks=%d", wins, conflicts, calls.Load())
	}
	after, _ := s.Channels(t.Context())
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: after.Revision, Channels: channelLivePatch()}); err != nil || calls.Load() != 2 {
		t.Fatal("no-op republished policy", err)
	}
	if err := os.WriteFile(a.Path, []byte("external edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: after.Revision, Channels: channelLivePatch()}); !errors.Is(err, consoleapi.ErrSettingsConflict) || calls.Load() != 2 {
		t.Fatal("external edit triggered callback", err)
	}
}

func TestChannelsLiveMixedIdentityAndPolicyRemainPending(t *testing.T) {
	a, s := ChannelsAdminFixture(t)
	var got config.Feishu
	s.BindAccessUpdater(func(f config.Feishu) { got = f })
	before, _ := s.Channels(t.Context())
	patch := channelLivePatch()
	patch.DefaultChannel = ChannelValue("console")
	patch.Feishu.Enabled = ChannelValue(false)
	patch.Feishu.AppID = ChannelValue("different-app")
	patch.Feishu.AppSecret = &channelsettings.Secret{Action: "replace", Value: ChannelValue("rotated-private-secret")}
	patch.Feishu.Domain = ChannelValue(config.DomainLark)
	patch.Feishu.OwnerOpenID = ChannelValue("different-owner")
	after, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: patch})
	if err != nil || !after.PendingRestart || after.ApplyMode != "mixed" {
		t.Fatalf("mixed update not pending: view=%+v err=%v", after, err)
	}
	wantEffective := cloneChannelSettings(before.Effective)
	wantEffective.Feishu.GroupPolicy = config.GroupPolicyAllowlist
	wantEffective.Feishu.AllowUnmentioned = true
	wantEffective.Feishu.AllowedSenders = []string{"allowed"}
	wantEffective.Feishu.BlockedSenders = []string{"blocked"}
	if !reflect.DeepEqual(after.Effective, wantEffective) || !reflect.DeepEqual(got, channelLivePolicy()) || s.appliedSecret != "original-private-secret" {
		t.Fatal("restart fields were applied or leaked through updater")
	}
	AssertNoChannelSecrets(t, after)
	if a.Cfg.Feishu.AppID != "different-app" || a.Cfg.Feishu.AppSecret != "rotated-private-secret" {
		t.Fatal("mixed desired declaration was not saved")
	}
	after, err = s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: after.Revision, Channels: channelsettings.Patch{Feishu: &channelsettings.FeishuPatch{GroupPolicy: ChannelValue(config.GroupPolicyOpen)}}})
	if err != nil || !after.PendingRestart || after.Effective.Feishu.GroupPolicy != config.GroupPolicyOpen || after.Effective.Feishu.AppID != before.Effective.Feishu.AppID {
		t.Fatal("subsequent live update cleared pending restart identity", err)
	}
}
