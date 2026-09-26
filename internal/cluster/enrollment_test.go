package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/cluster/clustertest"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/i18n"
)

func TestPeerEnrollmentReviewChangesFailBeforeIssuingIdentity(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	ports := clustertest.HoldEnrollmentPorts(t)
	peerAddress, raftAddress := ports.Peer, ports.Raft
	request := PeerEnrollmentRequest{Name: "reviewed-peer", PeerAddress: peerAddress, RaftAddress: raftAddress, Level: "restricted"}
	plan, err := peer.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	request.Level = "sealed"
	// The refusal wraps ErrConflict in every language, and its sentence
	// has nothing left unformatted.
	for _, locale := range []i18n.Locale{i18n.LocaleZH, i18n.LocaleEN} {
		_, err := peer.PrepareEnrollment(i18n.WithLocale(t.Context(), locale), request, "changed-plan", true)
		if !errors.Is(err, coordination.ErrConflict) {
			t.Fatalf("%s: unreviewed plan change accepted: %v", locale, err)
		}
		if strings.Contains(err.Error(), "%!") || !strings.HasPrefix(err.Error(), coordination.ErrConflict.Error()+": ") {
			t.Errorf("%s: refused with %q, want ErrConflict's text followed by the reason", locale, err)
		}
	}
	if _, err := peer.loadEnrollment("changed-plan"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unreviewed change created a node identity")
	}
}

func TestPeerEnrollmentPhysicalIdentityIsStableAndNotInstallationSpecific(t *testing.T) {
	first, err := physicalFailureDomain()
	if err != nil {
		t.Skip("OS machine identity unavailable")
	}
	second, err := physicalFailureDomain()
	if err != nil || first != second || len(first) != len("machine-")+64 {
		t.Fatal("physical failure domain is not a stable opaque machine hash")
	}
}

func TestPeerEnrollmentTakesADisplayNameAndAWorkspaceForTheMachine(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	ports := clustertest.HoldEnrollmentPorts(t)
	peerAddress, raftAddress := ports.Peer, ports.Raft
	// The remote account's home is where "~/" lands at import time.
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "work", "steve")
	request := PeerEnrollmentRequest{Name: "  办公 Linux 盒子 ", PeerAddress: peerAddress, RaftAddress: raftAddress, Level: "restricted"}
	plan, err := peer.PreviewEnrollment(t.Context(), request, true)
	if err != nil || plan.Request.Name != "办公 Linux 盒子" || plan.Request.WorkspaceDir != "~/steve-workspace" {
		t.Fatalf("a display name and a default workspace: %+v %v", plan.Request, err)
	}
	if !strings.Contains(strings.Join(plan.Effects, "\n"), "~/steve-workspace") {
		t.Fatalf("the reviewed effects do not name the workspace: %v", plan.Effects)
	}
	for _, bad := range []PeerEnrollmentRequest{{Name: "", PeerAddress: peerAddress, RaftAddress: raftAddress, Level: "restricted"}, {Name: "x", WorkspaceDir: "relative", PeerAddress: peerAddress, RaftAddress: raftAddress, Level: "restricted"}, {Name: "x", WorkspaceDir: "~/../etc", PeerAddress: peerAddress, RaftAddress: raftAddress, Level: "restricted"}} {
		if _, err := peer.PreviewEnrollment(t.Context(), bad, true); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	request = plan.Request
	request.WorkspaceDir = "~/work/steve"
	plan, err = peer.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	prepared, err := peer.PrepareEnrollment(t.Context(), request, "workspace-plan", true)
	if err != nil {
		t.Fatal(err)
	}
	var bundle PeerJoinPackage
	if err := json.Unmarshal(prepared.Payload, &bundle); err != nil || bundle.WorkspaceDir != "~/work/steve" || bundle.Name != "办公 Linux 盒子" {
		t.Fatalf("the join package does not carry the workspace: %+v %v", bundle, err)
	}
	stateDir := filepath.Join(t.TempDir(), "peer-state")
	if _, err := ImportPeerPackage(prepared.Payload, stateDir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("import did not create the workspace: %v", err)
	}
	var app struct {
		Projects map[string]struct {
			Home struct {
				Path string `json:"path"`
			} `json:"home"`
		} `json:"projects"`
	}
	raw, _ := os.ReadFile(filepath.Join(stateDir, "config.json"))
	if err := json.Unmarshal(raw, &app); err != nil || app.Projects["workspace"].Home.Path != workspace {
		t.Fatalf("the default project does not live in the workspace: %s %v", raw, err)
	}
	var worker struct {
		WorkspaceRoot string `json:"workspace_root"`
	}
	raw, _ = os.ReadFile(filepath.Join(stateDir, "cluster", "node.json"))
	if err := json.Unmarshal(raw, &worker); err != nil || worker.WorkspaceRoot != workspace {
		t.Fatalf("the executor does not run in the workspace: %s %v", raw, err)
	}
}

// A new node is set up in Chinese, the language its configuration names,
// and the import refuses a workspace in that language: it is that node's
// owner who reads the refusal, on that machine.
func TestPeerImportRefusesAWorkspaceInTheLanguageOfTheNodeItSetsUp(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	ports := clustertest.HoldEnrollmentPorts(t)
	request := PeerEnrollmentRequest{Name: "box", PeerAddress: ports.Peer, RaftAddress: ports.Raft, Level: "restricted"}
	plan, err := peer.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	prepared, err := peer.PrepareEnrollment(t.Context(), request, "language-plan", true)
	if err != nil {
		t.Fatal(err)
	}
	// The preview already refuses a relative workspace, so the only way one
	// reaches the importing machine is in a package changed on the way.
	var bundle PeerJoinPackage
	if err := json.Unmarshal(prepared.Payload, &bundle); err != nil {
		t.Fatal(err)
	}
	bundle.WorkspaceDir = "relative"
	payload, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ImportPeerPackage(payload, filepath.Join(t.TempDir(), "peer-state"))
	if want := i18n.New(i18n.LocaleZH).T(i18n.DesktopWorkspaceRelative); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("import refusal = %v, want it to say %q", err, want)
	}
}

func TestAbandonPeerEnrollmentArchivesTheRecordAndRefusesAJoinedNode(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	ports := clustertest.HoldEnrollmentPorts(t)
	peerAddress, raftAddress := ports.Peer, ports.Raft
	request := PeerEnrollmentRequest{Name: "abandoned", PeerAddress: peerAddress, RaftAddress: raftAddress, Level: "restricted"}
	plan, err := peer.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	if _, err := peer.PrepareEnrollment(t.Context(), request, "abandon-me", true); err != nil {
		t.Fatal(err)
	}
	if err := peer.AbandonPeerEnrollment(t.Context(), "abandon-me"); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.loadEnrollment("abandon-me"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned record still answers: %v", err)
	}
	if err := peer.AbandonPeerEnrollment(t.Context(), "abandon-me"); err == nil {
		t.Fatal("abandoning twice reported success")
	}
	// The same ports can be planned again: nothing of the attempt remains.
	if _, err := peer.PrepareEnrollment(t.Context(), request, "second-try", true); err != nil {
		t.Fatal(err)
	}
	record, _ := peer.loadEnrollment("second-try")
	record.Ready, record.Phase = true, "ready"
	if err := peer.saveEnrollment(record); err != nil {
		t.Fatal(err)
	}
	if err := peer.AbandonPeerEnrollment(t.Context(), "second-try"); !errors.Is(err, ErrEnrollmentJoined) {
		t.Fatalf("a joined node was abandoned from the enrollment dialog: %v", err)
	}
}

func TestAbandonPeerEnrollmentRemovesTheMemberAFailedJoinLeftBehind(t *testing.T) {
	root := ClusterPeerTestDir(t)
	options, _ := testPeerOptions(t, filepath.Join(root, "source"), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	source := StartTestPeer(t, options)
	WaitPeerReady(t, source)
	// A machine that got as far as joining but never finished enrolling
	// stays in the cluster; the record still points at it.
	second, _ := testPeerOptions(t, filepath.Join(root, "second"), source)
	second.Activate = func(context.Context, Activation, func(PeerApplicationEndpoint) error) (Deactivate, error) {
		return nil, nil
	}
	orphan := StartTestPeer(t, second)
	if _, err := source.Join(t.Context(), coordination.JoinRequest{ID: "join-orphan", Actor: "owner", Member: coordination.Member{NodeID: orphan.Config.NodeID, Address: orphan.Config.RaftAddress, APIAddress: orphan.Config.PeerURL, Voting: true}}); err != nil {
		t.Fatal(err)
	}
	ports := clustertest.HoldEnrollmentPorts(t)
	peerAddress, raftAddress := ports.Peer, ports.Raft
	request := PeerEnrollmentRequest{Name: "orphaned", PeerAddress: peerAddress, RaftAddress: raftAddress, Level: "restricted"}
	plan, err := source.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	if _, err := source.PrepareEnrollment(t.Context(), request, "orphan-op", true); err != nil {
		t.Fatal(err)
	}
	record, _ := source.loadEnrollment("orphan-op")
	record.NodeID, record.Phase = orphan.Config.NodeID, "synchronizing"
	if err := source.saveEnrollment(record); err != nil {
		t.Fatal(err)
	}
	state, _ := source.Runtime.Load().ReadState(t.Context())
	if _, member := state.Members[orphan.Config.NodeID]; !member {
		t.Fatal("fixture: the orphan is not a member")
	}
	if err := source.AbandonPeerEnrollment(t.Context(), "orphan-op"); err != nil {
		t.Fatal(err)
	}
	state, _ = source.Runtime.Load().ReadState(t.Context())
	if _, member := state.Members[orphan.Config.NodeID]; member {
		t.Fatal("abandoning left the orphan member in the cluster")
	}
	if _, err := source.loadEnrollment("orphan-op"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned record still answers: %v", err)
	}
	if _, err := source.PreviewEnrollment(t.Context(), PeerEnrollmentRequest{Name: "again", PeerAddress: orphan.Config.PeerAddress, RaftAddress: orphan.Config.RaftAddress, Level: "restricted"}, true); err != nil {
		t.Fatalf("the orphan's ports are still taken: %v", err)
	}
}
