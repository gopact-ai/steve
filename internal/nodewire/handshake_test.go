package nodewire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

// Both sides name the versions they speak and settle on the newest shared
// one; ranges that do not overlap are refused with both ranges named.
func TestHandshakeNegotiatesAVersion(t *testing.T) {
	if ProtocolVersion != 2 || ProtocolMin != 2 {
		t.Fatalf("protocol range v%d–v%d, want v2–v2", ProtocolMin, ProtocolVersion)
	}
	if got := Negotiate(2, 2); got != 2 {
		t.Fatalf("v2–v2 → %d", got)
	}
	if got := Negotiate(1, 9); got != ProtocolVersion {
		t.Fatalf("v1–v9 → %d, want %d", got, ProtocolVersion)
	}
	if got := Negotiate(1, 1); got != 0 {
		t.Fatalf("v1–v1 → %d, want 0", got)
	}
	if got := Negotiate(0, 1); got != 0 {
		t.Fatalf("a bare v1 → %d, want 0", got)
	}
	if got := Negotiate(ProtocolVersion+1, ProtocolVersion+3); got != 0 {
		t.Fatalf("a range above ours → %d, want 0", got)
	}
	if got := Negotiate(0, 2); got != 2 {
		t.Fatalf("a bare v2 → %d", got)
	}
}

// Overlapping ranges settle on the newest version both sides speak, from
// whichever side reaches further; ranges that only touch still meet.
func TestNegotiateSettlesOnTheNewestSharedVersion(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		peerMin, peerMax, lo, hi int
		want                     int
	}{
		{"the peer reaches past us", 2, 3, 2, 2, 2},
		{"we reach past the peer", 1, 2, 2, 3, 2},
		{"both reach past each other", 2, 4, 3, 5, 4},
		{"the peer inside our range", 3, 3, 2, 4, 3},
		{"ranges that touch", 1, 2, 2, 5, 2},
		{"a bare version inside our range", 0, 3, 2, 3, 3},
		{"the peer below us", 1, 1, 2, 3, 0},
		{"the peer above us", 4, 5, 2, 3, 0},
	} {
		if got := negotiate(tc.peerMin, tc.peerMax, tc.lo, tc.hi); got != tc.want {
			t.Errorf("%s: v%d–v%d against v%d–v%d → %d, want %d", tc.name, tc.peerMin, tc.peerMax, tc.lo, tc.hi, got, tc.want)
		}
	}
}

// A node whose range reaches past the hub's answers with the version they
// share, so the hub accepts it instead of seeing a version it cannot speak.
func TestNodeAnswersWithTheSharedVersion(t *testing.T) {
	hub, node := net.Pipe()
	defer hub.Close()
	done := make(chan error, 1)
	go func() {
		defer node.Close()
		hello, err := acceptClaim(node, func(string) bool { return true }, nil, Advert{Node: "n"}, ProtocolVersion, ProtocolVersion+1)
		if err == nil && hello.Version != ProtocolVersion {
			err = fmt.Errorf("hello settled on v%d, want v%d", hello.Version, ProtocolVersion)
		}
		done <- err
	}()
	advert, err := Dial(hub, Hello{Token: "t"})
	if err != nil {
		t.Fatalf("dial = %v, want the shared version accepted", err)
	}
	if advert.Version != ProtocolVersion {
		t.Fatalf("advert version = %d, want %d", advert.Version, ProtocolVersion)
	}
	if err := <-done; err != nil {
		t.Fatalf("accept = %v", err)
	}
}

// A hub that speaks only v1 is refused by the node with both ranges named,
// so the hub can report what it met.
func TestNodeRefusesAProtocolV1Hub(t *testing.T) {
	hub, node := net.Pipe()
	defer hub.Close()
	done := make(chan error, 1)
	go func() {
		defer node.Close()
		_, err := Accept(node, "t", Advert{Node: "n"})
		done <- err
	}()
	payload, _ := json.Marshal(Hello{Version: 1, Token: "t"})
	if err := WriteFrame(hub, Frame{Kind: KindOpen, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var advert Advert
	if err := readJSON(hub, &advert); err != nil {
		t.Fatal(err)
	}
	if advert.Refused != "hub speaks v1–v1, node speaks v2–v2" {
		t.Fatalf("refused = %q", advert.Refused)
	}
	if err := <-done; !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("accept = %v, want a version mismatch", err)
	}
}

// Dial refuses an advert on a version outside its range, naming both.
func TestDialRefusesAnAdvertOutsideItsRange(t *testing.T) {
	var reply bytes.Buffer
	if err := writeJSON(&reply, Advert{Version: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := Dial(struct {
		io.Reader
		io.Writer
	}{&reply, io.Discard}, Hello{})
	if !errors.Is(err, ErrVersionMismatch) || !strings.Contains(err.Error(), "node speaks v1, hub speaks v2") {
		t.Fatalf("dial = %v", err)
	}
}
