package cluster

import "testing"

func TestRestartDiscoveryCursorHandlesMembershipChanges(t *testing.T) {
	d := &restartDiscovery{}
	if got := d.next([]string{"a", "b", "c", "d"}, map[string]bool{}); got != "a" {
		t.Fatalf("first probe=%q", got)
	}
	if got := d.next([]string{"a", "b", "c", "d"}, map[string]bool{}); got != "b" {
		t.Fatalf("next call restarted instead of advancing: %q", got)
	}
	// b and c left, and bb joined. A missing cursor key is an insertion point,
	// not a reason to start again at a or skip the newly enrolled member.
	ids := []string{"a", "bb", "d"}
	tried := map[string]bool{}
	for _, want := range []string{"bb", "d", "a", ""} {
		if got := d.next(ids, tried); got != want {
			t.Fatalf("membership rotation=%q, want %q", got, want)
		}
	}
	if got := d.next(nil, map[string]bool{}); got != "" {
		t.Fatalf("empty membership had a probe: %q", got)
	}
	if got := d.next([]string{"only"}, map[string]bool{}); got != "only" {
		t.Fatalf("new membership not visited: %q", got)
	}
}
