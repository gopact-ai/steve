package sshconnect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
)

// PreviewToken stands in for the credential created only during Commit.
const PreviewToken = "pending-node-token"

// DefaultWorkspaceDir is where a newly enrolled machine keeps its work
// unless the owner chooses somewhere: a visible directory under the remote
// account's home.
const DefaultWorkspaceDir = "~/steve-workspace"

type Step struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

type Tool struct {
	Name       string `json:"name"`
	Available  bool   `json:"available"`
	Executable string `json:"executable,omitempty"`
}

type CheckResult struct {
	Candidate            Candidate              `json:"candidate"`
	Reachable            bool                   `json:"reachable"`
	Address              string                 `json:"address,omitempty"`
	User                 string                 `json:"user,omitempty"`
	OS                   string                 `json:"os,omitempty"`
	Arch                 string                 `json:"arch,omitempty"`
	Tools                []Tool                 `json:"tools"`
	Agents               []agenttools.Candidate `json:"agents"`
	ExistingInstallation bool                   `json:"existing_installation"`
	ExistingPaths        []string               `json:"existing_paths"`
	ExistingNode         *ExistingNodeRecord    `json:"existing_node,omitempty"`
	InstallationMode     InstallationMode       `json:"installation_mode"`
	// FreeLoopbackPorts are ports on the target's loopback a link could
	// bind for this machine's listeners; nil when the backend does not link
	// or the target could not answer.
	FreeLoopbackPorts []int     `json:"free_loopback_ports,omitempty"`
	Steps             []Step    `json:"steps"`
	CheckedAt         time.Time `json:"checked_at"`
}

// ExistingNodeRecord contains unverified values from fixed configuration
// files. It must not authorize adoption or establish service availability.
type ExistingNodeRecord struct {
	Name  string `json:"name,omitempty"`
	Owner string `json:"owner,omitempty"`
}

type InstallationMode string

const (
	InstallExecutor InstallationMode = "executor"
	InstallPeer     InstallationMode = "peer"
)

func (c CheckResult) HasTool(name string) bool {
	for _, tool := range c.Tools {
		if tool.Name == name {
			return tool.Available
		}
	}
	return false
}

type InstallRequest struct {
	Alias    string `json:"alias"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Level    string `json:"level,omitempty"`
	HubURL   string `json:"-"`
	RaftAddr string `json:"raft_addr,omitempty"`
	// WorkspaceDir is where the new machine keeps its work: the default
	// project directory and the root its executor runs in. A peer
	// installation creates it; "~/" means the remote account's home.
	WorkspaceDir string `json:"workspace_dir,omitempty"`
	// ApprovedReviewID comes only from the service's stored plan, never JSON.
	ApprovedReviewID string `json:"-"`
}

type Template struct {
	Script     string                `json:"script"`
	Effects    []string              `json:"effects"`
	Steps      []Step                `json:"steps"`
	Binary     *nodebootstrap.Binary `json:"binary,omitempty"`
	BinaryPath string                `json:"-"`
	// ReviewID binds non-script effects such as a peer network readdressing
	// action. A backend computes it from the stable public plan.
	ReviewID string `json:"review_id,omitempty"`
}

type InstallPlan struct {
	ID        string                `json:"id"`
	Request   InstallRequest        `json:"request"`
	Check     CheckResult           `json:"check"`
	Script    string                `json:"script"`
	Effects   []string              `json:"effects"`
	Steps     []Step                `json:"steps"`
	Ready     bool                  `json:"ready"`
	ExpiresAt time.Time             `json:"expires_at"`
	Binary    *nodebootstrap.Binary `json:"binary,omitempty"`
	ReviewID  string                `json:"review_id,omitempty"`
}

type InstallResult struct {
	PlanID     string `json:"plan_id"`
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
	Connected  bool   `json:"connected"`
	Status     string `json:"status"`
	Steps      []Step `json:"steps"`
	NodeID     string `json:"node_id,omitempty"`
	// Phase is the phase running now, or the one a failed installation
	// stopped in; empty once connected. Phases lists them all, in order.
	Phase  string    `json:"phase,omitempty"`
	Phases []string  `json:"phases,omitempty"`
	Log    []LogLine `json:"log,omitempty"`
}

// Registration is deliberately not serializable: its credential and exact
// script remain between the registration backend and the SSH process stdin.
type Registration struct {
	Name     string `json:"-"`
	Token    string `json:"-"`
	Script   string `json:"-"`
	ReviewID string `json:"-"`
	NodeID   string `json:"-"`
}

// RegistrationVerifier completes a specific installation operation. Peer
// enrollment uses it to verify membership and replicated state, not just TCP.
type RegistrationVerifier interface {
	VerifyRegistration(context.Context, string, string) error
}

// RegistrationRecovery can reconcile a previously approved persistent peer
// operation after a lost reply or process restart. It must never upload,
// install, or create a fresh operation.
type RegistrationRecovery interface {
	ResumeRegistration(context.Context, string) (InstallResult, error)
}

// RegistrationAbandoner gives up a registered operation that will not
// finish: whatever the operation left behind on this side is withdrawn so
// the machine can be enrolled again. It never touches the remote machine.
type RegistrationAbandoner interface {
	AbandonRegistration(context.Context, string) error
}

// Backend adapts the application's existing node registration protocol.
// Preview is read-only. Register may return a nonempty Registration with an
// error when registration committed but a later step failed.
type Backend interface {
	Preview(context.Context, InstallRequest, CheckResult) (Template, error)
	Register(context.Context, InstallRequest, CheckResult, string) (Registration, error)
	Verify(context.Context, string) error
}

type Options struct {
	ConfigPath       string
	Runner           Runner
	Backend          Backend
	PlanTTL          time.Duration
	InstallationMode InstallationMode
	// Text is the Hub's language, for a caller whose context names none.
	Text i18n.Catalog
}

type storedPlan struct {
	plan       InstallPlan
	revision   string
	running    bool
	done       bool
	result     InstallResult
	err        error
	connection Connection
	timer      *time.Timer
}

type Service struct {
	configPath string
	runner     Runner
	backend    Backend
	ttl        time.Duration
	now        func() time.Time
	mu         sync.Mutex
	plans      map[string]*storedPlan
	// upgrades is the latest upgrade operation of each machine, by node ID.
	upgrades         map[string]string
	closed           bool
	installationMode InstallationMode
	text             i18n.Catalog
	// An upload lives as long as bytes keep moving; these pace the watch.
	uploadTick, uploadStall, uploadReport time.Duration
}

func New(options Options) *Service {
	if options.Runner == nil {
		options.Runner = OpenSSH{}
	}
	if options.PlanTTL <= 0 {
		options.PlanTTL = 5 * time.Minute
	}
	if options.InstallationMode == "" {
		options.InstallationMode = InstallExecutor
	}
	return &Service{configPath: options.ConfigPath, runner: options.Runner, backend: options.Backend, ttl: options.PlanTTL, now: time.Now, plans: map[string]*storedPlan{}, installationMode: options.InstallationMode, text: options.Text, uploadTick: time.Second, uploadStall: uploadStallLimit, uploadReport: uploadReportEvery}
}

// speak is ctx carrying the language of whoever called, the Hub's when
// they named none, and the catalog in that language. Every entry point
// starts with it; what it calls reads the language back from ctx, and what
// it writes into an installation's log is written in that language.
func (s *Service) speak(ctx context.Context) (context.Context, i18n.Catalog) {
	text := s.text.For(ctx)
	return i18n.WithLocale(ctx, text.Locale()), text
}

func (s *Service) Discover(ctx context.Context) (Discovery, error) {
	ctx, _ = s.speak(ctx)
	return Discover(ctx, s.configPath)
}

func (s *Service) selected(ctx context.Context, alias string) (Candidate, string, error) {
	text := i18n.FromContext(ctx)
	if !aliasShape.MatchString(alias) {
		return Candidate{}, "", Fail(text, "configuration", "invalid_alias", text.T(i18n.SSHAliasInvalid), text.T(i18n.SSHAliasInvalidFix))
	}
	d, err := s.Discover(ctx)
	if err != nil {
		return Candidate{}, "", err
	}
	for _, c := range d.Candidates {
		if c.Alias == alias {
			return c, d.Revision, nil
		}
	}
	return Candidate{}, "", Fail(text, "configuration", "unknown_alias", text.T(i18n.SSHAliasUnknown), text.T(i18n.SSHAliasUnknownFix))
}

// Check is invoked only after a user selected an alias. OpenSSH retains the
// user's authentication and proxy configuration, with forwarding and local or
// remote startup commands disabled. Unknown host keys require normal SSH setup.
func (s *Service) Check(ctx context.Context, alias string) (CheckResult, error) {
	ctx, _ = s.speak(ctx)
	c, _, err := s.selected(ctx, alias)
	if err != nil {
		return CheckResult{}, err
	}
	connection, err := s.bind(ctx, c)
	if err != nil {
		return CheckResult{}, err
	}
	defer connection.Close()
	return s.check(ctx, c, connection)
}

func (s *Service) bind(ctx context.Context, c Candidate) (Connection, error) {
	text := i18n.FromContext(ctx)
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, Fail(text, "ssh", "closed", text.T(i18n.SSHClosed), text.T(i18n.SSHClosedCheckFix))
	}
	binder, ok := s.runner.(ConnectionBinder)
	if !ok {
		return nil, Fail(text, "ssh", "connection_binding", text.T(i18n.SSHNoBinding), text.T(i18n.SSHNoBindingFix))
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	connection, err := binder.Bind(ctx, c.Alias, s.arguments(c.Alias, ""))
	if err != nil {
		return nil, err
	}
	if connection == nil {
		return nil, Fail(text, "ssh", "connection_binding", text.T(i18n.SSHBindFailed), text.T(i18n.SSHBindFailedFix))
	}
	s.mu.Lock()
	closed = s.closed
	s.mu.Unlock()
	if closed {
		// The service closed while the master came up; the closed error is
		// the finding, and a failed teardown leaves only a stray directory.
		_ = connection.Close()
		return nil, Fail(text, "ssh", "closed", text.T(i18n.SSHClosed), text.T(i18n.SSHClosedCheckFix))
	}
	return connection, nil
}

func (s *Service) check(ctx context.Context, c Candidate, connection Connection) (CheckResult, error) {
	text := i18n.FromContext(ctx)
	result := CheckResult{Candidate: c, Tools: []Tool{}, Agents: []agenttools.Candidate{}, ExistingPaths: []string{}, InstallationMode: s.installationMode, Steps: []Step{}, CheckedAt: s.now().UTC()}
	if s.installationMode != InstallExecutor && s.installationMode != InstallPeer {
		return result, Fail(text, "configuration", "invalid_installation_mode", text.T(i18n.SSHModeInvalid), text.T(i18n.SSHModeInvalidFix))
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	output, err := connection.Run(ctx, "sh -s", probeScript)
	if err != nil {
		failure := connectionError(ctx, output.Stderr)
		result.Steps = append(result.Steps, failure.step())
		return result, failure
	}
	values, err := parseProbe(output.Stdout)
	if err != nil {
		failure := Fail(text, "environment", "invalid_probe", text.T(i18n.SSHProbeIncomplete), text.T(i18n.SSHProbeIncompleteFix))
		result.Steps = append(result.Steps, failure.step())
		return result, failure
	}
	result.Reachable = true
	result.Address = values["address"]
	result.User = values["user"]
	result.OS = map[string]string{"Linux": "linux", "Darwin": "darwin"}[values["os"]]
	result.Arch = map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[values["arch"]]
	result.ExistingInstallation = values["existing"] == "1"
	if values["existing_node_name"] != "" || values["existing_node_owner"] != "" {
		result.ExistingNode = &ExistingNodeRecord{Name: values["existing_node_name"], Owner: values["existing_node_owner"]}
	}
	for _, entry := range existingProbePaths {
		if values[entry.field] != "" {
			result.ExistingPaths = append(result.ExistingPaths, values[entry.field])
		}
	}
	result.Steps = append(result.Steps, Step{ID: "ssh", Status: "ready", Message: text.T(i18n.SSHReady, result.User+"@"+result.Address)})
	if result.OS == "" || result.Arch == "" {
		result.Steps = append(result.Steps, Step{ID: "platform", Status: "blocked", Message: text.T(i18n.SSHPlatformUnsupported), Suggestion: text.T(i18n.SSHPlatformUnsupportedFix)})
	} else {
		result.Steps = append(result.Steps, Step{ID: "platform", Status: "ready", Message: result.OS + "/" + result.Arch})
	}
	paths := map[string]string{}
	for _, name := range probeTools {
		result.Tools = append(result.Tools, Tool{Name: name, Available: values[name] == "1", Executable: values["path_"+name]})
		if values[name] == "1" {
			paths[name] = values["path_"+name]
		}
	}
	result.Agents = agenttools.FromPaths(paths)
	for _, name := range []string{"bash", "nohup"} {
		if !result.HasTool(name) {
			result.Steps = append(result.Steps, Step{ID: "tool_" + name, Status: "blocked", Message: text.T(i18n.SSHToolMissing, name), Suggestion: text.T(i18n.SSHToolMissingFix)})
		}
	}
	if result.ExistingInstallation {
		result.Steps = append(result.Steps, Step{ID: "existing_node", Status: "blocked", Message: text.T(i18n.SSHExistingNode), Suggestion: text.T(i18n.SSHExistingNodeFix)})
	}
	return result, nil
}

// Plan performs fresh read-only checks and saves an expiring review snapshot.
// A blocked plan still contains evidence and suggestions for the user.
func (s *Service) Plan(ctx context.Context, req InstallRequest) (InstallPlan, error) {
	ctx, text := s.speak(ctx)
	if err := validateRequest(text, req, s.installationMode); err != nil {
		return InstallPlan{}, err
	}
	if s.installationMode == InstallPeer && req.WorkspaceDir == "" {
		req.WorkspaceDir = DefaultWorkspaceDir
	}
	if s.backend == nil {
		return InstallPlan{}, Fail(text, "planning", "not_configured", text.T(i18n.SSHNotConfigured), text.T(i18n.SSHNotConfiguredFix))
	}
	c, revision, err := s.selected(ctx, req.Alias)
	if err != nil {
		return InstallPlan{}, err
	}
	connection, err := s.bind(ctx, c)
	if err != nil {
		return InstallPlan{}, err
	}
	keep := false
	defer func() {
		if !keep {
			// Plan is returning its own error; a failed teardown adds only a
			// stray socket directory to it.
			_ = connection.Close()
		}
	}()
	check, err := s.check(ctx, c, connection)
	if err != nil {
		return InstallPlan{Request: req, Check: check, Steps: check.Steps}, err
	}
	s.probeLoopbackPorts(ctx, connection, &check)
	template, err := s.backend.Preview(ctx, req, check)
	if err != nil {
		return InstallPlan{}, err
	}
	var nonce [24]byte
	rand.Read(nonce[:])
	plan := InstallPlan{ID: hex.EncodeToString(nonce[:]), Request: req, Check: check, Script: template.Script, Effects: template.Effects, Steps: append(append([]Step{}, check.Steps...), template.Steps...), ExpiresAt: s.now().Add(s.ttl).UTC()}
	plan.Binary = template.Binary
	plan.ReviewID = template.ReviewID
	plan.Ready = template.Script != "" && stepsReady(plan.Steps)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return InstallPlan{}, Fail(text, "ssh", "closed", text.T(i18n.SSHClosed), text.T(i18n.SSHClosedCheckFix))
	}
	var expired []Connection
	for id, stored := range s.plans {
		if !stored.running && s.now().After(stored.plan.ExpiresAt) {
			if stored.timer != nil {
				stored.timer.Stop()
			}
			if stored.connection != nil {
				expired = append(expired, stored.connection)
			}
			delete(s.plans, id)
		}
	}
	if len(s.plans) >= 128 {
		s.mu.Unlock()
		for _, old := range expired {
			// The expired plans are gone from the table; a master that fails
			// to tear down leaves a stray directory and no plan to report to.
			_ = old.Close()
		}
		return InstallPlan{}, Fail(text, "planning", "too_many_plans", text.T(i18n.SSHTooManyPlans), text.T(i18n.SSHTooManyPlansFix))
	}
	// The caller can edit its copy without altering what Commit will execute.
	s.plans[plan.ID] = &storedPlan{plan: clonePlan(plan), revision: revision, connection: connection}
	s.plans[plan.ID].timer = time.AfterFunc(s.ttl, func() { s.expire(plan.ID) })
	keep = true
	s.mu.Unlock()
	for _, old := range expired {
		// As above: nothing but a stray directory can come of it, and the
		// new plan's result is not the place to report it.
		_ = old.Close()
	}
	return plan, nil
}

func (s *Service) expire(id string) {
	s.mu.Lock()
	stored := s.plans[id]
	if stored == nil || stored.running {
		s.mu.Unlock()
		return
	}
	delete(s.plans, id)
	s.mu.Unlock()
	if stored.connection != nil {
		// The plan expired on its timer: nobody is waiting on it, and a
		// failed teardown leaves only a stray directory.
		_ = stored.connection.Close()
	}
}

// Close releases all private masters. A process crash is additionally bounded
// by OpenSSH's ControlPersist timeout, independent of request contexts.
func (s *Service) Close() error {
	s.mu.Lock()
	s.closed = true
	var connections []Connection
	for _, stored := range s.plans {
		if stored.timer != nil {
			stored.timer.Stop()
		}
		if stored.connection != nil {
			connections = append(connections, stored.connection)
		}
	}
	s.mu.Unlock()
	var err error
	for _, connection := range connections {
		err = errors.Join(err, connection.Close())
	}
	return err
}

// Commit is the only entry point that registers or installs a node. A replay
// returns the earlier outcome; a partial or uncertain installation is never
// repeated automatically, since its process may have survived a lost SSH link.
func (s *Service) Commit(ctx context.Context, id string) (InstallResult, error) {
	ctx, text := s.speak(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return InstallResult{}, Fail(text, "ssh", "closed", text.T(i18n.SSHClosed), text.T(i18n.SSHClosedReviewFix))
	}
	stored, ok := s.plans[id]
	if !ok {
		s.mu.Unlock()
		if recovery, ok := s.backend.(RegistrationRecovery); ok {
			return s.resumeRegistration(ctx, recovery, id)
		}
		return InstallResult{}, Fail(text, "planning", "unknown_plan", text.T(i18n.SSHPlanUnknown), text.T(i18n.SSHPlanUnknownFix))
	}
	if stored.done {
		result, err := cloneResult(stored.result), stored.err
		if recovery, ok := s.backend.(RegistrationRecovery); ok && result.Registered && !result.Connected {
			stored.done, stored.running = false, true
			stored.result.Status, stored.result.Phase = "installing", PhaseConnectivity
			// The failure being retried is no longer a finding; what the
			// earlier attempt did settle stays on record.
			stored.result.Steps = settledSteps(stored.result.Steps)
			stored.result.appendLog(s.now(), "steve", text.T(i18n.SSHResuming))
			s.mu.Unlock()
			result, err = s.resumeRegistration(WithReporter(ctx, func(message string) { s.noteStored(id, message) }), recovery, id)
			s.mu.Lock()
			// The resume answers only the outcome; the record of how the
			// installation got here, and what was said while resuming,
			// carries across.
			result.Phases, result.Log = stored.result.Phases, stored.result.Log
			result.Steps = append(append([]Step{}, stored.result.Steps...), result.Steps...)
			if result.Connected {
				result.Phase = ""
				result.appendLog(s.now(), "steve", text.T(i18n.SSHResumed))
			} else {
				result.Phase = PhaseConnectivity
				if err != nil {
					result.appendLog(s.now(), "steve", err.Error())
				}
			}
			stored.done, stored.running = true, false
			stored.result, stored.err = cloneResult(result), err
			s.mu.Unlock()
			return result, err
		}
		s.mu.Unlock()
		return result, err
	}
	if stored.running {
		result := cloneResult(stored.result)
		s.mu.Unlock()
		return result, Fail(text, "installation", "in_progress", text.T(i18n.SSHPlanRunning), text.T(i18n.SSHPlanRunningFix))
	}
	if s.now().After(stored.plan.ExpiresAt) || !stored.plan.Ready {
		s.mu.Unlock()
		if stored.connection != nil {
			// The plan is refused either way; a failed teardown leaves only
			// a stray directory behind the refusal.
			_ = stored.connection.Close()
		}
		return InstallResult{}, Fail(text, "planning", "plan_not_ready", text.T(i18n.SSHPlanNotReady), text.T(i18n.SSHPlanNotReadyFix))
	}
	stored.running = true
	stored.result = InstallResult{PlanID: id, Name: stored.plan.Request.Name, Status: "installing", Steps: []Step{}, Phases: s.phasesFor(stored.plan)}
	plan, revision := clonePlan(stored.plan), stored.revision
	connection := stored.connection
	s.mu.Unlock()
	result, err := s.commit(ctx, plan, revision, connection)
	if connection != nil {
		// The installation's outcome is the finding; the master has done
		// its work, and a failed teardown leaves only a stray directory.
		_ = connection.Close()
	}
	s.mu.Lock()
	stored.running, stored.done = false, true
	stored.result, stored.err = cloneResult(result), err
	s.mu.Unlock()
	return result, err
}

func (s *Service) resumeRegistration(ctx context.Context, recovery RegistrationRecovery, id string) (InstallResult, error) {
	text := i18n.FromContext(ctx)
	if len(id) != 48 {
		return InstallResult{}, Fail(text, "planning", "unknown_plan", text.T(i18n.SSHOperationUnknown), text.T(i18n.SSHOperationUnknownFix))
	}
	if _, err := hex.DecodeString(id); err != nil {
		return InstallResult{}, Fail(text, "planning", "unknown_plan", text.T(i18n.SSHOperationUnknown), text.T(i18n.SSHOperationUnknownFix))
	}
	ctx, cancel := context.WithTimeout(ctx, peerVerifyLimit)
	defer cancel()
	return recovery.ResumeRegistration(ctx, id)
}

// Abandon gives up on a plan or operation that will not finish. A plan that
// only previewed is forgotten; one that registered something is withdrawn
// by the backend so the machine can be enrolled afresh. Nothing on the
// remote machine is touched: what an earlier attempt installed there is
// listed in the plan's steps for the user to clean up.
func (s *Service) Abandon(ctx context.Context, id string) error {
	ctx, text := s.speak(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Fail(text, "ssh", "closed", text.T(i18n.SSHClosed), text.T(i18n.SSHClosedAbandonFix))
	}
	stored, ok := s.plans[id]
	if ok && stored.running {
		s.mu.Unlock()
		return Fail(text, "installation", "in_progress", text.T(i18n.SSHPlanRunning), text.T(i18n.SSHPlanRunningAbandonFix))
	}
	registered := !ok || stored.result.Registered
	s.mu.Unlock()
	if registered {
		// A refusal changes nothing: the plan stays exactly as it was.
		abandoner, supported := s.backend.(RegistrationAbandoner)
		if !supported {
			return Fail(text, "planning", "abandon_unsupported", text.T(i18n.SSHAbandonUnsupported), text.T(i18n.SSHAbandonUnsupportedFix))
		}
		if _, err := hex.DecodeString(id); err != nil || len(id) != 48 {
			return Fail(text, "planning", "unknown_plan", text.T(i18n.SSHOperationUnknown), text.T(i18n.SSHOperationUnknownFix))
		}
		abandonCtx, cancel := context.WithTimeout(ctx, abandonLimit)
		defer cancel()
		var step *StepError
		if err := abandoner.AbandonRegistration(abandonCtx, id); err != nil && !(errors.As(err, &step) && step.Code == "unknown_plan") {
			// An operation the backend no longer has is already gone; only
			// a refusal to withdraw one that exists is a finding.
			return err
		}
	}
	s.mu.Lock()
	stored, ok = s.plans[id]
	if ok {
		if stored.timer != nil {
			stored.timer.Stop()
		}
		delete(s.plans, id)
	}
	s.mu.Unlock()
	if ok && stored.connection != nil {
		// The plan is gone; a master that fails to tear down leaves only a
		// stray directory behind.
		_ = stored.connection.Close()
	}
	return nil
}

// abandonLimit bounds the withdrawal of a registered operation, which may
// have to remove a member from the cluster.
const abandonLimit = 30 * time.Second

func (s *Service) commit(ctx context.Context, plan InstallPlan, revision string, connection Connection) (InstallResult, error) {
	text := i18n.FromContext(ctx)
	result := InstallResult{PlanID: plan.ID, Name: plan.Request.Name, Status: "needs_attention", Steps: []Step{}, Phases: s.phasesFor(plan)}
	reject := func(failure *StepError) (InstallResult, error) {
		result.Steps = append(result.Steps, failure.step())
		result.appendLog(s.now(), "steve", failure.Error())
		return result, failure
	}
	s.enter(&result, PhasePreflight, text.T(i18n.SSHPreflight))
	candidate, current, err := s.selected(ctx, plan.Request.Alias)
	if err != nil {
		return result, err
	}
	if current != revision {
		return reject(Fail(text, "configuration", "config_changed", text.T(i18n.SSHConfigChanged), text.T(i18n.SSHConfigChangedFix)))
	}
	if connection == nil {
		return reject(Fail(text, "ssh", "connection_lost", text.T(i18n.SSHConnectionLost), text.T(i18n.SSHConnectionLostFix)))
	}
	check, err := s.check(ctx, candidate, connection)
	if err != nil {
		result.Steps = append(result.Steps, check.Steps...)
		return result, err
	}
	if check.OS != plan.Check.OS || check.Arch != plan.Check.Arch || check.Address != plan.Check.Address || check.User != plan.Check.User || !stepsReady(check.Steps) {
		return reject(Fail(text, "environment", "environment_changed", text.T(i18n.SSHEnvironmentChanged), text.T(i18n.SSHEnvironmentChangedFix)))
	}
	s.probeLoopbackPorts(ctx, connection, &check)
	if check.FreeLoopbackPorts == nil {
		// A probe the target did not finish this time says nothing new; the
		// reviewed plan's answer stands.
		check.FreeLoopbackPorts = plan.Check.FreeLoopbackPorts
	}
	template, err := s.backend.Preview(ctx, plan.Request, check)
	if err != nil {
		return result, err
	}
	if template.Script != plan.Script || template.ReviewID != plan.ReviewID || !stepsReady(template.Steps) {
		return reject(Fail(text, "planning", "plan_changed", text.T(i18n.SSHPlanChanged), text.T(i18n.SSHPlanChangedFix)))
	}
	var binaryReader io.Reader
	if template.BinaryPath != "" {
		// The failed step below says what went wrong; the reason is not shown.
		binary, metadata, err := nodebootstrap.OpenBinary(text, template.BinaryPath)
		if err != nil {
			return reject(Fail(text, "binary", "binary_unavailable", text.T(i18n.SSHBinaryUnreadable), text.T(i18n.SSHBinaryUnreadableFix)))
		}
		defer binary.Close()
		if plan.Binary == nil || metadata != *plan.Binary {
			return reject(Fail(text, "binary", "binary_changed", text.T(i18n.SSHBinaryChanged), text.T(i18n.SSHBinaryChangedFix)))
		}
		binaryReader = io.LimitReader(binary, metadata.Size)
	}
	s.enter(&result, PhaseRegistration, text.T(i18n.SSHRegistering, plan.Request.Name))
	registerRequest := plan.Request
	registerRequest.ApprovedReviewID = plan.ReviewID
	registration, err := s.backend.Register(ctx, registerRequest, check, plan.ID)
	result.Registered = registration.Name != ""
	result.NodeID = registration.NodeID
	if err != nil {
		return reject(Fail(text, "registration", "registration_failed", text.T(i18n.SSHRegistrationFailed, err.Error()), text.T(i18n.SSHRegistrationFailedFix)))
	}
	normalized := strings.ReplaceAll(registration.Script, registration.Token, PreviewToken)
	normalized = strings.ReplaceAll(normalized, plan.ID, nodebootstrap.PreviewUploadID)
	if !result.Registered || registration.Token == "" || normalized != plan.Script || registration.ReviewID != plan.ReviewID {
		return reject(Fail(text, "planning", "registration_changed", text.T(i18n.SSHRegistrationChanged), text.T(i18n.SSHRegistrationChangedFix)))
	}
	registeredMessage := text.T(i18n.SSHRegistered)
	_, peerRegistration := s.backend.(RegistrationVerifier)
	if peerRegistration {
		registeredMessage = text.T(i18n.SSHPeerRegistered)
	}
	result.Steps = append(result.Steps, Step{ID: "registration", Status: "ready", Message: registeredMessage})
	s.progress(result)
	if binaryReader != nil {
		uncertain := func(stalled bool) *StepError {
			if peerRegistration {
				return Fail(text, "upload", "upload_uncertain", text.T(pick(stalled, i18n.SSHPeerUploadStalled, i18n.SSHPeerUploadUnconfirmed)), text.T(i18n.SSHPeerUploadFix))
			}
			return Fail(text, "upload", "upload_uncertain", text.T(pick(stalled, i18n.SSHUploadStalled, i18n.SSHUploadUnconfirmed)), text.T(i18n.SSHUploadUncertainFix))
		}
		if failure := s.upload(ctx, &result, plan, connection, binaryReader, registration.Token, uncertain); failure != nil {
			return reject(failure)
		}
	}
	s.enter(&result, PhaseInstallation, text.T(i18n.SSHInstalling))
	installCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	out, err := connection.Run(installCtx, "bash -s", registration.Script)
	cancel()
	s.output(text, &result, out, registration.Token)
	if err != nil {
		s.note(&result, text.T(i18n.SSHScriptFailed, err.Error()))
		if binaryReader != nil {
			result.Steps = append(result.Steps, s.cleanupUpload(ctx, connection, plan.ID))
		}
		if peerRegistration {
			return reject(Fail(text, "installation", "installation_uncertain", text.T(i18n.SSHPeerStartUncertain), text.T(i18n.SSHPeerStartUncertainFix)))
		}
		return reject(Fail(text, "installation", "installation_uncertain", text.T(i18n.SSHInstallUncertain), text.T(i18n.SSHInstallUncertainFix)))
	}
	result.Steps = append(result.Steps, Step{ID: "installation", Status: "ready", Message: text.T(i18n.SSHInstalled)})
	s.progress(result)
	if linker, ok := s.backend.(Linker); ok {
		s.enter(&result, PhaseLink, text.T(i18n.SSHLinking))
		linkCtx, cancel := context.WithTimeout(ctx, linkLimit)
		err := linker.Link(linkCtx, plan.ID, registration)
		cancel()
		if err != nil {
			return reject(Fail(text, "link", "link_failed", text.T(i18n.SSHLinkFailed, err.Error()), text.T(i18n.SSHLinkFailedFix)))
		}
		result.Steps = append(result.Steps, Step{ID: "link", Status: "ready", Message: text.T(i18n.SSHLinked)})
		s.progress(result)
	}
	if failure := s.verifyConnectivity(ctx, &result, plan); failure != nil {
		return reject(failure)
	}
	return result, nil
}

// upload sends the node program over the fixed SSH connection and records
// the machine's output. A failed upload is cleaned up before it is reported
// as what the caller makes of an upload that was not confirmed, or that
// stalled and was stopped.
// A laptop pushing tens of MiB through a VPN can take many minutes, so the
// upload has no fixed budget: it goes on while bytes move and ends when
// they stop for uploadStall.
func (s *Service) upload(ctx context.Context, result *InstallResult, plan InstallPlan, connection Connection, binary io.Reader, token string, uncertain func(stalled bool) *StepError) *StepError {
	text := i18n.FromContext(ctx)
	s.enter(result, PhaseUpload, text.T(i18n.SSHUploading, plan.Binary.OS+"/"+plan.Binary.Arch, float64(plan.Binary.Size)/(1<<20)))
	command, _ := nodebootstrap.UploadCommand(plan.ID)
	uploadCtx, cancel := context.WithTimeout(ctx, uploadLimit)
	metered := &meteredReader{Reader: binary}
	settle := s.watchUpload(uploadCtx, cancel, result, metered, plan.Binary.Size)
	out, err := connection.Upload(uploadCtx, command, metered)
	stalled := settle()
	cancel()
	s.output(text, result, out, token)
	if err != nil {
		if stalled {
			s.note(result, text.T(i18n.SSHUploadStallNote, s.uploadStall.Round(time.Second), mib(metered.read.Load()), mib(plan.Binary.Size)))
		}
		result.Steps = append(result.Steps, s.cleanupUpload(ctx, connection, plan.ID))
		return uncertain(stalled)
	}
	result.Steps = append(result.Steps, Step{ID: "upload", Status: "ready", Message: text.T(i18n.SSHUploaded)})
	s.progress(*result)
	return nil
}

// verifyConnectivity waits for the installed node to reach the coordinator
// and settles the result as connected. Sub-phase reports from the backend
// are forwarded while verification runs and ignored once it has returned.
func (s *Service) verifyConnectivity(ctx context.Context, result *InstallResult, plan InstallPlan) *StepError {
	text := i18n.FromContext(ctx)
	verifier, peerRegistration := s.backend.(RegistrationVerifier)
	verifyTimeout := 15 * time.Second
	message := text.T(i18n.SSHWaitingNode, verifyTimeout)
	if peerRegistration {
		// Cluster enrollment keeps going as long as it makes progress; the
		// verifier stops it on a stall, this cap only bounds the request.
		verifyTimeout = peerVerifyLimit
		message = text.T(i18n.SSHWaitingPeer)
	}
	s.enter(result, PhaseConnectivity, message)
	reporter := &phaseReporter{s: s, result: result}
	verifyCtx, cancel := context.WithTimeout(WithReporter(ctx, reporter.report), verifyTimeout)
	var err error
	if peerRegistration {
		err = verifier.VerifyRegistration(verifyCtx, plan.Request.Name, plan.ID)
	} else {
		err = s.backend.Verify(verifyCtx, plan.Request.Name)
	}
	cancel()
	reporter.close()
	if err != nil {
		var stepErr *StepError
		if errors.As(err, &stepErr) {
			return stepErr
		}
		return Fail(text, "connectivity", "node_unreachable", text.T(i18n.SSHNodeUnreachable), text.T(i18n.SSHNodeUnreachableFix))
	}
	result.Status, result.Connected, result.Phase = "connected", true, ""
	message = text.T(i18n.SSHNodeConnected)
	if peerRegistration {
		message = text.T(i18n.SSHPeerConnected)
	}
	result.Steps = append(result.Steps, Step{ID: "connectivity", Status: "ready", Message: message})
	result.appendLog(s.now(), "steve", message)
	return nil
}

// progress publishes how far a running installation has come. Once the
// installation has settled, nothing may move its record back.
func (s *Service) progress(result InstallResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stored := s.plans[result.PlanID]; stored != nil && stored.running {
		stored.result = cloneResult(result)
		stored.result.Status = "installing"
	}
}

func (s *Service) cleanupUpload(ctx context.Context, connection Connection, id string) Step {
	text := i18n.FromContext(ctx)
	command, _ := nodebootstrap.CleanupUploadCommand(id)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := connection.Run(cleanupCtx, command, ""); err != nil {
		return Step{ID: "upload_cleanup", Status: "blocked", Message: text.T(i18n.SSHCleanupUnconfirmed), Suggestion: text.T(i18n.SSHCleanupUnconfirmedFix, id)}
	}
	return Step{ID: "upload_cleanup", Status: "ready", Message: text.T(i18n.SSHCleanedUp)}
}

func (s *Service) arguments(alias, command string) []string {
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=8", "-o", "ConnectionAttempts=1", "-o", "PermitLocalCommand=no", "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "RemoteCommand=none", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "UpdateHostKeys=no", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "-o", "Compression=yes"}
	if s.configPath != "" {
		path, _ := filepath.Abs(s.configPath)
		args = append(args, "-F", path)
	}
	return append(args, "--", alias, command)
}

var nodeNameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// validateRequest checks what the user typed before anything is opened. An
// executor's name becomes a configuration key and keeps the key shape; a
// peer's name is what people see for the machine, and any display name
// will do. A workspace directory is absolute or under the remote home.
func validateRequest(text i18n.Catalog, req InstallRequest, mode InstallationMode) error {
	if !aliasShape.MatchString(req.Alias) {
		return Fail(text, "planning", "invalid_alias", text.T(i18n.SSHMachineAliasInvalid), text.T(i18n.SSHMachineAliasInvalidFix))
	}
	if mode == InstallPeer {
		if _, err := coordination.MemberName(req.Name); err != nil {
			return Fail(text, "planning", "invalid_name", text.T(i18n.SSHMachineNameInvalid), text.T(i18n.SSHMachineNameInvalidFix))
		}
		if req.WorkspaceDir != "" && !validWorkspaceDir(req.WorkspaceDir) {
			return Fail(text, "planning", "invalid_workspace", text.T(i18n.SSHWorkspaceInvalid), text.T(i18n.SSHWorkspaceInvalidFix))
		}
	} else if !nodeNameShape.MatchString(req.Name) {
		return Fail(text, "planning", "invalid_name", text.T(i18n.SSHNodeNameInvalid), text.T(i18n.SSHNodeNameInvalidFix))
	} else if req.WorkspaceDir != "" {
		return Fail(text, "planning", "invalid_workspace", text.T(i18n.SSHExecutorWorkspace), text.T(i18n.SSHExecutorWorkspaceFix))
	}
	host, portText, err := net.SplitHostPort(req.Addr)
	port, portErr := strconv.Atoi(portText)
	if err != nil || host == "" || portErr != nil || port < 1 || port > 65535 || strings.ContainsAny(host, "\r\n\x00 \t/?#@") {
		return Fail(text, "planning", "invalid_address", text.T(i18n.SSHAddressInvalid), text.T(i18n.SSHAddressInvalidFix))
	}
	switch req.Level {
	case "", "public", "internal", "restricted", "sealed":
	default:
		return Fail(text, "planning", "invalid_level", text.T(i18n.SSHLevelInvalid), text.T(i18n.SSHLevelInvalidFix))
	}
	if req.HubURL != "" {
		u, err := url.Parse(req.HubURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(req.HubURL, "\r\n\x00'\\") {
			return Fail(text, "planning", "invalid_coordinator_url", text.T(i18n.SSHHubURLInvalid), text.T(i18n.SSHHubURLInvalidFix))
		}
	}
	return nil
}

// validWorkspaceDir accepts an absolute remote path or one under the remote
// home, with no control characters and nothing that walks back up.
func validWorkspaceDir(dir string) bool {
	if len(dir) > 512 || strings.ContainsAny(dir, "\r\n\x00\t") || dir != strings.TrimSpace(dir) {
		return false
	}
	if !strings.HasPrefix(dir, "/") && dir != "~" && !strings.HasPrefix(dir, "~/") {
		return false
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// peerVerifyLimit bounds one connectivity wait for a cluster enrollment.
// The enrollment verifier gives up earlier when nothing advances.
const peerVerifyLimit = 15 * time.Minute

// linkLimit bounds bringing the SSH tunnel up during an installation: a
// second authentication over a link that just carried the check.
const linkLimit = 90 * time.Second

// settledSteps keeps the steps an attempt completed and drops the ones it
// stopped on: the retry answers those again, and the log keeps their text.
func settledSteps(steps []Step) []Step {
	kept := make([]Step, 0, len(steps))
	for _, step := range steps {
		if step.Status != "blocked" {
			kept = append(kept, step)
		}
	}
	return kept
}

func stepsReady(steps []Step) bool {
	for _, step := range steps {
		if step.Status == "blocked" {
			return false
		}
	}
	return true
}

func clonePlan(plan InstallPlan) InstallPlan {
	// HubURL is intentionally absent from wire JSON but stays in the snapshot.
	raw, _ := json.Marshal(plan)
	var copy InstallPlan
	// raw is Marshal's own output for this very type, so it decodes.
	_ = json.Unmarshal(raw, &copy)
	copy.Request.HubURL = plan.Request.HubURL
	return copy
}

func cloneResult(result InstallResult) InstallResult {
	result.Steps = append([]Step{}, result.Steps...)
	result.Phases = append([]string(nil), result.Phases...)
	result.Log = append([]LogLine(nil), result.Log...)
	return result
}

// StepError is safe for HTTP responses; raw SSH output is used only to classify
// the failure and is never included in errors, logs or serialized results.
// Build one with Fail, so its Error joins message and suggestion the way
// the language they are written in does.
type StepError struct {
	Stage      string `json:"stage"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
	text       i18n.Catalog
}

func (e *StepError) Error() string { return e.text.T(i18n.SSHStepError, e.Message, e.Suggestion) }
func (e *StepError) step() Step {
	return Step{ID: e.Stage, Status: "blocked", Message: e.Message, Suggestion: e.Suggestion}
}

// Fail is a failed step whose message and suggestion are in text's language.
func Fail(text i18n.Catalog, stage, code, message, suggestion string) *StepError {
	return &StepError{Stage: stage, Code: code, Message: message, Suggestion: suggestion, text: text}
}

// pick is the key for an upload that stalled, or for one that ended
// otherwise unconfirmed.
func pick(stalled bool, ifStalled, otherwise i18n.Key) i18n.Key {
	if stalled {
		return ifStalled
	}
	return otherwise
}

func connectionError(ctx context.Context, stderr string) *StepError {
	text := i18n.FromContext(ctx)
	said := strings.ToLower(stderr)
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return Fail(text, "ssh", "cancelled", text.T(i18n.SSHCancelled), text.T(i18n.SSHCancelledFix))
	case strings.Contains(said, "host key verification failed"), strings.Contains(said, "remote host identification has changed"), strings.Contains(said, "no host key is known"):
		return Fail(text, "ssh", "host_key", text.T(i18n.SSHHostKey), text.T(i18n.SSHHostKeyFix))
	case strings.Contains(said, "permission denied"), strings.Contains(said, "authentication failed"), strings.Contains(said, "too many authentication failures"):
		return Fail(text, "ssh", "authentication", text.T(i18n.SSHAuthFailed), text.T(i18n.SSHAuthFailedFix))
	case errors.Is(ctx.Err(), context.DeadlineExceeded), strings.Contains(said, "timed out"), strings.Contains(said, "connection timeout"):
		return Fail(text, "ssh", "timeout", text.T(i18n.SSHTimeout), text.T(i18n.SSHTimeoutFix))
	case strings.Contains(said, "could not resolve hostname"), strings.Contains(said, "name or service not known"):
		return Fail(text, "ssh", "resolution", text.T(i18n.SSHResolution), text.T(i18n.SSHResolutionFix))
	case strings.Contains(said, "connection refused"), strings.Contains(said, "no route to host"):
		return Fail(text, "ssh", "unreachable", text.T(i18n.SSHUnreachable), text.T(i18n.SSHUnreachableFix))
	default:
		return Fail(text, "ssh", "connection_failed", text.T(i18n.SSHConnectionFailed), text.T(i18n.SSHConnectionFailedFix))
	}
}
