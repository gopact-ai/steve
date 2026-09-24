package admin

import (
	"sync"

	"github.com/gopact-ai/steve/internal/config"
)

// A ConfigStore guards one loaded configuration: the administration
// rewrites parts of it from the page while other parts of the same
// application read it. Each application owns one; two applications in one
// process do not wait for each other.
type ConfigStore struct {
	mu  *sync.RWMutex
	cfg *config.Config
}

// NewConfigStore guards cfg. Everything that reads or rewrites cfg in place
// must go through the returned store.
func NewConfigStore(cfg *config.Config) *ConfigStore {
	return &ConfigStore{mu: new(sync.RWMutex), cfg: cfg}
}

// ConfigMu guards the configuration of every Service built without a
// ConfigStore of its own.
var ConfigMu = &ConfigStore{mu: new(sync.RWMutex)}

// configStore is what guards a.Cfg.
func (a *Service) configStore() *ConfigStore {
	if a.ConfigStore != nil {
		return a.ConfigStore
	}
	return &ConfigStore{mu: ConfigMu.mu, cfg: a.Cfg}
}

// Read runs read with the configuration held still. read must not keep
// the pointer or anything it shares after it returns.
func (s *ConfigStore) Read(read func(*config.Config)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	read(s.cfg)
}

// Lock, Unlock, RLock and RUnlock guard code that works on the
// configuration pointer directly.
func (s *ConfigStore) Lock()    { s.mu.Lock() }
func (s *ConfigStore) Unlock()  { s.mu.Unlock() }
func (s *ConfigStore) RLock()   { s.mu.RLock() }
func (s *ConfigStore) RUnlock() { s.mu.RUnlock() }
