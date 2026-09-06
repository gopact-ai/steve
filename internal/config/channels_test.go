package config

import "testing"

func TestPeerAddressesKeepCredentialsInDedicatedField(t *testing.T) {
	for _, address := range []string{
		"https://hub.example?token=credential",
		"https://hub.example/#credential",
		"https://user:credential@hub.example",
	} {
		cfg := Config{Gateway: Gateway{OwnerID: "owner", HubID: "source", Peers: map[string]HubPeer{"target": {URL: address, Token: "credential"}}}}
		if err := cfg.ValidateChannels(); err == nil {
			t.Fatalf("accepted credential-bearing peer address %q", address)
		}
	}
	cfg := Config{Gateway: Gateway{OwnerID: "owner", HubID: "source", Peers: map[string]HubPeer{"target": {URL: "https://hub.example/steve", Token: "credential"}}}}
	if err := cfg.ValidateChannels(); err != nil {
		t.Fatal(err)
	}
}
