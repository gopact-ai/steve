package ability

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RequirementVersion is the AST's shape; bumped when meaning changes.
const RequirementVersion = "requirement.v1"

// Requirement is what work asks of a machine: a tree of all / any / not /
// one_of over capability atoms. The text form ("tool:docker", "a|b",
// "!network:public", "tool:docker@>=27") is only an input boundary; plans
// and attempts keep the tree.
type Requirement struct {
	All   []Requirement `json:"all,omitempty"`
	Any   []Requirement `json:"any,omitempty"`
	OneOf []Requirement `json:"one_of,omitempty"`
	Not   *Requirement  `json:"not,omitempty"`
	Cap   *Atom         `json:"cap,omitempty"`
}

// Atom is one capability the work needs.
type Atom struct {
	Kind    Kind              `json:"kind"`
	ID      string            `json:"id,omitempty"`
	Prefix  string            `json:"prefix,omitempty"`
	Scope   string            `json:"scope,omitempty"`
	Version string            `json:"version,omitempty"` // constraint, e.g. ">=27 <28" (semver) or "=1.2" (opaque)
	Attrs   map[string]string `json:"attrs,omitempty"`   // "k": ">=8" or "=x"
}

func (a Atom) String() string {
	id := a.ID
	if a.Prefix != "" {
		id = a.Prefix + "*"
	}
	s := string(a.Kind) + ":" + id
	if a.Version != "" {
		s += "@" + a.Version
	}
	return s
}

// Empty says the requirement asks nothing.
func (r Requirement) Empty() bool {
	return len(r.All) == 0 && len(r.Any) == 0 && len(r.OneOf) == 0 && r.Not == nil && r.Cap == nil
}

// Compile turns the text list into a tree: the list is an AND; within one
// item "a|b" is an OR, a leading "!" a NOT, "@…" a version constraint. A
// bare word is a tag; "any" is nothing.
func Compile(requires []string) (Requirement, error) {
	var all []Requirement
	for _, raw := range requires {
		item, ok, err := compileItem(raw)
		if err != nil {
			return Requirement{}, err
		}
		if ok {
			all = append(all, item)
		}
	}
	switch len(all) {
	case 0:
		return Requirement{}, nil
	case 1:
		return all[0], nil
	default:
		return Requirement{All: all}, nil
	}
}

func compileItem(raw string) (Requirement, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "any" {
		return Requirement{}, false, nil
	}
	if strings.Contains(raw, "|") {
		var any []Requirement
		for _, part := range strings.Split(raw, "|") {
			item, ok, err := compileItem(part)
			if err != nil {
				return Requirement{}, false, err
			}
			if ok {
				any = append(any, item)
			}
		}
		if len(any) == 0 {
			return Requirement{}, false, nil
		}
		return Requirement{Any: any}, true, nil
	}
	if strings.HasPrefix(raw, "!") {
		inner, ok, err := compileItem(raw[1:])
		if err != nil || !ok {
			return Requirement{}, false, err
		}
		return Requirement{Not: &inner}, true, nil
	}
	atom, err := ParseAtom(raw)
	if err != nil {
		return Requirement{}, false, err
	}
	return Requirement{Cap: &atom}, true, nil
}

// ParseAtom reads "kind:id", "kind:prefix*", "kind:id@scope" is NOT
// supported in text (scope comes from the agent), "kind:id@>=1.2" is a
// version constraint. A bare word is a tag.
func ParseAtom(raw string) (Atom, error) {
	raw = strings.TrimSpace(raw)
	kind, rest, has := strings.Cut(raw, ":")
	if !has {
		if strings.ContainsAny(raw, " @*!|") {
			return Atom{}, fmt.Errorf("requirement %q: a tag is one word", raw)
		}
		return Atom{Kind: Tag, ID: raw}, nil
	}
	k := Kind(kind)
	if !k.valid() {
		return Atom{}, fmt.Errorf("requirement %q: unknown kind %q", raw, kind)
	}
	id, version, _ := strings.Cut(rest, "@")
	id = strings.TrimSpace(id)
	if id == "" {
		return Atom{}, fmt.Errorf("requirement %q: empty id", raw)
	}
	if strings.ContainsAny(id, " \t|!") {
		return Atom{}, fmt.Errorf("requirement %q: bad id", raw)
	}
	a := Atom{Kind: k, Version: strings.TrimSpace(version)}
	if strings.HasSuffix(id, "*") {
		a.Prefix = strings.TrimSuffix(id, "*")
	} else {
		a.ID = id
	}
	return a, nil
}

// ValidateText refuses a requirement list that could never be matched.
func ValidateText(requires []string) error {
	_, err := Compile(requires)
	return err
}

// Verdict is three-valued: a requirement can be met, not met, or not
// decidable from what the snapshot covers.
type Verdict int

const (
	False Verdict = iota
	True
	Unsure
)

func (v Verdict) String() string {
	switch v {
	case True:
		return "true"
	case False:
		return "false"
	default:
		return "unknown"
	}
}

// Code says why an atom was not satisfied, in a form a program can act on.
type Code string

const (
	CodeAbsent       Code = "ABSENT"           // nothing of that kind and id
	CodeUnavailable  Code = "UNAVAILABLE"      // configured, but the check failed
	CodeDeclaredOnly Code = "DECLARED_ONLY"    // only a declaration, for a kind that must be observed
	CodeUnknown      Code = "UNKNOWN_COVERAGE" // the snapshot did not cover this kind
	CodeStale        Code = "STALE"            // the evidence is too old
	CodeVersion      Code = "VERSION"          // present, wrong version
	CodeAttr         Code = "ATTR"             // present, attribute does not satisfy
	CodeScope        Code = "SCOPE"            // present for another harness only
	CodeGated        Code = "NOT_SCHEDULABLE"  // this kind does not take part in placement yet
	CodeNegated      Code = "PRESENT"          // a NOT atom found the thing present
)

// AtomResult is one atom's verdict with its reason.
type AtomResult struct {
	Atom    string  `json:"atom"`
	Verdict Verdict `json:"verdict"`
	Code    Code    `json:"code,omitempty"`
	Matched string  `json:"matched,omitempty"` // the capability key that satisfied it
	Detail  string  `json:"detail,omitempty"`
}

// MatchResult is a whole requirement's verdict against one machine for
// one harness.
type MatchResult struct {
	Verdict Verdict      `json:"verdict"`
	Atoms   []AtomResult `json:"atoms"`
	Digest  string       `json:"digest,omitempty"`
}

// OK is true only when everything was met.
func (m MatchResult) OK() bool { return m.Verdict == True }

// Unmet lists the atoms that failed or could not be decided, in words a
// message can use.
func (m MatchResult) Unmet() string {
	var parts []string
	for _, a := range m.Atoms {
		if a.Verdict == True {
			continue
		}
		s := a.Atom + " (" + string(a.Code)
		if a.Detail != "" {
			s += ": " + a.Detail
		}
		parts = append(parts, s+")")
	}
	return strings.Join(parts, "; ")
}

// Match evaluates a requirement against a snapshot for an agent whose
// harness is scope, at time now. Scoped kinds (model, mcp, skill) match
// only entries for that harness. Unknown coverage yields Unsure, never
// False: "not listed" is only "absent" when the list was complete.
func Match(req Requirement, s *Snapshot, scope string, now time.Time) MatchResult {
	out := MatchResult{}
	if s != nil {
		out.Digest = s.Digest
	}
	out.Verdict = eval(req, s, scope, now, &out.Atoms)
	return out
}

func eval(r Requirement, s *Snapshot, scope string, now time.Time, atoms *[]AtomResult) Verdict {
	switch {
	case r.Cap != nil:
		res := evalAtom(*r.Cap, s, scope, now)
		*atoms = append(*atoms, res)
		return res.Verdict
	case r.Not != nil:
		inner := eval(*r.Not, s, scope, now, atoms)
		// The atom's own row already says what was found; a NOT that
		// found it present is a failure, an unknown stays unknown.
		if n := len(*atoms); n > 0 && (*atoms)[n-1].Verdict == True {
			(*atoms)[n-1] = AtomResult{Atom: "!" + (*atoms)[n-1].Atom, Verdict: False, Code: CodeNegated, Matched: (*atoms)[n-1].Matched}
		} else if n > 0 && inner == False {
			(*atoms)[n-1] = AtomResult{Atom: "!" + (*atoms)[n-1].Atom, Verdict: True}
		}
		return not(inner)
	case len(r.All) > 0:
		v := True
		for _, x := range r.All {
			v = and(v, eval(x, s, scope, now, atoms))
		}
		return v
	case len(r.Any) > 0:
		v := False
		for _, x := range r.Any {
			v = or(v, eval(x, s, scope, now, atoms))
		}
		return v
	case len(r.OneOf) > 0:
		trues, unsure := 0, 0
		for _, x := range r.OneOf {
			switch eval(x, s, scope, now, atoms) {
			case True:
				trues++
			case Unsure:
				unsure++
			}
		}
		switch {
		case trues == 1 && unsure == 0:
			return True
		case trues > 1:
			return False
		case unsure > 0:
			return Unsure
		default:
			return False
		}
	default:
		return True
	}
}

func and(a, b Verdict) Verdict {
	if a == False || b == False {
		return False
	}
	if a == Unsure || b == Unsure {
		return Unsure
	}
	return True
}

func or(a, b Verdict) Verdict {
	if a == True || b == True {
		return True
	}
	if a == Unsure || b == Unsure {
		return Unsure
	}
	return False
}

func not(v Verdict) Verdict {
	switch v {
	case True:
		return False
	case False:
		return True
	default:
		return Unsure
	}
}

func evalAtom(a Atom, s *Snapshot, scope string, now time.Time) AtomResult {
	res := AtomResult{Atom: a.String(), Verdict: False}
	if !a.Kind.Schedulable() {
		// Neither present nor absent can be asserted for a kind that is
		// not judged yet: a NOT over it must stay unknown too.
		res.Verdict, res.Code, res.Detail = Unsure, CodeGated, "this kind does not take part in placement yet"
		return res
	}
	if s == nil {
		res.Verdict, res.Code = Unsure, CodeUnknown
		return res
	}
	var best *Capability
	var bestCode Code
	scopeMiss := false
	for i := range s.Offers {
		c := &s.Offers[i]
		if c.Kind != a.Kind {
			continue
		}
		if !(a.ID != "" && c.ID == a.ID || a.Prefix != "" && strings.HasPrefix(c.ID, a.Prefix) || a.ID == "" && a.Prefix == "") {
			continue
		}
		if a.Kind.scoped() && scope != "" && c.Scope != scope {
			scopeMiss = true
			continue
		}
		code := judge(*c, a, now)
		if code == "" {
			res.Verdict, res.Matched = True, c.Key()
			return res
		}
		// Keep the most informative failure: a wrong version beats absent.
		if best == nil || rank(code) > rank(bestCode) {
			best, bestCode = c, code
		}
	}
	switch {
	case best != nil:
		res.Code, res.Matched = bestCode, best.Key()
		if bestCode == CodeUnavailable || bestCode == CodeVersion || bestCode == CodeAttr {
			res.Detail = best.Detail
		}
		if bestCode == CodeStale {
			res.Verdict = Unsure
		}
	case scopeMiss:
		res.Code, res.Detail = CodeScope, "present for another harness on this machine"
	case a.Kind != Tag && s.Coverage[a.Kind] != Complete:
		// Tags are declarations: the list of them is complete by nature.
		res.Verdict, res.Code = Unsure, CodeUnknown
		res.Detail = "the machine did not report this kind fully"
	default:
		res.Code = CodeAbsent
	}
	return res
}

func rank(c Code) int {
	switch c {
	case CodeVersion, CodeAttr:
		return 4
	case CodeUnavailable:
		return 3
	case CodeStale:
		return 2
	case CodeDeclaredOnly:
		return 1
	}
	return 0
}

// judge says why a capability that matched by kind and id still does not
// satisfy the atom, or "" when it does.
func judge(c Capability, a Atom, now time.Time) Code {
	switch c.Availability {
	case Unavailable:
		return CodeUnavailable
	case Unknown:
		if hasDeclared(c) && !c.Kind.declaredOK() {
			return CodeDeclaredOnly
		}
		return CodeUnavailable
	}
	if at := latest(c); !at.IsZero() && !Fresh(c.Kind, at, now) {
		return CodeStale
	}
	if a.Version != "" && !versionOK(c.Version, a.Version) {
		return CodeVersion
	}
	for k, want := range a.Attrs {
		if !attrOK(c.Attrs[k], want) {
			return CodeAttr
		}
	}
	return ""
}

func hasDeclared(c Capability) bool {
	for _, e := range c.Evidence {
		if e.Kind == Declared {
			return true
		}
	}
	return false
}

func latest(c Capability) time.Time {
	var at time.Time
	for _, e := range c.Evidence {
		if e.Kind != Declared && e.At.After(at) {
			at = e.At
		}
	}
	return at
}

// versionOK checks a constraint against a version. Semver constraints are
// space-separated comparators (">=27 <28"); opaque versions only equal.
func versionOK(v *Version, constraint string) bool {
	if v == nil {
		return false
	}
	constraint = strings.TrimSpace(constraint)
	if v.Scheme == "" || v.Scheme == "opaque" {
		return strings.TrimPrefix(constraint, "=") == v.Value
	}
	for _, part := range strings.Fields(constraint) {
		if !compare(v.Value, part) {
			return false
		}
	}
	return true
}

// compare handles one comparator against a dotted numeric version.
func compare(value, comparator string) bool {
	op := "="
	for _, candidate := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(comparator, candidate) {
			op = candidate
			comparator = strings.TrimPrefix(comparator, candidate)
			break
		}
	}
	a, b := nums(value), nums(comparator)
	c := cmp(a, b)
	switch op {
	case ">=":
		return c >= 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case "<":
		return c < 0
	case "^":
		return c >= 0 && len(a) > 0 && len(b) > 0 && a[0] == b[0]
	case "~":
		return c >= 0 && len(a) > 1 && len(b) > 1 && a[0] == b[0] && a[1] == b[1]
	default:
		return c == 0
	}
}

func nums(s string) []int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	var out []int
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func cmp(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// attrOK checks one attribute constraint: "=v", ">=8", "<16", or a bare
// value for equality.
func attrOK(have, want string) bool {
	if have == "" {
		return false
	}
	for _, op := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(want, op) {
			rest := strings.TrimSpace(strings.TrimPrefix(want, op))
			if op == "=" {
				return have == rest
			}
			x, errX := strconv.ParseFloat(have, 64)
			y, errY := strconv.ParseFloat(rest, 64)
			if errX != nil || errY != nil {
				return false
			}
			switch op {
			case ">=":
				return x >= y
			case "<=":
				return x <= y
			case ">":
				return x > y
			case "<":
				return x < y
			}
		}
	}
	return have == want
}

// Text renders a requirement back to its text form, canonical order.
func Text(r Requirement) string {
	switch {
	case r.Cap != nil:
		return r.Cap.String()
	case r.Not != nil:
		return "!" + Text(*r.Not)
	case len(r.Any) > 0:
		parts := make([]string, 0, len(r.Any))
		for _, x := range r.Any {
			parts = append(parts, Text(x))
		}
		return strings.Join(parts, "|")
	case len(r.OneOf) > 0:
		parts := make([]string, 0, len(r.OneOf))
		for _, x := range r.OneOf {
			parts = append(parts, Text(x))
		}
		sort.Strings(parts)
		return "one_of(" + strings.Join(parts, ",") + ")"
	case len(r.All) > 0:
		parts := make([]string, 0, len(r.All))
		for _, x := range r.All {
			parts = append(parts, Text(x))
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

// ErrNotSchedulable is returned when a requirement names a kind that does
// not take part in placement yet.
var ErrNotSchedulable = errors.New("requirement names a kind that is not schedulable yet")
