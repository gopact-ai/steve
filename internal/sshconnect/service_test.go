package sshconnect

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodebootstrap"
)

type recordedCommand struct {
	args   []string
	stdin  string
	upload bool
}
type recordingRunner struct {
	mu          sync.Mutex
	calls       []recordedCommand
	checkOutput string
	failInstall bool
	failUpload  bool
	stderr      string
}

type fixtureConnection struct {
	runner Runner
	args   []string
}

func (c *fixtureConnection) Run(ctx context.Context, command, input string) (Output, error) {
	args := append([]string{}, c.args...)
	args[len(args)-1] = command
	return c.runner.Run(ctx, args, input)
}
func (c *fixtureConnection) Upload(ctx context.Context, command string, input io.Reader) (Output, error) {
	args := append([]string{}, c.args...)
	args[len(args)-1] = command
	return c.runner.Upload(ctx, args, input)
}
func (c *fixtureConnection) Close() error { return nil }
func (r *recordingRunner) Bind(_ context.Context, _ string, args []string) (Connection, error) {
	return &fixtureConnection{runner: r, args: append([]string{}, args...)}, nil
}

const completeProbe = "STEVE_CHECK\tos\tLinux\nSTEVE_CHECK\tarch\tx86_64\nSTEVE_CHECK\tuser\tremote-user\nSTEVE_CHECK\taddress\t192.0.2.7\nSTEVE_CHECK\tbash\t1\nSTEVE_CHECK\tcurl\t1\nSTEVE_CHECK\tgit\t1\nSTEVE_CHECK\tnohup\t1\nSTEVE_CHECK\tsha256sum\t1\nSTEVE_CHECK\tshasum\t0\nSTEVE_CHECK\tbase64\t1\nSTEVE_CHECK\tnode\t1\nSTEVE_CHECK\tnpm\t1\nSTEVE_CHECK\tcodex\t1\nSTEVE_CHECK\tclaude\t0\nSTEVE_CHECK\tgrok\t0\nSTEVE_CHECK\tkimi\t0\nSTEVE_CHECK\texisting\t0\n"

func (r *recordingRunner) Run(_ context.Context, args []string, stdin string) (Output, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedCommand{args: append([]string(nil), args...), stdin: stdin})
	if r.stderr != "" {
		return Output{Stderr: r.stderr}, errors.New("SSH exit")
	}
	if strings.Contains(stdin, "STEVE_CHECK") {
		probe := r.checkOutput
		if probe == "" {
			probe = completeProbe
		}
		return Output{Stdout: probe}, nil
	}
	if r.failInstall {
		return Output{Stderr: "installation failed with secret-value"}, errors.New("SSH exit")
	}
	return Output{Stdout: "Node process started"}, nil
}

type fakeBackend struct {
	mu                           sync.Mutex
	registrations, verifications int
	script                       string
	verifyErr                    error
	binaryPath                   string
	reviewID                     string
	approvedReviewID             string
}

func (b *fakeBackend) Preview(context.Context, InstallRequest, CheckResult) (Template, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	script := b.script
	if script == "" {
		script = "# reviewed\nnode_token='" + PreviewToken + "'\n"
	}
	template := Template{Script: script, Effects: []string{"创建节点配置并启动服务"}, Steps: []Step{{ID: "binary", Status: "ready", Message: "安装包平台已匹配"}}, ReviewID: b.reviewID}
	if b.binaryPath != "" {
		metadata, err := nodebootstrap.InspectBinary(b.binaryPath)
		if err != nil {
			return Template{}, err
		}
		template.BinaryPath, template.Binary = b.binaryPath, &metadata
	}
	return template, nil
}
func (b *fakeBackend) Register(_ context.Context, req InstallRequest, _ CheckResult, id string) (Registration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.registrations++
	b.approvedReviewID = req.ApprovedReviewID
	script := b.script
	if script == "" {
		script = "# reviewed\nnode_token='" + PreviewToken + "'\n"
	}
	script = strings.ReplaceAll(script, nodebootstrap.PreviewUploadID, id)
	return Registration{Name: "remote", Token: "test-install-secret", Script: strings.ReplaceAll(script, PreviewToken, "test-install-secret"), ReviewID: b.reviewID}, nil
}

func (r *recordingRunner) Upload(_ context.Context, args []string, input io.Reader) (Output, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := io.ReadAll(input)
	r.calls = append(r.calls, recordedCommand{args: append([]string(nil), args...), stdin: string(data), upload: true})
	if r.failUpload {
		return Output{}, errors.New("lost upload connection")
	}
	return Output{}, err
}

func TestCommitStreamsBinaryAfterReviewWithoutCoordinatorHTTP(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	raw := make([]byte, 64)
	copy(raw, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	raw[16], raw[18], raw[20], raw[52] = 2, 62, 1, 64
	b.binaryPath = filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(b.binaryPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	b.script = "# use uploaded binary " + nodebootstrap.PreviewUploadID + "\nnode_token='" + PreviewToken + "'\n"
	req := installRequest()
	req.HubURL = ""
	plan, err := svc.Plan(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Binary == nil || plan.Binary.Size != 64 {
		t.Fatalf("missing binary review evidence: %#v", plan)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err != nil || !result.Connected {
		t.Fatalf("upload install = %#v %v", result, err)
	}
	uploads := 0
	for _, call := range r.calls {
		if call.upload {
			uploads++
			if call.stdin != string(raw) {
				t.Fatal("binary was not streamed unchanged")
			}
		}
		if strings.Contains(strings.Join(call.args, " "), "test-install-secret") {
			t.Fatal("credential in upload command")
		}
		if strings.Contains(call.stdin, "node_token") && strings.Contains(call.stdin, nodebootstrap.PreviewUploadID) {
			t.Fatal("real installation retained preview upload ID")
		}
	}
	if uploads != 1 {
		t.Fatalf("uploaded %d times", uploads)
	}
}

func TestFailedUploadCleansOnlyItsStagingAndNeverStartsInstaller(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	raw := make([]byte, 64)
	copy(raw, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	raw[16], raw[18], raw[20], raw[52] = 2, 62, 1, 64
	b.binaryPath = filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(b.binaryPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	b.script = "# upload " + nodebootstrap.PreviewUploadID + "\nnode_token='" + PreviewToken + "'\n"
	r.failUpload = true
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err == nil || !result.Registered || result.Connected {
		t.Fatalf("upload failure = %#v, %v", result, err)
	}
	cleanupFound := false
	for _, call := range r.calls {
		if !call.upload && strings.Contains(call.stdin, "node_token") {
			t.Fatal("failed upload started installer")
		}
		args := strings.Join(call.args, " ")
		if strings.Contains(args, "rm -f") {
			cleanupFound = true
			if !strings.Contains(args, ".upload-"+plan.ID) || strings.Contains(args, "rm -rf") {
				t.Fatal("cleanup escaped its bounded upload directory")
			}
		}
	}
	if !cleanupFound {
		t.Fatal("failed upload skipped cleanup")
	}
}

func TestBinaryChangeAfterReviewFailsBeforeRegistration(t *testing.T) {
	svc, _, b, _ := serviceFixture(t)
	raw := make([]byte, 64)
	copy(raw, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	raw[16], raw[18], raw[20], raw[52] = 2, 62, 1, 64
	b.binaryPath = filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(b.binaryPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	raw[63] = 1
	if err := os.WriteFile(b.binaryPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Commit(t.Context(), plan.ID); err == nil {
		t.Fatal("changed binary was installed")
	}
	if b.registrations != 0 {
		t.Fatal("changed binary registered node")
	}
}

type blockingInstallRunner struct {
	*recordingRunner
	entered, release chan struct{}
}

func (r *blockingInstallRunner) Bind(_ context.Context, _ string, args []string) (Connection, error) {
	return &fixtureConnection{runner: r, args: append([]string{}, args...)}, nil
}

func (r *blockingInstallRunner) Run(ctx context.Context, args []string, input string) (Output, error) {
	if strings.Contains(input, "node_token") {
		close(r.entered)
		<-r.release
	}
	return r.recordingRunner.Run(ctx, args, input)
}

func TestConcurrentCommitReturnsProgressAndDoesNotInstallTwice(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	block := &blockingInstallRunner{recordingRunner: r, entered: make(chan struct{}), release: make(chan struct{})}
	svc.runner = block
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { close(block.release) }) }
	defer release()
	completed := make(chan error, 1)
	go func() { _, err := svc.Commit(t.Context(), plan.ID); completed <- err }()
	select {
	case <-block.entered:
	case err := <-completed:
		t.Fatalf("first commit did not reach installer: %v", err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	var failure *StepError
	if !errors.As(err, &failure) || failure.Code != "in_progress" || result.Status != "installing" || !result.Registered {
		t.Fatalf("concurrent commit lost registration progress: %#v %v", result, err)
	}
	release()
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if b.registrations != 1 {
		t.Fatal("concurrent commit repeated registration")
	}
}
func (b *fakeBackend) Verify(context.Context, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verifications++
	return b.verifyErr
}

func serviceFixture(t *testing.T) (*Service, *recordingRunner, *fakeBackend, string) {
	t.Helper()
	path := configFixture(t, map[string]string{"config": "Host dev\nHostName dev.example\n"})
	runner, backend := &recordingRunner{}, &fakeBackend{}
	svc := New(Options{ConfigPath: path, Runner: runner, Backend: backend})
	t.Cleanup(func() { _ = svc.Close() })
	return svc, runner, backend, path
}

func installRequest() InstallRequest {
	return InstallRequest{Alias: "dev", Name: "remote", Addr: "192.0.2.7:7701", Level: "internal", HubURL: "https://coordinator.example"}
}

func TestDiscoveryAndPreviewNeverRegisterOrInstall(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	if _, err := svc.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Fatal("discovery ran SSH")
	}
	check, err := svc.Check(t.Context(), "dev")
	if err != nil || !check.Reachable || check.OS != "linux" || check.Arch != "amd64" || check.Address != "192.0.2.7" {
		t.Fatalf("check: %#v, %v", check, err)
	}
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil || !plan.Ready || plan.ID == "" {
		t.Fatalf("plan: %#v, %v", plan, err)
	}
	if b.registrations != 0 || b.verifications != 0 {
		t.Fatal("preview registered a node")
	}
	for _, call := range r.calls {
		if !strings.Contains(call.stdin, "STEVE_CHECK") {
			t.Fatal("preview ran installation")
		}
		args := strings.Join(call.args, " ")
		for _, required := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "PermitLocalCommand=no", "ClearAllForwardings=yes", "RemoteCommand=none", "-- dev sh -s"} {
			if !strings.Contains(args, required) {
				t.Errorf("SSH omitted %s: %v", required, call.args)
			}
		}
	}
}

func TestOnlyExplicitCommitRegistersAndRunsReviewedScriptOnce(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err != nil || !result.Registered || !result.Connected || result.Status != "connected" {
		t.Fatalf("commit = %#v, %v", result, err)
	}
	if _, err := svc.Commit(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if b.registrations != 1 || b.verifications != 1 {
		t.Fatal("replayed commit repeated mutation")
	}
	installs := 0
	for _, call := range r.calls {
		if strings.Contains(strings.Join(call.args, " "), "test-install-secret") {
			t.Fatal("secret in command arguments")
		}
		if strings.Contains(call.stdin, "test-install-secret") {
			installs++
		}
	}
	if installs != 1 {
		t.Fatalf("installation ran %d times", installs)
	}
}

func TestChangedConfigurationInvalidatesInstallationPlanBeforeRegistration(t *testing.T) {
	svc, _, b, path := serviceFixture(t)
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Host dev\nHostName another.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Commit(t.Context(), plan.ID); err == nil {
		t.Fatal("accepted stale SSH configuration")
	}
	if b.registrations != 0 {
		t.Fatal("registered before detecting changed configuration")
	}
}

func TestMissingToolsAndExistingNodeBlockCommit(t *testing.T) {
	for _, probe := range []string{strings.ReplaceAll(completeProbe, "bash\t1", "bash\t0"), strings.ReplaceAll(completeProbe, "existing\t0", "existing\t1")} {
		t.Run(probe[len(probe)-3:], func(t *testing.T) {
			svc, r, b, _ := serviceFixture(t)
			r.checkOutput = probe
			plan, err := svc.Plan(t.Context(), installRequest())
			if err != nil || plan.Ready {
				t.Fatalf("plan should explain blocker: %#v, %v", plan, err)
			}
			if _, err := svc.Commit(t.Context(), plan.ID); err == nil {
				t.Fatal("committed blocked plan")
			}
			if b.registrations != 0 {
				t.Fatal("blocked plan mutated registration")
			}
		})
	}
}

func TestConnectionFailureIsActionableAndDoesNotLeakSSHStderr(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	r.stderr = "Permission denied (publickey) while ProxyCommand used secret-value"
	check, err := svc.Check(t.Context(), "dev")
	if err == nil || check.Reachable {
		t.Fatal("expected authentication failure")
	}
	var failure *StepError
	if !errors.As(err, &failure) || failure.Code != "authentication" || failure.Suggestion == "" {
		t.Fatalf("unhelpful error: %v", err)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatal("SSH secret leaked")
	}
	if _, err := svc.Check(t.Context(), "-oProxyCommand=evil"); err == nil {
		t.Fatal("accepted option injection")
	}
	if _, err := svc.Check(t.Context(), "unlisted"); err == nil {
		t.Fatal("connected to unselected destination")
	}
}

func TestFailedInstallKeepsRegistrationAndIsNotReplayed(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	r.failInstall = true
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err == nil || !result.Registered || result.Connected || result.Status != "needs_attention" {
		t.Fatalf("missing partial installation state: %#v %v", result, err)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatal("installation output leaked")
	}
	_, _ = svc.Commit(t.Context(), plan.ID)
	if b.registrations != 1 {
		t.Fatal("uncertain installation repeated")
	}
}

func TestExpiredPlanAndChangedBootstrapCannotInstall(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		svc, _, b, _ := serviceFixture(t)
		now := time.Now()
		svc.now = func() time.Time { return now }
		plan, err := svc.Plan(t.Context(), installRequest())
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(15 * time.Minute)
		if _, err := svc.Commit(t.Context(), plan.ID); err == nil {
			t.Fatal("accepted expired plan")
		}
		if b.registrations != 0 {
			t.Fatal("expired plan registered node")
		}
	})
	t.Run("bootstrap changed", func(t *testing.T) {
		svc, _, b, _ := serviceFixture(t)
		plan, err := svc.Plan(t.Context(), installRequest())
		if err != nil {
			t.Fatal(err)
		}
		b.script = "different installation\n"
		if _, err := svc.Commit(t.Context(), plan.ID); err == nil {
			t.Fatal("accepted changed bootstrap")
		}
		if b.registrations != 0 {
			t.Fatal("changed bootstrap registered node")
		}
	})
}

func TestPublicNetworkPlanChangeIsRejectedBeforeRegistration(t *testing.T) {
	svc, _, b, _ := serviceFixture(t)
	b.reviewID = "source-network-1"
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	b.reviewID = "source-network-2"
	if _, err := svc.Commit(t.Context(), plan.ID); err == nil {
		t.Fatal("changed network effects installed under old approval")
	}
	if b.registrations != 0 {
		t.Fatal("registration ran before reviewing changed network effects")
	}
}

func TestCommitPassesOnlyTheStoredReviewIDToRegistration(t *testing.T) {
	svc, _, b, _ := serviceFixture(t)
	b.reviewID = "approved-network"
	request := installRequest()
	request.ApprovedReviewID = "untrusted-request-value"
	plan, err := svc.Plan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Commit(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if b.approvedReviewID != "approved-network" {
		t.Fatal("registration did not receive stored approval identity")
	}
	raw, _ := json.Marshal(request)
	if strings.Contains(string(raw), "untrusted-request-value") {
		t.Fatal("approved review ID is accepted as client JSON")
	}
}

type operationVerifier struct {
	*fakeBackend
	operation string
}

func (b *operationVerifier) VerifyRegistration(_ context.Context, _ string, operation string) error {
	b.operation = operation
	return nil
}

func TestCommitCompletesExactEnrollmentOperation(t *testing.T) {
	svc, _, b, _ := serviceFixture(t)
	backend := &operationVerifier{fakeBackend: b}
	svc.backend = backend
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err != nil || !result.Connected || backend.operation != plan.ID || b.verifications != 0 {
		t.Fatalf("wrong membership verification: %#v %v op=%s", result, err, backend.operation)
	}
}

type recoveryBackend struct {
	*fakeBackend
	resumes int
}

func (b *recoveryBackend) ResumeRegistration(_ context.Context, id string) (InstallResult, error) {
	b.resumes++
	return InstallResult{PlanID: id, Name: "remote", NodeID: "node-recovered", Registered: true, Connected: true, Status: "connected"}, nil
}

func TestCommitAfterRestartOnlyReconcilesExistingOperation(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	backend := &recoveryBackend{fakeBackend: b}
	svc.backend = backend
	id := strings.Repeat("d", 48)
	result, err := svc.Commit(t.Context(), id)
	if err != nil || !result.Connected || backend.resumes != 1 || len(r.calls) != 0 || b.registrations != 0 {
		t.Fatalf("recovery repeated deployment: %#v %v", result, err)
	}
}

func TestRepeatedCommitAfterPartialFailureOnlyReconciles(t *testing.T) {
	svc, r, b, _ := serviceFixture(t)
	backend := &recoveryBackend{fakeBackend: b}
	svc.backend = backend
	r.failInstall = true
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result, err := svc.Commit(t.Context(), plan.ID); err == nil || !result.Registered {
		t.Fatalf("expected partial install %#v %v", result, err)
	}
	calls := len(r.calls)
	result, err := svc.Commit(t.Context(), plan.ID)
	if err != nil || !result.Connected || backend.resumes != 1 || len(r.calls) != calls || b.registrations != 1 {
		t.Fatalf("partial recovery repeated deployment: %#v %v", result, err)
	}
}

func TestRemoteProbeRequiresCompleteStructuredReply(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	r.checkOutput = "login banner\nSTEVE_CHECK\tos\tLinux\n"
	if _, err := svc.Check(t.Context(), "dev"); err == nil {
		t.Fatal("accepted incomplete probe")
	}
}

func TestSSHAgentCandidatesReuseCatalogAndAreOnlyAdvisory(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	r.checkOutput = completeProbe + "STEVE_CHECK\tpath_codex\t/remote/bin/codex\nSTEVE_CHECK\tpath_node\t/remote/bin/node\n"
	check, err := svc.Check(t.Context(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Agents) != 4 {
		t.Fatalf("shared catalog not used: %#v", check.Agents)
	}
	for _, candidate := range check.Agents {
		if candidate.ID == "codex" && (!candidate.Installed || candidate.Adapter != "codex-acp" || len(candidate.Requires) != 1 || candidate.Requires[0] != "npm") {
			t.Fatalf("remote candidate = %#v", candidate)
		}
		if candidate.Registered || candidate.Configured {
			t.Fatal("SSH file discovery implicitly registered an Agent")
		}
	}
}

func TestOpenSSHRejectsNonRegularConfigurationViaDiscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	svc := New(Options{ConfigPath: path, Runner: &recordingRunner{}})
	if _, err := svc.Check(t.Context(), "dev"); err == nil {
		t.Fatal("nonregular config selected a destination")
	}
}
