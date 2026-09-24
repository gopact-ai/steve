package admin

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

// Two applications in one process keep their own configuration: a save
// that is still in flight in one does not hold up readers in the other.
func TestConfigurationSaveDoesNotHoldUpAnotherApplication(t *testing.T) {
	saving, other := agentAdminFixture(t), agentAdminFixture(t)
	saving.ConfigStore = NewConfigStore(saving.Cfg)
	other.ConfigStore = NewConfigStore(other.Cfg)
	entered, release := make(chan struct{}), make(chan struct{})
	saving.WriteConfig = func(path string, cfg *config.Config) error {
		close(entered)
		<-release
		return config.Save(path, cfg)
	}
	saved := make(chan error, 1)
	go func() {
		saved <- saving.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "new", Harness: "mock"})
	}()
	<-entered
	read := make(chan error, 1)
	go func() {
		_, err := NewSettings(other, other.Cfg).Settings(t.Context())
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("reading one application's settings waited for another application's save")
		close(release)
		<-read
		<-saved
		return
	}
	close(release)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
}

func TestConfigStoreReadSeesTheGuardedConfiguration(t *testing.T) {
	cfg := &config.Config{Gateway: config.Gateway{HubID: "hub-a"}}
	var seen string
	NewConfigStore(cfg).Read(func(cfg *config.Config) { seen = cfg.Gateway.HubID })
	if seen != "hub-a" {
		t.Fatalf("Read saw %q", seen)
	}
}
