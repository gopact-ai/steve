package sshconnect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/gopact-ai/steve/internal/nodebootstrap"
)

// PreviewToken stands in for the credential created only during Commit.
const PreviewToken = "pending-node-token"

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
	Steps                []Step                 `json:"steps"`
	CheckedAt            time.Time              `json:"checked_at"`
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
	Alias      string `json:"alias"`
	Name       string `json:"name"`
	Addr       string `json:"addr"`
	Level      string `json:"level,omitempty"`
	HubURL     string `json:"-"`
	RaftAddr   string `json:"raft_addr,omitempty"`
	SourceHost string `json:"source_host,omitempty"`
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
	configPath       string
	runner           Runner
	backend          Backend
	ttl              time.Duration
	now              func() time.Time
	mu               sync.Mutex
	plans            map[string]*storedPlan
	closed           bool
	installationMode InstallationMode
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
	return &Service{configPath: options.ConfigPath, runner: options.Runner, backend: options.Backend, ttl: options.PlanTTL, now: time.Now, plans: map[string]*storedPlan{}, installationMode: options.InstallationMode}
}

func (s *Service) Discover(ctx context.Context) (Discovery, error) {
	return Discover(ctx, s.configPath)
}

func (s *Service) selected(ctx context.Context, alias string) (Candidate, string, error) {
	if !aliasShape.MatchString(alias) {
		return Candidate{}, "", fail("configuration", "invalid_alias", "SSH 别名无效", "从发现列表中选择一台机器")
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
	return Candidate{}, "", fail("configuration", "unknown_alias", "SSH 配置中未找到这个别名", "刷新机器列表后重新选择")
}

// Check is invoked only after a user selected an alias. OpenSSH retains the
// user's authentication and proxy configuration, with forwarding and local or
// remote startup commands disabled. Unknown host keys require normal SSH setup.
func (s *Service) Check(ctx context.Context, alias string) (CheckResult, error) {
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
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, fail("ssh", "closed", "SSH 接入服务已关闭", "重新启动服务后检查")
	}
	binder, ok := s.runner.(ConnectionBinder)
	if !ok {
		return nil, fail("ssh", "connection_binding", "SSH 执行器不支持固定认证连接", "需要支持固定连接的 SSH 执行器；不会发送安装凭据")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	connection, err := binder.Bind(ctx, c.Alias, s.arguments(c.Alias, ""))
	if err != nil {
		return nil, err
	}
	if connection == nil {
		return nil, fail("ssh", "connection_binding", "未能建立固定 SSH 连接", "重新检查机器；不会发送安装凭据")
	}
	s.mu.Lock()
	closed = s.closed
	s.mu.Unlock()
	if closed {
		_ = connection.Close()
		return nil, fail("ssh", "closed", "SSH 接入服务已关闭", "重新启动服务后检查")
	}
	return connection, nil
}

func (s *Service) check(ctx context.Context, c Candidate, connection Connection) (CheckResult, error) {
	result := CheckResult{Candidate: c, Tools: []Tool{}, Agents: []agenttools.Candidate{}, ExistingPaths: []string{}, InstallationMode: s.installationMode, Steps: []Step{}, CheckedAt: s.now().UTC()}
	if s.installationMode != InstallExecutor && s.installationMode != InstallPeer {
		return result, fail("configuration", "invalid_installation_mode", "SSH 接入方式配置无效", "修正本机服务的接入配置后重试")
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
		failure := fail("environment", "invalid_probe", "SSH 已连接，但未收到完整的环境检查结果", "确认该账号允许运行标准 POSIX shell，修复启动脚本后重试")
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
	result.Steps = append(result.Steps, Step{ID: "ssh", Status: "ready", Message: "SSH 连接与认证已通过：" + result.User + "@" + result.Address})
	if result.OS == "" || result.Arch == "" {
		result.Steps = append(result.Steps, Step{ID: "platform", Status: "blocked", Message: "当前安装流程不支持这台机器的平台", Suggestion: "选择 Linux 或 macOS 的 amd64/arm64 机器"})
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
			result.Steps = append(result.Steps, Step{ID: "tool_" + name, Status: "blocked", Message: "未找到 " + name, Suggestion: "在远端账号的 PATH 中安装该工具后重新检查"})
		}
	}
	if result.ExistingInstallation {
		result.Steps = append(result.Steps, Step{ID: "existing_node", Status: "blocked", Message: "发现已有 Steve 配置或数据，已停止新安装", Suggestion: "这次检查无法确认服务是否正在运行、配置属于哪个工作台；请先核对原接入记录，不要覆盖或删除这些文件"})
	}
	return result, nil
}

// Plan performs fresh read-only checks and saves an expiring review snapshot.
// A blocked plan still contains evidence and suggestions for the user.
func (s *Service) Plan(ctx context.Context, req InstallRequest) (InstallPlan, error) {
	if err := validateRequest(req); err != nil {
		return InstallPlan{}, err
	}
	if s.backend == nil {
		return InstallPlan{}, fail("planning", "not_configured", "节点接入尚未配置", "配置节点注册服务后重试")
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
			_ = connection.Close()
		}
	}()
	check, err := s.check(ctx, c, connection)
	if err != nil {
		return InstallPlan{Request: req, Check: check, Steps: check.Steps}, err
	}
	template, err := s.backend.Preview(ctx, req, check)
	if err != nil {
		return InstallPlan{}, err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return InstallPlan{}, fmt.Errorf("无法生成安装计划 ID")
	}
	plan := InstallPlan{ID: hex.EncodeToString(nonce[:]), Request: req, Check: check, Script: template.Script, Effects: template.Effects, Steps: append(append([]Step{}, check.Steps...), template.Steps...), ExpiresAt: s.now().Add(s.ttl).UTC()}
	plan.Binary = template.Binary
	plan.ReviewID = template.ReviewID
	plan.Ready = template.Script != "" && stepsReady(plan.Steps)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return InstallPlan{}, fail("ssh", "closed", "SSH 接入服务已关闭", "重新启动服务后检查")
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
			_ = old.Close()
		}
		return InstallPlan{}, fail("planning", "too_many_plans", "待确认的安装计划过多", "等待旧计划过期后重试")
	}
	// The caller can edit its copy without altering what Commit will execute.
	s.plans[plan.ID] = &storedPlan{plan: clonePlan(plan), revision: revision, connection: connection}
	s.plans[plan.ID].timer = time.AfterFunc(s.ttl, func() { s.expire(plan.ID) })
	keep = true
	s.mu.Unlock()
	for _, old := range expired {
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
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return InstallResult{}, fail("ssh", "closed", "SSH 接入服务已关闭", "重新启动服务后核对原操作")
	}
	stored, ok := s.plans[id]
	if !ok {
		s.mu.Unlock()
		if recovery, ok := s.backend.(RegistrationRecovery); ok {
			return s.resumeRegistration(ctx, recovery, id)
		}
		return InstallResult{}, fail("planning", "unknown_plan", "安装计划不存在或已过期", "重新检查机器并生成计划")
	}
	if stored.done {
		result, err := cloneResult(stored.result), stored.err
		if recovery, ok := s.backend.(RegistrationRecovery); ok && result.Registered && !result.Connected {
			stored.done, stored.running = false, true
			stored.result.Status = "installing"
			s.mu.Unlock()
			result, err = s.resumeRegistration(ctx, recovery, id)
			s.mu.Lock()
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
		return result, fail("installation", "in_progress", "这个安装计划正在执行", "等待本次执行返回结果")
	}
	if s.now().After(stored.plan.ExpiresAt) || !stored.plan.Ready {
		s.mu.Unlock()
		if stored.connection != nil {
			_ = stored.connection.Close()
		}
		return InstallResult{}, fail("planning", "plan_not_ready", "安装计划已过期或仍有未解决的问题", "处理计划中列出的问题后重新检查")
	}
	stored.running = true
	stored.result = InstallResult{PlanID: id, Name: stored.plan.Request.Name, Status: "installing", Steps: []Step{}}
	plan, revision := clonePlan(stored.plan), stored.revision
	connection := stored.connection
	s.mu.Unlock()
	result, err := s.commit(ctx, plan, revision, connection)
	if connection != nil {
		_ = connection.Close()
	}
	s.mu.Lock()
	stored.running, stored.done = false, true
	stored.result, stored.err = cloneResult(result), err
	s.mu.Unlock()
	return result, err
}

func (s *Service) resumeRegistration(ctx context.Context, recovery RegistrationRecovery, id string) (InstallResult, error) {
	if len(id) != 48 {
		return InstallResult{}, fail("planning", "unknown_plan", "无法找到原接入操作", "请从已保存的接入记录核对操作")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return InstallResult{}, fail("planning", "unknown_plan", "无法找到原接入操作", "请从已保存的接入记录核对操作")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return recovery.ResumeRegistration(ctx, id)
}

func (s *Service) commit(ctx context.Context, plan InstallPlan, revision string, connection Connection) (InstallResult, error) {
	result := InstallResult{PlanID: plan.ID, Name: plan.Request.Name, Status: "needs_attention", Steps: []Step{}}
	reject := func(failure *StepError) (InstallResult, error) {
		result.Steps = append(result.Steps, failure.step())
		return result, failure
	}
	candidate, current, err := s.selected(ctx, plan.Request.Alias)
	if err != nil {
		return result, err
	}
	if current != revision {
		return reject(fail("configuration", "config_changed", "SSH 配置已在预览后改变", "重新检查机器并审阅新的安装计划"))
	}
	if connection == nil {
		return reject(fail("ssh", "connection_lost", "原检查使用的固定 SSH 连接已失效", "重新检查并审阅；不会改用其他连接发送凭据"))
	}
	check, err := s.check(ctx, candidate, connection)
	if err != nil {
		result.Steps = append(result.Steps, check.Steps...)
		return result, err
	}
	if check.OS != plan.Check.OS || check.Arch != plan.Check.Arch || check.Address != plan.Check.Address || check.User != plan.Check.User || !stepsReady(check.Steps) {
		return reject(fail("environment", "environment_changed", "机器环境已改变或不再满足接入条件", "重新检查并确认目标机器"))
	}
	template, err := s.backend.Preview(ctx, plan.Request, check)
	if err != nil {
		return result, err
	}
	if template.Script != plan.Script || template.ReviewID != plan.ReviewID || !stepsReady(template.Steps) {
		return reject(fail("planning", "plan_changed", "安装配置或安装包已在预览后改变", "审阅新的安装计划后再接入"))
	}
	var binaryReader io.Reader
	if template.BinaryPath != "" {
		binary, metadata, err := nodebootstrap.OpenBinary(template.BinaryPath)
		if err != nil {
			return reject(fail("binary", "binary_unavailable", "无法读取待上传的节点安装包", "重新检查安装包后生成计划"))
		}
		defer binary.Close()
		if plan.Binary == nil || metadata != *plan.Binary {
			return reject(fail("binary", "binary_changed", "待上传的节点安装包已在预览后改变", "重新审阅安装计划"))
		}
		binaryReader = io.LimitReader(binary, metadata.Size)
	}
	registerRequest := plan.Request
	registerRequest.ApprovedReviewID = plan.ReviewID
	registration, err := s.backend.Register(ctx, registerRequest, check, plan.ID)
	result.Registered = registration.Name != ""
	result.NodeID = registration.NodeID
	if err != nil {
		return reject(fail("registration", "registration_failed", "节点登记未完整完成", "检查资源页中的登记状态后处理；本次没有自动重试安装"))
	}
	normalized := strings.ReplaceAll(registration.Script, registration.Token, PreviewToken)
	normalized = strings.ReplaceAll(normalized, plan.ID, nodebootstrap.PreviewUploadID)
	if !result.Registered || registration.Token == "" || normalized != plan.Script || registration.ReviewID != plan.ReviewID {
		return reject(fail("planning", "registration_changed", "登记期间安装配置发生改变，安装已停止", "检查已登记的节点并重新审阅接入配置"))
	}
	registeredMessage := "节点已登记"
	_, peerRegistration := s.backend.(RegistrationVerifier)
	if peerRegistration {
		registeredMessage = "接入操作已保存，独立节点身份已准备"
	}
	result.Steps = append(result.Steps, Step{ID: "registration", Status: "ready", Message: registeredMessage})
	s.progress(result)
	if binaryReader != nil {
		command, _ := nodebootstrap.UploadCommand(plan.ID)
		uploadCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		_, err := connection.Upload(uploadCtx, command, binaryReader)
		cancel()
		if err != nil {
			result.Steps = append(result.Steps, s.cleanupUpload(ctx, connection, plan.ID))
			if peerRegistration {
				return reject(fail("upload", "upload_uncertain", "节点程序上传未确认完成，接入操作已保留", "检查 SSH 连接和本次接入记录；尚未确认加入投票成员"))
			}
			return reject(fail("upload", "upload_uncertain", "节点安装包上传未确认完成，节点登记已保留", "确认远端没有已安装节点后，在资源页移除这条未完成登记，再重新接入"))
		}
		result.Steps = append(result.Steps, Step{ID: "upload", Status: "ready", Message: "节点安装包已通过 SSH 上传，安装时将核验 SHA-256"})
		s.progress(result)
	}
	installCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	_, err = connection.Run(installCtx, "bash -s", registration.Script)
	cancel()
	if err != nil {
		if binaryReader != nil {
			result.Steps = append(result.Steps, s.cleanupUpload(ctx, connection, plan.ID))
		}
		if peerRegistration {
			return reject(fail("installation", "installation_uncertain", "远端节点启动未确认完成，接入操作已保留", "检查 ~/.steve-peer/peer.log 与原接入操作；SSH 断开后服务可能仍在运行，请先核对状态"))
		}
		return reject(fail("installation", "installation_uncertain", "远端安装未确认完成，节点登记已保留", "检查远端 ~/steve-node.log 与节点在线状态；SSH 断开后进程可能仍在运行，请勿重复安装"))
	}
	result.Steps = append(result.Steps, Step{ID: "installation", Status: "ready", Message: "远端安装脚本已完成"})
	s.progress(result)
	verifyTimeout := 15 * time.Second
	if _, ok := s.backend.(RegistrationVerifier); ok {
		verifyTimeout = 2 * time.Minute
	}
	verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	if verifier, ok := s.backend.(RegistrationVerifier); ok {
		err = verifier.VerifyRegistration(verifyCtx, plan.Request.Name, plan.ID)
	} else {
		err = s.backend.Verify(verifyCtx, plan.Request.Name)
	}
	cancel()
	if err != nil {
		var stepErr *StepError
		if errors.As(err, &stepErr) {
			return reject(stepErr)
		}
		return reject(fail("connectivity", "node_unreachable", "节点服务已启动，但协调节点尚未连通", "检查节点地址、端口和网络路由；SSH 代理连通不代表节点端口可直接访问"))
	}
	result.Status, result.Connected = "connected", true
	message := "协调节点已完成节点协议握手"
	if _, ok := s.backend.(RegistrationVerifier); ok {
		message = "节点已完成集群接入、状态同步和独立连接验证"
	}
	result.Steps = append(result.Steps, Step{ID: "connectivity", Status: "ready", Message: message})
	return result, nil
}

func (s *Service) progress(result InstallResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stored := s.plans[result.PlanID]; stored != nil {
		stored.result = cloneResult(result)
		stored.result.Status = "installing"
	}
}

func (s *Service) cleanupUpload(ctx context.Context, connection Connection, id string) Step {
	command, _ := nodebootstrap.CleanupUploadCommand(id)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := connection.Run(cleanupCtx, command, ""); err != nil {
		return Step{ID: "upload_cleanup", Status: "blocked", Message: "暂未确认临时上传文件已清理", Suggestion: "恢复 SSH 连接后可移除 ~/steve-bin/.upload-" + id + "，该目录仅属于本次上传"}
	}
	return Step{ID: "upload_cleanup", Status: "ready", Message: "本次临时上传已清理"}
}

func (s *Service) arguments(alias, command string) []string {
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=8", "-o", "ConnectionAttempts=1", "-o", "PermitLocalCommand=no", "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "RemoteCommand=none", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "UpdateHostKeys=no", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2"}
	if s.configPath != "" {
		path, _ := filepath.Abs(s.configPath)
		args = append(args, "-F", path)
	}
	return append(args, "--", alias, command)
}

var nodeNameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func validateRequest(req InstallRequest) error {
	if !aliasShape.MatchString(req.Alias) || !nodeNameShape.MatchString(req.Name) {
		return fail("planning", "invalid_name", "机器别名或节点名称无效", "从发现列表选择机器，节点名称使用小写字母、数字、点、下划线和连字符")
	}
	host, portText, err := net.SplitHostPort(req.Addr)
	port, portErr := strconv.Atoi(portText)
	if err != nil || host == "" || portErr != nil || port < 1 || port > 65535 || strings.ContainsAny(host, "\r\n\x00 \t/?#@") {
		return fail("planning", "invalid_address", "节点地址无效", "填写协调节点可以直接访问的主机名或 IP 与端口，例如 192.0.2.7:7701")
	}
	switch req.Level {
	case "", "public", "internal", "restricted", "sealed":
	default:
		return fail("planning", "invalid_level", "节点数据等级无效", "选择 public、internal、restricted 或 sealed")
	}
	if req.HubURL != "" {
		u, err := url.Parse(req.HubURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(req.HubURL, "\r\n\x00'\\") {
			return fail("planning", "invalid_coordinator_url", "协调服务地址无效", "使用有效的 HTTP 或 HTTPS 服务地址")
		}
	}
	return nil
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
	_ = json.Unmarshal(raw, &copy)
	copy.Request.HubURL = plan.Request.HubURL
	return copy
}

func cloneResult(result InstallResult) InstallResult {
	result.Steps = append([]Step{}, result.Steps...)
	return result
}

// StepError is safe for HTTP responses; raw SSH output is used only to classify
// the failure and is never included in errors, logs or serialized results.
type StepError struct {
	Stage      string `json:"stage"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
}

func (e *StepError) Error() string { return e.Message + "；" + e.Suggestion }
func (e *StepError) step() Step {
	return Step{ID: e.Stage, Status: "blocked", Message: e.Message, Suggestion: e.Suggestion}
}
func fail(stage, code, message, suggestion string) *StepError {
	return &StepError{Stage: stage, Code: code, Message: message, Suggestion: suggestion}
}

func connectionError(ctx context.Context, stderr string) *StepError {
	text := strings.ToLower(stderr)
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return fail("ssh", "cancelled", "连接检查已取消", "需要时重新检查这台机器")
	case strings.Contains(text, "host key verification failed"), strings.Contains(text, "remote host identification has changed"), strings.Contains(text, "no host key is known"):
		return fail("ssh", "host_key", "SSH 主机身份尚未通过校验", "先在终端用 SSH 核实并信任这台机器的主机密钥，再回来检查")
	case strings.Contains(text, "permission denied"), strings.Contains(text, "authentication failed"), strings.Contains(text, "too many authentication failures"):
		return fail("ssh", "authentication", "SSH 认证失败", "检查该别名的账号、密钥和 SSH agent；如需交互登录，先在终端完成后重试")
	case errors.Is(ctx.Err(), context.DeadlineExceeded), strings.Contains(text, "timed out"), strings.Contains(text, "connection timeout"):
		return fail("ssh", "timeout", "SSH 连接超时", "检查机器在线状态、VPN 或跳板机连接，再重新检查")
	case strings.Contains(text, "could not resolve hostname"), strings.Contains(text, "name or service not known"):
		return fail("ssh", "resolution", "SSH 无法解析目标地址", "检查 SSH config 中的 HostName、DNS 与网络连接")
	case strings.Contains(text, "connection refused"), strings.Contains(text, "no route to host"):
		return fail("ssh", "unreachable", "SSH 服务不可达", "检查机器的 SSH 服务、端口、网络路由及跳板机")
	default:
		return fail("ssh", "connection_failed", "SSH 连接或环境检查失败", "先在终端确认这个别名可以执行远端命令，再重新检查")
	}
}
