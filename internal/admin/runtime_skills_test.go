package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/skills"
)

func TestSkillShipperSymlinkSourceReachesRemote(t *testing.T) {
	live, target := shipperLive(t)
	peer := newSkillPeer(t, "remote")
	nodes := skillRegistry(t, peer)
	shipper := &SkillShipper{Live: live, Nodes: nodes}
	if err := live.Map.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	bundle, err := shipper.Pack()
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	want, err := skills.Pack([]skills.Ref{{Name: "alpha", Path: real}})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Hash != want.Hash {
		t.Errorf("symlink root bundle = %s, real directory bundle = %s", bundle.Hash, want.Hash)
	}
	if err := shipper.Ship(t.Context(), peer.name); err != nil {
		t.Fatal(err)
	}
	assertRemoteSkill(t, peer, "SKILL.md", "alpha instructions")
	assertRemoteSkill(t, peer, "scripts/helper.sh", "#!/bin/sh\necho helper\n")
	info, err := os.Stat(filepath.Join(peer.dir, "alpha", "scripts", "helper.sh"))
	if err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("remote executable bit: info = %v, err = %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(peer.dir, "alpha", "external")); !os.IsNotExist(err) {
		t.Fatalf("inner symlink was followed: %v", err)
	}
}

func TestSkillShipperFailureRetriesSavedDesired(t *testing.T) {
	for _, stage := range []string{"blob", "apply"} {
		t.Run(stage, func(t *testing.T) {
			live, _ := shipperLive(t)
			peer := newSkillPeer(t, "remote")
			peer.failure.Store(stage)
			shipper := &SkillShipper{Live: live, Nodes: skillRegistry(t, peer)}
			restarts := 0
			live.After = func() error {
				remoteErr := shipper.ShipAll(t.Context())
				restarts++
				return remoteErr
			}
			if err := live.Enable("alpha"); err == nil {
				t.Fatal("remote failure was reported as applied")
			}
			assertSavedEnabled(t, live)
			if restarts != 1 {
				t.Fatalf("failed propagation skipped local convergence: %d", restarts)
			}
			peer.failure.Store("")
			if err := live.Enable("alpha"); err != nil {
				t.Fatal(err)
			}
			assertRemoteSkill(t, peer, "SKILL.md", "alpha instructions")
			if restarts != 2 {
				t.Fatalf("successful retry restarts = %d, want 2", restarts)
			}
			if err := live.Enable("alpha"); err != nil {
				t.Fatal(err)
			}
			if restarts != 2 {
				t.Fatalf("applied value restarted again: %d", restarts)
			}
		})
	}
}

func TestSkillShipperAggregatesFailuresAndContinues(t *testing.T) {
	live, _ := shipperLive(t)
	if err := live.Map.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	first, second, good := newSkillPeer(t, "a"), newSkillPeer(t, "b"), newSkillPeer(t, "c")
	first.failure.Store("blob")
	second.failure.Store("apply")
	offline := newSkillPeer(t, "offline")
	offline.offline.Store(true)
	nodes := skillRegistry(t, first, second, good, offline)
	observed := map[string]map[string]string{}
	shipper := &SkillShipper{Live: live, Nodes: nodes, Observe: func(_, name, _ string, data map[string]string) {
		observed[name] = data
	}}
	err := shipper.ShipAll(t.Context())
	for _, name := range []string{`"a"`, `"b"`, `"offline"`} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("aggregate does not identify %s: %v", name, err)
		}
	}
	if !errors.Is(err, ErrSkillsPending) {
		t.Errorf("offline node is not pending: %v", err)
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 3 {
		t.Fatalf("failures are not individually unwrap-able: %v", err)
	}
	for _, peer := range []*skillPeer{first, second, good, offline} {
		event, ok := observed[peer.name]
		if !ok || (event["error"] == "") != (peer == good) {
			t.Errorf("wrong materialization observation for %s: %v", peer.name, event)
		}
	}
	assertRemoteSkill(t, good, "SKILL.md", "alpha instructions")
	if offline.dials.Load() != 0 {
		t.Fatal("ShipAll dialed an offline node")
	}
}

func TestSkillShipperOfflineRemainsPendingUntilRetry(t *testing.T) {
	live, _ := shipperLive(t)
	peer := newSkillPeer(t, "offline")
	peer.offline.Store(true)
	nodes := skillRegistry(t, peer)
	shipper := &SkillShipper{Live: live, Nodes: nodes}
	restarts := 0
	live.After = func() error {
		remoteErr := shipper.ShipAll(t.Context())
		restarts++
		return remoteErr
	}
	for range 2 {
		err := live.Enable("alpha")
		if !errors.Is(err, ErrSkillsPending) {
			t.Fatalf("offline result = %v, want pending", err)
		}
		assertSavedEnabled(t, live)
	}
	if restarts != 2 || peer.dials.Load() != 0 {
		t.Fatalf("pending skipped local convergence or dialed: restarts=%d dials=%d", restarts, peer.dials.Load())
	}
	peer.offline.Store(false)
	if _, err := nodes.Advert(t.Context(), peer.name); err != nil {
		t.Fatal(err)
	}
	// The existing node-up callback can ship without restarting anything.
	if err := shipper.Ship(t.Context(), peer.name); err != nil {
		t.Fatal(err)
	}
	assertRemoteSkill(t, peer, "SKILL.md", "alpha instructions")
	if err := live.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	if restarts != 3 || peer.applies.Load() != 1 {
		t.Fatalf("retry did not converge/deduplicate: restarts=%d applies=%d", restarts, peer.applies.Load())
	}
}

func TestSkillShipperReturnsDirectErrors(t *testing.T) {
	for _, failure := range []string{"blob", "apply", "unsupported", "offline", "canceled", "pack"} {
		t.Run(failure, func(t *testing.T) {
			live, target := shipperLive(t)
			peer := newSkillPeer(t, "remote")
			switch failure {
			case "unsupported":
				peer.features = []string{nodewire.FeatureJournal}
			case "offline":
				peer.offline.Store(true)
			default:
				peer.failure.Store(failure)
			}
			if failure == "pack" {
				// Map accepts an absolute hidden root but Pack rejects its name.
				hidden := filepath.Join(t.TempDir(), ".hidden")
				if err := os.Symlink(target, hidden); err != nil {
					t.Fatal(err)
				}
				if err := live.Map.Enable(hidden); err != nil {
					t.Fatal(err)
				}
			} else if err := live.Map.Enable("alpha"); err != nil {
				t.Fatal(err)
			}
			shipper := &SkillShipper{Live: live, Nodes: skillRegistry(t, peer)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "canceled" {
				cancel()
			}
			for _, ship := range []func(context.Context) error{
				func(ctx context.Context) error { return shipper.Ship(ctx, peer.name) },
				shipper.ShipAll,
			} {
				err := ship(ctx)
				if err == nil {
					t.Fatal("failed ship returned success")
				}
				if failure == "offline" && !errors.Is(err, ErrSkillsPending) {
					t.Fatalf("lost pending identity: %v", err)
				}
				if failure == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost context error identity: %v", err)
				}
			}
			if failure != "apply" && peer.applies.Load() != 0 {
				t.Fatal("apply reached a node despite an earlier failure")
			}
		})
	}
}

func shipperLive(t *testing.T) (*skills.Live, string) {
	t.Helper()
	root := t.TempDir()
	m, err := skills.Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "clone", "skills", "alpha")
	if err := os.MkdirAll(filepath.Join(target, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"SKILL.md": "alpha instructions", "scripts/helper.sh": "#!/bin/sh\necho helper\n"} {
		if err := os.WriteFile(filepath.Join(target, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(target, "SKILL.md"), filepath.Join(target, "external")); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(root, "source.skills")
	if err := os.MkdirAll(catalog, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(catalog, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := m.AddPath(catalog); err != nil {
		t.Fatal(err)
	}
	live := &skills.Live{Map: m, Dests: []string{filepath.Join(root, "runtime")}}
	if err := live.Apply(); err != nil {
		t.Fatal(err)
	}
	return live, target
}

func assertSavedEnabled(t *testing.T, live *skills.Live) {
	t.Helper()
	reopened, err := skills.Open(skills.DefaultPath(filepath.Dir(live.Dests[0])))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := reopened.Enabled()
	if err != nil || len(refs) != 1 || refs[0].Name != "alpha" {
		t.Fatalf("desired was not saved: refs = %v, err = %v", refs, err)
	}
}

func assertRemoteSkill(t *testing.T, peer *skillPeer, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(peer.dir, "alpha", filepath.FromSlash(name)))
	if err != nil || string(got) != want {
		t.Fatalf("remote %s/%s = %q, err = %v; want %q", peer.name, name, got, err, want)
	}
}

// skillPeer speaks only blob/apply/advert over an in-memory connection.
// The production Registry and wire run unchanged; received bytes are hashed
// and unpacked on the peer side, with no sockets or actual harness processes.
type skillPeer struct {
	name     string
	dir      string
	features []string
	failure  atomic.Value
	applies  atomic.Int32
	offline  atomic.Bool
	dials    atomic.Int32
	t        *testing.T
}

func newSkillPeer(t *testing.T, name string) *skillPeer {
	t.Helper()
	p := &skillPeer{name: name, dir: t.TempDir(), features: []string{nodewire.FeatureJournal, nodewire.FeatureSkills}, t: t}
	p.failure.Store("")
	return p
}

func skillRegistry(t *testing.T, peers ...*skillPeer) *node.Registry {
	t.Helper()
	configs := make(map[string]node.Config, len(peers))
	for _, peer := range peers {
		configs[peer.name] = node.Config{DialContext: peer.dial}
	}
	registry := node.NewRegistry("test-hub", configs)
	t.Cleanup(registry.Close)
	for _, peer := range peers {
		if peer.offline.Load() {
			continue
		}
		if _, err := registry.Advert(t.Context(), peer.name); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func (p *skillPeer) dial(context.Context, string) (net.Conn, error) {
	p.dials.Add(1)
	if p.offline.Load() {
		return nil, errors.New("offline")
	}
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		advert := nodewire.Advert{Node: p.name, Features: p.features}
		if _, err := nodewire.Accept(server, "", advert); err != nil {
			p.t.Errorf("peer handshake: %v", err)
			return
		}
		mux := nodewire.NewMux(server, false)
		defer mux.Close()
		blobs := map[string][]byte{}
		for {
			stream, err := mux.Accept(p.t.Context())
			if err != nil {
				return
			}
			err = p.handle(stream, &advert, blobs)
			code := "0"
			if err != nil {
				code = "1"
			}
			_ = stream.CloseWithReason(nodewire.ExitPrefix + code)
		}
	}()
	p.t.Cleanup(func() {
		client.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			p.t.Error("skill peer did not stop")
		}
	})
	return client, nil
}

func (p *skillPeer) handle(stream *nodewire.Stream, advert *nodewire.Advert, blobs map[string][]byte) error {
	switch req := stream.Request(); req.Kind {
	case nodewire.StreamBlob:
		size, err := nodewire.ReadSize(stream)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(stream, size))
		if err != nil {
			return err
		}
		if p.failure.Load() == "blob" {
			return errors.New("blob refused")
		}
		blobs[strings.TrimPrefix(req.Command, "put ")] = data
	case nodewire.StreamSkills:
		p.applies.Add(1)
		if p.failure.Load() == "apply" {
			return errors.New("apply refused")
		}
		hash := strings.TrimPrefix(req.Command, "apply ")
		data := blobs["skills-"+hash+".tar"]
		if skills.HashOf(data) != hash {
			return errors.New("wrong bundle bytes")
		}
		if _, err := skills.Unpack(data, p.dir); err != nil {
			return err
		}
		advert.Skills = hash
	case nodewire.StreamAdvert:
		return json.NewEncoder(stream).Encode(advert)
	default:
		p.t.Errorf("unexpected stream %q (must not restart or kill tasks)", req.Kind)
		return fmt.Errorf("unsupported stream %q", req.Kind)
	}
	return nil
}
