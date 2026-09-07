package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

func (p *clusterPeer) serveSSHLocal(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		http.Error(w, "本机节点正在关闭", http.StatusServiceUnavailable)
		return
	}
	if p.localSSH == nil {
		p.localSSH = sshconnect.New(sshconnect.Options{Backend: peerSSHBackend{peer: p}, InstallationMode: sshconnect.InstallPeer})
	}
	service := p.localSSH
	p.mu.Unlock()
	handler, err := httpapi.SSHHandler(peerSSHService{service}, p.uiToken, p.uiURL)
	if err != nil {
		peerHTTPError(w, err)
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
func (s peerSSHService) SSHCommit(ctx context.Context, id string) (sshconnect.InstallResult, error) {
	return s.service.Commit(ctx, id)
}

type peerEnrollmentService interface {
	PreviewPeerEnrollment(context.Context, PeerEnrollmentRequest) (PeerEnrollmentPlan, error)
	PreparePeerEnrollment(context.Context, PeerEnrollmentRequest, string) (PeerEnrollmentPackage, error)
	CompletePeerEnrollment(context.Context, string) (PeerEnrollmentResult, error)
	PeerEnrollmentStatus(context.Context, string) (PeerEnrollmentResult, error)
}

type peerSSHBackend struct {
	peer *clusterPeer
	// Tests supply a bounded enrollment fixture and a local verified package.
	enrollment peerEnrollmentService
	findBinary func(string) (string, bool)
}

func (b peerSSHBackend) service() peerEnrollmentService {
	if b.enrollment != nil {
		return b.enrollment
	}
	return b.peer
}

func sshPeerEnrollmentRequest(req sshconnect.InstallRequest) PeerEnrollmentRequest {
	return PeerEnrollmentRequest{Alias: req.Alias, Name: req.Name, PeerAddress: req.Addr, RaftAddress: req.RaftAddr, SourceHost: req.SourceHost, Level: req.Level}
}

func peerPlanHash(plan PeerEnrollmentPlan) string {
	return plan.reviewHash()
}

func (b peerSSHBackend) prepare(ctx context.Context, req sshconnect.InstallRequest, check sshconnect.CheckResult) (sshconnect.Template, nodebootstrap.PeerSpec, PeerEnrollmentPlan, error) {
	template := sshconnect.Template{Steps: []sshconnect.Step{}, Effects: []string{}}
	plan, err := b.service().PreviewPeerEnrollment(ctx, sshPeerEnrollmentRequest(req))
	if err != nil {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "peer_network", Status: "blocked", Message: err.Error(), Suggestion: "检查目标节点地址与本机独立互联地址后重新生成计划"})
		return template, nodebootstrap.PeerSpec{}, plan, nil
	}
	template.ReviewID = plan.ReviewID
	if template.ReviewID == "" || template.ReviewID != peerPlanHash(plan) {
		return template, nodebootstrap.PeerSpec{}, plan, errors.New("接入计划缺少有效审阅标识")
	}
	template.Effects = append(template.Effects, plan.Effects...)
	template.Effects = append(template.Effects, "通过 SSH 上传完整节点程序，校验后导入私有身份包到 ~/.steve-peer；执行日志保存到 ~/.steve-peer/peer.log")
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
		template.Steps = append(template.Steps, sshconnect.Step{ID: "source_network", Status: "ready", Message: "本机跨机连接将使用 " + plan.Source.APIAddress + " 和 " + plan.Source.Address, Suggestion: "这些地址应可由已加入节点独立访问；安装后会逐一验证"})
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
	var bundle peerJoinPackage
	if registered.Plan.ReviewID != plan.ReviewID || peerPlanHash(registered.Plan) != plan.ReviewID || json.Unmarshal(registered.Payload, &bundle) != nil || bundle.OperationID != installID || bundle.NodeID != registered.NodeID || bundle.ClusterID != plan.ClusterID || bundle.Name != plan.Request.Name || bundle.StorageLevel != plan.Request.Level || bundle.PeerAdvertise != plan.Request.PeerAddress || bundle.RaftAdvertise != plan.Request.RaftAddress || !reflect.DeepEqual(bundle.Seeds, plan.Seeds) {
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
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return &sshconnect.StepError{Stage: "peer_membership", Code: "peer_not_ready", Message: "节点已安装，但集群接入尚未确认完成", Suggestion: "检查 ~/.steve-peer/peer.log 与节点间 HTTPS/共识端口；保持原操作记录，恢复连接后核对接入状态"}
		}
		result, err := b.service().CompletePeerEnrollment(ctx, id)
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
		select {
		case <-ctx.Done():
			continue
		case <-ticker.C:
		}
	}
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
