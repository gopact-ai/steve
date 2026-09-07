package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

type countedContentRepair struct {
	contentreplica.Replicator
	reads, prepares int
}

func (c *countedContentRepair) Read(ctx context.Context, m contentreplica.Manifest, w io.Writer) (contentreplica.Manifest, error) {
	c.reads++
	return c.Replicator.Read(ctx, m, w)
}
func (c *countedContentRepair) Prepare(ctx context.Context, project, kind, key string, ref contentreplica.BlobRef, source io.ReadSeeker) (contentreplica.Manifest, error) {
	c.prepares++
	return c.Replicator.Prepare(ctx, project, kind, key, ref, source)
}

func recordRepairManifest(t *testing.T, book *ledger.Ledger, m contentreplica.Manifest) {
	t.Helper()
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, m); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestContentRepairDoesNotReadOrRetransmitHealthyCopies(t *testing.T) {
	peers, active := contentPeers(t)
	client, err := peers[0].contentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("healthy acknowledged replicas")
	ref := checkpoint.Reference(data)
	manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	recordRepairManifest(t, active.Ledger, manifest)
	worker, err := peers[0].newContentRepair(active, nil)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedContentRepair{Replicator: worker.client}
	worker.client = counted
	for range 2 {
		report, err := worker.sweep(t.Context())
		if err != nil || report.Healthy != 1 || report.Repaired != 0 {
			t.Fatalf("healthy repair scan: %+v %v", report, err)
		}
	}
	if counted.reads != 0 || counted.prepares != 0 {
		t.Fatalf("healthy content was retransmitted: reads=%d prepares=%d", counted.reads, counted.prepares)
	}
}

func TestContentRepairSurvivesSecondaryLossThenOriginalLoss(t *testing.T) {
	peers, active := contentPeers(t)
	source := peers[0]
	client, err := source.contentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("a repaired independent receipt survives the next machine loss")
	ref := checkpoint.Reference(data)
	manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	recordRepairManifest(t, active.Ledger, manifest)
	var secondary, third *clusterPeer
	for _, peer := range peers[1:] {
		has := false
		for _, receipt := range manifest.Receipts {
			has = has || receipt.NodeID == peer.config.NodeID
		}
		if has {
			secondary = peer
		} else {
			third = peer
		}
	}
	if secondary == nil || third == nil {
		t.Fatal("fixture did not create exactly two independent copies")
	}
	if err := secondary.Close(); err != nil {
		t.Fatal(err)
	}
	source.options.ContentRepairInterval = 25 * time.Millisecond
	var eventMu sync.Mutex
	var events []string
	stop := source.startContentRepair(active, func(kind, _, message string) {
		eventMu.Lock()
		events = append(events, kind+": "+message)
		eventMu.Unlock()
	})
	defer stop()
	var latest contentreplica.Manifest
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		latest, ok, err = contentreplica.Lookup(t.Context(), active.Ledger, manifest.ID)
		if err != nil {
			t.Fatal(err)
		}
		copied := false
		for _, receipt := range latest.Receipts {
			copied = copied || receipt.NodeID == third.config.NodeID
		}
		if ok && copied {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	found := false
	for _, receipt := range latest.Receipts {
		found = found || receipt.NodeID == third.config.NodeID
	}
	if !found {
		t.Fatal("background repair did not add the third node's durable receipt")
	}
	eventMu.Lock()
	notified := strings.Contains(strings.Join(events, "\n"), "content.repaired")
	eventMu.Unlock()
	if !notified {
		t.Fatal("repair completion was not visible in observations")
	}
	if len(source.runtime.Load().Status().Voters) != 3 {
		t.Fatal("secondary outage shrank voting membership")
	}
	// Restore the secondary's vote before another node fails. Three voters do
	// not have a majority after two simultaneous outages.
	restarted := startTestPeer(t, secondary.options)
	for deadline := time.Now().Add(6 * time.Second); ; {
		if restarted.runtime.Load().Status().AppVersion >= source.runtime.Load().Status().AppVersion {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("secondary did not restore its replicated control state")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(6 * time.Second); ; {
		_, err := third.runtime.Load().ReadState(t.Context())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := third.runtime.Load().Transfer(t.Context(), coordination.TransferRequest{ID: "after-repaired-copy", Actor: "owner", ExpectedEpoch: active.Assignment.Epoch, TargetNodeID: third.config.NodeID}); err != nil {
		t.Fatal(err)
	}
	next := waitPeerReady(t, third)
	reader, err := third.contentReplicator(next)
	if err != nil {
		t.Fatal(err)
	}
	current, ok, err := contentreplica.Lookup(t.Context(), next.Ledger, manifest.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := reader.Read(t.Context(), current, &output); err != nil || !bytes.Equal(output.Bytes(), data) {
		t.Fatalf("repaired third copy did not survive original loss: %q %v", output.Bytes(), err)
	}
}

func TestContentRepairReportsUnavailableAndContinuesIndependentObjects(t *testing.T) {
	peers, active := contentPeers(t)
	client, err := peers[0].contentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("still recoverable independent object")
	ref := checkpoint.Reference(data)
	good, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	recordRepairManifest(t, active.Ledger, good)
	missing := good
	for i := 0; ; i++ {
		other := checkpoint.Reference([]byte(fmt.Sprintf("lost-bytes-%d", i)))
		missing.Object.Blob = other
		missing.Object.Key = other.SHA256
		missing.ID = missing.Object.ID()
		if missing.ID < good.ID {
			break
		}
	}
	missing.Receipts = append([]contentreplica.Receipt(nil), good.Receipts...)
	for i := range missing.Receipts {
		missing.Receipts[i].ObjectID = missing.ID
	}
	recordRepairManifest(t, active.Ledger, missing)
	for _, peer := range peers[1:] {
		for _, receipt := range good.Receipts {
			if receipt.NodeID == peer.config.NodeID {
				if err := peer.Close(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	var observations []string
	worker, err := peers[0].newContentRepair(active, func(kind, _, message string) { observations = append(observations, kind+": "+message) })
	if err != nil {
		t.Fatal(err)
	}
	report, err := worker.sweep(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Unavailable != 1 || report.Repaired != 1 {
		t.Fatalf("one unavailable object blocked independent repair: %+v", report)
	}
	if !strings.Contains(strings.Join(observations, "\n"), "content.unavailable") {
		t.Fatal("lost content was not made visible")
	}
	firstCount := len(observations)
	if _, err := worker.sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(observations) != firstCount {
		t.Fatalf("unchanged unavailable state generated repeated notifications: %+v", observations)
	}
}

func TestContentRepairRejectsUnclassifiedScopeWithoutCopying(t *testing.T) {
	peers, active := contentPeers(t)
	data := []byte("unknown scope")
	ref := checkpoint.Reference(data)
	object := contentreplica.Object{Scope: contentreplica.Scope{ProjectID: "unknown", Level: "public", HomeNodeID: peers[0].config.NodeID}, Kind: contentreplica.Material, Key: ref.SHA256, Blob: ref}
	manifest := contentreplica.Manifest{ID: object.ID(), Object: object, RequiredCopies: 1, Protection: contentreplica.SingleNode, Receipts: []contentreplica.Receipt{{ObjectID: object.ID(), NodeID: peers[0].config.NodeID, FailureDomain: peers[0].config.FailureDomain, StoredAt: time.Now().UTC()}}}
	recordRepairManifest(t, active.Ledger, manifest)
	worker, err := peers[0].newContentRepair(active, nil)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedContentRepair{Replicator: worker.client}
	worker.client = counted
	report, err := worker.sweep(t.Context())
	if err != nil || report.Skipped != 1 || counted.prepares != 0 || counted.reads != 0 {
		t.Fatalf("unknown scope was copied: %+v read=%d prepare=%d %v", report, counted.reads, counted.prepares, err)
	}
	if err := active.Runtime.RestartGeneration(active.Generation); err != nil {
		t.Fatal(err)
	}
	waitPeerReady(t, peers[0])
	if _, err := worker.sweep(context.Background()); err == nil {
		t.Fatal("old repair generation acquired replacement authority")
	}
}

func TestContentRepairUpgradesSingleMachineManifestAfterJoiningPeers(t *testing.T) {
	root := clusterPeerTestDir(t)
	options, _ := testPeerOptions(t, filepath.Join(root, "source"), nil)
	options.Activate = func(context.Context, cluster.Activation, func(peerApplicationEndpoint) error) (cluster.Deactivate, error) {
		return nil, nil
	}
	source := startTestPeer(t, options)
	active := waitPeerReady(t, source)
	worker := source.Worker()
	d := platformconfig.Declaration{Settings: (&config.Config{Gateway: config.Gateway{OwnerID: "test-owner"}}).SettingsValues(), Channels: (&config.Config{Gateway: config.Gateway{OwnerID: "test-owner"}}).ChannelSettings(), Home: config.ProjectHome{Node: worker.Name, Path: "/fixture/home"}, Projects: map[string]config.Project{"workspace": {Level: "internal", Home: config.ProjectHome{Node: worker.Name, Path: "/fixture/workspace"}}}, Nodes: map[string]config.Node{worker.Name: {Addr: worker.Address, Token: worker.Token, Level: "restricted"}}}
	var err error
	d, err = platformconfig.New(active.Ledger).Save(t.Context(), 0, d)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	if err := d.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	projects := project.Open(active.Ledger)
	projects.SetHubID(source.config.ClusterID)
	if err := (config.ProjectController{Store: projects}).Reconcile(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	client, err := source.contentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("first-launch local material")
	ref := checkpoint.Reference(data)
	manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Protection != contentreplica.SingleNode || manifest.RequiredCopies != 1 {
		t.Fatal("initial single-machine content made a false independent-copy claim")
	}
	recordRepairManifest(t, active.Ledger, manifest)
	for _, name := range []string{"second", "third"} {
		options, _ := testPeerOptions(t, filepath.Join(root, name), source)
		options.Activate = func(context.Context, cluster.Activation, func(peerApplicationEndpoint) error) (cluster.Deactivate, error) {
			return nil, nil
		}
		peer := startTestPeer(t, options)
		if _, err := source.Join(t.Context(), coordination.JoinRequest{ID: "join-" + name, Actor: "owner", Member: coordination.Member{NodeID: peer.config.NodeID, Address: peer.config.RaftAddress, APIAddress: peer.config.PeerURL}}); err != nil {
			t.Fatal(err)
		}
		worker := peer.Worker()
		d.Nodes[worker.Name] = config.Node{Addr: worker.Address, Token: worker.Token, Level: "restricted"}
	}
	d, err = platformconfig.New(active.Ledger).Save(t.Context(), d.Revision, d)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := source.newContentRepair(active, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := repair.sweep(t.Context())
	if err != nil || report.Repaired != 1 {
		t.Fatalf("single-machine content was not upgraded: %+v %v", report, err)
	}
	upgraded, ok, err := contentreplica.Lookup(t.Context(), active.Ledger, manifest.ID)
	if err != nil || !ok || !upgraded.Recoverable() {
		t.Fatalf("upgraded independent receipts missing: %+v %v", upgraded, err)
	}
}

type quotaOnContentRead struct{ contentreplica.Replicator }

func (c quotaOnContentRead) Read(context.Context, contentreplica.Manifest, io.Writer) (contentreplica.Manifest, error) {
	return contentreplica.Manifest{}, checkpoint.ErrQuota
}

func TestContentRepairLocalFailuresAreVisibleWithoutClaimingRemoteDataLost(t *testing.T) {
	for _, failure := range []string{"quota", "staging"} {
		t.Run(failure, func(t *testing.T) {
			peers, active := contentPeers(t)
			client, err := peers[0].contentReplicator(active)
			if err != nil {
				t.Fatal(err)
			}
			data := []byte("a healthy source exists despite local repair failure")
			ref := checkpoint.Reference(data)
			manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			recordRepairManifest(t, active.Ledger, manifest)
			for _, peer := range peers[1:] {
				for _, receipt := range manifest.Receipts {
					if receipt.NodeID == peer.config.NodeID {
						if err := peer.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			var events []string
			worker, err := peers[0].newContentRepair(active, func(kind, _, message string) { events = append(events, kind+": "+message) })
			if err != nil {
				t.Fatal(err)
			}
			if failure == "quota" {
				worker.client = quotaOnContentRead{worker.client}
			} else if err := os.WriteFile(filepath.Join(peers[0].config.DataDir, "content-repair"), []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			report, err := worker.sweep(t.Context())
			if err != nil || report.Degraded != 1 || report.Unavailable != 0 {
				t.Fatalf("local failure misreported remote content: %+v %v", report, err)
			}
			if len(events) != 1 || !strings.HasPrefix(events[0], "content.degraded:") {
				t.Fatalf("repair failure was silent or mislabeled: %+v", events)
			}
			if _, err := worker.sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 {
				t.Fatal("unchanged local failure repeatedly notified")
			}
		})
	}
}
