package sshconnect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os/exec"
	"slices"
	"time"

	"github.com/gopact-ai/steve/internal/i18n"
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
	ctx, text := s.speak(ctx)
	backend, ok := s.backend.(UpgradeBackend)
	if !ok {
		return InstallResult{}, Fail(text, "preflight", "upgrade_unsupported", text.T(i18n.SSHUpgradeUnsupported), text.T(i18n.SSHUpgradeUnsupportedFix))
	}
	var nonce [24]byte
	rand.Read(nonce[:])
	id := hex.EncodeToString(nonce[:])
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return InstallResult{}, Fail(text, "ssh", "closed", text.T(i18n.SSHClosed), text.T(i18n.SSHClosedUpgradeFix))
	}
	if previous := s.plans[s.upgrades[nodeID]]; previous != nil && previous.running {
		s.mu.Unlock()
		return InstallResult{}, Fail(text, "installation", "in_progress", text.T(i18n.SSHUpgradeRunning), text.T(i18n.SSHUpgradeRunningFix))
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
	// The record stays readable for as long as an installation plan would.
	stored.plan.ExpiresAt = s.now().Add(s.ttl).UTC()
	stored.timer = time.AfterFunc(s.ttl, func() { s.expire(id) })
	s.mu.Unlock()
	return result, err
}

// UpgradeStatus reads how a machine's latest upgrade is going, or how its
// last one went until that record expires.
func (s *Service) UpgradeStatus(ctx context.Context, nodeID string) (InstallResult, error) {
	ctx, text := s.speak(ctx)
	s.mu.Lock()
	id, ok := s.upgrades[nodeID]
	s.mu.Unlock()
	if !ok {
		return InstallResult{}, Fail(text, "planning", "unknown_plan", text.T(i18n.SSHUpgradeUnknown), text.T(i18n.SSHUpgradeUnknownFix))
	}
	return s.Status(ctx, id)
}

func (s *Service) upgrade(ctx context.Context, backend UpgradeBackend, id, nodeID string) (InstallResult, error) {
	text := i18n.FromContext(ctx)
	result := InstallResult{PlanID: id, Name: nodeID, NodeID: nodeID, Status: "needs_attention", Steps: []Step{}, Phases: upgradePhases}
	reject := func(failure *StepError) (InstallResult, error) {
		result.Steps = append(result.Steps, failure.step())
		result.appendLog(s.now(), "steve", failure.Error())
		return result, failure
	}
	s.enter(&result, PhasePreflight, text.T(i18n.SSHUpgradePreflight))
	target, err := backend.UpgradeTarget(ctx, nodeID)
	if err != nil {
		return reject(Fail(text, "preflight", "upgrade_target", err.Error(), text.T(i18n.SSHUpgradeTargetFix)))
	}
	candidate, _, err := s.selected(ctx, target.Alias)
	if err != nil {
		return reject(stepOf(text, err))
	}
	connection, err := s.bind(ctx, candidate)
	if err != nil {
		return reject(stepOf(text, err))
	}
	defer connection.Close()
	check, err := s.check(ctx, candidate, connection)
	if err != nil {
		return reject(stepOf(text, err))
	}
	if check.OS == "" || check.Arch == "" {
		return reject(Fail(text, "preflight", "platform", text.T(i18n.SSHUpgradePlatformUnknown), text.T(i18n.SSHUpgradePlatformUnknownFix)))
	}
	if !slices.Contains(check.ExistingPaths, "~/.steve-peer") {
		return reject(Fail(text, "preflight", "peer_missing", text.T(i18n.SSHUpgradeNotPeer), text.T(i18n.SSHUpgradeNotPeerFix)))
	}
	if !check.HasTool("sha256sum") && !check.HasTool("shasum") {
		return reject(Fail(text, "preflight", "checksum", text.T(i18n.AdminSSHNoChecksum), text.T(i18n.SSHUpgradeChecksumFix)))
	}
	path, ok := target.FindBinary(check.OS + "/" + check.Arch)
	if !ok {
		return reject(Fail(text, "preflight", "binary_unavailable", text.T(i18n.SSHUpgradeNoBinary, check.OS+"/"+check.Arch), text.T(i18n.SSHUpgradeNoBinaryFix)))
	}
	// The failed step below says what went wrong; the reason is not shown.
	binary, metadata, err := nodebootstrap.OpenBinary(text, path)
	if err != nil {
		return reject(Fail(text, "preflight", "binary_unavailable", text.T(i18n.SSHUpgradeBinaryUnreadable), text.T(i18n.SSHUpgradeBinaryUnreadableFix)))
	}
	defer binary.Close()
	if metadata.OS != check.OS || metadata.Arch != check.Arch {
		return reject(Fail(text, "preflight", "binary_mismatch", text.T(i18n.SSHUpgradeBinaryMismatch), text.T(i18n.SSHUpgradeBinaryMismatchFix)))
	}
	result.Steps = append(result.Steps, Step{ID: "preflight", Status: "ready", Message: text.T(i18n.SSHUpgradeReady, check.User, check.Address, check.OS, check.Arch, target.Version)})
	s.progress(result)
	uncertain := func(stalled bool) *StepError {
		return Fail(text, "upload", "upload_uncertain", text.T(pick(stalled, i18n.SSHUpgradeUploadStalled, i18n.SSHUpgradeUploadUnconfirmed)), text.T(i18n.SSHUpgradeUploadFix))
	}
	if failure := s.upload(ctx, &result, InstallPlan{ID: id, Binary: &metadata}, connection, io.LimitReader(binary, metadata.Size), "", uncertain); failure != nil {
		return reject(failure)
	}
	if failure := s.swapProgram(ctx, &result, connection, id, metadata); failure != nil {
		return reject(failure)
	}
	s.enter(&result, PhaseConnectivity, text.T(i18n.SSHUpgradeReconnecting))
	reporter := &phaseReporter{s: s, result: &result}
	verifyCtx, cancel := context.WithTimeout(WithReporter(ctx, reporter.report), upgradeVerifyLimit)
	err = backend.Upgraded(verifyCtx, nodeID)
	cancel()
	reporter.close()
	if err != nil {
		return reject(Fail(text, "connectivity", "upgrade_unconfirmed", text.T(i18n.SSHUpgradeUnconfirmed, err.Error()), text.T(i18n.SSHUpgradeUnconfirmedFix)))
	}
	result.Status, result.Connected, result.Phase = "connected", true, ""
	message := text.T(i18n.SSHUpgraded, target.Version)
	result.Steps = append(result.Steps, Step{ID: "connectivity", Status: "ready", Message: message})
	result.appendLog(s.now(), "steve", message)
	return result, nil
}

// swapProgram runs the upgrade script: it verifies the uploaded program,
// puts it in place of the installed one and restarts the peer, restoring
// the previous program when the new one does not stay up.
func (s *Service) swapProgram(ctx context.Context, result *InstallResult, connection Connection, id string, metadata nodebootstrap.Binary) *StepError {
	text := i18n.FromContext(ctx)
	script, err := nodebootstrap.BuildPeerUpgrade(nodebootstrap.UpgradeSpec{UploadID: id, OS: metadata.OS, Arch: metadata.Arch, SHA256: metadata.SHA256})
	if err != nil {
		return Fail(text, "installation", "script", text.T(i18n.SSHUpgradeScriptFailed, err.Error()), text.T(i18n.SSHUpgradeScriptFailedFix))
	}
	s.enter(result, PhaseInstallation, text.T(i18n.SSHUpgradeSwapping))
	// Between stopping the peer and starting it again the machine is down;
	// an owner closing the page must not cut the script off there.
	installCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	out, err := connection.Run(installCtx, "bash -s", script)
	cancel()
	s.output(text, result, out, "")
	if err == nil {
		result.Steps = append(result.Steps, Step{ID: "installation", Status: "ready", Message: text.T(i18n.SSHUpgradeSwapped)})
		s.progress(*result)
		return nil
	}
	result.Steps = append(result.Steps, s.cleanupUpload(ctx, connection, id))
	var exit *exec.ExitError
	code := 0
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	}
	switch code {
	case 26:
		return Fail(text, "installation", "upgrade_rejected", text.T(i18n.SSHUpgradeRejected), text.T(i18n.SSHUpgradeRejectedFix))
	case 28:
		return Fail(text, "installation", "upgrade_down", text.T(i18n.SSHUpgradeDown), text.T(i18n.SSHUpgradeDownFix))
	default:
		return Fail(text, "installation", "upgrade_uncertain", text.T(i18n.SSHUpgradeScriptExited, err.Error()), text.T(i18n.SSHUpgradeScriptExitedFix))
	}
}

func stepOf(text i18n.Catalog, err error) *StepError {
	var step *StepError
	if errors.As(err, &step) {
		return step
	}
	return Fail(text, "preflight", "upgrade_failed", err.Error(), text.T(i18n.SSHUpgradeFailedFix))
}
