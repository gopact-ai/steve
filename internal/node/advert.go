package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/mcpscan"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/skills"
)

// advert reports what this machine can honestly do. Harnesses whose command
// is not on this node's PATH are listed as missing rather than omitted: a
// roster that hides what is broken sends the hub hunting for a node that
// silently vanished.
func (s *Server) advert() nodewire.Advert {
	adv := Advertise(s.conf().Name, s.conf().Harnesses, s.conf().Capabilities)
	adv.Snapshot = s.snapshot()
	adv.Features = nodewire.Features()
	if s.sessions != nil {
		adv.Features = append(adv.Features, nodewire.FeatureNodeSessions)
	}
	s.restart.mu.Lock()
	if s.restart.enabled && s.conf().StateDir != "" {
		adv.Features = append(adv.Features, nodewire.FeatureRestart)
	}
	s.restart.mu.Unlock()
	adv.SessionGraceMS = s.sessionGrace().Milliseconds()
	adv.WorkspaceRoot = s.conf().WorkspaceRoot
	adv.StateDir = s.conf().StateDir
	adv.Skills = s.currentSkills()
	adv.OwnSkills = OwnSkills(5 * time.Minute)
	adv.OwnMCP = OwnMCP(5 * time.Minute)
	adv.Health = CheckHealth(s.conf().WorkspaceRoot, s.conf().StateDir)
	// The reverse messaging port is part of every advert, not only the
	// handshake's: a refresh that dropped it would leave the hub thinking
	// this machine's agents cannot reach it.
	s.mu.Lock()
	adv.MCPPort = s.mcpPort
	s.mu.Unlock()
	return adv
}

// ownMCP is this machine's scan of its coding agents' own MCP servers,
// shapes only; the advert carries it.
var ownMCP mcpscan.Local

// OwnMCP is the machine's own MCP servers, rescanned when older than maxAge.
func OwnMCP(maxAge time.Duration) []nodewire.OwnMCP {
	found := ownMCP.Get(maxAge)
	out := make([]nodewire.OwnMCP, 0, len(found))
	for _, f := range found {
		out = append(out, nodewire.OwnMCP{Name: f.Name, Source: f.Source, Scope: f.Scope, Type: f.Type, Command: f.Command, Args: f.Args, URL: f.URL, EnvKeys: f.EnvKeys, HeaderKeys: f.HeaderKeys})
	}
	return out
}

// ownSkills is this machine's scan of its AI tools' own skills; the
// advert carries it, so the hub's refresh loop keeps its copy fresh.
var ownSkills skills.Local

// OwnSkills is the machine's own skills, rescanned when older than maxAge.
func OwnSkills(maxAge time.Duration) []nodewire.OwnSkill {
	found := ownSkills.Get(maxAge)
	out := make([]nodewire.OwnSkill, 0, len(found))
	for _, f := range found {
		out = append(out, nodewire.OwnSkill{Name: f.Name, Path: f.Path, Title: f.Title, Description: f.Description})
	}
	return out
}

// snapshot observes this machine now, as the next revision.
func (s *Server) snapshot() *ability.Snapshot {
	o := Observe{Harnesses: s.conf().Harnesses, Tools: s.conf().Tools, MCP: s.conf().MCPServers, Declares: s.conf().Declares, Tags: s.conf().Capabilities, Launch: s.launch.Lookup, Skills: s.skillEntries(), SkillsKnown: true}
	if rb, ok := s.broker.(remoteBroker); ok {
		// The servers are the broker's: what it lists is what there is,
		// and the broker vouches for them, not a PATH lookup here.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		listed, err := rb.List(ctx)
		if err != nil {
			log.Printf("steve-node: mcp broker: %v", err)
			o.MCPError = err.Error()
		} else {
			o.MCPListed = listed
		}
	}
	return Snapshot(s.conf().Name, s.generation, s.nextSequence(), o)
}

// admit is the node's final word before an attempt runs here: the hub
// placed on a snapshot it accepted earlier; the node re-checks the clauses
// it owns on an observation taken now and says what it found, with the
// revision it found it on. A refusal is a verdict, not an error.
func (s *Server) admit(stream *nodewire.Stream) {
	defer stream.Close()
	var req nodewire.AdmitRequest
	if err := json.NewDecoder(stream).Decode(&req); err != nil {
		_ = json.NewEncoder(stream).Encode(nodewire.AdmitReply{Error: "read request: " + err.Error()})
		return
	}
	snap := s.snapshot()
	if snap == nil {
		_ = json.NewEncoder(stream).Encode(nodewire.AdmitReply{Error: "this node cannot observe itself"})
		return
	}
	now := time.Now().UTC()
	m := ability.Match(req.Requirement, snap, req.Harness, now)
	adm := ability.AdmissionOf(m, snap, ability.SourceNode, now)
	// What the session will use is bound now, or the admission fails: a
	// server this machine does not have, or cannot start for a session,
	// is a definite no.
	var bindings []ability.Binding
	for _, id := range req.Uses {
		atom := "mcp:" + id
		if s.broker == nil {
			adm.Verdict, adm.Code = ability.False, ability.CodeAbsent
			adm.Atoms = append(adm.Atoms, ability.AtomResult{Atom: atom, Verdict: ability.False, Code: ability.CodeAbsent, Detail: "this node has no MCP servers"})
			continue
		}
		if adm.Verdict != ability.True {
			continue
		}
		d, err := s.broker.Bind(context.Background(), id, req.Attempt, req.Harness)
		switch {
		case errors.Is(err, ErrNoSuchServer):
			adm.Verdict, adm.Code = ability.False, ability.CodeAbsent
			adm.Atoms = append(adm.Atoms, ability.AtomResult{Atom: atom, Verdict: ability.False, Code: ability.CodeAbsent})
		case errors.Is(err, ErrUnbindable):
			adm.Verdict, adm.Code = ability.False, ability.CodeUnavailable
			adm.Atoms = append(adm.Atoms, ability.AtomResult{Atom: atom, Verdict: ability.False, Code: ability.CodeUnavailable, Detail: err.Error()})
		case err != nil:
			_ = json.NewEncoder(stream).Encode(nodewire.AdmitReply{Error: "bind " + id + ": " + err.Error()})
			return
		default:
			bindings = append(bindings, d)
			adm.Bound = append(adm.Bound, id)
		}
	}
	if adm.Verdict != ability.True {
		bindings, adm.Bound = nil, nil
	}
	log.Printf("steve-node: admission for attempt %s (%s): %s at %d/%d, bound %v", req.Attempt, req.Harness, adm.Verdict, snap.Generation, snap.Sequence, adm.Bound)
	if err := json.NewEncoder(stream).Encode(nodewire.AdmitReply{Admission: adm, Bindings: bindings, Nonce: req.Nonce}); err != nil {
		log.Printf("steve-node: admission reply: %v", err)
	}
}

// Advertise describes the machine this process runs on: its identity, and
// each configured harness checked against the PATH right here. The hub
// uses it for its own machine, so the fleet has one shape for every node
// and the coordinating one is not described by its config alone.
func Advertise(name string, specs map[string]HarnessSpec, caps []string) nodewire.Advert {
	ids := make([]string, 0, len(specs))
	for id := range specs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	harnesses := make([]nodewire.Harness, 0, len(ids))
	for _, id := range ids {
		spec := specs[id]
		h := nodewire.Harness{ID: id, Command: spec.Command, Models: spec.Models, Slots: spec.Slots}
		if _, err := exec.LookPath(spec.Command); err != nil {
			h.Missing = fmt.Sprintf("%q not on this node's PATH", spec.Command)
		}
		harnesses = append(harnesses, h)
	}
	hostname, ips := nodewire.Identity()
	git := gitVersion()
	return nodewire.Advert{
		Node: name, OS: runtime.GOOS, Arch: runtime.GOARCH,
		BuildVersion: nodewire.Version(), Hostname: hostname, IPs: ips,
		Harnesses: harnesses, Capabilities: caps,
		Git: git, GitMinimum: nodewire.MinimumGitVersion, GitWarning: nodewire.GitWarning(git),
	}
}

// sendAdvert answers a hub asking "check yourself again": the same advert
// the handshake carried, computed now, so a harness installed since then
// is seen without dropping the connection.
func (s *Server) sendAdvert(stream *nodewire.Stream) {
	defer stream.Close()
	s.touchOwner()
	if err := json.NewEncoder(stream).Encode(s.advert()); err != nil {
		log.Printf("steve-node: send advert: %v", err)
	}
}

// gitVersion reports the node's git, or nothing.
func gitVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(out), "git version "))
}
