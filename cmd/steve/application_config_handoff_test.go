package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
)

func TestSharedSettingsAndChannelConfigSurviveRealCoordinatorTransfer(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	first := StartTestPeer(t, options)
	WaitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), first)
	secondOptions.ApplicationReady = func(a *adminsvc.Service, _ cluster.ApplicationServer, _ cluster.Activation) error {
		adminsvc.ConfigMu.RLock()
		defer adminsvc.ConfigMu.RUnlock()
		if a.Cfg.Gateway.TaskMaxTurns != 42 || a.Cfg.Feishu.AppSecret != "fixture-shared-platform-secret" || a.Cfg.FeishuEnabled() {
			return errors.New("new coordinator did not apply private shared settings")
		}
		return nil
	}
	second := StartTestPeer(t, secondOptions)
	thirdOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), first)
	third := StartTestPeer(t, thirdOptions)
	for _, peer := range []*cluster.Peer{second, third} {
		if _, err := first.Join(context.Background(), coordination.JoinRequest{ID: "settings-join-" + peer.Config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.Config.NodeID, Address: peer.Config.RaftAddress, APIAddress: peer.Config.PeerURL}}); err != nil {
			t.Fatal(err)
		}
	}
	status, body := PeerRequest(t, first, http.MethodGet, "/console/settings", nil)
	var before consoleapi.SettingsView
	if status != http.StatusOK || json.Unmarshal(body, &before) != nil {
		t.Fatalf("initial settings: %d", status)
	}
	status, body = PeerRequest(t, first, http.MethodPut, "/console/settings", consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":42}}`)})
	var settings consoleapi.SettingsView
	if status != http.StatusOK || json.Unmarshal(body, &settings) != nil || settings.Revision == before.Revision {
		t.Fatalf("settings save: %d", status)
	}
	status, body = PeerRequest(t, first, http.MethodGet, "/console/channels", nil)
	var channelBefore consoleapi.ChannelsView
	if status != http.StatusOK || json.Unmarshal(body, &channelBefore) != nil {
		t.Fatalf("initial channels: %d", status)
	}
	status, body = PeerRequest(t, first, http.MethodPut, "/console/channels", consoleapi.ChannelsUpdate{BaseRevision: channelBefore.Revision, Channels: config.ChannelPatch{Feishu: &config.FeishuChannelPatch{Enabled: ChannelValue(false), AppID: ChannelValue("fixture-channel-app"), AppSecret: &config.ChannelSecret{Action: "replace", Value: ChannelValue("fixture-shared-platform-secret")}}}})
	var channelSaved consoleapi.ChannelsView
	if status != http.StatusOK || json.Unmarshal(body, &channelSaved) != nil || channelSaved.Revision == channelBefore.Revision {
		t.Fatalf("channel save: %d", status)
	}
	if strings.Contains(string(body), "fixture-shared-platform-secret") {
		t.Fatal("saved channel response disclosed private credentials")
	}
	AssertNoChannelSecrets(t, channelSaved)
	status, _ = PeerRequest(t, first, http.MethodPost, "/console/coordination/transfer", consoleapi.CoordinatorTransfer{CommandID: "settings-transfer", ExpectedEpoch: 1, TargetNodeID: second.Config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("transfer: %d", status)
	}
	WaitPeerReady(t, second)
	status, body = PeerRequest(t, first, http.MethodGet, "/console/settings", nil)
	var next consoleapi.SettingsView
	var effective config.SettingsValues
	if status != http.StatusOK || json.Unmarshal(body, &next) != nil || json.Unmarshal(next.Effective, &effective) != nil || effective.Gateway.TaskMaxTurns != 42 || next.PendingRestart {
		t.Fatalf("applied settings after transfer: status=%d budget=%d pending=%t", status, effective.Gateway.TaskMaxTurns, next.PendingRestart)
	}
	status, body = PeerRequest(t, first, http.MethodGet, "/console/channels", nil)
	var after consoleapi.ChannelsView
	if status != http.StatusOK || json.Unmarshal(body, &after) != nil || !after.Desired.Feishu.AppSecretConfigured || after.PendingRestart {
		t.Fatalf("applied channel after transfer: %d", status)
	}
	if strings.Contains(string(body), "fixture-shared-platform-secret") {
		t.Fatal("new coordinator disclosed private credentials")
	}
	AssertNoChannelSecrets(t, after)
	status, _ = PeerRequest(t, first, http.MethodPut, "/console/settings", consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":100}}`)})
	if status != http.StatusConflict {
		t.Fatalf("pre-transfer stale settings revision accepted: %d", status)
	}
}
