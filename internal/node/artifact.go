package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/nodewire"
)

const operationTimeout = 10 * time.Minute

// Artifact executes a platform operation where its files live. Old nodes
// report an unsupported feature; there is deliberately no shell fallback.
func (r *Registry) Artifact(ctx context.Context, name string, req nodewire.ArtifactRequest) (nodewire.ArtifactResult, error) {
	if name == "" {
		return artifact.RunOperation(ctx, req)
	}
	var reply nodewire.ArtifactReply
	if err := r.operation(ctx, name, nodewire.StreamArtifact, nodewire.FeatureArtifact, req, &reply); err != nil {
		return reply.Result, err
	}
	return reply.Result, artifact.DecodeFailure(reply.Error)
}

func (r *Registry) operation(ctx context.Context, name, kind, feature string, req, reply any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := r.connect(ctx, name)
	if err != nil {
		return err
	}
	if !nodewire.HasFeature(c.getAdvert().Features, feature) {
		return &nodewire.OperationFailure{Code: "unsupported", Message: fmt.Sprintf("node %q needs an upgrade for %s", name, feature)}
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: kind})
	if err != nil {
		return err
	}
	defer stream.Close()
	done := make(chan error, 1)
	go func() {
		if err := json.NewEncoder(stream).Encode(req); err != nil {
			done <- err
			return
		}
		done <- json.NewDecoder(stream).Decode(reply)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Closing only wakes the decoder; ctx carries the answer.
		_ = stream.Close()
		<-done // no decoder writes to reply after we return
		return ctx.Err()
	}
}

// The server cancels a running operation if the hub closes its stream or
// disconnects. An incomplete reply is a transport failure, never success.
func operationContext(ctx context.Context, stream *nodewire.Stream) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	go func() {
		select {
		case <-stream.Done():
			cancel()
		case <-ctx.Done():
			// Also wake a decoder waiting on an incomplete request; the
			// close itself has nothing to add to the cancellation.
			_ = stream.Close()
		}
	}()
	return ctx, cancel
}

func (s *Server) runArtifact(ctx context.Context, stream *nodewire.Stream) {
	defer stream.Close()
	ctx, cancel := operationContext(ctx, stream)
	defer cancel()
	var req nodewire.ArtifactRequest
	var reply nodewire.ArtifactReply
	if err := json.NewDecoder(io.LimitReader(stream, nodewire.MaxPayload)).Decode(&req); err != nil {
		reply.Error = &nodewire.OperationFailure{Code: "invalid_request", Message: err.Error()}
	} else {
		result, err := artifact.RunOperation(ctx, req)
		reply.Result, reply.Error = result, artifact.EncodeFailure(err)
	}
	if err := json.NewEncoder(stream).Encode(reply); err != nil {
		log.Printf("steve-node: artifact reply: %v", err)
	}
}
