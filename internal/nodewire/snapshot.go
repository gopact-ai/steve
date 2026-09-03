package nodewire

import (
	"time"

	"github.com/gopact-ai/steve/internal/ability"
)

// AdmitRequest asks a node for its final word on the part of a requirement
// it owns. The hub sends the tree, not text, so there is no second parser
// to disagree with the first; Generation and Sequence say which snapshot
// the hub placed on, so the reply can be read against it.
type AdmitRequest struct {
	Attempt     string              `json:"attempt"`
	Harness     string              `json:"harness"`
	Requirement ability.Requirement `json:"requirement"`
	Generation  int64               `json:"generation,omitempty"`
	Sequence    int64               `json:"sequence,omitempty"`
}

// AdmitReply is the node's verdict, on the snapshot it just took. Error is
// set when the node could not evaluate at all.
type AdmitReply struct {
	Admission ability.Admission `json:"admission"`
	Error     string            `json:"error,omitempty"`
}

// Features a node or hub may support beyond the base protocol. The
// handshake exchanges them; a hub opens a stream only to a node that
// lists the feature it needs, and treats a node without one as older,
// never as broken.
const (
	FeatureManifest  = "manifest.v1"
	FeatureAdmission = "execution_admission.v1"
	// FeatureSkills says the node takes skill bundles: a content-addressed
	// tar the hub puts in its blob directory and asks it to materialize
	// into every harness home it isolates.
	FeatureSkills = "skill_bundle.v1"
)

// Features is what this build supports.
func Features() []string { return []string{FeatureManifest, FeatureAdmission, FeatureSkills} }

// HasFeature says whether a list names a feature.
func HasFeature(list []string, feature string) bool {
	for _, f := range list {
		if f == feature {
			return true
		}
	}
	return false
}

// Synthesize builds a snapshot for an advert that carries none — an
// older node. It knows only what the old fields say: harnesses and tag
// words. Coverage is partial for everything else, so a requirement on a
// tool is unknown there, not absent; the source says legacy.
func Synthesize(adv Advert, now time.Time) *ability.Snapshot {
	if adv.Snapshot != nil {
		return adv.Snapshot
	}
	s := &ability.Snapshot{
		Schema: ability.Schema, Node: adv.Node, GeneratedAt: now, ReceivedAt: now, Source: "legacy",
		Coverage: map[ability.Kind]ability.Coverage{ability.Harness: ability.Complete, ability.Tag: ability.Complete},
	}
	for _, h := range adv.Harnesses {
		c := ability.Capability{Kind: ability.Harness, ID: h.ID, Assurance: ability.Existence,
			Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "advert", OK: h.Missing == "", Result: h.Missing, At: now}}}
		if h.Missing != "" {
			c.Detail = h.Missing
		}
		s.Offers = append(s.Offers, c)
	}
	for _, tag := range adv.Capabilities {
		s.Offers = append(s.Offers, ability.Capability{Kind: ability.Tag, ID: tag, Evidence: []ability.Evidence{{Kind: ability.Declared, Method: "config", OK: true}}})
	}
	if s.Node == "" {
		s.Node = "hub"
	}
	_ = ability.Validate(s)
	return s
}
