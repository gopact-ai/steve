package nodewire

import "testing"

// Nobody remembers node-b608f67f…; they remember "dev". A sentence about a
// machine reads as the name its owner gave it, and falls back to the
// identity only while the machine has no name.
func TestNameReadsTheNameItsOwnerGave(t *testing.T) {
	SetSelf("node-hub")
	t.Cleanup(func() { SetSelf(""); SetNames(nil) })
	SetNames(func() map[string]string { return map[string]string{"node-hub": "我的mac", "node-b608": "dev"} })
	for node, want := range map[string]string{"node-b608": "dev", "node-hub": "我的mac", "": "我的mac", "node-unnamed": "node-unnamed"} {
		if got := Name(node); got != want {
			t.Fatalf("Name(%q) = %q, want %q", node, got, want)
		}
	}
	if got := Place("node-b608"); got != "node-b608" {
		t.Fatalf("Place stopped being the identity: %q", got)
	}
}
