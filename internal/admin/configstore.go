package admin

import (
	"errors"
	"sync"

	"github.com/gopact-ai/steve/internal/config"
)

// A ConfigStore guards one loaded configuration: the administration
// rewrites parts of it from the page while other parts of the same
// application read it. Each application owns one; two applications in one
// process do not wait for each other.
type ConfigStore struct {
	// write is held by one writer at a time, across its save; mu only
	// while a writer changes the configuration readers see.
	write *sync.Mutex
	mu    *sync.RWMutex
	cfg   *config.Config
}

// NewConfigStore guards cfg. Everything that reads or rewrites cfg in place
// must go through the returned store.
func NewConfigStore(cfg *config.Config) *ConfigStore {
	return &ConfigStore{write: new(sync.Mutex), mu: new(sync.RWMutex), cfg: cfg}
}

// ConfigMu guards the configuration of every Service built without a
// ConfigStore of its own.
var ConfigMu = &ConfigStore{write: new(sync.Mutex), mu: new(sync.RWMutex)}

// configStore is what guards a.Cfg.
func (a *Service) configStore() *ConfigStore {
	if a.ConfigStore != nil {
		return a.ConfigStore
	}
	return &ConfigStore{write: ConfigMu.write, mu: ConfigMu.mu, cfg: a.Cfg}
}

// Read runs read with the configuration held still. read must not keep
// the pointer or anything it shares after it returns.
func (s *ConfigStore) Read(read func(*config.Config)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	read(s.cfg)
}

// errUnchanged, returned by an Update's change, ends the Update without
// saving: there is nothing to save.
var errUnchanged = errors.New("configuration unchanged")

// Update rewrites the configuration: change edits a copy, save persists
// the copy, and only then do readers see it. Readers are not held up while
// save runs; writers run one at a time. When change or save fails the
// configuration is left as it was, except that a save error for which
// config.Committed holds is published anyway: that configuration is
// already in place. Update returns the error from change or save.
//
// change and save must not use the store. change should only edit the
// copy it is given; anything slow belongs in save or outside Update.
func (s *ConfigStore) Update(change func(*config.Config) error, save func(*config.Config) error) error {
	return s.update(change, save, nil)
}

// update is Update that also runs published, when not nil, right after
// the saved configuration is in place and before any reader sees it.
// published must be quick and must not use the store.
func (s *ConfigStore) update(change, save func(*config.Config) error, published func(*config.Config)) error {
	s.write.Lock()
	defer s.write.Unlock()
	// Every writer holds write, so the configuration stands still here.
	candidate := s.cfg.Clone()
	if err := change(candidate); err != nil {
		if errors.Is(err, errUnchanged) {
			return nil
		}
		return err
	}
	err := save(candidate)
	if err != nil && !config.Committed(err) {
		return err
	}
	s.mu.Lock()
	*s.cfg = *candidate
	if published != nil {
		published(s.cfg)
	}
	s.mu.Unlock()
	return err
}

// Lock and Unlock hold the configuration for code that rewrites it in
// place; Lock waits for an Update in progress. RLock and RUnlock hold it
// for code that reads the pointer directly.
func (s *ConfigStore) Lock() {
	s.write.Lock()
	s.mu.Lock()
}

func (s *ConfigStore) Unlock() {
	s.mu.Unlock()
	s.write.Unlock()
}

func (s *ConfigStore) RLock()   { s.mu.RLock() }
func (s *ConfigStore) RUnlock() { s.mu.RUnlock() }
