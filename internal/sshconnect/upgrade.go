package sshconnect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/gopact-ai/steve/internal/nodebootstrap"
)

// UpgradeBackend is offered by a backend whose enrolled machines can be
// brought to this build over the SSH alias they were enrolled through.
type UpgradeBackend interface {
	// UpgradeTarget names the alias a machine is reached through and the
	// program to send it.
	UpgradeTarget(ctx context.Context, nodeID string) (UpgradeTarget, error)
	// Upgraded runs once the machine's program was swapped and its peer
	// restarted: the backend brings its own side up to date and returns
	// when the machine is back on the new build. It may Report progress.
	Upgraded(ctx context.Context, nodeID string) error
}

type UpgradeTarget struct {
	Alias string
	// Version is the build the machine is being brought to.
	Version string
	// FindBinary locates the program for a platform such as "linux/amd64".
	FindBinary func(platform string) (string, bool)
}

// upgradeVerifyLimit bounds how long the coordinator waits for a machine
// to come back on the new build once its program was swapped.
const upgradeVerifyLimit = 3 * time.Minute

var upgradePhases = []string{PhasePreflight, PhaseUpload, PhaseInstallation, PhaseConnectivity}

// Upgrade brings one enrolled machine to this build: the program is sent
// over a fresh SSH connection to the machine's alias, swapped in under
// ~/.steve-peer and the peer restarted on it; the backend then reopens
// what it keeps to the machine and waits until the machine reports the
// new build. It returns when the upgrade has settled; UpgradeStatus reads
// how far it has come meanwhile. One upgrade runs per machine at a time.
func (s *Service) Upgrade(ctx context.Context, nodeID string) (InstallResult, error) {
	backend, ok := s.backend.(UpgradeBackend)
	if !ok {
		return InstallResult{}, fail("preflight", "upgrade_unsupported", "这类接入的机器无法从这里升级", "在机器上重新安装节点程序")
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return InstallResult{}, fmt.Errorf("无法生成升级操作 ID")
	}
	id := hex.EncodeToString(nonce[:])
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return InstallResult{}, fail("ssh", "closed", "SSH 接入服务已关闭", "重新启动服务后再升级")
	}
	if previous := s.plans[s.upgrades[nodeID]]; previous != nil && previous.running {
		result := cloneResult(previous.result)
		s.mu.Unlock()
		return result, fail("installation", "in_progress", "这台机器正在升级", "等待本次升级返回结果")
	}
	stored := &storedPlan{plan: InstallPlan{ID: id, Request: InstallRequest{Name: nodeID}}, running: true, result: InstallResult{PlanID: id, Name: nodeID, NodeID: nodeID, Status: "installing", Steps: []Step{}, Phases: upgradePhases}}
	s.plans[id] = stored
	if s.upgrades == nil {
		s.upgrades = map[string]string{}
	}
	s.upgrades[nodeID] = id
	s.mu.Unlock()
	result, err := s.upgrade(ctx, backend, id, nodeID)
	s.mu.Lock()
	stored.running, stored.done = false, true
	stored.result, stored.err = cloneResult(result), err
	s.mu.Unlock()
	return result, err
}

// UpgradeStatus reads how a machine's latest upgrade is going, or went.
func (s *Service) UpgradeStatus(nodeID string) (InstallResult, error) {
	s.mu.Lock()
	id, ok := s.upgrades[nodeID]
	s.mu.Unlock()
	if !ok {
		return InstallResult{}, fail("planning", "unknown_plan", "这台机器没有进行中或刚结束的升级", "从机器列表发起升级")
	}
	return s.Status(id)
}

func (s *Service) upgrade(ctx context.Context, backend UpgradeBackend, id, nodeID string) (InstallResult, error) {
	result := InstallResult{PlanID: id, Name: nodeID, NodeID: nodeID, Status: "needs_attention", Steps: []Step{}, Phases: upgradePhases}
	reject := func(failure *StepError) (InstallResult, error) {
		result.Steps = append(result.Steps, failure.step())
		result.appendLog(s.now(), "steve", failure.Message+"；"+failure.Suggestion)
		return result, failure
	}
	s.enter(&result, PhasePreflight, "确认这台机器的 SSH 别名、平台和要发送的程序")
	target, err := backend.UpgradeTarget(ctx, nodeID)
	if err != nil {
		return reject(fail("preflight", "upgrade_target", err.Error(), "只有经由 SSH 接入、且隧道仍记录在本机的机器可以从这里升级"))
	}
	candidate, _, err := s.selected(ctx, target.Alias)
	if err != nil {
		return reject(stepOf(err))
	}
	connection, err := s.bind(ctx, candidate)
	if err != nil {
		return reject(stepOf(err))
	}
	defer connection.Close()
	check, err := s.check(ctx, candidate, connection)
	if err != nil {
		return reject(stepOf(err))
	}
	if check.OS == "" || check.Arch == "" {
		return reject(fail("preflight", "platform", "无法识别这台机器的平台", "只支持 Linux 或 macOS 的 amd64/arm64 机器"))
	}
	if !hasPath(check.ExistingPaths, "~/.steve-peer") {
		return reject(fail("preflight", "peer_missing", "机器上没有 ~/.steve-peer，不是一台已接入的节点", "先通过 SSH 接入这台机器"))
	}
	if !check.HasTool("sha256sum") && !check.HasTool("shasum") {
		return reject(fail("preflight", "checksum", "远端缺少 SHA-256 校验工具", "安装 sha256sum 或 shasum 后再升级"))
	}
	path, ok := target.FindBinary(check.OS + "/" + check.Arch)
	if !ok {
		return reject(fail("preflight", "binary_unavailable", "App 内没有适合 "+check.OS+"/"+check.Arch+" 的节点程序", "使用包含这个平台节点程序的桌面安装包"))
	}
	binary, metadata, err := nodebootstrap.OpenBinary(path)
	if err != nil {
		return reject(fail("preflight", "binary_unavailable", "无法读取待发送的节点程序", "重新安装 App 后再升级"))
	}
	defer binary.Close()
	if metadata.OS != check.OS || metadata.Arch != check.Arch {
		return reject(fail("preflight", "binary_mismatch", "节点程序的平台与机器不符", "使用目标平台的桌面安装包"))
	}
	result.Steps = append(result.Steps, Step{ID: "preflight", Status: "ready", Message: fmt.Sprintf("%s@%s，%s/%s；将发送 %s 版本的节点程序", check.User, check.Address, check.OS, check.Arch, target.Version)})
	s.progress(result)
	uncertain := func(reason string) *StepError {
		return fail("upload", "upload_uncertain", "节点程序上传"+reason+"，机器上的程序没有改变", "检查 SSH 连接后重新升级")
	}
	if failure := s.upload(ctx, &result, InstallPlan{ID: id, Binary: &metadata}, connection, io.LimitReader(binary, metadata.Size), "", uncertain); failure != nil {
		return reject(failure)
	}
	if failure := s.swapProgram(ctx, &result, connection, id, metadata); failure != nil {
		return reject(failure)
	}
	s.enter(&result, PhaseConnectivity, "重新建立到这台机器的 SSH 隧道，等待它以新版本回到集群")
	reporter := &phaseReporter{s: s, result: &result}
	verifyCtx, cancel := context.WithTimeout(WithReporter(ctx, reporter.report), upgradeVerifyLimit)
	err = backend.Upgraded(verifyCtx, nodeID)
	cancel()
	reporter.close()
	if err != nil {
		return reject(fail("connectivity", "upgrade_unconfirmed", "程序已替换，但机器还没有以新版本回到集群："+err.Error(), "稍后在机器列表核对版本；隧道会自动重连，不需要重复升级"))
	}
	result.Status, result.Connected, result.Phase = "connected", true, ""
	message := "机器已运行 " + target.Version + " 并回到集群"
	result.Steps = append(result.Steps, Step{ID: "connectivity", Status: "ready", Message: message})
	result.appendLog(s.now(), "steve", message)
	return result, nil
}

// swapProgram runs the upgrade script: it verifies the uploaded program,
// puts it in place of the installed one and restarts the peer, restoring
// the previous program when the new one does not stay up.
func (s *Service) swapProgram(ctx context.Context, result *InstallResult, connection Connection, id string, metadata nodebootstrap.Binary) *StepError {
	script, err := nodebootstrap.BuildPeerUpgrade(nodebootstrap.UpgradeSpec{UploadID: id, OS: metadata.OS, Arch: metadata.Arch, SHA256: metadata.SHA256})
	if err != nil {
		return fail("installation", "script", "无法生成升级脚本："+err.Error(), "检查节点程序的平台信息")
	}
	s.enter(result, PhaseInstallation, "校验 SHA-256 后替换 ~/.steve-peer/bin/steve 并重启节点进程；新程序起不来会自动换回旧程序")
	installCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	out, err := connection.Run(installCtx, "bash -s", script)
	cancel()
	s.output(result, out, "")
	if err == nil {
		result.Steps = append(result.Steps, Step{ID: "installation", Status: "ready", Message: "节点程序已替换，节点进程已在新程序上重启"})
		s.progress(*result)
		return nil
	}
	result.Steps = append(result.Steps, s.cleanupUpload(ctx, connection, id))
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 26 {
		return fail("installation", "upgrade_rejected", "新程序没有保持运行，机器已换回原来的程序", "查看上面的输出和机器上的 ~/.steve-peer/peer.log")
	}
	return fail("installation", "upgrade_uncertain", "升级脚本退出异常："+err.Error(), "检查机器上的 ~/.steve-peer/peer.log 和进程状态；SSH 断开后重启可能仍在进行")
}

func stepOf(err error) *StepError {
	var step *StepError
	if errors.As(err, &step) {
		return step
	}
	return fail("preflight", "upgrade_failed", err.Error(), "处理后重新升级")
}

func hasPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
