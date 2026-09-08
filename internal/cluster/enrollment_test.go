package cluster

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/coordination"
)

func TestPeerEnrollmentReviewChangesFailBeforeIssuingIdentity(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	peerAddress, raftAddress := FreeEnrollmentPorts(t)
	request := PeerEnrollmentRequest{Name: "reviewed-peer", PeerAddress: peerAddress, RaftAddress: raftAddress, SourceHost: "127.0.0.1", Level: "restricted"}
	plan, err := peer.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	request.Level = "sealed"
	if _, err := peer.PrepareEnrollment(t.Context(), request, "changed-plan", true); !errors.Is(err, coordination.ErrConflict) {
		t.Fatalf("unreviewed plan change accepted: %v", err)
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
