package ability

import "time"

// Admission is the last check before an attempt runs: who evaluated the
// requirement, on which revision of which snapshot, and what it found. It
// is written on the attempt at leased→prepared, so "why was this allowed
// to run" can be answered later without trusting a placement decision made
// against a snapshot that may since have changed.
//
// Source says how much the verdict is worth: "node" is the machine
// re-checking its own offers on a fresh observation; "hub" is the hub
// evaluating the part of the requirement it owns (models, and hub-local
// machines); "cached" is a verdict from the hub's last accepted snapshot
// because the node could not be asked; "legacy" is a node that does not
// speak the admission protocol at all.
type Admission struct {
	Node       string       `json:"node,omitempty"`
	Source     string       `json:"source"`
	Verdict    Verdict      `json:"verdict"`
	Code       Code         `json:"code,omitempty"`
	Generation int64        `json:"generation,omitempty"`
	Sequence   int64        `json:"sequence,omitempty"`
	Digest     string       `json:"digest,omitempty"`
	Atoms      []AtomResult `json:"atoms,omitempty"`
	// Bound names the MCP servers the machine bound for this attempt's
	// session. The bindings themselves are not kept: they are keys.
	Bound []string  `json:"bound,omitempty"`
	At    time.Time `json:"at"`
}

// Binding is how a session reaches one MCP server the machine bound for
// it: a launcher command the agent can run, which carries no secret.
type Binding struct {
	Name string `json:"name"`
	// Transport is "" or "stdio" for a launcher command, "http" or "sse"
	// for a URL on the machine's loopback proxy.
	Transport string   `json:"transport,omitempty"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	URL       string   `json:"url,omitempty"`
}

const (
	SourceNode   = "node"
	SourceHub    = "hub"
	SourceCached = "cached"
	SourceLegacy = "legacy"

	// CodeAdmitted is the code of a verdict that found nothing wanting.
	CodeAdmitted Code = "ADMITTED"
	// CodeNoBinding says the work needs an MCP server bound on the machine
	// and the machine does not bind them: an older node.
	CodeNoBinding Code = "NO_MCP_BINDING"
	// CodeNoAdmission says the machine could not be asked: an older node,
	// or a source that cannot re-observe. The verdict is Unsure.
	CodeNoAdmission Code = "NO_ADMISSION"
)

// OK says the admission found every requirement met.
func (a Admission) OK() bool { return a.Verdict == True }

// Refused says the admission found a requirement definitely unmet. Unsure
// is neither: the caller decides whether uncertainty may run.
func (a Admission) Refused() bool { return a.Verdict == False }

// Unmet renders what was wanting, one atom per clause.
func (a Admission) Unmet() string {
	return MatchResult{Verdict: a.Verdict, Atoms: a.Atoms, Digest: a.Digest}.Unmet()
}

// AdmissionOf turns a match into an admission record against the snapshot
// it was evaluated on.
func AdmissionOf(m MatchResult, s *Snapshot, source string, now time.Time) Admission {
	a := Admission{Source: source, Verdict: m.Verdict, Atoms: m.Atoms, At: now, Code: CodeAdmitted}
	if s != nil {
		a.Node, a.Generation, a.Sequence, a.Digest = s.Node, s.Generation, s.Sequence, s.Digest
	}
	if m.Verdict != True {
		a.Code = CodeUnknown
		for _, atom := range m.Atoms {
			if atom.Verdict == False && atom.Code != "" {
				a.Code = atom.Code
				break
			}
		}
		if a.Code == CodeUnknown {
			for _, atom := range m.Atoms {
				if atom.Verdict == Unsure && atom.Code != "" {
					a.Code = atom.Code
					break
				}
			}
		}
	}
	return a
}

// NodeOwned says a kind is something the machine itself observes and can
// re-check at admission. Models are observed by the hub through the
// harness, so they are the hub's to judge.
func NodeOwned(k Kind) bool { return k.Schedulable() && k != Model }

// Partition splits a requirement into the clauses a judge owns outright
// and the rest. Only top-level AND clauses are split, and a clause goes
// to the judge only if every atom in it is theirs: an "any" mixing owned
// and foreign atoms stays with the caller, because its truth needs both.
// Both halves together are the whole requirement.
func Partition(req Requirement, owned func(Kind) bool) (mine, rest Requirement) {
	clauses := []Requirement{req}
	if len(req.All) > 0 && len(req.Any) == 0 && len(req.OneOf) == 0 && req.Not == nil && req.Cap == nil {
		clauses = req.All
	}
	for _, clause := range clauses {
		if clause.Empty() {
			continue
		}
		if every(clause, owned) {
			mine.All = append(mine.All, clause)
		} else {
			rest.All = append(rest.All, clause)
		}
	}
	return mine, rest
}

func every(r Requirement, owned func(Kind) bool) bool {
	if r.Cap != nil && !owned(r.Cap.Kind) {
		return false
	}
	if r.Not != nil && !every(*r.Not, owned) {
		return false
	}
	for _, group := range [][]Requirement{r.All, r.Any, r.OneOf} {
		for _, child := range group {
			if !every(child, owned) {
				return false
			}
		}
	}
	return true
}
