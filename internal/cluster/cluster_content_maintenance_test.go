package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/material"
)

type lostContentReplies struct{ peerContentTransport }

func (transport lostContentReplies) Put(ctx context.Context, node string, upload contentreplica.Upload, source io.Reader) (contentreplica.Receipt, error) {
	if _, err := transport.peerContentTransport.Put(ctx, node, upload, source); err != nil {
		return contentreplica.Receipt{}, err
	}
	// Fault injection after the real authenticated receiver has fsynced its
	// promise. Only the reply is lost; storage and ledger remain real.
	return contentreplica.Receipt{}, io.ErrUnexpectedEOF
}

func contentClientLosingReplies(t *testing.T, peer *Peer, active Activation) *contentreplica.Client {
	t.Helper()
	client, err := contentreplica.New(contentreplica.Config{
		Ledger: active.Ledger,
		NodeID: peer.Config.NodeID,
		Local:  peerLocalContent{peer: peer},
		Remote: lostContentReplies{peerContentTransport{peer: peer, active: active}},
		Policy: peerContentPolicy{peer: peer},
		Scope: func(ctx context.Context, id string) (contentreplica.Scope, error) {
			current, err := peer.contentState(ctx, id)
			return contentScope(current.project), err
		},
		Members: func(ctx context.Context) ([]string, error) {
			state, err := active.Runtime.ReadState(ctx)
			var nodes []string
			for node := range state.Members {
				nodes = append(nodes, node)
			}
			return nodes, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func contentMaintenanceHTTP(t *testing.T, peer, target *Peer, active Activation, body string, writer uint64) int {
	t.Helper()
	state, err := active.Runtime.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	transport, origin, err := peer.remoteTransport(state.Members[target.Config.NodeID])
	if err != nil {
		t.Fatal(err)
	}
	var source io.Reader
	if body != "" {
		source = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin.String()+clusterContentPath, source)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Steve-Coordinator-Epoch", strconv.FormatUint(state.Coordinator.Epoch, 10))
	request.Header.Set("X-Steve-Writer-Generation", strconv.FormatUint(writer, 10))
	response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode
}

func TestContentMaintenanceRejectsCallerDeleteListsAndStaleWriter(t *testing.T) {
	peers, active := contentPeers(t)
	if status := contentMaintenanceHTTP(t, peers[0], peers[1], active, `{"delete":["caller-selected-object"]}`, active.WriterGeneration); status != http.StatusBadRequest {
		t.Fatalf("caller-supplied deletion list accepted: HTTP %d", status)
	}
	if status := contentMaintenanceHTTP(t, peers[0], peers[1], active, "", active.WriterGeneration+1); status != http.StatusForbidden {
		t.Fatalf("wrong writer maintenance accepted: HTTP %d", status)
	}
	if status := contentMaintenanceHTTP(t, peers[0], peers[1], active, "", active.WriterGeneration); status != http.StatusOK {
		t.Fatalf("authenticated empty maintenance request failed: HTTP %d", status)
	}
}

func TestContentMaintenanceAppliesOwnerAbortWithoutCollectingUnknownReceipts(t *testing.T) {
	peers, active := contentPeers(t)
	state, err := peers[0].contentState(t.Context(), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	shared := []byte("identical bytes with an independent unknown receipt")
	scope := contentScope(state.project)
	object := contentreplica.Object{Scope: scope, Kind: contentreplica.Material, Key: checkpoint.Reference(shared).SHA256, Blob: checkpoint.Reference(shared)}
	unknown := contentreplica.Upload{ID: strings.Repeat("a", 64), Object: object}
	receipts := map[string]contentreplica.Receipt{}
	for _, peer := range peers {
		store, release, err := peer.acquireContent()
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := store.Put(t.Context(), unknown, bytes.NewReader(shared))
		release()
		if err != nil {
			t.Fatal(err)
		}
		receipts[peer.Config.NodeID] = receipt
	}
	owner, err := material.Open(t.TempDir(), active.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.SetReplication(contentClientLosingReplies(t, peers[0], active))
	unshared := []byte("failed owner preparation may release only this upload")
	for _, data := range [][]byte{shared, unshared} {
		if _, err := owner.Upload(t.Context(), "workspace", "lost reply", "text/plain", bytes.NewReader(data)); !errors.Is(err, contentreplica.ErrIncomplete) {
			t.Fatalf("lost receiver replies must fail real material preparation: %v", err)
		}
	}
	if items, err := owner.List(t.Context(), "workspace"); err != nil || len(items) != 0 {
		t.Fatalf("failed owner published metadata: %+v %v", items, err)
	}
	released := contentreplica.Object{Scope: scope, Kind: contentreplica.Material, Key: checkpoint.Reference(unshared).SHA256, Blob: checkpoint.Reference(unshared)}
	peers[0].Options.ContentRepairInterval = 50 * time.Millisecond
	stop := peers[0].StartContentRepair(active, nil)
	defer stop()
	deadline := time.Now().Add(15 * time.Second)
	for {
		collected := true
		for _, peer := range peers {
			store, release, err := peer.acquireContent()
			if err != nil {
				t.Fatal(err)
			}
			err = store.Get(t.Context(), released, &bytes.Buffer{})
			release()
			collected = collected && (errors.Is(err, checkpoint.ErrIncomplete) || errors.Is(err, os.ErrNotExist))
			entries, err := os.ReadDir(filepath.Join(peer.Config.DataDir, "content", "retained"))
			if err != nil {
				t.Fatal(err)
			}
			collected = collected && len(entries) == 1
			retired, err := os.ReadDir(filepath.Join(peer.Config.DataDir, "content", "retired"))
			if err != nil {
				t.Fatal(err)
			}
			collected = collected && len(retired) == 0
		}
		var unfinished int
		if err := active.Ledger.DB().QueryRow(`SELECT count(*) FROM bindings WHERE kind IN ('content-upload','content-receipt','content-release')`).Scan(&unfinished); err != nil {
			t.Fatal(err)
		}
		collected = collected && unfinished == 0
		if collected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("real maintenance loop did not finish exact receipt collection/ledger compaction: unfinished=%d", unfinished)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	for _, peer := range peers {
		store, release, err := peer.acquireContent()
		if err != nil {
			t.Fatal(err)
		}
		replay, err := store.Put(t.Context(), unknown, bytes.NewReader(shared))
		release()
		before := receipts[peer.Config.NodeID]
		if err != nil || replay.Key() != before.Key() || !replay.StoredAt.Equal(before.StoredAt) {
			t.Fatalf("maintenance changed or released independent unknown promise: %+v %v", replay, err)
		}
	}
}

func TestContentMaintenanceCompactsSuccessfulOwnerReplacementCycles(t *testing.T) {
	peers, active := contentPeers(t)
	replication, err := peers[0].ContentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := material.Open(t.TempDir(), active.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.SetReplication(replication)
	data := []byte("the current successful material owner remains readable")
	var current material.Material
	for cycle := range 4 {
		current, err = owner.Upload(t.Context(), "workspace", "stable owner", "text/plain", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		peers[0].Options.ContentRepairInterval = 50 * time.Millisecond
		stop := peers[0].StartContentRepair(active, nil)
		err = waitOnlyCurrentContentUpload(t, active)
		stop()
		if err != nil {
			t.Fatalf("successful owner replacement cycle %d: %v", cycle, err)
		}
		m, err := owner.Get(t.Context(), "workspace", current.ID)
		if err != nil || m.Content == nil || !m.Content.Complete() {
			t.Fatalf("maintenance damaged current owner: %+v %v", m, err)
		}
		current = m
	}
	activeMarkers := 0
	for _, peer := range peers {
		base := filepath.Join(peer.Config.DataDir, "content")
		active, err := os.ReadDir(filepath.Join(base, "retained"))
		if err != nil {
			t.Fatal(err)
		}
		retired, err := os.ReadDir(filepath.Join(base, "retired"))
		if err != nil || len(retired) != 0 {
			t.Fatalf("completed normal replacements still consume object quota: %d %v", len(retired), err)
		}
		activeMarkers += len(active)
	}
	if activeMarkers != len(current.Content.Receipts) {
		t.Fatalf("old exact receipts survived completed maintenance: %d want %d", activeMarkers, len(current.Content.Receipts))
	}
}

func waitOnlyCurrentContentUpload(t *testing.T, active Activation) error {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var uploads, uncollected int
		if err := active.Ledger.DB().QueryRow(`SELECT count(*) FROM bindings WHERE kind='content-upload'`).Scan(&uploads); err != nil {
			return err
		}
		if err := active.Ledger.DB().QueryRow(`SELECT count(*) FROM bindings WHERE kind='content-release' AND json_extract(data, '$.collected')=0`).Scan(&uncollected); err != nil {
			return err
		}
		if uploads == 1 && uncollected == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("live uploads=%d unfinished releases=%d", uploads, uncollected)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A peer that cannot check a maintenance request for now answers before the
// coordinator stops waiting for it, so the coordinator hears why instead of
// only that it heard nothing.
func TestContentMaintenanceRefusalReachesTheCoordinatorInTime(t *testing.T) {
	previous := contentMaintenanceTimeout
	contentMaintenanceTimeout = 3 * time.Second
	t.Cleanup(func() { contentMaintenanceTimeout = previous })
	peers, active := contentPeers(t)
	stuck := contentStateReader(func(ctx context.Context) (coordination.State, error) {
		select {
		case <-ctx.Done():
			return coordination.State{}, ctx.Err()
		case <-time.After(10 * time.Second):
			return coordination.State{}, errors.New("the leader never answered")
		}
	})
	peers[1].readContentState.Store(&stuck)
	t.Cleanup(func() { peers[1].readContentState.Store(nil) })
	_, err := (peerContentTransport{peer: peers[0], active: active}).collect(t.Context(), peers[1].Config.NodeID)
	if !errors.Is(err, contentreplica.ErrUnavailable) {
		t.Fatalf("maintenance on a peer that cannot check it: %v; want the peer's answer that it could not", err)
	}
}
