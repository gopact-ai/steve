package sshconnect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
)

// RestartBackend is offered by a backend whose enrolled machines can have
// their peer restarted over the SSH alias they were enrolled through, on
// the program they already have.
type RestartBackend interface {
	// RestartTarget names the alias a machine is reached through.
	RestartTarget(ctx context.Context, nodeID string) (string, error)
	// Knows reports whether nodeID names a machine the backend knows of,
	// as UpgradeBackend.Knows does.
	Knows(ctx context.Context, nodeID string) bool
	// Restarted runs once the machine's peer was started: it returns when
	// the machine is back in the cluster, on whatever build it runs. It may
	// Report progress. After a manual restart ctx does not end with the
	// request that asked for it, only when a restart stops waiting: the
	// backend ends the wait itself when it closes.
	Restarted(ctx context.Context, nodeID string) error
	// RecordRestart keeps what a restart did among the cluster's events.
	RecordRestart(ctx context.Context, record RestartRecord) error
}

// What a restart did, as recorded.
const (
	// RestartRestarted: the peer was running and was restarted.
	RestartRestarted = "restarted"
	// RestartStarted: no peer was running and one was started.
	RestartStarted = "started"
	// RestartRunning: automatic start found the peer running and left it.
	RestartRunning = "running"
	// RestartFailed: the restart did not bring the machine back.
	RestartFailed = "failed"
	// RestartStopped: automatic start stopped for the machine, because
	// its last start failed or because its starts all came back and died
	// again.
	RestartStopped = "stopped"
)

// RestartRecord is one restart of a machine's peer, by hand or automatic.
// By is the node that ran it; the backend fills it in.
type RestartRecord struct {
	NodeID    string    `json:"node_id"`
	By        string    `json:"by,omitempty"`
	Automatic bool      `json:"automatic"`
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
}

// RestartState is how a machine's restarts stand on this node: its latest
// restart, running or finished, and whether automatic start ran it; and
// how automatic start stands for the machine where this node watches it.
type RestartState struct {
	Restart   *InstallResult  `json:"restart,omitempty"`
	Automatic bool            `json:"automatic,omitempty"`
	AutoStart *AutoStartState `json:"auto_start,omitempty"`
}

// restartUnsupported is the failure a restart, or its status, returns for
// any node ID where the backend restarts no machine.
func restartUnsupported(text i18n.Catalog) *StepError {
	return Fail(text, "preflight", "restart_unsupported", text.T(i18n.SSHRestartUnsupported), text.T(i18n.SSHRestartUnsupportedFix))
}

const (
	// restartScriptLimit bounds the restart script: up to 30 seconds for
	// the peer to stop and a few to see the new one stay up.
	restartScriptLimit = 3 * time.Minute
	// restartVerifyLimit bounds how long a restart waits for the machine
	// to come back, as long as an upgrade waits for its new build.
	restartVerifyLimit = upgradeVerifyLimit
	// restartRecordLimit bounds recording a restart among the cluster's
	// events once it finished.
	restartRecordLimit = 15 * time.Second
)

// restartPhaseOrder is the phases a restart goes through, in order.
var restartPhaseOrder = []string{PhasePreflight, PhaseRestart, PhaseConnectivity}

// Restart restarts one enrolled machine's peer on the program it already
// has: over a fresh SSH connection to the machine's alias the running peer
// is stopped the way an upgrade stops it, or none is found, and the peer
// is started; the backend then waits until the machine is back in the
// cluster. It returns when the restart has settled; RestartStatus reads
// how far it has come meanwhile. A machine runs one upgrade or restart at
// a time, whether started by hand or by automatic start.
func (s *Service) Restart(ctx context.Context, nodeID string) (InstallResult, error) {
	ctx, text := s.speak(ctx)
	backend, ok := s.backend.(RestartBackend)
	if !ok {
		return InstallResult{}, restartUnsupported(text)
	}
	if !backend.Knows(ctx, nodeID) {
		return InstallResult{}, unknownNode(text, nodeID, i18n.SSHRestartUnknownNodeFix)
	}
	s.mu.Lock()
	stored, failure := s.claimRestart(text, nodeID, false)
	s.mu.Unlock()
	if failure != nil {
		return InstallResult{}, failure
	}
	result, outcome, err := s.restart(ctx, backend, stored.plan.ID, nodeID, nodebootstrap.RestartSpec{})
	if err == nil {
		s.mu.Lock()
		s.resumeAutoStart(nodeID)
		s.mu.Unlock()
	}
	// The restart happened whether or not the page that asked for it is
	// still open; so is its record.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restartRecordLimit)
	s.record(recordCtx, backend, &result, RestartRecord{NodeID: nodeID, Outcome: outcome, Reason: reasonOf(err)})
	cancel()
	s.settle(stored, result, err)
	return result, err
}

// RestartStatus reads a machine's latest restart, running or finished
// until its record expires, and how automatic start stands for it. A
// machine never restarted from here has neither. Where the backend
// restarts no machine, it is refused as Restart is.
func (s *Service) RestartStatus(ctx context.Context, nodeID string) (RestartState, error) {
	ctx, text := s.speak(ctx)
	backend, ok := s.backend.(RestartBackend)
	if !ok {
		return RestartState{}, restartUnsupported(text)
	}
	if !backend.Knows(ctx, nodeID) {
		return RestartState{}, unknownNode(text, nodeID, i18n.SSHRestartUnknownNodeFix)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var state RestartState
	if stored := s.plans[s.restarts[nodeID]]; stored != nil {
		result := cloneResult(stored.result)
		state.Restart, state.Automatic = &result, stored.automatic
	}
	if watch := s.watches[nodeID]; watch != nil {
		auto := watch.state
		state.AutoStart = &auto
	}
	return state, nil
}

// claimRestart records a restart of nodeID as running, unless the service
// closed or the machine is being upgraded or restarted. s.mu is held.
func (s *Service) claimRestart(text i18n.Catalog, nodeID string, automatic bool) (*storedPlan, *StepError) {
	if s.closed {
		return nil, Fail(text, "ssh", "closed", text.T(i18n.SSHClosed), text.T(i18n.SSHClosedRestartFix))
	}
	if failure := s.busy(text, nodeID); failure != nil {
		return nil, failure
	}
	var nonce [24]byte
	rand.Read(nonce[:])
	id := hex.EncodeToString(nonce[:])
	stored := &storedPlan{plan: InstallPlan{ID: id, Request: InstallRequest{Name: nodeID}}, running: true, automatic: automatic, result: InstallResult{PlanID: id, Name: nodeID, NodeID: nodeID, Status: "installing", Steps: []Step{}, Phases: restartPhaseOrder}}
	s.plans[id] = stored
	if s.restarts == nil {
		s.restarts = map[string]string{}
	}
	s.restarts[nodeID] = id
	return stored, nil
}

// busy is the refusal of an upgrade or restart of a machine that is being
// upgraded or restarted already. s.mu is held.
func (s *Service) busy(text i18n.Catalog, nodeID string) *StepError {
	if s.running(s.upgrades, nodeID) {
		return Fail(text, "installation", "in_progress", text.T(i18n.SSHUpgradeRunning), text.T(i18n.SSHUpgradeRunningFix))
	}
	if s.running(s.restarts, nodeID) {
		return Fail(text, "installation", "in_progress", text.T(i18n.SSHRestartRunning), text.T(i18n.SSHRestartRunningFix))
	}
	return nil
}

// running reports whether the operation ids records for nodeID is still
// running. s.mu is held.
func (s *Service) running(ids map[string]string, nodeID string) bool {
	stored := s.plans[ids[nodeID]]
	return stored != nil && stored.running
}

// settle ends a running upgrade or restart with its result, readable for
// as long as an installation plan would be. A manual restart or an upgrade
// notes when it settled, for automatic start to set aside what it saw of
// the machine before.
func (s *Service) settle(stored *storedPlan, result InstallResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	stored.running, stored.done = false, true
	stored.result, stored.err = cloneResult(result), err
	stored.plan.ExpiresAt = now.Add(s.ttl).UTC()
	id := stored.plan.ID
	stored.timer = time.AfterFunc(s.ttl, func() { s.expire(id) })
	if !stored.automatic {
		if s.settled == nil {
			s.settled = map[string]time.Time{}
		}
		s.settled[stored.plan.Request.Name] = now
	}
}

// record hands a restart to the backend to keep among the cluster's
// events; a record that could not be kept is noted in the restart's log.
func (s *Service) record(ctx context.Context, backend RestartBackend, result *InstallResult, record RestartRecord) {
	record.At = s.now().UTC()
	if err := backend.RecordRestart(ctx, record); err != nil {
		s.note(result, i18n.FromContext(ctx).T(i18n.SSHRestartRecordFailed, err.Error()))
	}
}

// reasonOf is what a failed restart tells the owner, without its advice.
func reasonOf(err error) string {
	var step *StepError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &step):
		return step.Message
	default:
		return err.Error()
	}
}

// restart runs one restart of nodeID and returns what it did: restarted or
// started the peer, found it running (spec.IfStopped), or failed.
func (s *Service) restart(ctx context.Context, backend RestartBackend, id, nodeID string, spec nodebootstrap.RestartSpec) (InstallResult, string, error) {
	text := i18n.FromContext(ctx)
	result := InstallResult{PlanID: id, Name: nodeID, NodeID: nodeID, Status: "needs_attention", Steps: []Step{}, Phases: restartPhaseOrder}
	reject := func(outcome string, failure *StepError) (InstallResult, string, error) {
		result.Steps = append(result.Steps, failure.step())
		result.appendLog(s.now(), "steve", failure.Error())
		return result, outcome, failure
	}
	s.enter(&result, PhasePreflight, text.T(i18n.SSHRestartPreflight))
	alias, err := backend.RestartTarget(ctx, nodeID)
	if err != nil {
		return reject(RestartFailed, Fail(text, "preflight", "restart_target", err.Error(), text.T(i18n.SSHRestartTargetFix)))
	}
	candidate, _, err := s.selected(ctx, alias)
	if err != nil {
		return reject(RestartFailed, stepOf(text, err, "restart_failed", i18n.SSHRestartFailedFix))
	}
	connection, err := s.bind(ctx, candidate)
	if err != nil {
		return reject(RestartFailed, stepOf(text, err, "restart_failed", i18n.SSHRestartFailedFix))
	}
	defer connection.Close()
	check, err := s.check(ctx, candidate, connection)
	if err != nil {
		return reject(RestartFailed, stepOf(text, err, "restart_failed", i18n.SSHRestartFailedFix))
	}
	if !slices.Contains(check.ExistingPaths, "~/.steve-peer") {
		return reject(RestartFailed, Fail(text, "preflight", "peer_missing", text.T(i18n.SSHUpgradeNotPeer), text.T(i18n.SSHUpgradeNotPeerFix)))
	}
	result.Steps = append(result.Steps, Step{ID: "preflight", Status: "ready", Message: text.T(i18n.SSHRestartReady, check.User, check.Address)})
	s.progress(result)
	outcome, failure := s.startPeer(ctx, &result, connection, spec)
	if failure != nil {
		return reject(outcome, failure)
	}
	s.enter(&result, PhaseConnectivity, text.T(i18n.SSHRestartReconnecting))
	reporter := &phaseReporter{s: s, result: &result}
	confirmCtx := ctx
	if !spec.IfStopped {
		// The peer was stopped and started again whether or not the page
		// that asked for it is still open; so the machine is waited for.
		// Starting a peer only where none ran stopped nothing, and ends
		// when automatic start lets go of the machine.
		confirmCtx = context.WithoutCancel(ctx)
	}
	verifyCtx, cancel := context.WithTimeout(WithReporter(confirmCtx, reporter.report), restartVerifyLimit)
	err = backend.Restarted(verifyCtx, nodeID)
	cancel()
	reporter.close()
	if err != nil {
		return reject(RestartFailed, Fail(text, "connectivity", "restart_unconfirmed", text.T(i18n.SSHRestartUnconfirmed, err.Error()), text.T(i18n.SSHRestartUnconfirmedFix)))
	}
	result.Status, result.Connected, result.Phase = "connected", true, ""
	message := text.T(pick(outcome == RestartStarted, i18n.SSHStarted, i18n.SSHRestarted))
	result.Steps = append(result.Steps, Step{ID: "connectivity", Status: "ready", Message: message})
	result.appendLog(s.now(), "steve", message)
	return result, outcome, nil
}

// startPeer runs the restart script and reads what it did from the
// verdict it ends on.
func (s *Service) startPeer(ctx context.Context, result *InstallResult, connection Connection, spec nodebootstrap.RestartSpec) (string, *StepError) {
	text := i18n.FromContext(ctx)
	s.enter(result, PhaseRestart, text.T(pick(spec.IfStopped, i18n.SSHAutoStarting, i18n.SSHRestarting)))
	runCtx := ctx
	if !spec.IfStopped {
		// Between stopping the peer and starting it again the machine is
		// down; an owner closing the page must not cut the script off
		// there. Starting a peer only where none runs stops nothing, and
		// ends when automatic start lets go of the machine.
		runCtx = context.WithoutCancel(ctx)
	}
	runCtx, cancel := context.WithTimeout(runCtx, restartScriptLimit)
	out, err := connection.Run(runCtx, "bash -s", nodebootstrap.BuildPeerRestart(spec))
	cancel()
	verdict := restartVerdict(&out)
	s.output(text, result, out, "")
	if err != nil {
		var exit *exec.ExitError
		code := 0
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		switch code {
		case 21:
			return RestartFailed, Fail(text, "restart", "restart_busy", text.T(i18n.SSHRestartBusy), text.T(i18n.SSHRestartBusyFix))
		case 28:
			return RestartFailed, Fail(text, "restart", "restart_down", text.T(i18n.SSHRestartDown), text.T(i18n.SSHRestartDownFix))
		case 29:
			return RestartFailed, Fail(text, "restart", "restart_unlocated", text.T(i18n.SSHRestartUnlocated), text.T(i18n.SSHPeerUnlocatedFix))
		case 31:
			return RestartFailed, Fail(text, "restart", "restart_not_stopped", text.T(i18n.SSHRestartNotStopped), text.T(i18n.SSHRestartNotStoppedFix))
		case 30:
			return RestartFailed, Fail(text, "restart", "peer_missing", text.T(i18n.SSHUpgradeNotPeer), text.T(i18n.SSHUpgradeNotPeerFix))
		default:
			return RestartFailed, Fail(text, "restart", "restart_uncertain", text.T(i18n.SSHRestartScriptExited, err.Error()), text.T(i18n.SSHRestartScriptExitedFix))
		}
	}
	switch verdict {
	case RestartRestarted, RestartStarted:
		result.Steps = append(result.Steps, Step{ID: "restart", Status: "ready", Message: text.T(pick(verdict == RestartStarted, i18n.SSHRestartPeerStarted, i18n.SSHRestartPeerRestarted))})
		s.progress(*result)
		return verdict, nil
	case RestartRunning:
		return RestartRunning, Fail(text, "restart", "peer_running", text.T(i18n.SSHRestartPeerRunning), text.T(i18n.SSHRestartPeerRunningFix))
	}
	return RestartFailed, Fail(text, "restart", "restart_uncertain", text.T(i18n.SSHRestartNoVerdict), text.T(i18n.SSHRestartScriptExitedFix))
}

// restartVerdict takes the restart script's verdict line out of what it
// wrote and returns the verdict; the owner reads the rest.
func restartVerdict(out *Output) string {
	var verdict string
	lines := strings.SplitAfter(out.Stdout, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if said, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "STEVE_RESTART\t"); ok {
			verdict = said
			continue
		}
		kept = append(kept, line)
	}
	out.Stdout = strings.Join(kept, "")
	return verdict
}
