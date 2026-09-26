package admin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/i18n"
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

func (a *Service) SSHStatus(_ context.Context, id string) (sshconnect.InstallResult, error) {
	return a.sshService().Status(id)
}

func (a *Service) SSHAbandon(ctx context.Context, id string) error {
	return a.sshService().Abandon(ctx, id)
}

func (a *Service) SSHBrowse(ctx context.Context, req sshconnect.BrowseRequest) (sshconnect.Listing, error) {
	return a.sshService().Browse(ctx, req)
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
	b.admin.ConfigStore.rlock()
	_, exists := a.cfg().Nodes[req.Name]
	binary := a.cfg().Gateway.NodeBinary
	b.admin.ConfigStore.runlock()
	if binary == "" && desktop.IsManagedConfig(a.Path) {
		if bundled, ok := desktop.BundledNodeBinary(check.OS + "/" + check.Arch); ok {
			binary = bundled
		}
	}
	template := sshconnect.Template{Steps: []sshconnect.Step{}, Effects: []string{
		textFor(ctx).T(i18n.AdminSSHEffectRegister),
		textFor(ctx).T(i18n.AdminSSHEffectUpload),
		textFor(ctx).T(i18n.AdminSSHEffectStart),
		textFor(ctx).T(i18n.AdminSSHEffectVerify),
	}}
	blocked := func(id, message, suggestion string) {
		template.Steps = append(template.Steps, sshconnect.Step{ID: id, Status: "blocked", Message: message, Suggestion: suggestion})
	}
	if exists || req.Name == a.NodeName {
		blocked("node_name", textFor(ctx).T(i18n.AdminSSHNameTaken), textFor(ctx).T(i18n.AdminSSHNameTakenFix))
	}
	if binary == "" {
		blocked("binary", textFor(ctx).T(i18n.AdminSSHNoBinary), textFor(ctx).T(i18n.AdminSSHNoBinaryFix))
		return template, nodebootstrap.Spec{}, nil
	}
	metadata, err := nodebootstrap.InspectBinary(textFor(ctx), binary)
	if err != nil {
		blocked("binary", err.Error(), textFor(ctx).T(i18n.AdminSSHBinaryUnsupportedFix))
		return template, nodebootstrap.Spec{}, nil
	}
	template.Binary, template.BinaryPath = &metadata, binary
	if metadata.OS != check.OS || metadata.Arch != check.Arch {
		blocked("binary", textFor(ctx).T(i18n.AdminSSHBinaryMismatch, metadata.OS+"/"+metadata.Arch), textFor(ctx).T(i18n.AdminSSHBinaryMismatchFix, check.OS+"/"+check.Arch))
	} else {
		template.Steps = append(template.Steps, sshconnect.Step{ID: "binary", Status: "ready", Message: textFor(ctx).T(i18n.AdminSSHBinaryMatched, metadata.SHA256)})
	}
	if !check.HasTool("sha256sum") && !check.HasTool("shasum") {
		blocked("tool_checksum", textFor(ctx).T(i18n.AdminSSHNoChecksum), textFor(ctx).T(i18n.AdminSSHNoChecksumFix))
	}
	_, port, _ := net.SplitHostPort(req.Addr)
	spec := nodebootstrap.Spec{Name: req.Name, Port: port, Token: sshconnect.PreviewToken, Harnesses: map[string]nodebootstrap.Harness{},
		UploadID: nodebootstrap.PreviewUploadID,
		OS:       metadata.OS, Arch: metadata.Arch, SHA256: metadata.SHA256}
	script, err := nodebootstrap.Build(spec)
	if err != nil {
		return template, nodebootstrap.Spec{}, fmt.Errorf(textFor(ctx).T(i18n.AdminSSHScriptFailed), err)
	}
	template.Script = script
	template.Steps = append(template.Steps, sshconnect.Step{ID: "node_address", Status: "ready", Message: textFor(ctx).T(i18n.AdminSSHNodeAddress, req.Addr), Suggestion: textFor(ctx).T(i18n.AdminSSHNodeAddressFix)})
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
		return errors.New(textFor(ctx).T(i18n.AdminNodeRegistrationOff))
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

// SSHUpgrade and SSHUpgradeStatus answer for the executor backend too: it
// upgrades nothing, and the service says so.
func (a *Service) SSHUpgrade(ctx context.Context, nodeID string) (sshconnect.InstallResult, error) {
	return a.sshService().Upgrade(ctx, nodeID)
}

func (a *Service) SSHUpgradeStatus(_ context.Context, nodeID string) (sshconnect.InstallResult, error) {
	return a.sshService().UpgradeStatus(nodeID)
}
