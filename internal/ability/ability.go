// Package ability is the domain of what a machine can do and what work
// requires of it: a Snapshot a node reports, a Requirement work states, and
// the three-valued match between them.
//
// It has no dependencies inside steve. nodewire carries Snapshots as
// versioned DTOs; roster, exec and the read model evaluate them here.
// Machines are different on purpose — what this package guarantees is
// that each says truthfully what it has, and that a requirement is judged
// against that without guessing.
package ability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Schema is the snapshot's shape; bumped when meaning changes.
const Schema = "ability.v1"

// Kind is the class of a capability; the id is unique within a kind and
// scope.
type Kind string

const (
	Harness    Kind = "harness"
	Model      Kind = "model"
	MCP        Kind = "mcp"
	Skill      Kind = "skill"
	Tool       Kind = "tool"
	Hardware   Kind = "hardware"
	Network    Kind = "network"
	Credential Kind = "credential"
	A2A        Kind = "a2a"
	// Tag is the old free-form capability word.
	Tag Kind = "tag"
)

var kinds = []Kind{Harness, Model, MCP, Skill, Tool, Hardware, Network, Credential, A2A, Tag}

func (k Kind) valid() bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// scoped kinds belong to a harness on the machine, not to the machine:
// Claude's models are not Codex's.
func (k Kind) scoped() bool { return k == Model || k == MCP || k == Skill }

// declaredOK says a declaration alone may satisfy a requirement of this
// kind. Networks and credentials cannot be checked from inside a process;
// a binary or a server can, so it must be.
func (k Kind) declaredOK() bool { return k == Tag }

// Schedulable says the kind takes part in placement at all. MCP and skill
// wait for their delivery paths (a node-side broker, atomic
// materialisation); A2A waits for a ledger-mediated connector; network and
// credential wait for probes that check them without revealing them. Until
// then they are observed and shown, never matched — a declared credential
// is not a credential. Tags stay schedulable as the legacy declaration.
func (k Kind) Schedulable() bool {
	switch k {
	case MCP, Skill, A2A, Network, Credential:
		return false
	}
	return true
}

// Availability is what the evidence adds up to.
type Availability string

const (
	Available   Availability = "available"
	Unavailable Availability = "unavailable"
	Unknown     Availability = "unknown"
)

// EvidenceKind says how something is known.
type EvidenceKind string

const (
	Declared EvidenceKind = "declared"
	Observed EvidenceKind = "observed"
	Derived  EvidenceKind = "derived"
)

// Assurance is how far an observation went: that the thing exists, that
// it starts, that it works.
type Assurance string

const (
	Existence  Assurance = "existence"
	Launchable Assurance = "launchable"
	Functional Assurance = "functional"
)

// Evidence is one reason to believe a capability's state.
type Evidence struct {
	Kind   EvidenceKind `json:"kind"`
	Method string       `json:"method,omitempty"`
	Result string       `json:"result,omitempty"`
	OK     bool         `json:"ok"`
	At     time.Time    `json:"at,omitzero"`
}

// Version is a version under a scheme that says how it compares.
type Version struct {
	Scheme string `json:"scheme,omitempty"` // semver | date | opaque
	Value  string `json:"value"`
}

// Capability is one entry of a snapshot.
type Capability struct {
	Kind         Kind              `json:"kind"`
	ID           string            `json:"id"`
	Scope        string            `json:"scope,omitempty"`
	Version      *Version          `json:"version,omitempty"`
	Availability Availability      `json:"availability"`
	Evidence     []Evidence        `json:"evidence,omitempty"`
	Assurance    Assurance         `json:"assurance,omitempty"`
	Attrs        map[string]string `json:"attrs,omitempty"`
	Detail       string            `json:"detail,omitempty"`
}

// Key is (kind, id, scope) as one string.
func (c Capability) Key() string {
	if c.Scope != "" {
		return string(c.Kind) + ":" + c.ID + "@" + c.Scope
	}
	return string(c.Kind) + ":" + c.ID
}

// Coverage says whether a kind was fully checked: only then does "not in
// the list" mean "not there".
type Coverage string

const (
	Complete    Coverage = "complete"
	Partial     Coverage = "partial"
	Unsupported Coverage = "unsupported"
	Errored     Coverage = "error"
)

// Snapshot is what one machine reports at one moment.
type Snapshot struct {
	Schema      string            `json:"schema"`
	Node        string            `json:"node"`
	Generation  int64             `json:"generation"`
	Sequence    int64             `json:"sequence"`
	GeneratedAt time.Time         `json:"generated_at"`
	ReceivedAt  time.Time         `json:"received_at,omitzero"`
	Digest      string            `json:"digest,omitempty"`
	Coverage    map[Kind]Coverage `json:"coverage"`
	Offers      []Capability      `json:"offers"`
	Features    []string          `json:"features,omitempty"`
	// Source says where the snapshot came from: "node" for a real
	// report, "legacy" for one synthesised from an older advert.
	Source string `json:"source,omitempty"`
}

// Limits keep a snapshot a snapshot. A snapshot past them is rejected
// whole, never trimmed: a trimmed list would pass "incomplete" off as
// "absent".
const (
	MaxOffers = 200
	MaxBytes  = 64 << 10
	MaxID     = 64
	MaxDetail = 200
	MaxAttrs  = 16
	MaxAttr   = 128
)

// Validate refuses a malformed snapshot and settles availability from
// evidence. It normalises in place: sorts offers, computes the digest.
func Validate(s *Snapshot) error {
	if s.Schema != Schema {
		return fmt.Errorf("snapshot schema %q, want %s", s.Schema, Schema)
	}
	if s.Node == "" {
		return errors.New("snapshot names no node")
	}
	if len(s.Offers) > MaxOffers {
		return fmt.Errorf("snapshot lists %d offers, limit %d", len(s.Offers), MaxOffers)
	}
	if s.Coverage == nil {
		s.Coverage = map[Kind]Coverage{}
	}
	seen := map[string]bool{}
	for i := range s.Offers {
		c := &s.Offers[i]
		if !c.Kind.valid() {
			return fmt.Errorf("offer %d: unknown kind %q", i, c.Kind)
		}
		if c.ID == "" || len(c.ID) > MaxID || strings.ContainsAny(c.ID, " \t\n@") {
			return fmt.Errorf("offer %d: bad id %q", i, c.ID)
		}
		if c.Kind.scoped() && c.Scope == "" {
			return fmt.Errorf("offer %s: %s needs a harness scope", c.ID, c.Kind)
		}
		if len(c.Detail) > MaxDetail {
			return fmt.Errorf("offer %s: detail too long", c.Key())
		}
		if len(c.Attrs) > MaxAttrs {
			return fmt.Errorf("offer %s: too many attrs", c.Key())
		}
		for k, v := range c.Attrs {
			if len(k) > MaxAttr || len(v) > MaxAttr {
				return fmt.Errorf("offer %s: attr too long", c.Key())
			}
		}
		if seen[c.Key()] {
			return fmt.Errorf("offer %s listed twice", c.Key())
		}
		seen[c.Key()] = true
		c.Availability = settle(*c)
	}
	sort.Slice(s.Offers, func(i, j int) bool { return s.Offers[i].Key() < s.Offers[j].Key() })
	raw, err := json.Marshal(s.Offers)
	if err != nil {
		return err
	}
	if len(raw) > MaxBytes {
		return fmt.Errorf("snapshot is %d bytes, limit %d", len(raw), MaxBytes)
	}
	s.Digest = digest(s.Offers, s.Coverage)
	return nil
}

// settle decides availability from evidence: an observation wins; a
// declaration alone only counts for kinds that cannot be observed.
func settle(c Capability) Availability {
	var declared, observed bool
	var ok bool
	for _, e := range c.Evidence {
		switch e.Kind {
		case Observed, Derived:
			observed = true
			ok = e.OK
		case Declared:
			declared = true
		}
	}
	switch {
	case observed && ok:
		return Available
	case observed:
		return Unavailable
	case declared && c.Kind.declaredOK():
		return Available
	case declared:
		// Declared only: shown as such, not counted on.
		return Unknown
	default:
		return Unknown
	}
}

// digest is content only: times are left out, so a self-check that found
// nothing new produces the same digest and no false drift.
func digest(offers []Capability, coverage map[Kind]Coverage) string {
	h := sha256.New()
	for _, c := range offers {
		fmt.Fprintf(h, "%s|%s|%s|%s|", c.Key(), c.Availability, c.Assurance, versionString(c.Version))
		keys := make([]string, 0, len(c.Attrs))
		for k := range c.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(h, "%s=%s;", k, c.Attrs[k])
		}
		h.Write([]byte("\n"))
	}
	ks := make([]string, 0, len(coverage))
	for k := range coverage {
		ks = append(ks, string(k))
	}
	sort.Strings(ks)
	for _, k := range ks {
		fmt.Fprintf(h, "cov:%s=%s\n", k, coverage[Kind(k)])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func versionString(v *Version) string {
	if v == nil {
		return ""
	}
	return v.Scheme + ":" + v.Value
}

// Fresh says whether a kind's evidence in this snapshot is recent enough
// to be trusted for a hard requirement. Networks and credentials go stale
// fast; a binary on PATH does not.
func Fresh(k Kind, at, now time.Time) bool {
	if at.IsZero() {
		return true
	}
	ttl := 15 * time.Minute
	switch k {
	case Network, Credential:
		ttl = 5 * time.Minute
	case Hardware:
		ttl = time.Hour
	case Tag:
		return true
	}
	return now.Sub(at) <= ttl
}

// Change is one difference between two snapshots.
type Change struct {
	Key  string `json:"key"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

func (c Change) String() string {
	switch {
	case c.From == "":
		return c.Key + " appeared (" + c.To + ")"
	case c.To == "":
		return c.Key + " disappeared"
	default:
		return c.Key + ": " + c.From + " → " + c.To
	}
}

// Diff says what changed between two snapshots, keyed and sorted.
func Diff(before, after *Snapshot) []Change {
	index := func(s *Snapshot) map[string]Capability {
		m := map[string]Capability{}
		if s == nil {
			return m
		}
		for _, c := range s.Offers {
			m[c.Key()] = c
		}
		return m
	}
	was, now := index(before), index(after)
	var out []Change
	for key, c := range now {
		prev, ok := was[key]
		switch {
		case !ok:
			out = append(out, Change{Key: key, To: string(c.Availability)})
		case prev.Availability != c.Availability:
			out = append(out, Change{Key: key, From: string(prev.Availability), To: string(c.Availability)})
		case versionString(prev.Version) != versionString(c.Version) && c.Version != nil:
			out = append(out, Change{Key: key, From: versionString(prev.Version), To: versionString(c.Version)})
		}
	}
	for key := range was {
		if _, ok := now[key]; !ok {
			out = append(out, Change{Key: key, From: string(was[key].Availability)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Compact renders what a machine offers as one line per kind, ids only,
// for a prompt: never detail, never paths. perKind bounds each list; the
// rest is counted, and the count says to ask for more.
func Compact(s *Snapshot, scope string, perKind int) string {
	if s == nil {
		return ""
	}
	if perKind <= 0 {
		perKind = 12
	}
	byKind := map[Kind][]string{}
	for _, c := range s.Offers {
		if c.Availability != Available || (c.Scope != "" && scope != "" && c.Scope != scope) {
			continue
		}
		byKind[c.Kind] = append(byKind[c.Kind], c.ID)
	}
	var parts []string
	for _, k := range kinds {
		ids := byKind[k]
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		extra := ""
		if len(ids) > perKind {
			extra = fmt.Sprintf(" (+%d more; ask steve_fleet)", len(ids)-perKind)
			ids = ids[:perKind]
		}
		parts = append(parts, string(k)+": "+strings.Join(ids, ", ")+extra)
	}
	return strings.Join(parts, " · ")
}
