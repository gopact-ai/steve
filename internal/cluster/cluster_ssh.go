package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

func (p *Peer) serveSSHLocal(w http.ResponseWriter, r *http.Request) {
	p.Mu.Lock()
	if p.closing {
		p.Mu.Unlock()
		http.Error(w, p.text.For(r.Context()).T(i18n.ClusterNodeClosing), http.StatusServiceUnavailable)
		return
	}
	if p.localSSH == nil {
		p.localSSH = sshconnect.New(sshconnect.Options{Backend: peerSSHBackend{peer: p}, InstallationMode: sshconnect.InstallPeer, Text: p.text})
	}
	service := p.localSSH
	p.Mu.Unlock()
	handler, err := p.Options.SSHHandler(peerSSHService{service}, p.UIToken, p.UiURL)
	if err != nil {
		HTTPError(w, err)
		return
	}
	handler.ServeHTTP(w, r)
}

type peerSSHService struct{ service *sshconnect.Service }

func (s peerSSHService) SSHDiscover(ctx context.Context) (sshconnect.Discovery, error) {
	return s.service.Discover(ctx)
}
func (s peerSSHService) SSHCheck(ctx context.Context, alias string) (sshconnect.CheckResult, error) {
	return s.service.Check(ctx, alias)
}
func (s peerSSHService) SSHPlan(ctx context.Context, req sshconnect.InstallRequest) (sshconnect.InstallPlan, error) {
	return s.service.Plan(ctx, req)
}
func (s peerSSHService) SSHStatus(ctx context.Context, id string) (sshconnect.InstallResult, error) {
	return s.service.Status(ctx, id)
}
func (s peerSSHService) SSHCommit(ctx context.Context, id string) (sshconnect.InstallResult, error) {
	return s.service.Commit(ctx, id)
}
func (s peerSSHService) SSHAbandon(ctx context.Context, id string) error {
	return s.service.Abandon(ctx, id)
}
func (s peerSSHService) SSHBrowse(ctx context.Context, req sshconnect.BrowseRequest) (sshconnect.Listing, error) {
	return s.service.Browse(ctx, req)
}
func (s peerSSHService) SSHUpgrade(ctx context.Context, nodeID string) (sshconnect.InstallResult, error) {
	return s.service.Upgrade(ctx, nodeID)
}
func (s peerSSHService) SSHUpgradeStatus(ctx context.Context, nodeID string) (sshconnect.InstallResult, error) {
	return s.service.UpgradeStatus(ctx, nodeID)
}

type peerEnrollmentService interface {
	PreviewPeerEnrollment(context.Context, PeerEnrollmentRequest) (PeerEnrollmentPlan, error)
	PreparePeerEnrollment(context.Context, PeerEnrollmentRequest, string) (PeerEnrollmentPackage, error)
	CompletePeerEnrollment(context.Context, string) (PeerEnrollmentResult, error)
	PeerEnrollmentStatus(context.Context, string) (PeerEnrollmentResult, error)
	AbandonPeerEnrollment(context.Context, string) error
	OpenEnrollmentLink(context.Context, string) error
}

// peerSSHBackend is called by sshconnect.Service, which has put the
// language of whoever started the enrollment on every ctx it passes; what
// the backend says goes into that person's installation record.
type peerSSHBackend struct {
	peer *Peer
	// Tests supply a bounded enrollment fixture and a local verified package.
	enrollment peerEnrollmentService
	findBinary func(string) (string, bool)
	// stallLimit is how long an enrollment may sit in one phase before the
	// wait gives up; zero means the default.
	stallLimit time.Duration
}

// peerStallLimit is how long enrollment may stay in one phase. Progress
// resets it, so a slow but moving enrollment is never cut off.
const peerStallLimit = 5 * time.Minute

func (b peerSSHBackend) service() peerEnrollmentService {
	if b.enrollment != nil {
		return b.enrollment
	}
	return b.peer
}

func sshPeerEnrollmentRequest(req sshconnect.InstallRequest, hubRoute coordination.Route) PeerEnrollmentRequest {
	return PeerEnrollmentRequest{Alias: req.Alias, Name: req.Name, PeerAddress: req.Addr, RaftAddress: req.RaftAddr, HubRoute: hubRoute, Level: req.Level, WorkspaceDir: req.WorkspaceDir}
}

// hubRouteFor picks, from the loopback ports the machine reported free,
// the two at which this node's listeners will appear there. Ports the
// machine's own node will listen on are passed over, since that node
// binds every interface. The choice is a function of the check alone, so
// the plan a user reviews and the one that is registered agree.
func hubRouteFor(text i18n.Catalog, req sshconnect.InstallRequest, check sshconnect.CheckResult) (coordination.Route, *sshconnect.Step) {
	taken := map[string]bool{}
	if _, port, err := net.SplitHostPort(req.Addr); err == nil {
		taken[port] = true
		if n, err := strconv.Atoi(port); err == nil {
			taken[strconv.Itoa(n+1)] = true
		}
	}
	if _, port, err := net.SplitHostPort(req.RaftAddr); err == nil {
		taken[port] = true
	}
	var picked []string
	for _, port := range check.FreeLoopbackPorts {
		if number := strconv.Itoa(port); !taken[number] {
			picked = append(picked, net.JoinHostPort("127.0.0.1", number))
		}
		if len(picked) == 2 {
			return coordination.Route{Raft: picked[0], API: picked[1]}, nil
		}
	}
	if check.FreeLoopbackPorts == nil {
		return coordination.Route{}, &sshconnect.Step{ID: "peer_link", Status: "blocked", Message: text.T(i18n.ClusterTunnelPortsUnknown), Suggestion: text.T(i18n.ClusterTunnelPortsUnknownFix)}
	}
	return coordination.Route{}, &sshconnect.Step{ID: "peer_link", Status: "blocked", Message: text.T(i18n.ClusterTunnelPortsBusy, sshconnect.FirstLoopbackPort, sshconnect.LastLoopbackPort), Suggestion: text.T(i18n.ClusterTunnelPortsBusyFix)}
}

func peerPlanHash(plan PeerEnrollmentPlan) string {
	return plan.reviewHash()
}

func (b peerSSHBackend) prepare(ctx context.Context, req sshconnect.InstallRequest, check sshconnect.CheckResult) (sshconnect.Template, nodebootstrap.PeerSpec, PeerEnrollmentPlan, error) {
	text := i18n.FromContext(ctx)
	template := sshconnect.Template{Steps: []sshconnect.Step{}, Effects: []string{}}
	hubRoute, blocked := hubRouteFor(text, req, check)
	if blocked != nil {
		template.Steps = append(template.Steps, *blocked)
		return template, nodebootstrap.PeerSpec{}, PeerEnrollmentPlan{}, nil
	}
	plan, err := b.service().PreviewPeerEnrollment(ctx, sshPeerEnrollmentRequest(req, hubRoute))
	if err != nil {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "peer_network", Status: "blocked", Message: err.Error(), Suggestion: text.T(i18n.ClusterPlanAddressFix)})
		return template, nodebootstrap.PeerSpec{}, plan, nil
	}
	template.ReviewID = plan.ReviewID
	if template.ReviewID == "" || template.ReviewID != peerPlanHash(plan) {
		return template, nodebootstrap.PeerSpec{}, plan, errors.New(text.T(i18n.ClusterPlanReviewMissing))
	}
	template.Effects = append(template.Effects, plan.Effects...)
	template.Effects = append(template.Effects, text.T(i18n.ClusterPlanUploadEffect))
	template.Steps = append(template.Steps, sshconnect.Step{ID: "workspace", Status: "ready", Message: text.T(i18n.ClusterPlanWorkspace, plan.Request.WorkspaceDir)})
	find := b.findBinary
	if find == nil {
		find = desktop.BundledPeerBinary
	}
	path, ok := find(check.OS + "/" + check.Arch)
	if !ok {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "binary", Status: "blocked", Message: text.T(i18n.ClusterPlanNoPackage), Suggestion: text.T(i18n.ClusterPlanNoPackageFix, check.OS+"/"+check.Arch)})
		return template, nodebootstrap.PeerSpec{}, plan, nil
	}
	metadata, err := nodebootstrap.InspectBinary(text, path)
	if err != nil {
		return template, nodebootstrap.PeerSpec{}, plan, err
	}
	if metadata.OS != check.OS || metadata.Arch != check.Arch {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "binary", Status: "blocked", Message: text.T(i18n.ClusterPlanPackageMismatch), Suggestion: text.T(i18n.ClusterPlanPackageMismatchFix)})
	}
	if !check.HasTool("sha256sum") && !check.HasTool("shasum") {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "checksum", Status: "blocked", Message: text.T(i18n.AdminSSHNoChecksum), Suggestion: text.T(i18n.AdminSSHNoChecksumFix)})
	}
	if !check.HasTool("base64") {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "base64", Status: "blocked", Message: text.T(i18n.ClusterPlanNoBase64), Suggestion: text.T(i18n.ClusterPlanNoBase64Fix)})
	}
	template.Binary, template.BinaryPath = &metadata, path
	template.Steps = append(template.Steps, sshconnect.Step{ID: "peer_network", Status: "ready", Message: text.T(i18n.ClusterPlanPorts, plan.Request.PeerAddress, plan.Request.RaftAddress)})
	template.Steps = append(template.Steps, sshconnect.Step{ID: "peer_link", Status: "ready", Message: text.T(i18n.ClusterPlanTunnel, hubRoute.Raft, hubRoute.API), Suggestion: text.T(i18n.ClusterPlanTunnelHint)})
	spec := nodebootstrap.PeerSpec{OS: metadata.OS, Arch: metadata.Arch, SHA256: metadata.SHA256, UploadID: nodebootstrap.PreviewUploadID, JoinPackage: nodebootstrap.PreviewJoinPackage}
	template.Script, err = nodebootstrap.BuildPeer(spec)
	return template, spec, plan, err
}

func (b peerSSHBackend) Preview(ctx context.Context, req sshconnect.InstallRequest, check sshconnect.CheckResult) (sshconnect.Template, error) {
	template, _, _, err := b.prepare(ctx, req, check)
	return template, err
}

func (b peerSSHBackend) Register(ctx context.Context, req sshconnect.InstallRequest, check sshconnect.CheckResult, installID string) (sshconnect.Registration, error) {
	template, spec, plan, err := b.prepare(ctx, req, check)
	if err != nil {
		return sshconnect.Registration{}, err
	}
	if req.ApprovedReviewID == "" || template.ReviewID != req.ApprovedReviewID {
		return sshconnect.Registration{}, errors.New(i18n.FromContext(ctx).T(i18n.ClusterPlanChanged))
	}
	for _, step := range template.Steps {
		if step.Status == "blocked" {
			return sshconnect.Registration{}, errors.New(step.Message)
		}
	}
	preparedRequest := plan.Request
	preparedRequest.ExpectedPlanHash = req.ApprovedReviewID
	registered, err := b.service().PreparePeerEnrollment(ctx, preparedRequest, installID)
	result := sshconnect.Registration{ReviewID: template.ReviewID, NodeID: registered.NodeID}
	if registered.NodeID != "" {
		result.Name = plan.Request.Name
	}
	if err != nil {
		return result, err
	}
	var bundle PeerJoinPackage
	if registered.Plan.ReviewID != plan.ReviewID || peerPlanHash(registered.Plan) != plan.ReviewID || json.Unmarshal(registered.Payload, &bundle) != nil || bundle.OperationID != installID || bundle.NodeID != registered.NodeID || bundle.ClusterID != plan.ClusterID || bundle.Name != plan.Request.Name || bundle.WorkspaceDir != plan.Request.WorkspaceDir || bundle.StorageLevel != plan.Request.Level || bundle.PeerAdvertise != plan.Request.PeerAddress || bundle.RaftAdvertise != plan.Request.RaftAddress || !reflect.DeepEqual(bundle.Seeds, plan.Seeds) || !routesToOneOf(bundle.Routes, plan.Seeds, plan.Request.HubRoute) {
		return result, errors.New(i18n.FromContext(ctx).T(i18n.ClusterPackageMismatch))
	}
	result.Token = base64.StdEncoding.EncodeToString(registered.Payload)
	spec.JoinPackage, spec.UploadID = result.Token, installID
	result.Script, err = nodebootstrap.BuildPeer(spec)
	return result, err
}

// routesToOneOf says the package routes exactly one member, a seed, at
// the reviewed tunnel ports: the node preparing it names itself, and the
// review saw only the ports.
func routesToOneOf(routes map[string]coordination.Route, seeds []coordination.Member, want coordination.Route) bool {
	for nodeID, route := range routes {
		if route != want {
			return false
		}
		known := false
		for _, seed := range seeds {
			known = known || seed.NodeID == nodeID
		}
		if !known {
			return false
		}
	}
	return len(routes) == 1
}

// Link brings up the SSH session the enrolled machine and this node talk
// through, before anything is installed on the machine.
func (b peerSSHBackend) Link(ctx context.Context, installID string, _ sshconnect.Registration) error {
	return b.service().OpenEnrollmentLink(ctx, installID)
}

func (b peerSSHBackend) Verify(ctx context.Context, name string) error {
	return errors.New(i18n.FromContext(ctx).T(i18n.ClusterVerifyNeedsOperation))
}

func (b peerSSHBackend) VerifyRegistration(ctx context.Context, name, id string) error {
	text := i18n.FromContext(ctx)
	limit := b.stallLimit
	if limit <= 0 {
		limit = peerStallLimit
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var last PeerEnrollmentResult
	advanced, reported := time.Now(), map[string]bool{}
	for {
		if ctx.Err() != nil {
			why := text.T(i18n.ClusterInstalledWaitLimit)
			if errors.Is(ctx.Err(), context.Canceled) {
				why = text.T(i18n.ClusterInstalledWaitInterrupted)
			}
			return peerWaitStopped(text, last, why)
		}
		result, err := b.service().CompletePeerEnrollment(ctx, id)
		// Progress is a new phase or something new said within one; the same
		// failures repeating, however many of them alternate, is a stall.
		if result.Phase != "" && result.Phase != last.Phase {
			advanced, reported = time.Now(), map[string]bool{}
			sshconnect.Report(ctx, text.T(i18n.ClusterPhaseLog, peerPhaseText(text, result.Phase)))
		}
		if result.Error != "" && !reported[result.Error] {
			reported[result.Error], advanced = true, time.Now()
			sshconnect.Report(ctx, text.T(i18n.ClusterProblemLog, result.Error))
		}
		last = result
		if err == nil && result.OperationID == id && result.Name == name && result.Ready && result.Phase == "ready" {
			return nil
		}
		if result.OperationID != "" && result.OperationID != id {
			return errors.New(text.T(i18n.ClusterVerifyOtherOperation))
		}
		if result.Name != "" && result.Name != name {
			return errors.New(text.T(i18n.ClusterVerifyOtherNode))
		}
		if err == nil && result.Ready {
			return errors.New(text.T(i18n.ClusterVerifyNoReady))
		}
		if result.Phase != "awaiting_peer" && result.Phase != "synchronizing" && result.Phase != "registering_worker" && result.Phase != "joined" {
			return sshconnect.Fail(text, "peer_membership", "peer_not_ready", text.T(i18n.ClusterJoinIncomplete), text.T(i18n.ClusterJoinIncompleteFix))
		}
		if time.Since(advanced) > limit {
			return peerWaitStopped(text, last, text.T(i18n.ClusterInstalledStalled, peerPhaseText(text, last.Phase), limit))
		}
		select {
		case <-ctx.Done():
			continue
		case <-ticker.C:
		}
	}
}

// peerWaitStopped explains a wait that ended without the node joining:
// why, which names the phase it stopped in, and the last thing that went
// wrong there.
func peerWaitStopped(text i18n.Catalog, last PeerEnrollmentResult, why string) *sshconnect.StepError {
	suggestion := text.T(i18n.ClusterWaitStoppedFix)
	if last.Error != "" {
		suggestion = text.T(i18n.ClusterWaitStoppedLastFix, last.Error)
	}
	return sshconnect.Fail(text, "peer_membership", "peer_not_ready", why, suggestion)
}

// peerPhaseText names an enrollment phase for the installation log.
func peerPhaseText(text i18n.Catalog, phase string) string {
	switch phase {
	case "awaiting_peer":
		return text.T(i18n.ClusterPhaseAwaitingPeer)
	case "synchronizing":
		return text.T(i18n.ClusterPhaseSynchronizing)
	case "registering_worker":
		return text.T(i18n.ClusterPhaseRegisteringWorker)
	case "joined":
		return text.T(i18n.ClusterPhaseJoined)
	case "ready":
		return text.T(i18n.ClusterPhaseReady)
	default:
		return phase
	}
}

// AbandonRegistration withdraws an enrollment; the two refusals a user can
// act on are reported as findings rather than service failures.
func (b peerSSHBackend) AbandonRegistration(ctx context.Context, id string) error {
	text := i18n.FromContext(ctx)
	err := b.service().AbandonPeerEnrollment(ctx, id)
	switch {
	case errors.Is(err, ErrEnrollmentGone):
		return sshconnect.Fail(text, "planning", "unknown_plan", text.T(i18n.ClusterEnrollmentGone), text.T(i18n.ClusterAbandonGoneFix))
	case errors.Is(err, ErrEnrollmentJoined):
		return sshconnect.Fail(text, "peer_membership", "already_joined", text.T(i18n.ClusterEnrollmentJoined), text.T(i18n.ClusterAbandonJoinedFix))
	}
	return err
}

func (b peerSSHBackend) ResumeRegistration(ctx context.Context, id string) (sshconnect.InstallResult, error) {
	text := i18n.FromContext(ctx)
	record, err := b.service().PeerEnrollmentStatus(ctx, id)
	if err != nil {
		return sshconnect.InstallResult{}, sshconnect.Fail(text, "planning", "unknown_plan", text.T(i18n.ClusterResumeUnknown), text.T(i18n.ClusterResumeUnknownFix))
	}
	result := sshconnect.InstallResult{PlanID: id, Name: record.Name, NodeID: record.NodeID, Registered: true, Status: "needs_attention", Steps: []sshconnect.Step{}}
	err = b.VerifyRegistration(ctx, record.Name, id)
	if err != nil {
		var step *sshconnect.StepError
		if errors.As(err, &step) {
			result.Steps = append(result.Steps, sshconnect.Step{ID: step.Stage, Status: "blocked", Message: step.Message, Suggestion: step.Suggestion})
		}
		return result, err
	}
	result.Connected, result.Status = true, "connected"
	result.Steps = append(result.Steps, sshconnect.Step{ID: "peer_membership", Status: "ready", Message: text.T(i18n.ClusterResumeReady)})
	return result, nil
}

type SSHControl interface {
	SSHDiscover(context.Context) (sshconnect.Discovery, error)
	SSHCheck(context.Context, string) (sshconnect.CheckResult, error)
	SSHPlan(context.Context, sshconnect.InstallRequest) (sshconnect.InstallPlan, error)
	SSHCommit(context.Context, string) (sshconnect.InstallResult, error)
	SSHStatus(context.Context, string) (sshconnect.InstallResult, error)
	SSHAbandon(context.Context, string) error
	SSHBrowse(context.Context, sshconnect.BrowseRequest) (sshconnect.Listing, error)
	SSHUpgrade(context.Context, string) (sshconnect.InstallResult, error)
	SSHUpgradeStatus(context.Context, string) (sshconnect.InstallResult, error)
}
