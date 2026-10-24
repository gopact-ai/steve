package config

import "testing"

func TestValidateChannelsTrimsTheOwner(t *testing.T) {
	cfg := Config{Gateway: Gateway{OwnerID: "   "}}
	if err := cfg.ValidateChannels(); err == nil {
		t.Fatal("blank owner accepted")
	}
}
