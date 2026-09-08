package node

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// Snapshot observes the machine into an ability snapshot. Everything that
// can be checked is checked (existence on PATH; hardware present); the
// rest is declared and says so. Coverage names the kinds that were fully
// checked, so "not listed" means "absent" only for those.
func Snapshot(name string, generation, sequence int64, o Observe) *ability.Snapshot {
	now := time.Now().UTC()
	s := &ability.Snapshot{
		Schema: ability.Schema, Node: name, Generation: generation, Sequence: sequence, GeneratedAt: now,
		Coverage: map[ability.Kind]ability.Coverage{
			ability.Harness: ability.Complete, ability.Tool: ability.Complete, ability.MCP: ability.Complete,
			ability.Hardware: ability.Partial, ability.Network: ability.Complete, ability.Credential: ability.Complete,
			ability.Tag: ability.Complete, ability.Model: ability.Partial, ability.Skill: ability.Unsupported, ability.A2A: ability.Unsupported,
		},
		Features: nodewire.Features(), Source: "node",
	}
	observed := func(kind ability.Kind, id, cmd string) ability.Capability {
		c := ability.Capability{Kind: kind, ID: id, Assurance: ability.Existence}
		path, err := exec.LookPath(cmd)
		if err != nil {
			c.Evidence = []ability.Evidence{{Kind: ability.Observed, Method: "path", OK: false, Result: fmt.Sprintf("%q not on this node's PATH", cmd), At: now}}
			c.Detail = fmt.Sprintf("%q not on this node's PATH", cmd)
			return c
		}
		c.Evidence = []ability.Evidence{{Kind: ability.Observed, Method: "path", OK: true, Result: path, At: now}}
		// Being on PATH is existence; having started is launchable. The
		// launch check ran on its own clock, so it carries its own time,
		// and a binary that would not start makes the entry unavailable.
		if o.Launch != nil {
			if r, ok := o.Launch(path); ok {
				c.Evidence = append(c.Evidence, ability.Evidence{Kind: ability.Observed, Method: "launch", OK: r.OK, Result: r.Result, At: r.At})
				if r.OK {
					c.Assurance = ability.Launchable
					c.Version = r.Version
				} else {
					c.Detail = "does not start: " + r.Result
				}
			}
		}
		return c
	}
	ids := make([]string, 0, len(o.Harnesses))
	for id := range o.Harnesses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s.Offers = append(s.Offers, observed(ability.Harness, id, o.Harnesses[id].Command))
	}
	for _, tool := range o.Tools {
		s.Offers = append(s.Offers, observed(ability.Tool, tool, tool))
	}
	if o.MCPError != "" {
		s.Coverage[ability.MCP] = ability.Errored
	}
	listed := make([]string, 0, len(o.MCPListed))
	for id := range o.MCPListed {
		listed = append(listed, id)
	}
	sort.Strings(listed)
	for _, id := range listed {
		for _, h := range ids {
			s.Offers = append(s.Offers, ability.Capability{Kind: ability.MCP, ID: id, Scope: h, Assurance: ability.Existence, Attrs: map[string]string{"transport": o.MCPListed[id]},
				Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "broker", OK: true, At: now}}})
		}
	}
	names := make([]string, 0, len(o.MCP))
	for id := range o.MCP {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		spec := o.MCP[id]
		// MCP servers are offered per harness scope? They are the machine's;
		// a harness starts them. Until a broker exists they are observed
		// only and never scheduled, so scope is every configured harness.
		for _, h := range ids {
			var c ability.Capability
			switch spec.Type {
			case "stdio", "":
				c = observed(ability.MCP, id, spec.Command)
			case "http", "sse":
				// Reached through the node's loopback proxy, which adds the
				// configured headers; the URL itself is checked for shape.
				parsed, perr := url.ParseRequestURI(spec.URL)
				ok := perr == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
				c = ability.Capability{Kind: ability.MCP, ID: id, Assurance: ability.Existence, Attrs: map[string]string{"transport": spec.Type},
					Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "url", OK: ok, At: now}}}
				if !ok {
					c.Detail = "url is not http(s)"
				}
			default:
				c = ability.Capability{Kind: ability.MCP, ID: id, Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "config", OK: false, Result: "unknown type " + spec.Type, At: now}}, Detail: "unknown type " + spec.Type}
			}
			c.Scope = h
			s.Offers = append(s.Offers, c)
		}
	}
	s.Offers = append(s.Offers, hardware(now)...)
	// Skills are what this machine materialized into its harness homes:
	// present for every harness, addressed by content. A machine that
	// isolates its homes knows the whole list, so the kind is covered.
	if o.SkillsKnown {
		s.Coverage[ability.Skill] = ability.Complete
		for _, sk := range o.Skills {
			for _, h := range ids {
				s.Offers = append(s.Offers, ability.Capability{Kind: ability.Skill, ID: sk.Name, Scope: h, Assurance: ability.Existence,
					Version:  &ability.Version{Scheme: "opaque", Value: sk.Hash},
					Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "materialized", OK: true, Result: sk.Hash[:12], At: now}}})
			}
		}
	}
	for _, d := range o.Declares {
		atom, err := ability.ParseAtom(d)
		if err != nil || atom.ID == "" {
			continue
		}
		s.Offers = append(s.Offers, ability.Capability{Kind: atom.Kind, ID: atom.ID, Evidence: []ability.Evidence{{Kind: ability.Declared, Method: "config", OK: true}}, Detail: "declared in config"})
	}
	for _, tag := range o.Tags {
		s.Offers = append(s.Offers, ability.Capability{Kind: ability.Tag, ID: tag, Evidence: []ability.Evidence{{Kind: ability.Declared, Method: "config", OK: true}}})
	}
	if err := ability.Validate(s); err != nil {
		// A snapshot this machine cannot even validate is not reported; the
		// hub sees an old-style advert and treats coverage as partial.
		slog.Error(fmt.Sprintf("steve-node: snapshot invalid, not reported: %v", err))
		return nil
	}
	return s
}

// hardware is what can be seen without root: CPUs, the architecture, and
// whether an NVIDIA GPU is present.
func hardware(now time.Time) []ability.Capability {
	seen := func(id, detail string, attrs map[string]string) ability.Capability {
		return ability.Capability{Kind: ability.Hardware, ID: id, Assurance: ability.Existence, Attrs: attrs, Detail: detail,
			Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "probe", OK: true, At: now}}}
	}
	out := []ability.Capability{
		seen("cpu", "", map[string]string{"count": strconv.Itoa(runtime.NumCPU())}),
		seen(runtime.GOARCH, "", nil),
	}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		out = append(out, seen("gpu", "nvidia-smi on PATH", map[string]string{"vendor": "nvidia"}))
	} else if _, err := os.Stat("/dev/nvidia0"); err == nil {
		out = append(out, seen("gpu", "/dev/nvidia0", map[string]string{"vendor": "nvidia"}))
	}
	return out
}
