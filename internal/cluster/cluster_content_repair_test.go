package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
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
	client, err := peers[0].ContentReplicator(active)
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

// A repair round reads the committed state through the leader once, however
// many objects it looks at.
func TestContentRepairRoundReadsTheCommittedStateOnce(t *testing.T) {
	peers, active := contentPeers(t)
	client, err := peers[0].ContentReplicator(active)
	if err != nil {
		t.Fatal(err)
	}
	const objects = 4
	for i := range objects {
		data := []byte(fmt.Sprintf("healthy object %d of one round", i))
		ref := checkpoint.Reference(data)
		manifest, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		recordRepairManifest(t, active.Ledger, manifest)
	}
	worker, err := peers[0].newContentRepair(active, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := peers[0].Runtime.Load()
	before := runtime.stateReads.Load()
	report, err := worker.sweep(t.Context())
	if err != nil || report.Healthy != objects {
		t.Fatalf("a round over healthy content: %+v %v", report, err)
	}
	if reads := runtime.stateReads.Load() - before; reads != 1 {
		t.Fatalf("a round over %d healthy objects read the committed state %d times; want one read", objects, reads)
	}
}

func TestContentRepairSurvivesSecondaryLossThenOriginalLoss(t *testing.T) {
	peers, active := contentPeers(t)
	source := peers[0]
	client, err := source.ContentReplicator(active)
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
	var secondary, third *Peer
	for _, peer := range peers[1:] {
		has := false
		for _, receipt := range manifest.Receipts {
			has = has || receipt.NodeID == peer.Config.NodeID
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
	source.Options.ContentRepairInterval = 25 * time.Millisecond
	var eventMu sync.Mutex
	var events []string
	stop := source.StartContentRepair(active, func(kind, _, message string, _ map[string]string) {
		eventMu.Lock()
		events = append(events, kind+": "+message)
		eventMu.Unlock()
	})
	defer stop()
	// The repair loop reports under eventMu; scanning the slice keeps the
	// check cheap so the loop is not held up by its own observer.
	observed := func(marker string) bool {
		eventMu.Lock()
		defer eventMu.Unlock()
		for _, event := range events {
			if strings.Contains(event, marker) {
				return true
			}
		}
		return false
	}
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
			copied = copied || receipt.NodeID == third.Config.NodeID
		}
		// The receipt lands before the repair loop reports it; stopping
		// the loop on the receipt alone can cut the observation off.
		if ok && copied && observed("content.repaired") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	found := false
	for _, receipt := range latest.Receipts {
		found = found || receipt.NodeID == third.Config.NodeID
	}
	if !found {
		t.Fatal("background repair did not add the third node's durable receipt")
	}
	if !observed("content.repaired") {
		t.Fatal("repair completion was not visible in observations")
	}
	if len(source.Runtime.Load().Status().Voters) != 3 {
		t.Fatal("secondary outage shrank voting membership")
	}
	// Restore the secondary's vote before another node fails. Three voters do
	// not have a majority after two simultaneous outages.
	restarted := StartTestPeer(t, secondary.Options)
	for deadline := time.Now().Add(6 * time.Second); ; {
		if restarted.Runtime.Load().Status().AppVersion >= source.Runtime.Load().Status().AppVersion {
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
		_, err := third.Runtime.Load().ReadState(t.Context())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := third.Runtime.Load().Transfer(t.Context(), coordination.TransferRequest{ID: "after-repaired-copy", Actor: "owner", ExpectedEpoch: active.Assignment.Epoch, TargetNodeID: third.Config.NodeID}); err != nil {
		t.Fatal(err)
	}
	next := WaitPeerReady(t, third)
	reader, err := third.ContentReplicator(next)
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
	client, err := peers[0].ContentReplicator(active)
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
	missingObject := good.Object
	var lost []byte
	for i := 0; ; i++ {
		lost = []byte(fmt.Sprintf("lost-bytes-%d", i))
		other := checkpoint.Reference(lost)
		missingObject.Blob = other
		missingObject.Key = other.SHA256
		if missingObject.ID() < good.ID {
			break
		}
	}
	missing, err := client.Prepare(t.Context(), "workspace", contentreplica.Material, missingObject.Key, missingObject.Blob, bytes.NewReader(lost))
	if err != nil {
		t.Fatal(err)
	}
	recordRepairManifest(t, active.Ledger, missing)
	// Lose the actual acknowledged copies, not fabricate receipts belonging
	// to another upload. Independent content must still be repaired.
	for _, peer := range peers {
		files, err := filepath.Glob(filepath.Join(peer.Config.DataDir, "content", "blobs", "*", missing.Object.Blob.SHA256))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, peer := range peers[1:] {
		for _, receipt := range good.Receipts {
			if receipt.NodeID == peer.Config.NodeID {
				if err := peer.Close(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	var observations []string
	worker, err := peers[0].newContentRepair(active, func(kind, _, message string, _ map[string]string) {
		observations = append(observations, kind+": "+message)
	})
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
	object := contentreplica.Object{Scope: contentreplica.Scope{ProjectID: "unknown", Level: "public", HomeNodeID: peers[0].Config.NodeID}, Kind: contentreplica.Material, Key: ref.SHA256, Blob: ref}
	manifest := contentreplica.Manifest{ID: object.ID(), Object: object, RequiredCopies: 1, Protection: contentreplica.SingleNode, Receipts: []contentreplica.Receipt{{UploadID: strings.Repeat("1", 64), ObjectID: object.ID(), NodeID: peers[0].Config.NodeID, FailureDomain: peers[0].Config.FailureDomain, StoredAt: time.Now().UTC()}}}
	if !manifest.Complete() {
		t.Fatal("fixture must have a structurally valid receipt")
	}
	// An unclassified imported row cannot use normal preparation. Inject
	// only this fixture and verify repair refuses it before copying bytes.
	if err := active.Ledger.PutBinding(t.Context(), contentreplica.ManifestKind, manifest.ID, manifest); err != nil {
		t.Fatal(err)
	}
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
	WaitPeerReady(t, peers[0])
	if _, err := worker.sweep(context.Background()); err == nil {
		t.Fatal("old repair generation acquired replacement authority")
	}
}

func TestContentRepairUpgradesSingleMachineManifestAfterJoiningPeers(t *testing.T) {
	root := ClusterPeerTestDir(t)
	options, _ := testPeerOptions(t, filepath.Join(root, "source"), nil)
	options.Activate = func(context.Context, Activation, func(PeerApplicationEndpoint) error) (Deactivate, error) {
		return nil, nil
	}
	source := StartTestPeer(t, options)
	active := WaitPeerReady(t, source)
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
	projects.SetHubID(source.Config.ClusterID)
	if err := (configbuild.ProjectController{Store: projects}).Reconcile(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	client, err := source.ContentReplicator(active)
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
		options.Activate = func(context.Context, Activation, func(PeerApplicationEndpoint) error) (Deactivate, error) {
			return nil, nil
		}
		peer := StartTestPeer(t, options)
		if _, err := source.Join(t.Context(), coordination.JoinRequest{ID: "join-" + name, Actor: "owner", Member: coordination.Member{NodeID: peer.Config.NodeID, Address: peer.Config.RaftAddress, APIAddress: peer.Config.PeerURL, Voting: true}}); err != nil {
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
			client, err := peers[0].ContentReplicator(active)
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
					if receipt.NodeID == peer.Config.NodeID {
						if err := peer.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			var events []string
			worker, err := peers[0].newContentRepair(active, func(kind, _, message string, _ map[string]string) { events = append(events, kind+": "+message) })
			if err != nil {
				t.Fatal(err)
			}
			if failure == "quota" {
				worker.client = quotaOnContentRead{worker.client}
			} else if err := os.WriteFile(filepath.Join(peers[0].Config.DataDir, "content-repair"), []byte("not a directory"), 0o600); err != nil {
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

// Once its business generation has ended, content repair says nothing: not
// that the scan or the maintenance failed, nor anything about the content.
// Neither failed; the next generation checks again. It says nothing whether
// the committed state it reads no longer names this generation before the
// runtime retires it, or a peer answers that it is no longer the writer —
// and maintenance then stops asking the other peers, which would answer
// the same.
func TestContentRepairSaysNothingOnceItsGenerationHasEnded(t *testing.T) {
	peers, active := contentPeers(t)
	var mu sync.Mutex
	var observations []string
	observe := func(kind, _, message string, _ map[string]string) {
		mu.Lock()
		defer mu.Unlock()
		observations = append(observations, kind+": "+message)
	}

	ended := active
	ended.WriterGeneration++
	peers[0].Options.ContentRepairInterval = 10 * time.Millisecond
	runtime := peers[0].Runtime.Load()
	before := runtime.stateReads.Load()
	logs := captureRuntimeLog(t)
	stop := peers[0].StartContentRepair(ended, observe)
	waitFor(t, 10*time.Second, "the scan to find its generation ended", func() bool {
		return strings.Contains(logs.String(), "content repair: scan stopped")
	})
	time.Sleep(20 * peers[0].Options.ContentRepairInterval)
	stop()
	if logged, reads := strings.Count(logs.String(), "content repair: scan stopped"), runtime.stateReads.Load()-before; logged != 1 || reads != 1 {
		t.Errorf("repair whose scan found its generation ended logged that %d times and read the committed state %d times over 20 intervals; want it to end there, once each", logged, reads)
	}
	logged := logs.String()
	if !strings.Contains(logged, "content repair: scan stopped") || strings.Contains(logged, "content repair: maintenance stopped") {
		t.Errorf("repair for a generation the committed state no longer names logged %q; want the scan stopped and no maintenance", logged)
	}
	mu.Lock()
	if len(observations) != 0 {
		t.Errorf("repair for a generation the committed state no longer names said %q; want nothing", observations)
	}
	observations = nil
	mu.Unlock()

	worker, err := peers[0].newContentRepair(active, observe)
	if err != nil {
		t.Fatal(err)
	}
	var asked []*atomic.Int64
	for _, peer := range peers[1:] {
		asked = append(asked, supersede(t, peer))
	}
	worker.runMaintenance(t.Context())
	if len(observations) != 0 || asked[0].Load()+asked[1].Load() != 1 {
		t.Errorf("maintenance that a peer told its generation has ended said %q after asking %d peers; want nothing, one peer asked", observations, asked[0].Load()+asked[1].Load())
	}
}

// Content repair belongs to one generation, which cannot come back: once
// maintenance has heard from a peer that it ended, repair ends too, with
// no further round to scan, ask the peers again or log the end again.
func TestContentRepairEndsWhenMaintenanceHearsItsGenerationEnded(t *testing.T) {
	peers, active := contentPeers(t)
	var asked []*atomic.Int64
	for _, peer := range peers[1:] {
		asked = append(asked, supersede(t, peer))
	}
	peers[0].Options.ContentRepairInterval = 10 * time.Millisecond
	logs := captureRuntimeLog(t)
	stop := peers[0].StartContentRepair(active, nil)
	waitFor(t, 10*time.Second, "maintenance to hear its generation ended", func() bool {
		return strings.Contains(logs.String(), "content repair: maintenance stopped")
	})
	time.Sleep(20 * peers[0].Options.ContentRepairInterval)
	stop()
	logged := logs.String()
	if n := strings.Count(logged, "content repair: maintenance stopped"); n != 1 || strings.Contains(logged, "content repair: scan stopped") || asked[0].Load()+asked[1].Load() != 1 {
		t.Errorf("repair whose maintenance heard its generation ended logged that %d times and asked %d peers over 20 intervals (scan stopped logged: %v); want it to end there, one peer asked", n, asked[0].Load()+asked[1].Load(), strings.Contains(logged, "content repair: scan stopped"))
	}
}
