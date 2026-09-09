package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (r *Registry) SetPluginAuthorizer(authorize func(context.Context, string, nodewire.PluginRequest) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pluginAuthority = authorize
}

func (r *Registry) Plugins(ctx context.Context, name string, request nodewire.PluginRequest) (nodewire.PluginReply, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return nodewire.PluginReply{}, err
	}
	if !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeaturePlugins) {
		return nodewire.PluginReply{}, fmt.Errorf("%w: node %s does not support plugin packages", plugins.ErrIncompatible, name)
	}
	request.Node = name
	if request.Authority.ClusterID == "" {
		request.Authority.ClusterID = r.hub
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamPlugins, Command: string(request.Action)})
	if err != nil {
		return nodewire.PluginReply{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	if err := writePluginMessage(stream, request); err != nil {
		return nodewire.PluginReply{}, err
	}
	for challenge := 0; challenge < 3; challenge++ {
		var reply nodewire.PluginReply
		if err := readPluginMessage(stream, &reply); err != nil {
			if ctx.Err() != nil {
				return reply, ctx.Err()
			}
			return reply, err
		}
		if reply.Authorize {
			r.mu.Lock()
			authorize := r.pluginAuthority
			r.mu.Unlock()
			answer := nodewire.SessionAuthorization{}
			if authorize == nil {
				err = errors.New("plugin authority verifier unavailable")
			} else {
				err = authorize(ctx, name, request)
			}
			answer.Allowed = err == nil
			if err := writePluginMessage(stream, answer); err != nil {
				return reply, err
			}
			continue
		}
		if reply.Error != "" {
			return reply, pluginReplyError(reply)
		}
		if request.Action != nodewire.PluginSecrets {
			expected, err := request.Deployment.Hash()
			if err != nil || reply.Receipt == nil || reply.Receipt.Schema != plugins.Schema || reply.Receipt.PreparedAt.IsZero() || reply.Receipt.Hash != expected {
				return reply, fmt.Errorf("%w: node replied for another plugin deployment", plugins.ErrIntegrity)
			}
			actual, err := reply.Receipt.Deployment.Hash()
			if err != nil || actual != expected {
				return reply, plugins.ErrIntegrity
			}
		}
		return reply, nil
	}
	return nodewire.PluginReply{}, errors.New("too many plugin authorization challenges")
}

func pluginReplyError(reply nodewire.PluginReply) error {
	kind := plugins.ErrUnavailable
	switch reply.ErrorCode {
	case "invalid":
		kind = plugins.ErrInvalid
	case "conflict":
		kind = plugins.ErrConflict
	case "integrity":
		kind = plugins.ErrIntegrity
	case "incompatible":
		kind = plugins.ErrIncompatible
	}
	return fmt.Errorf("%w: %s", kind, reply.Error)
}
