package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

func TestPeerSSHLocalCheckAdvertisesFullNodeMode(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("Host fixture\nHostName fixture.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := filepath.Join(home, "commands")
	if err := os.Mkdir(commands, 0o700); err != nil {
		t.Fatal(err)
	}
	// Avoid all network connections: multiplex lifecycle calls succeed locally,
	// and only the read-only production probe executes in this fixture HOME.
	script := `#!/bin/sh
for value do
  case "$value" in -M|-O) exit 0;; esac
  final="$value"
done
[ "$final" = 'sh -s' ] || exit 91
SSH_CONNECTION='127.0.0.1 10000 127.0.0.1 22'
export SSH_CONNECTION
exec /bin/sh -s
`
	if err := os.WriteFile(filepath.Join(commands, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", commands+":/usr/bin:/bin")
	peer := &clusterPeer{uiToken: strings.Repeat("a", 32), uiURL: "http://127.0.0.1:7700"}
	t.Cleanup(func() {
		if peer.localSSH != nil {
			_ = peer.localSSH.Close()
		}
	})
	req := httptest.NewRequest(http.MethodPost, "/console/ssh/check", strings.NewReader(`{"alias":"fixture"}`))
	req.Header.Set("Authorization", "Bearer "+peer.uiToken)
	response := httptest.NewRecorder()
	peer.serveSSHLocal(response, req)
	var check sshconnect.CheckResult
	if err := json.Unmarshal(response.Body.Bytes(), &check); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || check.InstallationMode != sshconnect.InstallPeer || !check.Reachable {
		t.Fatalf("desktop check did not advertise peer installation: %d %s", response.Code, response.Body.String())
	}
}

type sshEnrollmentFixture struct {
	prepares, completes int
	plan                PeerEnrollmentPlan
	packageValue        peerJoinPackage
	completeResult      PeerEnrollmentResult
	completeError       error
	prepareError        error
}

func (f *sshEnrollmentFixture) PreviewPeerEnrollment(context.Context, PeerEnrollmentRequest) (PeerEnrollmentPlan, error) {
	return f.plan, nil
}

func (f *sshEnrollmentFixture) PreparePeerEnrollment(_ context.Context, request PeerEnrollmentRequest, id string) (PeerEnrollmentPackage, error) {
	if request.ExpectedPlanHash != f.plan.ReviewID {
		return PeerEnrollmentPackage{}, errors.New("missing reviewed plan hash")
	}
	f.prepares++
	pkg := f.packageValue
	pkg.OperationID = id
	raw, _ := json.Marshal(pkg)
	return PeerEnrollmentPackage{NodeID: pkg.NodeID, Payload: raw, Plan: f.plan}, f.prepareError
}

func (f *sshEnrollmentFixture) CompletePeerEnrollment(_ context.Context, id string) (PeerEnrollmentResult, error) {
	f.completes++
	out := f.completeResult
	out.OperationID = id
	return out, f.completeError
}

func (f *sshEnrollmentFixture) PeerEnrollmentStatus(_ context.Context, id string) (PeerEnrollmentResult, error) {
	out := f.completeResult
	out.OperationID = id
	return out, nil
}

func peerSSHFixture(t *testing.T) (peerSSHBackend, *sshEnrollmentFixture, sshconnect.InstallRequest, sshconnect.CheckResult) {
	t.Helper()
	request := sshconnect.InstallRequest{Alias: "dev", Name: "remote", Addr: "192.0.2.5:7701", Level: "restricted"}
	resolved := PeerEnrollmentRequest{Alias: request.Alias, Name: request.Name, PeerAddress: request.Addr, RaftAddress: "192.0.2.5:7702", SourceHost: "192.0.2.1", Level: "restricted"}
	source := coordination.Member{NodeID: "local", Address: "192.0.2.1:7712", APIAddress: "https://192.0.2.1:7711"}
	fixture := &sshEnrollmentFixture{plan: PeerEnrollmentPlan{Request: resolved, ClusterID: "test-cluster", Source: source, Seeds: []coordination.Member{source}, UpdateSourceAddress: true, Effects: []string{"更新本机跨机地址为192.0.2.1，随后验证所有节点独立互联"}}, packageValue: peerJoinPackage{Version: 1, ClusterID: "test-cluster", NodeID: "node-new", Name: "remote", StorageLevel: "restricted", PeerAdvertise: resolved.PeerAddress, RaftAdvertise: resolved.RaftAddress, Seeds: []coordination.Member{source}, PrivateKey: []byte("private-leaf-key"), OwnerToken: "private-owner-token", WorkerToken: "private-worker-token"}, completeResult: PeerEnrollmentResult{NodeID: "node-new", Name: "remote", Phase: "ready", Ready: true}}
	fixture.plan.ReviewID = fixture.plan.reviewHash()
	request.ApprovedReviewID = fixture.plan.ReviewID
	binary := InstallBinaryFixture(t)
	backend := peerSSHBackend{enrollment: fixture, findBinary: func(string) (string, bool) { return binary, true }}
	check := SshCheckFixture()
	check.Tools = append(check.Tools, sshconnect.Tool{Name: "base64", Available: true})
	return backend, fixture, request, check
}

func TestPeerSSHPreviewAndCommitKeepEnrollmentBundlePrivate(t *testing.T) {
	b, fixture, request, check := peerSSHFixture(t)
	preview, err := b.Preview(t.Context(), request, check)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.prepares != 0 || preview.ReviewID == "" || preview.Binary == nil {
		t.Fatal("preview prepared credentials or omitted review identity")
	}
	if !strings.Contains(preview.Script, "peer-import") || !strings.Contains(preview.Script, nodebootstrap.PreviewJoinPackage) {
		t.Fatal("preview is not a full peer installation")
	}
	encoded, _ := json.Marshal(preview)
	if strings.Contains(string(encoded), "private-") {
		t.Fatal("preview exposed a join credential")
	}
	id := strings.Repeat("a", 48)
	registration, err := b.Register(t.Context(), request, check, id)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.prepares != 1 || registration.Name != "remote" || registration.Token == "" || registration.ReviewID != preview.ReviewID {
		t.Fatalf("registration = %#v", registration)
	}
	private, err := base64.StdEncoding.DecodeString(registration.Token)
	var decoded peerJoinPackage
	decodeErr := json.Unmarshal(private, &decoded)
	if err != nil || decodeErr != nil || string(decoded.PrivateKey) != "private-leaf-key" {
		t.Fatal("registration did not carry the private package only to stdin")
	}
	normalized := strings.ReplaceAll(registration.Script, registration.Token, sshconnect.PreviewToken)
	normalized = strings.ReplaceAll(normalized, id, nodebootstrap.PreviewUploadID)
	if normalized != preview.Script {
		t.Fatal("prepared peer script differs from reviewed script")
	}
	if err := b.VerifyRegistration(t.Context(), "remote", id); err != nil || fixture.completes != 1 {
		t.Fatalf("membership completion = %v", err)
	}
}

func TestPeerSSHRejectsDifferentPackageNetworkAndIncompleteMembership(t *testing.T) {
	b, fixture, request, check := peerSSHFixture(t)
	fixture.packageValue.RaftAdvertise = "192.0.2.99:9900"
	if _, err := b.Register(t.Context(), request, check, strings.Repeat("b", 48)); err == nil {
		t.Fatal("unreviewed network package accepted")
	}
	fixture.completeResult.Ready = false
	fixture.completeResult.Phase = "synchronizing"
	fixture.completeError = errors.New("not yet synchronized")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := b.VerifyRegistration(ctx, "remote", strings.Repeat("b", 48)); err == nil {
		t.Fatal("unready peer treated as installed")
	}
}

func TestPeerSSHRecoverUsesStoredOperationWithoutPreparingAnotherNode(t *testing.T) {
	b, fixture, _, _ := peerSSHFixture(t)
	id := strings.Repeat("c", 48)
	result, err := b.ResumeRegistration(t.Context(), id)
	if err != nil || !result.Connected || result.NodeID != "node-new" || fixture.prepares != 0 || fixture.completes != 1 {
		t.Fatalf("unsafe recovery = %#v %v", result, err)
	}
}

func TestPeerSSHFailureAfterPreparingIdentityRetainsOperation(t *testing.T) {
	b, fixture, request, check := peerSSHFixture(t)
	fixture.prepareError = errors.New("source network update was not confirmed")
	registration, err := b.Register(t.Context(), request, check, strings.Repeat("f", 48))
	if err == nil || registration.Name != "remote" || registration.NodeID != "node-new" || registration.Script != "" || registration.Token != "" {
		t.Fatalf("partial preparation lost receipt or returned install script: %#v %v", registration, err)
	}
}

func TestPeerSSHRejectsFreshPlanBeforeAnyPrepareSideEffect(t *testing.T) {
	b, fixture, request, check := peerSSHFixture(t)
	approved := request.ApprovedReviewID
	fixture.plan.Request.SourceHost = "192.0.2.99"
	fixture.plan.Source.APIAddress = "https://192.0.2.99:7711"
	fixture.plan.ReviewID = fixture.plan.reviewHash()
	if fixture.plan.ReviewID == approved {
		t.Fatal("fixture did not change review ID")
	}
	if _, err := b.Register(t.Context(), request, check, strings.Repeat("9", 48)); err == nil {
		t.Fatal("newly generated plan replaced original approval")
	}
	if fixture.prepares != 0 {
		t.Fatal("unreviewed network change reached identity/network preparation")
	}
}
