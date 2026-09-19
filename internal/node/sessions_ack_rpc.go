package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/nodewire"
)

type receiptAuthorizationStreamKey struct{}

func (CoordinatorSessionAuthorizer) AuthorizeNodeReceipt(ctx context.Context, principal string, authority nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
	if principal == "" || principal != authority.ClusterID {
		return errors.New("authenticated owner differs from the receipt cluster")
	}
	stream, ok := ctx.Value(receiptAuthorizationStreamKey{}).(*nodewire.Stream)
	if !ok || ctx.Err() != nil {
		return errors.New("receipt authority requires a live authenticated acknowledgement")
	}
	if err := writeSessionMessage(stream, nodewire.SessionReceiptReply{Authorize: &receipt}); err != nil {
		return err
	}
	var answer nodewire.SessionAuthorization
	if err := readSessionMessage(stream, &answer); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !answer.Allowed {
		return fmt.Errorf("coordinator rejected receipt acknowledgement: %s", answer.Error)
	}
	return nil
}

func (r *Registry) SetNodeReceiptAuthorizer(authorize func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionReceipt) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receiptAuthority = authorize
}

func (s *Server) receiptStream(parent context.Context, principal string, stream *nodewire.Stream) {
	defer stream.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ctx = context.WithValue(ctx, receiptAuthorizationStreamKey{}, stream)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stream.Done():
			cancel()
		case <-done:
		}
	}()
	var request nodewire.SessionReceiptRequest
	var err error
	if s.sessions == nil {
		err = errors.New("node sessions are not enabled")
	} else if err = readSessionMessage(stream, &request); err == nil {
		err = s.sessions.AcknowledgeReceipt(ctx, principal, request)
	}
	reply := nodewire.SessionReceiptReply{Ack: &request.Receipt}
	if err != nil {
		reply.Ack, reply.Error = nil, err.Error()
	}
	_ = writeSessionMessage(stream, reply)
}

// AcknowledgeNodeReceipt never accepts an action/binding-only challenge. The
// authenticated worker must request and acknowledge the exact original key.
func (r *Registry) AcknowledgeNodeReceipt(ctx context.Context, node string, request nodewire.SessionReceiptRequest) error {
	if err := request.Receipt.Validate(); err != nil {
		return err
	}
	if request.Receipt.Binding.NodeID != node {
		return errors.New("receipt belongs to another node connection")
	}
	conn, err := r.connect(ctx, node)
	if err != nil {
		return err
	}
	if !nodewire.HasFeature(conn.getAdvert().Features, nodewire.FeatureNodeReceipts) {
		return errors.New("node does not support exact receipt acknowledgement")
	}
	stream, err := conn.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamNodeReceipts})
	if err != nil {
		return err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	if err := writeSessionMessage(stream, request); err != nil {
		return err
	}
	for challenges := 0; ; challenges++ {
		var reply nodewire.SessionReceiptReply
		if err := readSessionMessage(stream, &reply); err != nil {
			return err
		}
		if reply.Authorize == nil {
			if reply.Error != "" {
				return errors.New(reply.Error)
			}
			if reply.Ack == nil || *reply.Ack != request.Receipt {
				return errors.New("node acknowledgement differs from original receipt")
			}
			return nil
		}
		if challenges != 0 || *reply.Authorize != request.Receipt || reply.Ack != nil || reply.Error != "" {
			return errors.New("node requested unrelated receipt authority")
		}
		r.mu.Lock()
		authorize := r.receiptAuthority
		r.mu.Unlock()
		if authorize == nil {
			err = errors.New("coordinator has no receipt authority verifier")
		} else {
			err = authorize(ctx, node, request.Authority, request.Receipt)
		}
		answer := nodewire.SessionAuthorization{Allowed: err == nil}
		if err != nil {
			answer.Error = err.Error()
		}
		if err := writeSessionMessage(stream, answer); err != nil {
			return err
		}
	}
}
