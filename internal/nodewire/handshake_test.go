package nodewire

import "testing"

// Both sides name the versions they speak and settle on the newest shared
// one; ranges that do not overlap are refused with both ranges named.
func TestHandshakeNegotiatesAVersion(t *testing.T) {
	if got := Negotiate(1, 1); got != 1 {
		t.Fatalf("v1–v1 → %d", got)
	}
	if got := Negotiate(1, 9); got != ProtocolVersion {
		t.Fatalf("v1–v9 → %d, want %d", got, ProtocolVersion)
	}
	if got := Negotiate(ProtocolVersion+1, ProtocolVersion+3); got != 0 {
		t.Fatalf("a range above ours → %d, want 0", got)
	}
	if got := Negotiate(0, 1); got != 1 {
		t.Fatalf("a bare version → %d", got)
	}
}
