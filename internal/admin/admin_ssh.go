package admin

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

func (a *Service) sshService() *sshconnect.Service {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if a.ssh == nil {
		a.ssh = sshconnect.New(sshconnect.Options{Backend: sshNodeBackend{admin: a}})
	}
	return a.ssh
}

func (a *Service) CloseSSH() {
	a.Mu.Lock()
	service := a.ssh
	a.Mu.Unlock()
	if service != nil {
		// Shutdown: the SSH service holds only outbound connections, and
		// whatever one of them was doing has already reported its own
		// result to the console.
		_ = service.Close()
	}
}

func (a *Service) SSHDiscover(ctx context.Context) (sshconnect.Discovery, error) {
	return a.sshService().Discover(ctx)
}

func (a *Service) SSHCheck(ctx context.Context, alias string) (sshconnect.CheckResult, error) {
	return a.sshService().Check(ctx, alias)
}

func (a *Service) SSHPlan(ctx context.Context, req sshconnect.InstallRequest) (sshconnect.InstallPlan, error) {
	return a.sshService().Plan(ctx, req)
}

func (a *Service) SSHCommit(ctx context.Context, id string) (sshconnect.InstallResult, error) {
	return a.sshService().Commit(ctx, id)
}

type sshNodeBackend struct{ admin *Service }

func (b sshNodeBackend) Preview(ctx context.Context, req sshconnect.InstallRequest, check sshconnect.CheckResult) (sshconnect.Template, error) {
	template, _, err := b.prepare(ctx, req, check)
	return template, err
}

func (b sshNodeBackend) prepare(ctx context.Context, req sshconnect.InstallRequest, check sshconnect.CheckResult) (sshconnect.Template, nodebootstrap.Spec, error) {
	if err := ctx.Err(); err != nil {
		return sshconnect.Template{}, nodebootstrap.Spec{}, err
	}
	a := b.admin
	ConfigMu.RLock()
	_, exists := a.Cfg.Nodes[req.Name]
	binary := a.Cfg.Gateway.NodeBinary
	ConfigMu.RUnlock()
	if binary == "" && desktop.IsManagedConfig(a.Path) {
		if bundled, ok := desktop.BundledNodeBinary(check.OS + "/" + check.Arch); ok {
			binary = bundled
		}
	}
	template := sshconnect.Template{Steps: []sshconnect.Step{}, Effects: []string{
		"登记这台机器的节点名称、服务地址和数据等级",
		"通过 SSH 上传并校验节点安装包，写入仅当前账号可读的 ~/steve-bin/node.json",
		"创建 ~/steve-work，启动节点后台服务，日志保存在 ~/steve-node.log",
		"使用已有节点协议验证协调节点能否直接连接；SSH 连接不承担常驻隧道",
	}}
	blocked := func(id, message, suggestion string) {
		template.Steps = append(template.Steps, sshconnect.Step{ID: id, Status: "blocked", Message: message, Suggestion: suggestion})
	}
	if exists || req.Name == NodeName() {
		blocked("node_name", "这个节点名称已被使用", "使用其他名称，已有节点通过其管理入口调整")
	}
	if binary == "" {
		blocked("binary", "尚未配置可验证的节点安装包", "提供与目标平台匹配的 steve-node，并设置 gateway.node_binary")
		return template, nodebootstrap.Spec{}, nil
	}
	metadata, err := nodebootstrap.InspectBinary(binary)
	if err != nil {
		blocked("binary", err.Error(), "提供 Linux 或 macOS 的 amd64/arm64 节点安装包")
		return template, nodebootstrap.Spec{}, nil
	}
	template.Binary, template.BinaryPath = &metadata, binary
	if metadata.OS != check.OS || metadata.Arch != check.Arch {
		blocked("binary", "节点安装包平台为 "+metadata.OS+"/"+metadata.Arch+"，与目标机器不匹配", "提供 "+check.OS+"/"+check.Arch+" 的 steve-node 安装包")
	} else {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "binary", Status: "ready", Message: "安装包平台已匹配，SHA-256: " + metadata.SHA256})
	}
	if !check.HasTool("sha256sum") && !check.HasTool("shasum") {
		blocked("tool_checksum", "远端缺少 SHA-256 校验工具", "安装 sha256sum 或 shasum 后重新检查")
	}
	_, port, _ := net.SplitHostPort(req.Addr)
	spec := nodebootstrap.Spec{Name: req.Name, Port: port, Token: sshconnect.PreviewToken, Harnesses: map[string]nodebootstrap.Harness{},
		UploadID: nodebootstrap.PreviewUploadID,
		OS:       metadata.OS, Arch: metadata.Arch, SHA256: metadata.SHA256}
	script, err := nodebootstrap.Build(spec)
	if err != nil {
		return template, nodebootstrap.Spec{}, fmt.Errorf("无法生成节点安装脚本：%w", err)
	}
	template.Script = script
	template.Steps = append(template.Steps, sshconnect.Step{ID: "node_address", Status: "ready", Message: "节点服务将使用 " + req.Addr, Suggestion: "安装后将实际验证此地址；请确保协调节点与其他节点可以直接访问"})
	return template, spec, nil
}

func (b sshNodeBackend) Register(ctx context.Context, req sshconnect.InstallRequest, check sshconnect.CheckResult, installID string) (sshconnect.Registration, error) {
	template, spec, err := b.prepare(ctx, req, check)
	if err != nil {
		return sshconnect.Registration{}, err
	}
	for _, step := range template.Steps {
		if step.Status == "blocked" {
			return sshconnect.Registration{}, fmt.Errorf("%s", step.Message)
		}
	}
	result, err := b.admin.AddNode(ctx, consoleapi.AddNodeRequest{Name: req.Name, Addr: req.Addr, Level: req.Level, HubURL: req.HubURL})
	registration := sshconnect.Registration{Name: result.Name, Token: result.Token}
	if err != nil {
		return registration, err
	}
	spec.Token, spec.UploadID = result.Token, installID
	registration.Script, err = nodebootstrap.Build(spec)
	return registration, err
}

func (b sshNodeBackend) Verify(ctx context.Context, name string) error {
	if b.admin.Nodes == nil {
		return fmt.Errorf("节点注册服务不可用")
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := b.admin.Nodes.Advert(ctx, name); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
