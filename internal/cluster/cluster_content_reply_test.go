package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/coordination"
)

// contentReply is what one peer answered another's content request.
type contentReply struct {
	status  int
	body    []byte
	elapsed time.Duration
	err     error
}

// contentHTTP sends a content request from one peer to another over the
// cluster's mutual TLS, with the coordinator headers the caller chooses.
func contentHTTP(t *testing.T, from, to *Peer, method string, object *contentreplica.Object, epoch, writer string, timeout time.Duration) contentReply {
	t.Helper()
	state, err := from.Runtime.Load().ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	transport, origin, err := from.remoteTransport(state.Members[to.Config.NodeID])
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, origin.String()+clusterContentPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if object != nil {
		raw, _ := json.Marshal(object)
		request.Header.Set(contentObjectHeader, base64.RawURLEncoding.EncodeToString(raw))
	}
	if epoch != "" {
		request.Header.Set("X-Steve-Coordinator-Epoch", epoch)
	}
	if writer != "" {
		request.Header.Set("X-Steve-Writer-Generation", writer)
	}
	started := time.Now()
	response, err := (&http.Client{Transport: transport, Timeout: timeout}).Do(request)
	if err != nil {
		return contentReply{elapsed: time.Since(started), err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return contentReply{status: response.StatusCode, body: body, elapsed: time.Since(started), err: err}
}

func coordinatorHeaders(active Activation) (string, string) {
	return strconv.FormatUint(active.Assignment.Epoch, 10), strconv.FormatUint(active.WriterGeneration, 10)
}

func workspaceObject(peers []*Peer, data []byte) contentreplica.Object {
	ref := checkpoint.Reference(data)
	return contentreplica.Object{Scope: contentreplica.Scope{ProjectID: "workspace", Level: "internal", HomeNodeID: peers[0].Config.NodeID}, Kind: contentreplica.Material, Key: ref.SHA256, Blob: ref}
}

// behind makes a peer's content checks see a committed state its replica
// never reaches, and counts how often they read it.
func behind(peer *Peer) *atomic.Int64 {
	runtime := peer.Runtime.Load()
	reads := &atomic.Int64{}
	read := contentStateReader(func(ctx context.Context) (coordination.State, error) {
		reads.Add(1)
		state, err := runtime.ReadState(ctx)
		state.AppVersion += 1000
		return state, err
	})
	peer.readContentState.Store(&read)
	return reads
}

// A peer whose replica is behind the committed state waits a bounded time
// for it to catch up, reading the committed state once, and then answers:
// it does not ask the leader for the state again and again until the
// request ends.
func TestContentPeerAnswersWithinABoundWhenItsReplicaLags(t *testing.T) {
	peers, active := contentPeers(t)
	receiver := peers[1]
	bound := receiver.Runtime.Load().config.Coordination.ApplyTimeout
	reads := behind(receiver)
	object := workspaceObject(peers, []byte("content on a lagging replica"))
	epoch, writer := coordinatorHeaders(active)
	reply := contentHTTP(t, peers[0], receiver, http.MethodGet, &object, epoch, writer, 3*bound)
	switch {
	case reply.err != nil || reply.elapsed > bound+2*time.Second:
		t.Fatalf("the request did not end within %s of waiting for the replica: took %s, %d reads of the committed state, err=%v", bound, reply.elapsed.Round(time.Millisecond), reads.Load(), reply.err)
	case reply.status == http.StatusOK:
		t.Fatalf("a lagging replica served content: %q", reply.body)
	case reads.Load() != 1:
		t.Fatalf("the request read the committed state %d times, want once", reads.Load())
	}
}
