package node

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/skills"
)

// Advert returns what the node last told us it can run.
func (r *Registry) Advert(ctx context.Context, name string) (nodewire.Advert, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return nodewire.Advert{}, err
	}
	return c.getAdvert(), nil
}

// Refresh asks a connected node to check itself again and returns the fresh
// advert. A harness repaired after the handshake becomes visible this way,
// without dropping the connection and every session riding on it.
func (r *Registry) Refresh(ctx context.Context, name string) (nodewire.Advert, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return nodewire.Advert{}, err
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamAdvert})
	if err != nil {
		return nodewire.Advert{}, fmt.Errorf("node %q: ask for advert: %w", name, err)
	}
	defer stream.Close()
	var adv nodewire.Advert
	if err := json.NewDecoder(stream).Decode(&adv); err != nil {
		return nodewire.Advert{}, fmt.Errorf("node %q: read advert: %w", name, err)
	}
	r.accept(name, &adv)
	r.noteDrift(name, adv)
	if adv.MCPPort == 0 {
		// An older node reports its messaging port only at the handshake;
		// the connection still has it.
		adv.MCPPort = c.getAdvert().MCPPort
	}
	c.setAdvert(adv)
	r.mu.Lock()
	if last := r.last[name]; last != nil && last.Up {
		last.Advert = adv
	}
	r.mu.Unlock()
	return adv, nil
}

// Admit asks a node for its final word on the clauses of a requirement it
// owns, on an observation it takes now. A node that does not speak the
// admission protocol answers Unsure with NO_ADMISSION rather than an
// error: the caller records that nobody re-checked, and decides.
func (r *Registry) Admit(ctx context.Context, name string, req nodewire.AdmitRequest) (ability.Admission, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return ability.Admission{}, err
	}
	if !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureAdmission) {
		return ability.Admission{Node: name, Source: ability.SourceLegacy, Verdict: ability.Unsure, Code: ability.CodeNoAdmission, At: time.Now().UTC()}, nil
	}
	if len(req.Uses) > 0 && !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureMCP) {
		adm := ability.Admission{Node: name, Source: ability.SourceLegacy, Verdict: ability.False, Code: ability.CodeNoBinding, At: time.Now().UTC()}
		for _, id := range req.Uses {
			adm.Atoms = append(adm.Atoms, ability.AtomResult{Atom: "mcp:" + id, Verdict: ability.False, Code: ability.CodeNoBinding})
		}
		return adm, nil
	}
	var nonce [12]byte
	_, _ = cryptorand.Read(nonce[:])
	req.Nonce = hex.EncodeToString(nonce[:])
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamAdmit})
	if err != nil {
		return ability.Admission{}, fmt.Errorf("node %q: ask for admission: %w", name, err)
	}
	defer stream.Close()
	if err := json.NewEncoder(stream).Encode(req); err != nil {
		return ability.Admission{}, fmt.Errorf("node %q: send admission request: %w", name, err)
	}
	var reply nodewire.AdmitReply
	if err := json.NewDecoder(stream).Decode(&reply); err != nil {
		return ability.Admission{}, fmt.Errorf("node %q: read admission: %w", name, err)
	}
	if reply.Error != "" {
		return ability.Admission{}, fmt.Errorf("node %q: admission: %s", name, reply.Error)
	}
	if reply.Nonce != req.Nonce {
		return ability.Admission{}, fmt.Errorf("node %q: admission reply answers another request", name)
	}
	c.bindingsMu.Lock()
	if c.lastBindings == nil {
		c.lastBindings = map[string][]ability.Binding{}
	}
	c.lastBindings[req.Attempt] = reply.Bindings
	c.bindingsMu.Unlock()
	return reply.Admission, nil
}

// Inspect asks a node what repositories a directory holds.
func (r *Registry) Inspect(ctx context.Context, name, path string) ([]nodewire.Repo, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return nil, err
	}
	if !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureInspect) {
		return nil, fmt.Errorf("node %q runs an older steve-node that cannot inspect directories", name)
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamInspect, Command: path})
	if err != nil {
		return nil, fmt.Errorf("node %q: inspect: %w", name, err)
	}
	defer stream.Close()
	var reply nodewire.InspectReply
	if err := json.NewDecoder(stream).Decode(&reply); err != nil {
		return nil, fmt.Errorf("node %q: inspect: %w", name, err)
	}
	if reply.Error != "" {
		return nil, errors.New(reply.Error)
	}
	return reply.Repos, nil
}

// MCPProbe asks a node what tools one of its MCP servers offers.
func (r *Registry) MCPProbe(ctx context.Context, name, server string) (nodewire.MCPProbeReply, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return nodewire.MCPProbeReply{}, err
	}
	if !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureMCPProbe) {
		return nodewire.MCPProbeReply{}, fmt.Errorf("node %q runs an older steve-node that cannot probe MCP servers", name)
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamMCPProbe, Command: server})
	if err != nil {
		return nodewire.MCPProbeReply{}, fmt.Errorf("node %q: probe: %w", name, err)
	}
	defer stream.Close()
	var reply nodewire.MCPProbeReply
	if err := json.NewDecoder(stream).Decode(&reply); err != nil {
		return nodewire.MCPProbeReply{}, fmt.Errorf("node %q: probe: %w", name, err)
	}
	return reply, nil
}

// AdoptMCP tells a node to copy one of its coding agents' own MCP
// servers into its settings; the values never pass through here.
func (r *Registry) AdoptMCP(ctx context.Context, name, source, server string) (nodewire.Settings, error) {
	if strings.ContainsAny(source+server, " \t\n") {
		return nodewire.Settings{}, fmt.Errorf("bad source or server name")
	}
	return r.configStream(ctx, name, "adopt "+source+" "+server, nil)
}

// Settings reads what a node offers, as its operator wrote it.
func (r *Registry) Settings(ctx context.Context, name string) (nodewire.Settings, error) {
	return r.configStream(ctx, name, "get", nil)
}

// Configure rewrites what a node offers. The node validates, writes its
// own file, and answers with what is in force; a fresh advert follows.
func (r *Registry) Configure(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	out, err := r.configStream(ctx, name, "set", &set)
	if err != nil && !settingsCommitted(err) {
		return out, err
	}
	if _, err := r.Refresh(ctx, name); err != nil {
		log.Printf("node: %s: refresh after configure: %v", name, err)
	}
	return out, err
}

func (r *Registry) configStream(ctx context.Context, name, verb string, set *nodewire.Settings) (nodewire.Settings, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return nodewire.Settings{}, err
	}
	if !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureConfig) {
		return nodewire.Settings{}, fmt.Errorf("node %q runs an older steve-node that cannot be configured from here; edit its node.json", name)
	}
	if set != nil && !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureConfigRevision) {
		return nodewire.Settings{}, nodewire.ErrSettingsRevisionUnsupported
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamConfig, Command: verb})
	if err != nil {
		return nodewire.Settings{}, fmt.Errorf("node %q: config: %w", name, err)
	}
	defer stream.Close()
	if set != nil {
		if err := json.NewEncoder(stream).Encode(set); err != nil {
			return nodewire.Settings{}, err
		}
	}
	var reply nodewire.ConfigReply
	if err := json.NewDecoder(stream).Decode(&reply); err != nil {
		return nodewire.Settings{}, fmt.Errorf("node %q: config: %w", name, err)
	}
	if reply.Error != "" {
		if reply.ErrorCode == "settings_committed" {
			return reply.Settings, &settingsCommittedError{err: errors.New(reply.Error)}
		}
		if reply.ErrorCode == nodewire.SettingsRevisionConflictCode {
			return reply.Settings, fmt.Errorf("%w: %s", nodewire.ErrSettingsRevisionConflict, reply.Error)
		}
		return reply.Settings, errors.New(reply.Error)
	}
	return reply.Settings, nil
}

// Release tells a node an attempt is over: whatever it bound for the
// attempt is dropped and stopped. A node without bindings has nothing to
// release; a node that is gone has already lost them.
func (r *Registry) Release(ctx context.Context, name, attempt string) error {
	r.mu.Lock()
	c := r.live[name]
	r.mu.Unlock()
	if c == nil || !c.alive() || !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureMCP) {
		return nil
	}
	c.bindingsMu.Lock()
	delete(c.lastBindings, attempt)
	c.bindingsMu.Unlock()
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamRelease, Command: attempt})
	if err != nil {
		return fmt.Errorf("node %q: release %s: %w", name, attempt, err)
	}
	defer stream.Close()
	return awaitExit(ctx, stream, name)
}

// Bindings are the launchers the node handed back for an attempt's
// admission, taken once: they go into session/new and nowhere else.
func (r *Registry) Bindings(ctx context.Context, name, attempt string) []ability.Binding {
	c, err := r.connect(ctx, name)
	if err != nil {
		return nil
	}
	c.bindingsMu.Lock()
	defer c.bindingsMu.Unlock()
	out := c.lastBindings[attempt]
	delete(c.lastBindings, attempt)
	return out
}

// PushSkills sends a skill bundle to a node and has it materialized. A
// node that already has this bundle is not sent it again; a node that does
// not take bundles is left alone, and its snapshot says so.
func (r *Registry) PushSkills(ctx context.Context, name string, b skills.Bundle) error {
	c, err := r.connect(ctx, name)
	if err != nil {
		return err
	}
	adv := c.getAdvert()
	if !nodewire.HasFeature(adv.Features, nodewire.FeatureSkills) {
		return fmt.Errorf("node %q does not take skill bundles", name)
	}
	if adv.Skills == b.Hash {
		return nil
	}
	if err := r.PutBlob(ctx, name, "skills-"+b.Hash+".tar", bytes.NewReader(b.Data), int64(len(b.Data))); err != nil {
		return err
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamSkills, Command: "apply " + b.Hash})
	if err != nil {
		return fmt.Errorf("node %q: apply skills: %w", name, err)
	}
	defer stream.Close()
	if err := awaitExit(ctx, stream, name); err != nil {
		return fmt.Errorf("node %q: apply skills: %w", name, err)
	}
	adv.Skills = b.Hash
	c.setAdvert(adv)
	log.Printf("node: %s materialized skills %s (%d skills)", name, b.Hash[:12], len(b.Skills))
	// The snapshot the hub holds predates the skills; ask for a new one
	// now rather than at the next tick, so placement sees them at once.
	if _, err := r.Refresh(ctx, name); err != nil {
		log.Printf("node: %s: refresh after skills: %v", name, err)
	}
	return nil
}

// MCPEndpoint is the URL an agent on this node should call to reach the
// hub's messaging server. It is a loopback address on the node's own
// machine; the node forwards it here over the connection it already holds.
func (r *Registry) MCPEndpoint(ctx context.Context, name string) (string, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return "", err
	}
	if c.getAdvert().MCPPort == 0 {
		return "", fmt.Errorf("node %q offers no reverse messaging channel", name)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", c.getAdvert().MCPPort), nil
}
