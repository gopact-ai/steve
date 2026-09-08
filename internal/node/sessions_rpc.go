package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gopact-ai/steve/internal/nodewire"
)

type sessionAuthorizationStreamKey struct{}

// CoordinatorSessionAuthorizer lets an execution-only node consult its owning
// cluster through the authenticated session RPC. A full peer instead verifies
// authority against its own replicated consensus state.
type CoordinatorSessionAuthorizer struct{}

func (CoordinatorSessionAuthorizer) AuthorizeNodeSession(ctx context.Context, principal string, authority nodewire.SessionAuthority, _ nodewire.SessionBinding, action nodewire.SessionAction) error {
	if principal == "" || principal != authority.ClusterID {
		return errors.New("authenticated owner differs from the session cluster")
	}
	stream, ok := ctx.Value(sessionAuthorizationStreamKey{}).(*nodewire.Stream)
	if !ok || ctx.Err() != nil {
		return errors.New("session authority requires a live authenticated request")
	}
	if err := writeSessionMessage(stream, nodewire.SessionReply{AuthorizeAction: action}); err != nil {
		return err
	}
	var reply nodewire.SessionAuthorization
	if err := readSessionMessage(stream, &reply); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !reply.Allowed {
		if reply.Error != "" {
			return fmt.Errorf("coordinator rejected session authority: %s", reply.Error)
		}
		return errors.New("coordinator rejected session authority")
	}
	return nil
}

// SetSessionAuthorizer installs the current coordinator activation's committed
// execution verifier. The target name comes from this registry's connection,
// not from claims supplied by the worker.
func (r *Registry) SetSessionAuthorizer(authorize func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionAuthority = authorize
}

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
	ctx = context.WithValue(ctx, sessionAuthorizationStreamKey{}, stream)
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
		return nodewire.SessionState{}, &nodewire.SessionNotDispatched{Cause: err}
	}
	if !nodewire.HasFeature(conn.getAdvert().Features, nodewire.FeatureNodeSessions) {
		return nodewire.SessionState{}, &nodewire.SessionNotDispatched{Cause: sessionError("unavailable", "node does not support node-owned sessions")}
	}
	stream, err := conn.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamNodeSessions})
	if err != nil {
		return nodewire.SessionState{}, &nodewire.SessionNotDispatched{Cause: err}
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	if err := writeSessionMessage(stream, request); err != nil {
		return nodewire.SessionState{}, err
	}
	var reply nodewire.SessionReply
	for challenges := 0; ; challenges++ {
		reply = nodewire.SessionReply{}
		if err := readSessionMessage(stream, &reply); err != nil {
			return nodewire.SessionState{}, err
		}
		if reply.AuthorizeAction == "" {
			break
		}
		if challenges >= 2 || (reply.AuthorizeAction != request.Action && !(request.Action == nodewire.SessionActionOpen && reply.AuthorizeAction == nodewire.SessionActionStart)) {
			return nodewire.SessionState{}, errors.New("node requested unrelated session authorization")
		}
		r.mu.Lock()
		authorize := r.sessionAuthority
		r.mu.Unlock()
		answer := nodewire.SessionAuthorization{}
		if authorize == nil {
			err = errors.New("coordinator has no active session authority verifier")
		} else if request.Binding.NodeID != node {
			err = errors.New("session binding differs from authenticated node connection")
		} else {
			err = authorize(ctx, node, request.Authority, request.Binding, reply.AuthorizeAction)
		}
		answer.Allowed = err == nil
		if err != nil {
			answer.Error = fmt.Sprint(err)
		}
		if err := writeSessionMessage(stream, answer); err != nil {
			return nodewire.SessionState{}, err
		}
	}
	if reply.ErrorCode != "" || reply.Error != "" {
		return nodewire.SessionState{}, sessionError(reply.ErrorCode, reply.Error)
	}
	if reply.State == nil {
		return nodewire.SessionState{}, sessionError("uncertain", "node returned no session receipt")
	}
	return *reply.State, nil
}
