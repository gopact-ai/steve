package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// cluster's mutual TLS, with the headers the caller chooses.
func contentHTTP(t *testing.T, from, to *Peer, method string, headers map[string]string, timeout time.Duration) contentReply {
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
	for name, value := range headers {
		request.Header.Set(name, value)
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

// contentHeaders are the headers of a content request for object from the
// activation's coordinator.
func contentHeaders(active Activation, object *contentreplica.Object) map[string]string {
	epoch, writer := coordinatorHeaders(active)
	headers := map[string]string{"X-Steve-Coordinator-Epoch": epoch, "X-Steve-Writer-Generation": writer}
	if object != nil {
		raw, _ := json.Marshal(object)
		headers[contentObjectHeader] = base64.RawURLEncoding.EncodeToString(raw)
	}
	return headers
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
	reply := contentHTTP(t, peers[0], receiver, http.MethodGet, contentHeaders(active, &object), 3*bound)
	switch {
	case reply.err != nil || reply.elapsed > bound+2*time.Second:
		t.Fatalf("the request did not end within %s of waiting for the replica: took %s, %d reads of the committed state, err=%v", bound, reply.elapsed.Round(time.Millisecond), reads.Load(), reply.err)
	case reply.status == http.StatusOK:
		t.Fatalf("a lagging replica served content: %q", reply.body)
	case reads.Load() != 1:
		t.Fatalf("the request read the committed state %d times, want once", reads.Load())
	}
}

// Every refusal a peer sends is JSON with a code, and its status says
// whether asking again can help: 403 for a caller or a placement the
// committed state does not admit, 503 for a check that could not be made.
func TestContentPeerRefusesInJSONWithACodeAndAStatusThatSaysWhetherToRetry(t *testing.T) {
	peers, active := contentPeers(t)
	receiver := peers[1]
	object := workspaceObject(peers, []byte("content a peer refuses"))
	forged := object
	forged.Scope.Level = "public"
	epoch, _ := coordinatorHeaders(active)
	staleWriter := contentHeaders(active, &object)
	staleWriter["X-Steve-Writer-Generation"] = strconv.FormatUint(active.WriterGeneration+1, 10)
	undescribed := contentHeaders(active, nil)
	undescribed[contentObjectHeader] = "not a descriptor!"
	failing := contentStateReader(func(context.Context) (coordination.State, error) {
		return coordination.State{}, errors.New("the leader is out of reach")
	})
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		arrange func()
		status  int
		code    string
	}{
		{name: "method", method: http.MethodDelete, headers: contentHeaders(active, &object), status: http.StatusMethodNotAllowed, code: "method"},
		{name: "no coordinator credentials", method: http.MethodGet, headers: map[string]string{}, status: http.StatusForbidden, code: "authority"},
		{name: "a writer generation that is not current", method: http.MethodGet, headers: staleWriter, status: http.StatusForbidden, code: "stale"},
		{name: "maintenance from a writer generation that is not current", method: http.MethodPost, headers: map[string]string{"X-Steve-Coordinator-Epoch": epoch, "X-Steve-Writer-Generation": staleWriter["X-Steve-Writer-Generation"]}, status: http.StatusForbidden, code: "stale"},
		{name: "descriptor", method: http.MethodGet, headers: undescribed, status: http.StatusBadRequest, code: "invalid"},
		{name: "placement", method: http.MethodGet, headers: contentHeaders(active, &forged), status: http.StatusForbidden, code: "placement"},
		{name: "committed state out of reach", method: http.MethodGet, headers: contentHeaders(active, &object), arrange: func() { receiver.readContentState.Store(&failing) }, status: http.StatusServiceUnavailable, code: "unavailable"},
		{name: "replica behind", method: http.MethodGet, headers: contentHeaders(active, &object), arrange: func() { behind(receiver) }, status: http.StatusServiceUnavailable, code: "lagging"},
	}
	for _, c := range cases {
		receiver.readContentState.Store(nil)
		if c.arrange != nil {
			c.arrange()
		}
		reply := contentHTTP(t, peers[0], receiver, c.method, c.headers, time.Minute)
		var body struct {
			Code string `json:"code"`
		}
		if reply.err != nil || reply.status != c.status || json.Unmarshal(reply.body, &body) != nil || body.Code != c.code {
			t.Errorf("%s: HTTP %d %q err=%v, want HTTP %d with code %q", c.name, reply.status, reply.body, reply.err, c.status, c.code)
		}
	}
	receiver.readContentState.Store(nil)
}

// The hub reads a peer's refusal by its code: a replica that is behind or a
// check that could not be made is a temporary unavailability, not a
// placement refusal; a writer generation the peer does not know as current
// is this generation's end.
func TestContentReplyKeepsTemporaryRefusalsApartFromPlacement(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	reply := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
	}
	type want struct{ transient, placement, inactive bool }
	cases := []struct {
		name     string
		response *http.Response
		want     want
	}{
		{"lagging", reply(http.StatusServiceUnavailable, `{"code":"lagging"}`), want{transient: true}},
		{"unavailable", reply(http.StatusServiceUnavailable, `{"code":"unavailable"}`), want{transient: true}},
		{"stale", reply(http.StatusForbidden, `{"code":"stale"}`), want{inactive: true}},
		{"authority", reply(http.StatusForbidden, `{"code":"authority"}`), want{placement: true}},
		{"placement", reply(http.StatusForbidden, `{"code":"placement"}`), want{placement: true}},
	}
	for _, c := range cases {
		err := contentReplyError("node-b", c.response)
		got := want{transient: errors.Is(err, contentreplica.ErrUnavailable), placement: errors.Is(err, contentreplica.ErrPlacement), inactive: errors.Is(err, ErrInactive)}
		if got != c.want {
			t.Errorf("%s: %v reads as %+v, want %+v", c.name, err, got, c.want)
		}
	}
	unreadable := strings.Repeat("x", 64) + "beyond the first 64 bytes"
	contentReplyError("node-b", reply(http.StatusBadGateway, unreadable))
	if !strings.Contains(logs.String(), strings.Repeat("x", 64)) || strings.Contains(logs.String(), "beyond") {
		t.Errorf("an unreadable reply is not logged with its first 64 bytes: %s", logs.String())
	}
}

// A coordinator storing content on a peer whose replica is behind gets a
// temporary unavailability back, not a placement refusal: the copy it did
// not get does not say the peer may not hold it.
func TestContentHubTakesALaggingReceiverAsUnavailable(t *testing.T) {
	peers, active := contentPeers(t)
	behind(peers[1])
	data := []byte("content for a lagging receiver")
	object := workspaceObject(peers, data)
	_, err := (peerContentTransport{peer: peers[0], active: active}).Put(t.Context(), peers[1].Config.NodeID, contentreplica.Upload{ID: strings.Repeat("b", 64), Object: object}, bytes.NewReader(data))
	if !errors.Is(err, contentreplica.ErrUnavailable) || errors.Is(err, contentreplica.ErrPlacement) {
		t.Fatalf("a lagging receiver reads as %v", err)
	}
}

// A peer says why it refused a content request — who asked, with which
// coordinator epoch and writer generation, and the reason — once per caller
// and code for a while, not once per request.
func TestContentPeerLogsWhyItRefusedOncePerCallerAndCode(t *testing.T) {
	peers, active := contentPeers(t)
	receiver := peers[1]
	var logs syncBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	object := workspaceObject(peers, []byte("content a peer refuses and says why"))
	stale := contentHeaders(active, &object)
	stale["X-Steve-Writer-Generation"] = strconv.FormatUint(active.WriterGeneration+1, 10)
	for range 3 {
		if reply := contentHTTP(t, peers[0], receiver, http.MethodGet, stale, time.Minute); reply.status != http.StatusForbidden {
			t.Fatalf("stale writer: HTTP %d %q %v", reply.status, reply.body, reply.err)
		}
	}
	if reply := contentHTTP(t, peers[0], receiver, http.MethodGet, map[string]string{}, time.Minute); reply.status != http.StatusForbidden {
		t.Fatalf("no credentials: HTTP %d %q %v", reply.status, reply.body, reply.err)
	}
	var refusals []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "cluster: content refused") {
			refusals = append(refusals, line)
		}
	}
	epoch, _ := coordinatorHeaders(active)
	caller := "caller=" + peers[0].Config.NodeID
	switch {
	case len(refusals) != 2:
		t.Fatalf("want one line for the stale writer and one for the missing credentials, got %d:\n%s", len(refusals), strings.Join(refusals, "\n"))
	case !strings.Contains(refusals[0], "level=WARN") || !strings.Contains(refusals[0], caller) || !strings.Contains(refusals[0], "code=stale") || !strings.Contains(refusals[0], "epoch="+epoch) || !strings.Contains(refusals[0], "writer="+stale["X-Steve-Writer-Generation"]) || !strings.Contains(refusals[0], "committed epoch"):
		t.Fatalf("the stale writer's refusal does not say who, with what, and why: %s", refusals[0])
	case !strings.Contains(refusals[1], caller) || !strings.Contains(refusals[1], "code=authority"):
		t.Fatalf("the missing credentials' refusal does not say who and why: %s", refusals[1])
	}
}

// A repair that cannot check a placement for now — the committed state out
// of reach — leaves the content for its next round: it neither declares the
// placement blocked nor counts the copies it could not check as lost, and
// it copies nothing.
func TestContentRepairWaitsOutAPlacementItCouldNotCheck(t *testing.T) {
	peers, active := contentPeers(t)
	client, err := peers[0].ContentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("placement checked next round")
	ref := checkpoint.Reference(data)
	manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	recordRepairManifest(t, active.Ledger, manifest)
	var observations []string
	worker, err := peers[0].newContentRepair(active, func(kind, _, message string, _ map[string]string) {
		observations = append(observations, kind+": "+message)
	})
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedContentRepair{Replicator: worker.client}
	worker.client = counted
	unreachable := contentStateReader(func(context.Context) (coordination.State, error) {
		return coordination.State{}, errors.New("leader out of reach")
	})
	peers[0].readContentState.Store(&unreachable)
	t.Cleanup(func() { peers[0].readContentState.Store(nil) })

	if live, err := worker.reachableDomains(t.Context(), manifest, map[string]bool{}); !errors.Is(err, contentreplica.ErrUnavailable) {
		t.Fatalf("copies whose placement could not be checked: %d live, err=%v; want unavailable", live, err)
	}
	report, err := worker.sweep(t.Context())
	if err != nil || report.Degraded != 1 || report.Skipped != 0 || counted.reads != 0 || counted.prepares != 0 {
		t.Fatalf("repair without a placement check: %+v read=%d prepare=%d %v; want degraded, nothing copied", report, counted.reads, counted.prepares, err)
	}
	if strings.Contains(strings.Join(observations, "\n"), "content.placement_blocked") {
		t.Fatalf("an unchecked placement was reported blocked: %q", observations)
	}

	peers[0].readContentState.Store(nil)
	if report, err := worker.sweep(t.Context()); err != nil || report.Healthy != 1 {
		t.Fatalf("repair once the placement can be checked: %+v %v", report, err)
	}
}

// supersede makes a peer's content checks see a writer generation after
// the caller's, and counts how often they read the committed state.
func supersede(t *testing.T, peer *Peer) *atomic.Int64 {
	runtime := peer.Runtime.Load()
	reads := &atomic.Int64{}
	read := contentStateReader(func(ctx context.Context) (coordination.State, error) {
		reads.Add(1)
		state, err := runtime.ReadState(ctx)
		state.WriterGeneration++
		return state, err
	})
	peer.readContentState.Store(&read)
	t.Cleanup(func() { peer.readContentState.Store(nil) })
	return reads
}

// A repair whose peers answer that its writer generation is no longer the
// current one stops there: the generation has ended, so it neither reports
// the content unavailable or short of copies nor asks the next peer, which
// would answer the same.
func TestContentRepairStopsWhenAPeerSaysItsGenerationHasEnded(t *testing.T) {
	peers, active := contentPeers(t)
	client, err := peers[0].ContentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(text string) contentreplica.Manifest {
		data := []byte(text)
		ref := checkpoint.Reference(data)
		manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		recordRepairManifest(t, active.Ledger, manifest)
		return manifest
	}
	// Read: this node's copy is gone, so the other copy is read from its peer.
	reading := prepare("content read from a peer that has seen a later writer")
	files, err := filepath.Glob(filepath.Join(peers[0].Config.DataDir, "content", "blobs", "*", reading.Object.Blob.SHA256))
	if err != nil || len(files) == 0 {
		t.Fatalf("this node's copy: %q %v", files, err)
	}
	for _, file := range files {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	// Prepare: this node's copy is read, and a second one is stored on a peer.
	storing := prepare("content stored on a peer that has seen a later writer")
	var observations []string
	worker, err := peers[0].newContentRepair(active, func(kind, _, message string, _ map[string]string) {
		observations = append(observations, kind+": "+message)
	})
	if err != nil {
		t.Fatal(err)
	}
	var reads []*atomic.Int64
	for _, peer := range peers[1:] {
		reads = append(reads, supersede(t, peer))
	}
	for _, c := range []struct {
		name     string
		manifest contentreplica.Manifest
	}{{"read", reading}, {"prepare", storing}} {
		for _, r := range reads {
			r.Store(0)
		}
		observations = nil
		// The copy on the other peer is taken as out of reach, so the
		// repair reads or stores one.
		availability := map[string]bool{}
		for _, receipt := range c.manifest.Receipts {
			if receipt.NodeID != peers[0].Config.NodeID {
				availability[receipt.NodeID] = false
			}
		}
		_, err := worker.repairOne(t.Context(), c.manifest, availability)
		asked := reads[0].Load() + reads[1].Load()
		if !errors.Is(err, ErrInactive) || len(observations) != 0 || asked != 1 {
			t.Errorf("%s: %v with %q after %d peer checks; want the generation's end, no notice, one peer asked", c.name, err, observations, asked)
		}
	}
}
