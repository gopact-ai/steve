package app

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
)

func TestTurnPolicyReadsTheLatestPublishedSettings(t *testing.T) {
	if timeout, autoResolve := turnPolicy(nil); timeout != nil || autoResolve != nil {
		t.Fatal("without runtime settings the coordinator must keep its fixed policy")
	}
	cfg := &config.Config{}
	cfg.Gateway.PromptTimeout = config.Duration(10 * time.Minute)
	cfg.Policies.Landing.Conflicts = config.ConflictsByAgent
	settings := config.NewRuntimeSettings(cfg)
	timeout, autoResolve := turnPolicy(settings)
	if timeout() != 10*time.Minute || !autoResolve() {
		t.Fatalf("initial policy: timeout %v, auto-resolve %v", timeout(), autoResolve())
	}
	saved := *cfg
	saved.Gateway.PromptTimeout = config.Duration(19 * time.Minute)
	saved.Policies.Landing.Conflicts = config.ConflictsManual
	settings.Publish(&saved)
	if timeout() != 19*time.Minute || autoResolve() {
		t.Fatalf("saved policy: timeout %v, auto-resolve %v", timeout(), autoResolve())
	}
}
