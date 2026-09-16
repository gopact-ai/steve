package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

func (p *Peer) serveSSHLocal(w http.ResponseWriter, r *http.Request) {
	p.Mu.Lock()
	if p.closing {
		p.Mu.Unlock()
		http.Error(w, "本机节点正在关闭", http.StatusServiceUnavailable)
		return
	}
	if p.localSSH == nil {
		p.localSSH = sshconnect.New(sshconnect.Options{Backend: peerSSHBackend{peer: p}, InstallationMode: sshconnect.InstallPeer})
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
func (s peerSSHService) SSHStatus(_ context.Context, id string) (sshconnect.InstallResult, error) {
	return s.service.Status(id)
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

type peerEnrollmentService interface {
	PreviewPeerEnrollment(context.Context, PeerEnrollmentRequest) (PeerEnrollmentPlan, error)
	PreparePeerEnrollment(context.Context, PeerEnrollmentRequest, string) (PeerEnrollmentPackage, error)
	CompletePeerEnrollment(context.Context, string) (PeerEnrollmentResult, error)
	PeerEnrollmentStatus(context.Context, string) (PeerEnrollmentResult, error)
	AbandonPeerEnrollment(context.Context, string) error
	PeerSourceCandidates(context.Context) ([]string, string)
}

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

// SourceEndpoints names this machine's candidate addresses, registered one
// first, and the HTTPS port a joining node must reach.
func (b peerSSHBackend) SourceEndpoints(ctx context.Context) ([]string, string) {
	return b.service().PeerSourceCandidates(ctx)
}

func (b peerSSHBackend) service() peerEnrollmentService {
	if b.enrollment != nil {
		return b.enrollment
	}
	return b.peer
}

func sshPeerEnrollmentRequest(req sshconnect.InstallRequest) PeerEnrollmentRequest {
	return PeerEnrollmentRequest{Alias: req.Alias, Name: req.Name, PeerAddress: req.Addr, RaftAddress: req.RaftAddr, SourceHost: req.SourceHost, Level: req.Level, WorkspaceDir: req.WorkspaceDir}
}

func peerPlanHash(plan PeerEnrollmentPlan) string {
	return plan.reviewHash()
}

func (b peerSSHBackend) prepare(ctx context.Context, req sshconnect.InstallRequest, check sshconnect.CheckResult) (sshconnect.Template, nodebootstrap.PeerSpec, PeerEnrollmentPlan, error) {
	template := sshconnect.Template{Steps: []sshconnect.Step{}, Effects: []string{}}
	plan, err := b.service().PreviewPeerEnrollment(ctx, sshPeerEnrollmentRequest(req))
	if err == nil && req.SourceHost == "" {
		// The target already said which of this machine's addresses it can
		// open; a registered address it cannot reach gives way to one it can.
		if pick, ok := reachableSourceHost(check.SourceHosts, plan.Request.SourceHost); ok && pick != plan.Request.SourceHost {
			req.SourceHost = pick
			plan, err = b.service().PreviewPeerEnrollment(ctx, sshPeerEnrollmentRequest(req))
		}
	}
	if err != nil {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "peer_network", Status: "blocked", Message: err.Error(), Suggestion: "检查目标节点地址与本机独立互联地址后重新生成计划"})
		return template, nodebootstrap.PeerSpec{}, plan, nil
	}
	if step, found := unreachableSourceStep(check.SourceHosts, plan, req.SourceHost != ""); found {
		template.Steps = append(template.Steps, step)
		if step.Status == "blocked" {
			return template, nodebootstrap.PeerSpec{}, plan, nil
		}
	}
	template.ReviewID = plan.ReviewID
	if template.ReviewID == "" || template.ReviewID != peerPlanHash(plan) {
		return template, nodebootstrap.PeerSpec{}, plan, errors.New("接入计划缺少有效审阅标识")
	}
	template.Effects = append(template.Effects, plan.Effects...)
	template.Effects = append(template.Effects, "通过 SSH 上传完整节点程序，校验后导入私有身份包到 ~/.steve-peer；执行日志保存到 ~/.steve-peer/peer.log")
	template.Steps = append(template.Steps, sshconnect.Step{ID: "workspace", Status: "ready", Message: "工作目录 " + plan.Request.WorkspaceDir + "；目标机会在安装时创建，并拒绝系统目录或家目录本身"})
	find := b.findBinary
	if find == nil {
		find = desktop.BundledPeerBinary
	}
	path, ok := find(check.OS + "/" + check.Arch)
	if !ok {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "binary", Status: "blocked", Message: "App 内没有适合目标平台的完整节点安装包", Suggestion: "使用包含 " + check.OS + "/" + check.Arch + " 节点程序的桌面安装包"})
		return template, nodebootstrap.PeerSpec{}, plan, nil
	}
	metadata, err := nodebootstrap.InspectBinary(path)
	if err != nil {
		return template, nodebootstrap.PeerSpec{}, plan, err
	}
	if metadata.OS != check.OS || metadata.Arch != check.Arch {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "binary", Status: "blocked", Message: "完整节点安装包的平台不匹配", Suggestion: "更换为目标平台的安装包"})
	}
	if !check.HasTool("sha256sum") && !check.HasTool("shasum") {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "checksum", Status: "blocked", Message: "远端缺少 SHA-256 校验工具", Suggestion: "安装 sha256sum 或 shasum 后重新检查"})
	}
	if !check.HasTool("base64") {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "base64", Status: "blocked", Message: "远端缺少 base64 工具", Suggestion: "安装系统基础工具后重新检查"})
	}
	template.Binary, template.BinaryPath = &metadata, path
	template.Steps = append(template.Steps, sshconnect.Step{ID: "peer_network", Status: "ready", Message: fmt.Sprintf("目标 HTTPS %s；共识连接 %s", plan.Request.PeerAddress, plan.Request.RaftAddress)})
	if plan.UpdateSourceAddress {
		message := "本机跨机连接将使用 " + plan.Source.APIAddress + " 和 " + plan.Source.Address
		if pick, ok := reachableSourceHost(check.SourceHosts, plan.Request.SourceHost); ok && pick == plan.Request.SourceHost {
			message = "目标机已确认能连到本机 " + plan.Request.SourceHost + "；" + message
		}
		template.Steps = append(template.Steps, sshconnect.Step{ID: "source_network", Status: "ready", Message: message, Suggestion: "这些地址应可由已加入节点独立访问；安装后会逐一验证"})
	}
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
		return sshconnect.Registration{}, errors.New("接入计划已在审阅后改变；请重新审阅，尚未准备身份或修改网络")
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
	if registered.Plan.ReviewID != plan.ReviewID || peerPlanHash(registered.Plan) != plan.ReviewID || json.Unmarshal(registered.Payload, &bundle) != nil || bundle.OperationID != installID || bundle.NodeID != registered.NodeID || bundle.ClusterID != plan.ClusterID || bundle.Name != plan.Request.Name || bundle.WorkspaceDir != plan.Request.WorkspaceDir || bundle.StorageLevel != plan.Request.Level || bundle.PeerAdvertise != plan.Request.PeerAddress || bundle.RaftAdvertise != plan.Request.RaftAddress || !reflect.DeepEqual(bundle.Seeds, plan.Seeds) {
		return result, errors.New("私有入组包与已审阅网络计划不同，安装已停止")
	}
	result.Token = base64.StdEncoding.EncodeToString(registered.Payload)
	spec.JoinPackage, spec.UploadID = result.Token, installID
	result.Script, err = nodebootstrap.BuildPeer(spec)
	return result, err
}

func (b peerSSHBackend) Verify(ctx context.Context, name string) error {
	return errors.New("集群接入需要原始操作 ID 才能确认")
}

func (b peerSSHBackend) VerifyRegistration(ctx context.Context, name, id string) error {
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
			why := "接入等待已到上限"
			if errors.Is(ctx.Err(), context.Canceled) {
				why = "接入等待被中断"
			}
			return peerWaitStopped(last, why)
		}
		result, err := b.service().CompletePeerEnrollment(ctx, id)
		// Progress is a new phase or something new said within one; the same
		// failures repeating, however many of them alternate, is a stall.
		if result.Phase != "" && result.Phase != last.Phase {
			advanced, reported = time.Now(), map[string]bool{}
			sshconnect.Report(ctx, "集群接入阶段："+peerPhaseText(result.Phase))
		}
		if result.Error != "" && !reported[result.Error] {
			reported[result.Error], advanced = true, time.Now()
			sshconnect.Report(ctx, "当前问题："+result.Error)
		}
		last = result
		if err == nil && result.OperationID == id && result.Name == name && result.Ready && result.Phase == "ready" {
			return nil
		}
		if result.OperationID != "" && result.OperationID != id {
			return errors.New("接入验证返回了其他操作的结果")
		}
		if result.Name != "" && result.Name != name {
			return errors.New("接入验证返回了其他节点的结果")
		}
		if err == nil && result.Ready {
			return errors.New("接入验证缺少完成状态")
		}
		if result.Phase != "awaiting_peer" && result.Phase != "synchronizing" && result.Phase != "registering_worker" && result.Phase != "joined" {
			return &sshconnect.StepError{Stage: "peer_membership", Code: "peer_not_ready", Message: "节点已安装，但集群接入未完成", Suggestion: "检查原操作记录和 ~/.steve-peer/peer.log，处理网络或状态同步问题后继续确认"}
		}
		if time.Since(advanced) > limit {
			return peerWaitStopped(last, fmt.Sprintf("集群接入停在「%s」超过 %s 没有进展", peerPhaseText(last.Phase), limit))
		}
		select {
		case <-ctx.Done():
			continue
		case <-ticker.C:
		}
	}
}

// peerWaitStopped explains a wait that ended without the node joining: the
// phase it stopped in, and the last thing that went wrong there.
func peerWaitStopped(last PeerEnrollmentResult, why string) *sshconnect.StepError {
	suggestion := "检查 ~/.steve-peer/peer.log 与节点间 HTTPS/共识端口；原操作记录已保留，处理后点「继续核对接入结果」，不会重新安装"
	if last.Error != "" {
		suggestion = "最近一次失败：" + last.Error + "。处理后点「继续核对接入结果」，不会重新安装"
	}
	return &sshconnect.StepError{Stage: "peer_membership", Code: "peer_not_ready", Message: "节点已安装，但" + why, Suggestion: suggestion}
}

// reachableSourceHost picks this machine's address for the plan from what
// the target reported: the current one if the target can open it, else the
// first it can. Without a report there is nothing to choose from.
func reachableSourceHost(probed []sshconnect.SourceHost, current string) (string, bool) {
	for _, host := range probed {
		if host.Host == current && host.Reachable {
			return current, true
		}
	}
	for _, host := range probed {
		if host.Reachable {
			return host.Host, true
		}
	}
	return "", false
}

// unreachableSourceStep reports a source address the target already failed
// to open, naming every address that was tried. An automatic choice blocks
// the plan; an address the user typed stays, with the finding beside it,
// since the probe can be wrong where the user is not.
func unreachableSourceStep(probed []sshconnect.SourceHost, plan PeerEnrollmentPlan, explicit bool) (sshconnect.Step, bool) {
	tried, known := make([]string, 0, len(probed)), false
	for _, host := range probed {
		tried = append(tried, host.Host)
		known = known || host.Host == plan.Request.SourceHost
	}
	if pick, ok := reachableSourceHost(probed, plan.Request.SourceHost); !known || ok && pick == plan.Request.SourceHost {
		return sshconnect.Step{}, false
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(plan.Source.APIAddress, "https://"))
	message := fmt.Sprintf("目标机连不上本机的 %s 端口：已从目标机试连 %s", port, strings.Join(tried, "、"))
	if explicit {
		return sshconnect.Step{ID: "source_network", Status: "ready", Message: message + "；将按填写的 " + plan.Request.SourceHost + " 继续", Suggestion: "安装后节点间会再次验证；若确认目标机能访问该地址，可以继续"}, true
	}
	return sshconnect.Step{ID: "source_network", Status: "blocked", Message: message, Suggestion: "确认两台机器在同一网络或 VPN 内、本机防火墙放行该端口；或在「本机互联地址」填写目标机能访问到的本机地址后重新检查"}, true
}

// peerPhaseText names an enrollment phase for the installation log.
func peerPhaseText(phase string) string {
	switch phase {
	case "awaiting_peer":
		return "等待节点进程首次连上协调节点"
	case "synchronizing":
		return "节点已连上，正在校验双向连接并同步集群状态"
	case "registering_worker":
		return "状态已同步，正在登记执行服务"
	case "joined":
		return "已加入集群，正在做独立连接验证"
	case "ready":
		return "接入完成"
	default:
		return phase
	}
}

// AbandonRegistration withdraws an enrollment; the two refusals a user can
// act on are reported as findings rather than service failures.
func (b peerSSHBackend) AbandonRegistration(ctx context.Context, id string) error {
	err := b.service().AbandonPeerEnrollment(ctx, id)
	switch {
	case errors.Is(err, ErrEnrollmentGone):
		return &sshconnect.StepError{Stage: "planning", Code: "unknown_plan", Message: err.Error(), Suggestion: "刷新接入记录；这次接入没有留下需要撤回的东西"}
	case errors.Is(err, ErrEnrollmentJoined):
		return &sshconnect.StepError{Stage: "peer_membership", Code: "already_joined", Message: err.Error(), Suggestion: "在资源页的机群成员里移除这台机器；接入对话框不能撤销已完成的接入"}
	}
	return err
}

func (b peerSSHBackend) ResumeRegistration(ctx context.Context, id string) (sshconnect.InstallResult, error) {
	record, err := b.service().PeerEnrollmentStatus(ctx, id)
	if err != nil {
		return sshconnect.InstallResult{}, &sshconnect.StepError{Stage: "planning", Code: "unknown_plan", Message: "未找到已确认的接入操作", Suggestion: "请刷新接入记录；不会重新上传或安装"}
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
	result.Steps = append(result.Steps, sshconnect.Step{ID: "peer_membership", Status: "ready", Message: "原接入操作已确认完成；节点成员、协作副本和执行服务就绪"})
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
}
