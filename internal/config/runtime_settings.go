package config

import "sync/atomic"

// RuntimeSettings publishes committed policy as one immutable value. Consumers
// capture the relevant values when starting work; changing policy never resets
// a session, rewrites a task budget, or changes an already running timer.
type RuntimeSettings struct {
	value atomic.Pointer[SettingsValues]
}

func NewRuntimeSettings(cfg *Config) *RuntimeSettings {
	s := &RuntimeSettings{}
	s.Publish(cfg)
	return s
}

func (s *RuntimeSettings) Publish(cfg *Config) {
	v := cfg.SettingsValues()
	v.Gateway.Locale = cfg.EffectiveLocale()
	v.Gateway.OwnerID = cfg.EffectiveOwnerID()
	s.value.Store(&v)
}

func (s *RuntimeSettings) Load() SettingsValues { return *s.value.Load() }
