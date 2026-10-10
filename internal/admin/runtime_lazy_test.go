package admin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestLazyHubSettingsSaveAndAgentBindNeverLaunchTheAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := hubNodeSettingsFixture(t)
	dir := t.TempDir()
	program, counter, tool := filepath.Join(dir, "custom-agent"), filepath.Join(dir, "starts"), filepath.Join(dir, "host-tool")
	if err := os.WriteFile(program, []byte("#!/bin/sh\necho start >> '"+counter+"'\necho 'fixture 1.2.3'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho 'tool 2.3.4'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	a.cfg().Gateway.Tools = []string{filepath.Base(tool)}
	a.Observation = NewLocalObservation(node.NewLaunchProbe())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); a.Observation.RunLaunchProbe(ctx, a.ConfigStore) }()
	t.Cleanup(func() { cancel(); <-done })
	wait := func(after time.Time) {
		t.Helper()
		until := time.Now().Add(4 * time.Second)
		for time.Now().Before(until) {
			r, ok := a.Observation.Launch.Lookup(tool)
			if ok && r.At.After(after) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("host tool launch pass did not finish")
	}
	wait(time.Time{})
	before, _ := a.Observation.Launch.Lookup(tool)
	set, err := a.NodeSettings(t.Context(), a.NodeName)
	if err != nil {
		t.Fatal(err)
	}
	set.Harnesses["custom"] = nodewire.HarnessSetting{Command: program, Args: []string{"acp", "", "含 空格"}, Env: []string{"MODE=ordinary"}}
	if _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); err != nil {
		t.Fatal(err)
	}
	wait(before.At)
	if err := a.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "custom-agent", Harness: "custom"}); err != nil {
		t.Fatal(err)
	}
	// Config and availability reads never initialize ACP or probe its models.
	for range 3 {
		if _, err := a.NodeSettings(t.Context(), a.NodeName); err != nil {
			t.Fatal(err)
		}
		advert := ObservedHubAdvert(a.NodeName, a.ConfigStore, a.Observation)
		if advert.Snapshot == nil {
			t.Fatal("no hub availability snapshot")
		}
		for _, capability := range advert.Snapshot.Offers {
			if capability.Kind == ability.Harness && capability.ID == "custom" && (capability.Assurance != ability.Existence || capability.Version != nil || len(capability.Evidence) != 1 || capability.Evidence[0].Method != "path") {
				t.Errorf("unverified harness claimed launch/ACP readiness: %+v", capability)
			}
			if capability.Kind == ability.Tool && capability.ID == "host-tool" && capability.Assurance != ability.Launchable {
				t.Errorf("ordinary host tool lost launch evidence: %+v", capability)
			}
		}
	}
	if raw, err := os.ReadFile(counter); err == nil {
		t.Errorf("save/bind/read launched the agent: %q", raw)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
