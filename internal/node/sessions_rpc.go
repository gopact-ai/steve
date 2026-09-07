package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func readSessionMessage(reader io.Reader, value any) error {
	size, err := nodewire.ReadSize(reader)
	if err != nil {
		return err
	}
	if size < 1 || size > nodewire.NodeSessionMaxBytes {
		return sessionError("invalid", "node session message exceeds limit")
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(reader, data); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func writeSessionMessage(writer io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > nodewire.NodeSessionMaxBytes {
		return sessionError("invalid", "node session message exceeds limit")
	}
	if err := nodewire.WriteSize(writer, int64(len(raw))); err != nil {
		return err
	}
	_, err = writer.Write(raw)
	return err
}

func (s *Server) sessionStream(parent context.Context, principal string, stream *nodewire.Stream) {
	defer stream.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stream.Done():
			cancel()
		case <-done:
		}
	}()
	var request nodewire.SessionRequest
	reply := nodewire.SessionReply{}
	var err error
	if s.sessions == nil {
		err = sessionError("forbidden", "node sessions are not enabled")
	} else if err = readSessionMessage(stream, &request); err == nil {
		var state nodewire.SessionState
		state, err = s.sessions.Do(ctx, principal, request)
		if err == nil {
			reply.State = &state
		}
	}
	if err != nil {
		reply.ErrorCode = "unavailable"
		reply.Error = err.Error()
		var classified *SessionError
		if errors.As(err, &classified) {
			reply.ErrorCode = classified.Code
		}
	}
	_ = writeSessionMessage(stream, reply)
}

// NodeSession uses the existing authenticated connection, with per-request
// cancellation. Disconnecting this RPC never releases a node-owned execution.
func (r *Registry) NodeSession(ctx context.Context, node string, request nodewire.SessionRequest) (nodewire.SessionState, error) {
	conn, err := r.connect(ctx, node)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	if !nodewire.HasFeature(conn.getAdvert().Features, nodewire.FeatureNodeSessions) {
		return nodewire.SessionState{}, sessionError("unavailable", "node does not support node-owned sessions")
	}
	stream, err := conn.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamNodeSessions})
	if err != nil {
		return nodewire.SessionState{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	if err := writeSessionMessage(stream, request); err != nil {
		return nodewire.SessionState{}, err
	}
	var reply nodewire.SessionReply
	if err := readSessionMessage(stream, &reply); err != nil {
		return nodewire.SessionState{}, err
	}
	if reply.ErrorCode != "" || reply.Error != "" {
		return nodewire.SessionState{}, sessionError(reply.ErrorCode, reply.Error)
	}
	if reply.State == nil {
		return nodewire.SessionState{}, sessionError("uncertain", "node returned no session receipt")
	}
	return *reply.State, nil
}
