package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func (r *Registry) AgentTools(ctx context.Context, name string) (agenttools.Discovery, error) {
	reply, err := r.agentToolsStream(ctx, name, "discover-agents", nil)
	if err != nil {
		return agenttools.Discovery{}, err
	}
	if reply.Discovery == nil || reply.Discovery.Revision == "" {
		return agenttools.Discovery{}, fmt.Errorf("node %q does not support agent discovery; update steve-node", name)
	}
	return *reply.Discovery, nil
}

func (r *Registry) EnrollAgent(ctx context.Context, name string, request agenttools.InstallRequest) (agenttools.Enrollment, error) {
	reply, err := r.agentToolsStream(ctx, name, "enroll-agent", &request)
	if err != nil && !settingsCommitted(err) {
		return agenttools.Enrollment{}, err
	}
	if reply.Enrollment == nil || reply.Enrollment.Revision == "" {
		return agenttools.Enrollment{}, fmt.Errorf("node %q returned no agent enrollment receipt", name)
	}
	canonical, ok := agenttools.Lookup(request.CandidateID)
	if !ok || reply.Enrollment.CandidateID != request.CandidateID || reply.Enrollment.Harness != canonical.Harness {
		return agenttools.Enrollment{}, fmt.Errorf("node %q returned another tool's enrollment receipt", name)
	}
	if _, refreshErr := r.Refresh(ctx, name); refreshErr != nil {
		log.Printf("node: %s: refresh after agent enrollment: %v", name, refreshErr)
	}
	return *reply.Enrollment, err
}

func (r *Registry) agentToolsStream(ctx context.Context, name, verb string, request *agenttools.InstallRequest) (agentToolsReply, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return agentToolsReply{}, err
	}
	if !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureConfigRevision) {
		return agentToolsReply{}, nodewire.ErrSettingsRevisionUnsupported
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamConfig, Command: verb})
	if err != nil {
		return agentToolsReply{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	if request != nil {
		if err := json.NewEncoder(stream).Encode(request); err != nil {
			return agentToolsReply{}, err
		}
	}
	var reply agentToolsReply
	if err := json.NewDecoder(io.LimitReader(stream, 128<<10)).Decode(&reply); err != nil {
		if ctx.Err() != nil {
			return agentToolsReply{}, ctx.Err()
		}
		return agentToolsReply{}, fmt.Errorf("node %q: agent tools: %w", name, err)
	}
	return reply, agentToolsError(reply)
}

// SettingsCommitted reports that node configuration was atomically replaced
// but its final directory sync failed. Its live configuration matches the file.
func SettingsCommitted(err error) bool { return settingsCommitted(err) }
