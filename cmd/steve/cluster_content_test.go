package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/project"
)

func contentPeers(t *testing.T) ([]*clusterPeer, cluster.Activation) {
	t.Helper()
	root := clusterPeerTestDir(t)
	var peers []*clusterPeer
	for i := 0; i < 3; i++ {
		var source *clusterPeer
		if i > 0 {
			source = peers[0]
		}
		options, _ := testPeerOptions(t, filepath.Join(root, string(rune('a'+i))), source)
		options.Activate = func(context.Context, cluster.Activation, func(peerApplicationEndpoint) error) (cluster.Deactivate, error) {
			return nil, nil
		}
		peer := startTestPeer(t, options)
		peers = append(peers, peer)
		if i == 0 {
			waitPeerReady(t, peer)
		} else if _, err := source.Join(t.Context(), coordination.JoinRequest{ID: "content-join-" + peer.config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.config.NodeID, Name: peer.config.Name, Address: peer.config.RaftAddress, APIAddress: peer.config.PeerURL}}); err != nil {
			t.Fatal(err)
		}
	}
	active := waitPeerReady(t, peers[0])
	declaration := platformconfig.Declaration{Settings: (&config.Config{Gateway: config.Gateway{OwnerID: "test-owner"}}).SettingsValues(), Channels: (&config.Config{Gateway: config.Gateway{OwnerID: "test-owner"}}).ChannelSettings(), Home: config.ProjectHome{Node: peers[0].config.NodeID, Path: "/fixture/home"}, Nodes: map[string]config.Node{}, Projects: map[string]config.Project{"workspace": {Level: "internal", Home: config.ProjectHome{Node: peers[0].config.NodeID, Path: "/fixture/workspace"}}}}
	for _, peer := range peers {
		worker := peer.Worker()
		declaration.Nodes[worker.Name] = config.Node{Addr: worker.Address, Token: worker.Token, Level: "restricted"}
	}
	if _, err := platformconfig.New(active.Ledger).Save(t.Context(), 0, declaration); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	if err := declaration.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	projects := project.Open(active.Ledger)
	projects.SetHubID(peers[0].config.ClusterID)
	if err := (config.ProjectController{Store: projects}).Reconcile(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	return peers, active
}

func TestContentPeerCopiesSurviveOriginalCoordinatorLoss(t *testing.T) {
	peers, active := contentPeers(t)
	client, err := peers[0].contentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("replicated material survives the original coordinator")
	ref := checkpoint.Reference(data)
	manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Recoverable() || len(manifest.Receipts) != 2 {
		t.Fatalf("content lacks independent durable receipts: %+v", manifest)
	}
	if err := active.Ledger.Update(t.Context(), func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, manifest); return err }); err != nil {
		t.Fatal(err)
	}
	if err := peers[0].Close(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		state, err := peers[1].runtime.Load().ReadState(t.Context())
		if err == nil && state.Coordinator == active.Assignment {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("surviving majority did not elect a consensus leader: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := peers[1].runtime.Load().Transfer(t.Context(), coordination.TransferRequest{ID: "content-transfer", Actor: "owner", ExpectedEpoch: active.Assignment.Epoch, TargetNodeID: peers[1].config.NodeID, Reason: "test source machine loss"}); err != nil {
		t.Fatal(err)
	}
	next := waitPeerReady(t, peers[1])
	stored, ok, err := contentreplica.Lookup(t.Context(), next.Ledger, manifest.ID)
	if err != nil || !ok {
		t.Fatalf("replica manifest missing: %t %v", ok, err)
	}
	restoredClient, err := peers[1].contentReplicator(next)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	repaired, err := restoredClient.Read(t.Context(), stored, &output)
	if err != nil || !bytes.Equal(output.Bytes(), data) || !repaired.Complete() {
		t.Fatalf("restore after source loss: %q %+v %v", output.Bytes(), repaired, err)
	}
}

func TestContentPeerRejectsUnclassifiedAndForgedScope(t *testing.T) {
	peers, active := contentPeers(t)
	client, err := peers[0].contentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("scope-bound data")
	ref := checkpoint.Reference(data)
	if _, err := client.Prepare(t.Context(), "", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data)); !errors.Is(err, contentreplica.ErrPlacement) {
		t.Fatalf("unclassified material accepted: %v", err)
	}
	object := contentreplica.Object{Scope: contentreplica.Scope{ProjectID: "workspace", Level: "public", HomeNodeID: peers[0].config.NodeID}, Kind: contentreplica.Material, Key: ref.SHA256, Blob: ref}
	if _, err := (peerContentTransport{peer: peers[0], active: active}).Put(t.Context(), peers[1].config.NodeID, object, bytes.NewReader(data)); !errors.Is(err, contentreplica.ErrPlacement) {
		t.Fatalf("sender-chosen classification accepted: %v", err)
	}
}

func TestContentReceiverDoesNotTreatCoordinatorRoleAsDataPermission(t *testing.T) {
	peers, active := contentPeers(t)
	client, err := peers[0].contentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("restricted-content-marker")
	ref := checkpoint.Reference(data)
	manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	declarations := platformconfig.New(active.Ledger)
	d, _, err := declarations.Load()
	if err != nil {
		t.Fatal(err)
	}
	source := d.Nodes[peers[0].config.NodeID]
	source.Level = "public"
	d.Nodes[peers[0].config.NodeID] = source
	if _, err := declarations.Save(t.Context(), d.Revision, d); err != nil {
		t.Fatal(err)
	}
	state, err := peers[0].runtime.Load().ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target := manifest.Receipts[1].NodeID
	transport, origin, err := peers[0].remoteTransport(state.Members[target])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(manifest.Object)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin.String()+clusterContentPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(contentObjectHeader, base64.RawURLEncoding.EncodeToString(raw))
	request.Header.Set("X-Steve-Coordinator-Epoch", strconv.FormatUint(state.Coordinator.Epoch, 10))
	request.Header.Set("X-Steve-Writer-Generation", strconv.FormatUint(state.WriterGeneration, 10))
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusForbidden || bytes.Contains(body, data) {
		t.Fatalf("receiver disclosed data to insufficiently privileged coordinator: %d %q %v", response.StatusCode, body, err)
	}
}

func TestContentClientCannotBorrowAuthorityFromReplacementGeneration(t *testing.T) {
	peers, active := contentPeers(t)
	client, err := peers[0].contentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("old-generation-content")
	ref := checkpoint.Reference(data)
	manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := active.Runtime.RestartGeneration(active.Generation); err != nil {
		t.Fatal(err)
	}
	next := waitPeerReady(t, peers[0])
	if next.WriterGeneration <= active.WriterGeneration {
		t.Fatal("writer generation did not advance")
	}
	if _, err := client.Prepare(context.Background(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data)); !errors.Is(err, cluster.ErrInactive) {
		t.Fatalf("old client acquired replacement writer: %v", err)
	}
	var output bytes.Buffer
	if _, err := client.Read(context.Background(), manifest, &output); !errors.Is(err, cluster.ErrInactive) || output.Len() != 0 {
		t.Fatalf("old client read after replacement: %q %v", output.Bytes(), err)
	}
}

func TestContentUploadDeadlineReleasesHalfOpenRequest(t *testing.T) {
	peers, active := contentPeers(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()
		peers[1].serveContent(w, r.WithContext(ctx))
	}))
	var err error
	server.TLS, err = peers[1].identity.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	defer server.Close()
	tlsConfig, err := peers[0].identity.ClientConfig(peers[1].config.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := tls.Dial("tcp", server.Listener.Addr().String(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ref := checkpoint.Reference(bytes.Repeat([]byte("x"), 4096))
	object := contentreplica.Object{Scope: contentreplica.Scope{ProjectID: "workspace", Level: "internal", HomeNodeID: peers[0].config.NodeID}, Kind: contentreplica.Material, Key: ref.SHA256, Blob: ref}
	raw, _ := json.Marshal(object)
	if _, err := fmt.Fprintf(connection, "PUT %s HTTP/1.1\r\nHost: localhost\r\nContent-Length: 4096\r\n%s: %s\r\nX-Steve-Coordinator-Epoch: %d\r\nX-Steve-Writer-Generation: %d\r\nConnection: close\r\n\r\nx", clusterContentPath, contentObjectHeader, base64.RawURLEncoding.EncodeToString(raw), active.Assignment.Epoch, active.WriterGeneration); err != nil {
		t.Fatal(err)
	}
	// Only one byte arrives; the other peer never completes its advertised body.
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPut})
	if err == nil {
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("partial content was acknowledged")
		}
	}
	released := make(chan struct{})
	go func() { peers[1].contentOps.Wait(); close(released) }()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("half-open upload retained content operation beyond its deadline")
	}
}
