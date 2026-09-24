package admin

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

// Two applications in one process keep their own configuration: a save
// that is still in flight in one does not hold up readers in the other.
func TestConfigurationSaveDoesNotHoldUpAnotherApplication(t *testing.T) {
	saving, other := agentAdminFixture(t), agentAdminFixture(t)
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
		_, err := NewSettings(other, other.cfg()).Settings(t.Context())
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

func TestConfigStoreUpdateLeavesTheConfigurationAloneWhenSavingFails(t *testing.T) {
	cfg := &config.Config{Nodes: map[string]config.Node{"a": {Addr: "127.0.0.1:1"}}}
	store := NewConfigStore(cfg)
	err := store.Update(func(c *config.Config) error {
		c.Nodes["b"] = config.Node{Addr: "127.0.0.1:2"}
		c.Gateway.HubID = "changed"
		return nil
	}, func(*config.Config) error { return errors.New("disk full") })
	if err == nil || err.Error() != "disk full" {
		t.Fatalf("Update = %v, want the save error", err)
	}
	if len(cfg.Nodes) != 1 || cfg.Gateway.HubID != "" {
		t.Fatalf("a failed save changed the configuration: %+v", cfg)
	}
}

func TestConfigStoreUpdateSavesNothingWhenTheChangeFails(t *testing.T) {
	cfg := &config.Config{}
	store := NewConfigStore(cfg)
	refused := errors.New("refused")
	saved := false
	err := store.Update(func(c *config.Config) error {
		c.Gateway.HubID = "changed"
		return refused
	}, func(*config.Config) error { saved = true; return nil })
	if !errors.Is(err, refused) || saved || cfg.Gateway.HubID != "" {
		t.Fatalf("Update = %v, saved=%v, hub=%q", err, saved, cfg.Gateway.HubID)
	}
}

func TestConfigStoreUpdatePublishesASaveThatIsAlreadyInPlace(t *testing.T) {
	cfg := &config.Config{}
	store := NewConfigStore(cfg)
	err := store.Update(func(c *config.Config) error {
		c.Gateway.HubID = "changed"
		return nil
	}, func(*config.Config) error { return &config.CommittedError{Err: errors.New("sync dir")} })
	if !config.Committed(err) {
		t.Fatalf("Update = %v, want the committed error", err)
	}
	if cfg.Gateway.HubID != "changed" {
		t.Fatal("a configuration already in place was not published")
	}
}

// Readers keep reading the previous configuration while a save is in
// flight; a writer working in place waits for the save to finish.
func TestConfigStoreUpdateSavesWithoutHoldingUpReaders(t *testing.T) {
	cfg := &config.Config{Gateway: config.Gateway{HubID: "before"}}
	store := NewConfigStore(cfg)
	entered, release := make(chan struct{}), make(chan struct{})
	updated := make(chan error, 1)
	go func() {
		updated <- store.Update(func(c *config.Config) error {
			c.Gateway.HubID = "after"
			return nil
		}, func(*config.Config) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	read := make(chan string, 1)
	go store.Read(func(c *config.Config) { read <- c.Gateway.HubID })
	select {
	case hub := <-read:
		if hub != "before" {
			t.Errorf("a reader saw %q before the save finished", hub)
		}
	case <-time.After(2 * time.Second):
		t.Error("a reader waited for the save")
	}
	locked := make(chan string, 1)
	go func() {
		store.Lock()
		defer store.Unlock()
		locked <- cfg.Gateway.HubID
	}()
	select {
	case <-locked:
		t.Error("a writer working in place did not wait for the save")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if hub := <-locked; hub != "after" {
		t.Fatalf("the writer saw %q after the save", hub)
	}
}

func TestConfigStoreUpdateWithNothingToChangeSavesNothing(t *testing.T) {
	store := NewConfigStore(&config.Config{})
	saved := false
	err := store.Update(func(*config.Config) error { return errUnchanged }, func(*config.Config) error { saved = true; return nil })
	if err != nil || saved {
		t.Fatalf("Update = %v, saved=%v", err, saved)
	}
}

// readsDuringSave runs write with its configuration save held open and
// reports whether read finished while the save was held.
func readsDuringSave(t *testing.T, a *Service, write func() error, read func() error) bool {
	t.Helper()
	save := a.WriteConfig
	if save == nil {
		save = config.Save
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	a.WriteConfig = func(path string, cfg *config.Config) error {
		once.Do(func() { close(entered) })
		<-release
		return save(path, cfg)
	}
	written := make(chan error, 1)
	go func() { written <- write() }()
	select {
	case <-entered:
	case err := <-written:
		t.Fatalf("the change finished without saving: %v", err)
	}
	reading := make(chan error, 1)
	go func() { reading <- read() }()
	var finished bool
	select {
	case err := <-reading:
		if err != nil {
			t.Fatal(err)
		}
		finished = true
	case <-time.After(time.Second):
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if !finished {
		if err := <-reading; err != nil {
			t.Fatal(err)
		}
	}
	a.WriteConfig = save
	return finished
}

// A service built without a configuration reads none and cannot change one.
func TestServiceWithoutAConfigurationCannotChangeOne(t *testing.T) {
	a := &Service{}
	changed := false
	err := a.updateConfig(t.Context(), func(*config.Config) error { changed = true; return nil })
	if !errors.Is(err, errNoConfiguration) || changed {
		t.Fatalf("updateConfig = %v, changed=%v", err, changed)
	}
	a.configStore().Read(func(cfg *config.Config) {
		if cfg != nil {
			t.Errorf("a service without a configuration read %+v", cfg)
		}
	})
}

// Two changes made while the first is still being saved both stay: the
// second is made on top of the first, not on the configuration they both
// started from.
func TestConfigStoreKeepsBothOfTwoConcurrentUpdates(t *testing.T) {
	cfg := &config.Config{Nodes: map[string]config.Node{}}
	store := NewConfigStore(cfg)
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- store.Update(func(c *config.Config) error {
			c.Nodes["first"] = config.Node{Addr: "127.0.0.1:1"}
			return nil
		}, func(*config.Config) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	var savedSecond map[string]config.Node
	changing := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		second <- store.Update(func(c *config.Config) error {
			close(changing)
			c.Nodes["second"] = config.Node{Addr: "127.0.0.1:2"}
			return nil
		}, func(c *config.Config) error {
			savedSecond = c.Nodes
			return nil
		})
	}()
	// Give the second change time to start while the first is saved.
	select {
	case <-changing:
		t.Error("the second change was made while the first was being saved")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if _, ok := cfg.Nodes[name]; !ok {
			t.Errorf("the %s change was lost: %v", name, cfg.Nodes)
		}
		if _, ok := savedSecond[name]; !ok {
			t.Errorf("the second save lacks the %s change: %v", name, savedSecond)
		}
	}
}
