package ability

import (
	"strings"
	"testing"
	"time"
)

func snap(t *testing.T, offers []Capability, cov map[Kind]Coverage) *Snapshot {
	t.Helper()
	s := &Snapshot{Schema: Schema, Node: "n", Coverage: cov, Offers: offers}
	if err := Validate(s); err != nil {
		t.Fatal(err)
	}
	return s
}

var now = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func obs(ok bool) []Evidence { return []Evidence{{Kind: Observed, Method: "path", OK: ok, At: now}} }
func decl() []Evidence       { return []Evidence{{Kind: Declared, Method: "config", OK: true}} }

// Availability is settled from evidence: an observation decides; a bare
// declaration counts only for kinds nobody can check.
func TestValidateSettlesAvailabilityAndRejectsBadSnapshots(t *testing.T) {
	s := snap(t, []Capability{
		{Kind: Tool, ID: "docker", Evidence: obs(true)},
		{Kind: Tool, ID: "kubectl", Evidence: obs(false), Detail: "not on PATH"},
		{Kind: Network, ID: "internal", Evidence: decl()},
		{Kind: MCP, ID: "github", Scope: "codex", Evidence: decl()},
	}, map[Kind]Coverage{Tool: Complete})
	got := map[string]Availability{}
	for _, c := range s.Offers {
		got[c.Key()] = c.Availability
	}
	if got["tool:docker"] != Available || got["tool:kubectl"] != Unavailable || got["network:internal"] != Unknown || got["mcp:github@codex"] != Unknown {
		t.Fatalf("availability = %v", got)
	}
	if s.Digest == "" {
		t.Fatal("no digest")
	}
	// The digest ignores time: the same content later is the same digest.
	later := snap(t, []Capability{
		{Kind: Tool, ID: "docker", Evidence: []Evidence{{Kind: Observed, Method: "path", OK: true, At: now.Add(time.Hour)}}},
		{Kind: Tool, ID: "kubectl", Evidence: obs(false), Detail: "not on PATH"},
		{Kind: Network, ID: "internal", Evidence: decl()},
		{Kind: MCP, ID: "github", Scope: "codex", Evidence: decl()},
	}, map[Kind]Coverage{Tool: Complete})
	if later.Digest != s.Digest {
		t.Fatal("digest changed with time alone")
	}
	for _, bad := range []Snapshot{
		{Schema: "x", Node: "n"},
		{Schema: Schema},
		{Schema: Schema, Node: "n", Offers: []Capability{{Kind: "quantum", ID: "x"}}},
		{Schema: Schema, Node: "n", Offers: []Capability{{Kind: Model, ID: "gpt"}}},
		{Schema: Schema, Node: "n", Offers: []Capability{{Kind: Tool, ID: "a"}, {Kind: Tool, ID: "a"}}},
		{Schema: Schema, Node: "n", Offers: []Capability{{Kind: Tool, ID: strings.Repeat("x", 65)}}},
	} {
		b := bad
		if err := Validate(&b); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	many := make([]Capability, MaxOffers+1)
	for i := range many {
		many[i] = Capability{Kind: Tag, ID: "t" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Evidence: decl()}
	}
	over := Snapshot{Schema: Schema, Node: "n", Offers: many}
	if err := Validate(&over); err == nil {
		t.Fatal("an oversized snapshot was accepted rather than rejected whole")
	}
}

// Three-valued matching: met, not met, or unknown when the kind was not
// covered; scoped kinds match only their harness; NOT of unknown stays
// unknown; version constraints compare.
func TestMatchIsThreeValuedAndScoped(t *testing.T) {
	s := snap(t, []Capability{
		{Kind: Harness, ID: "codex", Evidence: obs(true), Assurance: Launchable},
		{Kind: Tool, ID: "docker", Evidence: obs(true), Version: &Version{Scheme: "semver", Value: "27.1.0"}},
		{Kind: Tool, ID: "kubectl", Evidence: obs(false), Detail: "not on PATH"},
		{Kind: Hardware, ID: "gpu", Evidence: obs(true), Attrs: map[string]string{"count": "2"}},
		{Kind: Model, ID: "gpt-5.6", Scope: "codex", Evidence: obs(true)},
		{Kind: Network, ID: "internal", Evidence: decl()},
	}, map[Kind]Coverage{Harness: Complete, Tool: Complete, Hardware: Complete, Model: Complete, Network: Complete})
	cases := []struct {
		req   string
		scope string
		want  Verdict
		code  Code
	}{
		{"tool:docker", "", True, ""},
		{"tool:docker@>=27 <28", "", True, ""},
		{"tool:docker@>=28", "", False, CodeVersion},
		{"tool:kubectl", "", False, CodeUnavailable},
		{"tool:gh", "", False, CodeAbsent},
		{"credential:prod", "", Unsure, CodeGated},
		{"network:internal", "", Unsure, CodeGated},
		{"model:gpt-5*", "codex", True, ""},
		{"model:gpt-5*", "claude-code", False, CodeScope},
		{"!network:public", "", Unsure, CodeGated}, // gated kinds are not decidable either way
		{"!tool:docker", "", False, CodeNegated},
		{"tool:gh|tool:docker", "", True, ""},
		{"gpu", "", False, CodeAbsent}, // tag, not hardware
		{"hardware:gpu", "", True, ""},
		{"mcp:github", "codex", Unsure, CodeGated},
	}
	for _, tc := range cases {
		req, err := Compile([]string{tc.req})
		if err != nil {
			t.Fatalf("%s: %v", tc.req, err)
		}
		got := Match(req, s, tc.scope, now)
		if got.Verdict != tc.want {
			t.Errorf("%s (scope %q): verdict %s, want %s — %s", tc.req, tc.scope, got.Verdict, tc.want, got.Unmet())
			continue
		}
		if tc.code != "" {
			found := false
			for _, a := range got.Atoms {
				if a.Code == tc.code {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: want code %s, atoms %+v", tc.req, tc.code, got.Atoms)
			}
		}
	}
	// A hard requirement on stale evidence is unsure, not false.
	old := snap(t, []Capability{{Kind: Tool, ID: "gh", Evidence: []Evidence{{Kind: Observed, OK: true, At: now.Add(-time.Hour)}}}}, map[Kind]Coverage{Tool: Complete})
	req, _ := Compile([]string{"tool:gh"})
	if got := Match(req, old, "", now); got.Verdict != Unsure || got.Atoms[0].Code != CodeStale {
		t.Fatalf("stale tool: %+v", got)
	}
	// Text round-trips canonically.
	req, _ = Compile([]string{"tool:docker@>=27", "a|!b", "gpu"})
	if got := Text(req); got != "tool:docker@>=27 tag:a|!tag:b tag:gpu" {
		t.Fatalf("text = %q", got)
	}
	if err := ValidateText([]string{"quantum:x"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestDiffAndCompact(t *testing.T) {
	before := snap(t, []Capability{{Kind: Tool, ID: "docker", Evidence: obs(true), Version: &Version{Scheme: "semver", Value: "27.1"}}, {Kind: Tool, ID: "gh", Evidence: obs(true)}}, nil)
	after := snap(t, []Capability{{Kind: Tool, ID: "docker", Evidence: obs(true), Version: &Version{Scheme: "semver", Value: "27.2"}}, {Kind: Tool, ID: "gh", Evidence: obs(false)}, {Kind: Hardware, ID: "gpu", Evidence: obs(true)}}, nil)
	changes := Diff(before, after)
	joined := ""
	for _, c := range changes {
		joined += c.String() + "; "
	}
	for _, want := range []string{"hardware:gpu appeared (available)", "tool:docker: semver:27.1 → semver:27.2", "tool:gh: available → unavailable"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diff lacks %q: %s", want, joined)
		}
	}
	if got := Compact(after, "", 12); got != "tool: docker · hardware: gpu" {
		t.Fatalf("compact = %q", got)
	}
}
