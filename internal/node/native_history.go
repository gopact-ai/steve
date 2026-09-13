package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
)

type nativeHistoryReply struct {
	Entries   []nativehistory.Entry    `json:"entries,omitempty"`
	Reference *nativehistory.Reference `json:"reference,omitempty"`
	Error     string                   `json:"error,omitempty"`
}

func (s *Server) nativeHistorySource(source nativehistory.Source) (nativehistory.Source, error) {
	if _, ok := s.conf().Harnesses[source.Harness]; !ok {
		return source, errors.New("selected native tool is not registered on this machine")
	}
	if source.Home != "" {
		return source, nil
	}
	dirs := map[string]string{"codex": ".codex", "claude-code": ".claude", "grok": ".grok", "dsh": ".dsh"}
	dir, ok := dirs[source.Harness]
	if !ok {
		return source, nativehistory.ErrUnsupported
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return source, err
	}
	source.Home = filepath.Join(home, dir)
	return source, nil
}

func (s *Server) configureNativeHistory(stream *nodewire.Stream) {
	base := s.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	go func() {
		select {
		case <-stream.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	var request nativehistory.ImportRequest
	decoder := json.NewDecoder(io.LimitReader(stream, 16<<10))
	decoder.DisallowUnknownFields()
	out := nativeHistoryReply{}
	err := decoder.Decode(&request)
	if err == nil {
		request.Source, err = s.nativeHistorySource(request.Source)
	}
	if err == nil {
		switch stream.Request().Command {
		case "list-native-history":
			out.Entries, err = nativehistory.List(ctx, request.Source)
			if err == nil && len(out.Entries) > 0 {
				err = s.checkNativeResume(ctx, request.Source.Harness)
			}
		case "import-native-history":
			var ref nativehistory.Reference
			ref, err = s.snapshotNativeHistory(ctx, request)
			if err == nil {
				out.Reference = &ref
			}
		}
	}
	if err != nil {
		out.Error = err.Error()
	}
	_ = json.NewEncoder(stream).Encode(out)
}

func (s *Server) snapshotNativeHistory(ctx context.Context, request nativehistory.ImportRequest) (nativehistory.Reference, error) {
	if err := s.checkNativeResume(ctx, request.Source.Harness); err != nil {
		return nativehistory.Reference{}, err
	}
	return nativehistory.Snapshot(ctx, filepath.Join(s.conf().StateDir, "native-imports"), request)
}

func (s *Server) checkNativeResume(ctx context.Context, harnessID string) error {
	if s.sessions == nil {
		return errors.New("node-owned sessions are unavailable")
	}
	spec, ok := s.conf().Harnesses[harnessID]
	if !ok {
		return errors.New("selected tool is no longer registered")
	}
	broker, err := permission.New(permission.PolicyRead)
	if err != nil {
		return err
	}
	host := acphost.New(s.sessions.hostConfig(harnessID, spec, broker))
	defer host.Close()
	supported, err := host.SupportsResume(ctx)
	if err != nil {
		return err
	}
	if !supported {
		return errors.New("selected adapter does not support session/load or session/resume")
	}
	return nil
}

func (r *Registry) NativeHistory(ctx context.Context, name string, source nativehistory.Source) ([]nativehistory.Entry, error) {
	reply, err := r.nativeHistoryStream(ctx, name, "list-native-history", nativehistory.ImportRequest{Source: source})
	return reply.Entries, err
}

func (r *Registry) ImportNativeHistory(ctx context.Context, name string, request nativehistory.ImportRequest) (nativehistory.Reference, error) {
	reply, err := r.nativeHistoryStream(ctx, name, "import-native-history", request)
	if err != nil {
		return nativehistory.Reference{}, err
	}
	if reply.Reference == nil {
		return nativehistory.Reference{}, errors.New("node returned no native import receipt")
	}
	ref := *reply.Reference
	if ref.Harness != request.Source.Harness || ref.NativeID != request.NativeID || ref.Revision != request.Revision || (request.Source.Home != "" && ref.SourceHome != request.Source.Home) {
		return nativehistory.Reference{}, errors.New("node returned another native import receipt")
	}
	return ref, nil
}

func (r *Registry) nativeHistoryStream(ctx context.Context, name, verb string, request nativehistory.ImportRequest) (nativeHistoryReply, error) {
	conn, err := r.connect(ctx, name)
	if err != nil {
		return nativeHistoryReply{}, err
	}
	if !nodewire.HasFeature(conn.getAdvert().Features, nodewire.FeatureNativeHistory) {
		return nativeHistoryReply{}, errors.New("node does not support native history migration; update steve-node")
	}
	stream, err := conn.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamConfig, Command: verb})
	if err != nil {
		return nativeHistoryReply{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	if err := json.NewEncoder(stream).Encode(request); err != nil {
		return nativeHistoryReply{}, err
	}
	var reply nativeHistoryReply
	if err := json.NewDecoder(io.LimitReader(stream, 8<<20)).Decode(&reply); err != nil {
		return reply, err
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}
