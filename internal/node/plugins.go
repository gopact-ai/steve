package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

type PluginAuthorizer interface {
	AuthorizePlugins(context.Context, string, nodewire.PluginRequest) error
}

type pluginAuthorizationKey struct{}

type CoordinatorPluginAuthorizer struct{}

func (CoordinatorPluginAuthorizer) AuthorizePlugins(ctx context.Context, principal string, request nodewire.PluginRequest) error {
	if principal == "" || principal != request.Authority.ClusterID {
		return errors.New("plugin request differs from authenticated owner")
	}
	stream, ok := ctx.Value(pluginAuthorizationKey{}).(*nodewire.Stream)
	if !ok {
		return errors.New("plugin authorization requires a live connection")
	}
	if err := writePluginMessage(stream, nodewire.PluginReply{Authorize: true}); err != nil {
		return err
	}
	var answer nodewire.SessionAuthorization
	if err := readPluginMessage(stream, &answer); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !answer.Allowed {
		return errors.New("plugin operation is not authorized by the current coordinator")
	}
	return nil
}

func (s *Server) pluginStore() *plugins.Store {
	return &plugins.Store{Dir: filepath.Join(s.conf().StateDir, "plugins")}
}

func (s *Server) pluginOperation(ctx context.Context, principal string, req nodewire.PluginRequest) (nodewire.PluginReply, error) {
	cfg := s.conf()
	if cfg.StateDir == "" || req.Node != cfg.Name || req.Authority.ClusterID == "" {
		return nodewire.PluginReply{}, plugins.ErrInvalid
	}
	authorize := cfg.PluginAuthorizer
	if authorize != nil {
		if err := authorize.AuthorizePlugins(ctx, principal, req); err != nil {
			return nodewire.PluginReply{}, err
		}
	} else {
		if cfg.SessionAuthorizer != nil || principal != req.Authority.ClusterID || (req.Authority.CoordinatorNodeID != "" && req.Authority.CoordinatorNodeID != principal) || req.Authority.CoordinatorEpoch != 0 || req.Authority.WriterGeneration != 0 {
			return nodewire.PluginReply{}, errors.New("plugin operation has no matching node authority")
		}
	}
	store := s.pluginStore()
	switch req.Action {
	case nodewire.PluginRuntimeClose:
		return nodewire.PluginReply{Runtime: req.Runtime}, s.closePluginRuntime(ctx, req)
	case nodewire.PluginRuntimeList:
		infos, err := store.RuntimeInfos()
		return nodewire.PluginReply{Runtimes: infos}, err
	case nodewire.PluginRuntimeRetire, nodewire.PluginRuntimeRemove:
		if req.Runtime == nil || req.Runtime.Selection.Node != cfg.Name {
			return nodewire.PluginReply{}, plugins.ErrInvalid
		}
		if req.Action == nodewire.PluginRuntimeRetire {
			return nodewire.PluginReply{Runtime: req.Runtime}, store.RetireRuntime(ctx, *req.Runtime)
		}
		if err := store.CheckRuntimeRemovable(*req.Runtime); err != nil {
			return nodewire.PluginReply{}, err
		}
		if err := s.pluginRuntimePool().Drop(req.Runtime.ID); err != nil {
			return nodewire.PluginReply{}, err
		}
		return nodewire.PluginReply{Runtime: req.Runtime}, store.RemoveRuntime(ctx, *req.Runtime)
	case nodewire.PluginRuntimePrepare, nodewire.PluginRuntimeInspect:
		return s.pluginRuntimeOperation(ctx, req)
	case nodewire.PluginSecrets:
		secrets, err := store.Secrets()
		return nodewire.PluginReply{Secrets: secrets}, err
	case nodewire.PluginInspect:
		hash, err := req.Deployment.Hash()
		if err != nil {
			return nodewire.PluginReply{}, err
		}
		if req.Deployment.Node != cfg.Name {
			return nodewire.PluginReply{}, plugins.ErrInvalid
		}
		receipt, err := store.Deployment(hash)
		if err != nil {
			return nodewire.PluginReply{}, err
		}
		if _, err := store.ResolveServers(req.Deployment, plugins.Environment{}); err != nil {
			return nodewire.PluginReply{}, err
		}
		return nodewire.PluginReply{Receipt: &receipt}, nil
	case nodewire.PluginPrepare:
		return s.preparePlugin(ctx, principal, req, store)
	default:
		return nodewire.PluginReply{}, plugins.ErrIncompatible
	}
}

func (s *Server) preparePlugin(ctx context.Context, principal string, req nodewire.PluginRequest, store *plugins.Store) (nodewire.PluginReply, error) {
	hash, err := req.Deployment.Hash()
	if err != nil || req.Deployment.Node != req.Node {
		return nodewire.PluginReply{}, plugins.ErrInvalid
	}
	bundle, err := plugins.DecodeBundle(req.Bundle)
	if err != nil {
		return nodewire.PluginReply{}, err
	}
	if bundle.Digest != req.Deployment.Digest || bundle.Manifest.ID != req.Deployment.PackageID {
		return nodewire.PluginReply{}, plugins.ErrIntegrity
	}
	if err := os.MkdirAll(s.conf().StateDir, 0700); err != nil {
		return nodewire.PluginReply{}, err
	}
	_, err = store.Install(ctx, plugins.InstallRequest{CommandID: hash, ExpectedDigest: bundle.Digest, Bundle: bundle, Source: plugins.Source{Kind: "bundle", Location: bundle.Digest}})
	if err != nil {
		return nodewire.PluginReply{}, err
	}
	if authorize := s.conf().PluginAuthorizer; authorize != nil {
		if err := authorize.AuthorizePlugins(ctx, principal, req); err != nil {
			return nodewire.PluginReply{}, err
		}
	}
	receipt, err := store.PrepareDeployment(ctx, req.Deployment, plugins.Environment{})
	return nodewire.PluginReply{Receipt: &receipt}, err
}

func (s *Server) pluginStream(parent context.Context, principal string, stream *nodewire.Stream) {
	defer stream.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	ctx = context.WithValue(ctx, pluginAuthorizationKey{}, stream)
	go func() {
		select {
		case <-stream.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	var req nodewire.PluginRequest
	err := readPluginMessage(stream, &req)
	reply := nodewire.PluginReply{}
	if err == nil && stream.Request().Command != "" && stream.Request().Command != string(req.Action) {
		err = plugins.ErrInvalid
	}
	if err == nil {
		reply, err = s.pluginOperation(ctx, principal, req)
	}
	if err != nil {
		reply = nodewire.PluginReply{ErrorCode: pluginErrorCode(err), Error: err.Error()}
	}
	if err := writePluginMessage(stream, reply); err != nil {
		closeStream(stream, "plugin reply unavailable")
	}
}

func pluginErrorCode(err error) string {
	switch {
	case errors.Is(err, plugins.ErrInvalid):
		return "invalid"
	case errors.Is(err, plugins.ErrConflict):
		return "conflict"
	case errors.Is(err, plugins.ErrIntegrity):
		return "integrity"
	case errors.Is(err, plugins.ErrIncompatible):
		return "incompatible"
	default:
		return "unavailable"
	}
}

func readPluginMessage(r io.Reader, value any) error {
	size, err := nodewire.ReadSize(r)
	if err != nil {
		return err
	}
	if size < 1 || size > nodewire.PluginMaxBytes {
		return fmt.Errorf("%w: plugin message too large", plugins.ErrInvalid)
	}
	raw := make([]byte, int(size))
	if _, err := io.ReadFull(r, raw); err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
func writePluginMessage(w io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > nodewire.PluginMaxBytes {
		return plugins.ErrInvalid
	}
	if err := nodewire.WriteSize(w, int64(len(raw))); err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}
