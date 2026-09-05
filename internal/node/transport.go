package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// Transport returns an acphost transport that runs harnessID on this node.
// The host above it cannot tell the difference from a local subprocess —
// that is the whole point of tunnelling ACP rather than inventing an RPC.
func (r *Registry) Transport(nodeName, harnessID string) acphost.Transport {
	return remoteTransport{registry: r, node: nodeName, harness: harnessID}
}

type remoteTransport struct {
	registry *Registry
	node     string
	harness  string
}

func (t remoteTransport) Name() string { return t.node + "/" + t.harness }

func (t remoteTransport) Start(ctx context.Context) (acphost.Process, error) {
	c, err := t.registry.connect(ctx, t.node)
	if err != nil {
		return nil, err
	}
	if !c.offers(t.harness) {
		return nil, fmt.Errorf("node %q does not offer harness %q: %s",
			t.node, t.harness, c.harnessTrouble(t.harness))
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	req := nodewire.OpenRequest{Kind: nodewire.StreamACP, Harness: t.harness, Stream: hex.EncodeToString(nonce[:])}
	resumable := nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureJournal)
	if !resumable {
		req.Stream = ""
	}
	stream, err := c.mux.Open(req)
	if err != nil {
		return nil, fmt.Errorf("open session stream on %q: %w", t.node, err)
	}
	if !resumable {
		return legacyRemoteProcess{stream: stream}, nil
	}
	return newRemoteProcess(t, c, stream), nil
}

// offers reports whether the node advertised a usable harness by that id.
// The advert is the authority: a config line claiming the node runs codex
// does not make the binary exist there.
func (c *conn) offers(harnessID string) bool {
	for _, h := range c.getAdvert().Harnesses {
		if h.ID == harnessID {
			return h.Missing == ""
		}
	}
	return false
}

func (c *conn) harnessTrouble(harnessID string) string {
	for _, h := range c.getAdvert().Harnesses {
		if h.ID == harnessID {
			return h.Missing
		}
	}
	return "not advertised"
}

// remoteProcess is the agent running on the node. Its stdio is one stream;
// closing that stream is what tells the node to kill the child.
type legacyRemoteProcess struct{ stream *nodewire.Stream }

func (p legacyRemoteProcess) Stdout() io.ReadCloser { return p.stream }
func (p legacyRemoteProcess) Stdin() io.WriteCloser { return p.stream }

func (p legacyRemoteProcess) Wait() error {
	<-p.stream.Done()
	return nil
}

func (p legacyRemoteProcess) Kill() { _ = p.stream.Close() }
