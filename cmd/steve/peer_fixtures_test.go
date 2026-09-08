package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/hashicorp/raft"
)

func ClusterPeerTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "steve-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func testPeerOptions(t *testing.T, root string, source *cluster.Peer) (cluster.PeerOptions, *desktop.Installation) {
	t.Helper()
	installed, err := desktop.Bootstrap(desktop.Options{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	clusterPath, err := cluster.PrepareDesktopCluster(installed.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if source != nil {
		cfg, err := cluster.LoadClusterPeerConfig(clusterPath)
		if err != nil {
			t.Fatal(err)
		}
		caPEM, err := cluster.ReadClusterPrivate(source.Config.CACertFile)
		if err != nil {
			t.Fatal(err)
		}
		caBlock, _ := pem.Decode(caPEM)
		ca, err := x509.ParseCertificate(caBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM, err := cluster.ReadClusterPrivate(source.Config.CAKeyFile)
		if err != nil {
			t.Fatal(err)
		}
		keyBlock, _ := pem.Decode(keyPEM)
		key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		cert, private, err := cluster.IssueNodeCertificate(ca, key.(ed25519.PrivateKey), source.Config.ClusterID, cfg.NodeID)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ClusterID = source.Config.ClusterID
		cfg.Bootstrap = false
		cfg.CAKeyFile = ""
		cfg.Seeds = []coordination.Member{{NodeID: source.Config.NodeID, Name: source.Config.Name, Address: source.Config.RaftAddress, APIAddress: source.Config.PeerURL}}
		for _, file := range []struct {
			path string
			data []byte
		}{{cfg.CACertFile, caPEM}, {cfg.CertFile, cert}, {cfg.KeyFile, private}, {cfg.OwnerTokenFile, []byte(source.OwnerToken)}} {
			if err := cluster.WritePrivate(file.path, file.data, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := cluster.SaveClusterJSON(clusterPath, cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	isolated, err := cluster.LoadClusterPeerConfig(clusterPath)
	if err != nil {
		t.Fatal(err)
	}
	isolated.RaftAddress = "127.0.0.1:0"
	isolated.PeerAddress = "127.0.0.1:0"
	isolated.RaftBindAddress = "127.0.0.1:0"
	isolated.PeerBindAddress = "127.0.0.1:0"
	isolated.PeerURL = ""
	if err := cluster.SaveClusterJSON(clusterPath, isolated, false); err != nil {
		t.Fatal(err)
	}
	settings := raft.DefaultConfig()
	return cluster.PeerOptions{ConfigPath: installed.Paths.Config, ClusterPath: clusterPath, RaftConfig: settings, PollInterval: 25 * time.Millisecond, TestFailureDomain: func() (string, error) { return "test-domain-" + installed.NodeID, nil }}, installed
}

func testPeerApplication(t *testing.T, activations *atomic.Int32) func(context.Context, cluster.Activation, func(cluster.PeerApplicationEndpoint) error) (cluster.Deactivate, error) {
	t.Helper()
	return func(ctx context.Context, activation cluster.Activation, ready func(cluster.PeerApplicationEndpoint) error) (cluster.Deactivate, error) {
		activations.Add(1)
		document := activation.Ledger.Document("peer-test-data")
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		token, err := cluster.ClusterRandomToken()
		if err != nil {
			return nil, err
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cluster.ConstantToken(r.Header.Get("Authorization"), token) || r.URL.Query().Get("token") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
				http.Error(w, "proxy leaked client credentials", http.StatusBadRequest)
				return
			}
			if r.Method == http.MethodPost {
				data, err := io.ReadAll(io.LimitReader(r.Body, 1024))
				if err != nil {
					cluster.HTTPError(w, err)
					return
				}
				if err := document.Save(data); err != nil {
					cluster.HTTPError(w, err)
					return
				}
			}
			data, _, err := document.Load()
			if err != nil {
				cluster.HTTPError(w, err)
				return
			}
			cluster.WriteJSON(w, map[string]any{"node_id": activation.NodeID, "generation": activation.Generation, "data": string(data)})
		}), BaseContext: func(net.Listener) context.Context { return ctx }}
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		if err := ready(cluster.PeerApplicationEndpoint{URL: "http://" + listener.Addr().String(), Token: token}); err != nil {
			server.Close()
			return nil, err
		}
		return func(context.Context) error { server.Close(); <-done; return nil }, nil
	}
}

func StartTestPeer(t *testing.T, options cluster.PeerOptions) *cluster.Peer {
	t.Helper()
	peer, err := openClusterPeer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := peer.Close(); err != nil {
			t.Errorf("close peer: %v", err)
		}
	})
	return peer
}

func WaitPeerReady(t *testing.T, peer *cluster.Peer) cluster.Activation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	active, err := peer.Runtime.Load().WaitReady(ctx)
	if err != nil {
		t.Fatalf("peer did not activate: %v; status=%+v", err, peer.Runtime.Load().Status())
	}
	return active
}

func PeerRequest(t *testing.T, peer *cluster.Peer, method, path string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, peer.UiURL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+peer.UIToken)
	request.Header.Set("Cookie", "local-secret-cookie")
	request.Header.Set("Referer", peer.UiURL+"/?token="+peer.UIToken)
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}
