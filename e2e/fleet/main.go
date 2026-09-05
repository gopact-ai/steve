// Command fleet checks delegation, snapshots, usage and landing on a live hub.
// It deliberately uses only the console HTTP API and the local project directory.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const totalTimeout = 10 * time.Minute

type options struct {
	hub, token, project, agent, targetNode, targetAgent string
	scenario                                            string
	timeout                                             time.Duration
}

func main() {
	opts, err := parseOptions(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FLEET FAIL:", err)
		os.Exit(1)
	}
	g := newGate(opts, os.Stdout)
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	if opts.scenario == "autonomous" {
		err = g.runAutonomous(ctx)
	} else {
		err = g.run(ctx)
	}
	if err != nil {
		g.diagnostics(ctx)
	}
	cancel()
	g.client.CloseIdleConnections()
	if err != nil {
		g.log("FLEET FAIL elapsed=%s conversation=%s command_id=%s file=%s: %v",
			time.Since(g.started).Round(time.Millisecond), g.conversation, g.commandID, g.filename, err)
		os.Exit(1)
	}
}

func parseOptions(args []string, out io.Writer) (options, error) {
	var o options
	f := flag.NewFlagSet("fleet", flag.ContinueOnError)
	f.SetOutput(out)
	f.StringVar(&o.hub, "hub", "", "hub read-model URL (HUB, then gateway.read_model_addr)")
	// Resolve TOKEN after parsing so -help never prints the credential.
	f.StringVar(&o.token, "token", "", "read-model token (TOKEN, then gateway.read_model_token)")
	config := f.String("config", "config.e2e.json", "local hub connection config")
	f.StringVar(&o.project, "project", env("PROJECT", "scratch"), "project with its main directory on this hub")
	f.StringVar(&o.agent, "agent", env("AGENT", "claude"), "coordinating agent on the hub")
	f.StringVar(&o.targetNode, "target-node", env("TARGET_NODE", "node-b"), "remote node")
	f.StringVar(&o.targetAgent, "target-agent", env("TARGET_AGENT", "shipper"), "agent on the remote node")
	f.StringVar(&o.scenario, "scenario", env("SCENARIO", "delegate"), "delegate (one named delegation) or autonomous (the agent splits and places the work itself)")
	f.DurationVar(&o.timeout, "timeout", totalTimeout, "total client deadline (at most 10m; 20m for autonomous, its default)")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	limit := totalTimeout
	switch o.scenario {
	case "delegate":
	case "autonomous":
		limit = autonomousTimeout
	default:
		return o, fmt.Errorf("-scenario must be delegate or autonomous, not %q", o.scenario)
	}
	timeoutSet := false
	f.Visit(func(fl *flag.Flag) {
		if fl.Name == "timeout" {
			timeoutSet = true
		}
	})
	if !timeoutSet {
		o.timeout = limit
	}
	if f.NArg() != 0 {
		return o, errors.New("unexpected positional arguments; use -help")
	}
	if o.timeout <= 0 || o.timeout > limit {
		return o, fmt.Errorf("-timeout must be greater than zero and at most %s", limit)
	}
	for name, value := range map[string]string{"project": o.project, "agent": o.agent, "target-node": o.targetNode, "target-agent": o.targetAgent} {
		if value == "" || strings.ContainsAny(value, " \t\r\n") {
			return o, fmt.Errorf("-%s must be a nonempty ID without whitespace", name)
		}
	}
	if o.hub == "" {
		o.hub = os.Getenv("HUB")
	}
	if o.token == "" {
		o.token = os.Getenv("TOKEN")
	}
	if o.hub == "" || o.token == "" {
		var cfg struct {
			Gateway struct {
				Addr  string `json:"read_model_addr"`
				Token string `json:"read_model_token"`
			} `json:"gateway"`
		}
		raw, err := os.ReadFile(*config)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return o, fmt.Errorf("read config %s: %w", *config, err)
		}
		if err == nil {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return o, fmt.Errorf("decode config %s: %w", *config, err)
			}
		}
		if o.hub == "" {
			o.hub = cfg.Gateway.Addr
		}
		if o.token == "" {
			o.token = cfg.Gateway.Token
		}
	}
	if o.token == "" {
		return o, fmt.Errorf("missing token: set TOKEN or -token, or gateway.read_model_token in %s", *config)
	}
	if o.hub == "" {
		o.hub = "http://127.0.0.1:7710"
	}
	var err error
	o.hub, err = hubURL(o.hub)
	return o, err
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func hubURL(addr string) (string, error) {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("hub must be an HTTP(S) URL without credentials, query or fragment")
	}
	// A listening address in config is not a destination; run on the hub.
	if u.Hostname() == "" || net.ParseIP(u.Hostname()).IsUnspecified() {
		if u.Port() == "" {
			return "", errors.New("hub address needs a host or listening port")
		}
		u.Host = net.JoinHostPort("127.0.0.1", u.Port())
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// Keep the wire shapes independent of server implementation so the gate can
// check a deployed binary from a newer checkout, using only the standard library.
type agent struct {
	ID       string `json:"id"`
	Node     string `json:"node"`
	Eligible bool   `json:"eligible"`
	Why      string `json:"why"`
}

type project struct {
	ID   string `json:"id"`
	Node string `json:"node"`
	Path string `json:"path"`
}

type tokens struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CachedRead  int64 `json:"cached_read"`
	CachedWrite int64 `json:"cached_write"`
	Total       int64 `json:"total"`
}

func (t tokens) present() bool {
	return t.Input > 0 || t.Output > 0 || t.CachedRead > 0 || t.CachedWrite > 0 || t.Total > 0
}

type attemptRow struct {
	Agent    string `json:"agent"`
	Node     string `json:"node"`
	Outcome  string `json:"outcome"`
	Tokens   tokens `json:"tokens"`
	Reported bool   `json:"reported"`
}

type task struct {
	ID          string       `json:"id"`
	Parent      string       `json:"parent"`
	State       string       `json:"state"`
	Member      string       `json:"member"`
	Node        string       `json:"node"`
	Project     string       `json:"project_id"`
	Channel     string       `json:"channel"`
	AttemptRows []attemptRow `json:"attempt_rows"`
}

type state struct {
	Hub struct {
		Node    string `json:"node"`
		Version string `json:"version"`
	} `json:"hub"`
	Agents   []agent   `json:"agents"`
	Projects []project `json:"projects"`
	Tasks    []task    `json:"tasks"`
}

type step struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Agent   string `json:"agent"`
	Node    string `json:"node"`
	Attempt string `json:"attempt"`
	Answer  string `json:"answer"`
}

type reply struct {
	ID           string    `json:"id"`
	At           time.Time `json:"at"`
	Conversation string    `json:"conversation"`
	Kind         string    `json:"kind"`
	Error        string    `json:"error"`
	Text         string    `json:"text"`
	Process      struct {
		Steps []step `json:"steps"`
	} `json:"process"`
	Injected struct {
		Project string `json:"project"`
		Agent   string `json:"agent"`
	} `json:"injected"`
}

type attempt struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Agent    string `json:"agent"`
	Node     string `json:"node"`
	Artifact string `json:"artifact"`
}

type gate struct {
	options
	client                                                        *http.Client
	out                                                           io.Writer
	started                                                       time.Time
	conversation, commandID, replyID, taskID, attemptID, filename string
}

func newGate(o options, out io.Writer) *gate {
	return &gate{options: o, out: out, started: time.Now(), client: &http.Client{
		// The synchronous send may occupy the entire remaining deadline.
		// Never retry a POST or follow a redirect to another console.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (g *gate) log(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if g.token != "" {
		line = strings.ReplaceAll(line, g.token, "[REDACTED]")
	}
	fmt.Fprintln(g.out, line)
}

func (g *gate) request(ctx context.Context, method, path string, body, result any) error {
	if method == http.MethodGet {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, g.hub+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		message := strings.TrimSpace(string(raw))
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &failure) == nil && failure.Error != "" {
			message = failure.Error
		}
		return fmt.Errorf("%s %s: HTTP %s: %s", method, path, resp.Status, clip(message))
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (g *gate) send(ctx context.Context, phase, input string) (reply, error) {
	g.commandID = g.conversation + "-" + phase
	g.log("SEND %s command_id=%s", phase, g.commandID)
	var response struct {
		Reply reply  `json:"reply"`
		Error string `json:"error"`
	}
	err := g.request(ctx, http.MethodPost, "/console/send", map[string]string{
		"conversation": g.conversation, "input": input, "command_id": g.commandID,
	}, &response)
	if err != nil {
		return reply{}, fmt.Errorf("%s: %w", phase, err)
	}
	r := response.Reply
	if response.Error != "" || r.Error != "" || r.Kind != "reply" || r.Conversation != g.conversation || r.At.IsZero() {
		return r, fmt.Errorf("%s: invalid reply: error=%q reply.error=%q kind=%q conversation=%q text=%q",
			phase, response.Error, r.Error, r.Kind, r.Conversation, clip(r.Text))
	}
	return r, nil
}

func (g *gate) run(ctx context.Context) error {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("create run ID: %w", err)
	}
	id := g.started.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(nonce[:])
	g.conversation = "console:e2e-fleet-" + id
	g.filename = "e2e-fleet-" + id + ".txt"
	content := "fleet e2e " + id + "\n"
	g.log("FLEET START hub=%s project=%s agent=%s target=%s/%s timeout=%s", g.hub, g.project, g.agent, g.targetNode, g.targetAgent, g.timeout)
	g.log("RUN conversation=%s file=%s", g.conversation, g.filename)

	var initial state
	if err := g.request(ctx, http.MethodGet, "/state", nil, &initial); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	home, err := g.preflight(initial)
	if err != nil {
		return err
	}
	path := filepath.Join(home, g.filename)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("preflight: expected new file %s to be absent (stat error: %v)", path, err)
	}
	g.log("PASS preflight hub_node=%s hub_version=%s home=%s", initial.Hub.Node, initial.Hub.Version, home)
	if _, err := g.send(ctx, "project", "/project use "+g.project); err != nil {
		return err
	}
	if _, err := g.send(ctx, "agent", "/use "+g.agent); err != nil {
		return err
	}
	prompt := fmt.Sprintf(`Run one small fleet regression in project %q. Use steve_delegate to delegate one task to agent %q on node %q: create the new file %q at the root of the supplied project worktree, containing exactly the single line %q followed by a newline. The child must not git init or git commit, and must not change any other files. You only coordinate; do not create or edit this file yourself or use SSH to bypass delegation. Await the child until it is done and its result has landed in the main project directory, then give a short final reply.`, g.project, g.targetAgent, g.targetNode, g.filename, strings.TrimSuffix(content, "\n"))
	first, err := g.send(ctx, "delegate", prompt)
	if err != nil {
		return err
	}
	// Send's reply may have no ID. Match its timestamp to the persisted final
	// reply in this fresh conversation, never to a verb reply or milestone.
	var transcript struct {
		Replies []reply `json:"replies"`
	}
	if err := g.request(ctx, http.MethodGet, "/console/replies?conversation="+url.QueryEscape(g.conversation), nil, &transcript); err != nil {
		return err
	}
	var final *reply
	for i := range transcript.Replies {
		r := &transcript.Replies[i]
		if r.Kind == "reply" && r.Conversation == g.conversation && r.At.Equal(first.At) {
			final = r
			break
		}
	}
	if final == nil || final.ID == "" {
		return fmt.Errorf("final reply at %s missing from persisted transcript (%d replies)", first.At.Format(time.RFC3339Nano), len(transcript.Replies))
	}
	g.replyID = final.ID
	if final.Error != "" || final.Injected.Project != g.project || final.Injected.Agent != g.agent {
		return fmt.Errorf("reply=%s: expected project=%s agent=%s; got project=%s agent=%s error=%q", final.ID, g.project, g.agent, final.Injected.Project, final.Injected.Agent, final.Error)
	}
	child, err := g.delegation(*final)
	if err != nil {
		return err
	}
	g.taskID, g.attemptID = strings.TrimPrefix(child.ID, "#"), child.Attempt
	g.log("PASS reply=%s step=%s kind=delegate state=done agent=%s node=%s attempt=%s", final.ID, child.ID, child.Agent, child.Node, child.Attempt)

	var detail struct {
		Task     task      `json:"task"`
		Attempts []attempt `json:"attempts"`
	}
	if err := g.request(ctx, http.MethodGet, "/console/tasks/"+url.PathEscape(g.taskID), nil, &detail); err != nil {
		return err
	}
	if err := g.checkTask(detail.Task); err != nil {
		return err
	}
	var matched bool
	for _, a := range detail.Attempts {
		if a.ID == g.attemptID && a.Kind == "delegate" && a.Agent == g.targetAgent && a.Node == g.targetNode && a.Artifact != "" {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("task #%s: delegate attempt %s missing or has wrong agent/node/artifact: %+v", g.taskID, g.attemptID, detail.Attempts)
	}
	g.log("PASS task=#%s parent=#%s project=%s attempt=%s", g.taskID, detail.Task.Parent, detail.Task.Project, g.attemptID)
	var index struct {
		Attempt   string `json:"attempt"`
		Project   string `json:"project"`
		Note      string `json:"note"`
		Truncated bool   `json:"truncated"`
		Changes   []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"changes"`
	}
	if err := g.request(ctx, http.MethodGet, "/console/attempts/"+url.PathEscape(g.attemptID)+"/changes", nil, &index); err != nil {
		return err
	}
	matched = false
	for _, c := range index.Changes {
		if c.Path == g.filename && c.Status == "A" {
			matched = true
		}
	}
	if index.Attempt != g.attemptID || index.Project != g.project || !matched {
		return fmt.Errorf("attempt %s: changes index must contain A %s in project %s; got %+v", g.attemptID, g.filename, g.project, index)
	}
	g.log("PASS changes attempt=%s status=A path=%s", g.attemptID, g.filename)
	var current state
	if err := g.request(ctx, http.MethodGet, "/state", nil, &current); err != nil {
		return err
	}
	usage, err := g.usage(current)
	if err != nil {
		return err
	}
	g.log("PASS usage task=#%s agent=%s node=%s reported=true input=%d output=%d cached_read=%d cached_write=%d total=%d", g.taskID, g.targetAgent, g.targetNode, usage.Input, usage.Output, usage.CachedRead, usage.CachedWrite, usage.Total)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("landing: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(content)) {
		return fmt.Errorf("landing: %s must be a regular file with %d bytes; mode=%s bytes=%d", path, len(content), info.Mode(), info.Size())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("landing: read %s: %w", path, err)
	}
	if string(raw) != content {
		return fmt.Errorf("landing: %s has wrong content: got %q, want %q", path, clip(string(raw)), content)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("total deadline: %w", err)
	}
	g.log("PASS landing path=%s bytes=%d content=%q", path, info.Size(), strings.TrimSuffix(content, "\n"))
	g.log("FLEET PASS elapsed=%s conversation=%s task=#%s attempt=%s", time.Since(g.started).Round(time.Millisecond), g.conversation, g.taskID, g.attemptID)
	return nil
}

func (g *gate) preflight(s state) (string, error) {
	if s.Hub.Node == "" || g.targetNode == s.Hub.Node {
		return "", fmt.Errorf("preflight: target node %q must differ from hub node %q", g.targetNode, s.Hub.Node)
	}
	for _, want := range []struct{ id, node string }{{g.agent, s.Hub.Node}, {g.targetAgent, g.targetNode}} {
		found := false
		for _, a := range s.Agents {
			if a.ID == want.id {
				if a.Node != want.node || !a.Eligible {
					return "", fmt.Errorf("preflight: agent=%s expected node=%s eligible=true; got node=%s eligible=%t why=%s", a.ID, want.node, a.Node, a.Eligible, a.Why)
				}
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("preflight: agent %q not found on node %s", want.id, want.node)
		}
	}
	for _, p := range s.Projects {
		if p.ID == g.project {
			if p.Node != s.Hub.Node || !filepath.IsAbs(p.Path) {
				return "", fmt.Errorf("preflight: project=%s must have its main directory on hub=%s; node=%s path=%q", p.ID, s.Hub.Node, p.Node, p.Path)
			}
			info, err := os.Stat(p.Path)
			if err != nil {
				return "", fmt.Errorf("preflight: run on the hub; stat project directory %s: %w", p.Path, err)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("preflight: project path %s is not a directory", p.Path)
			}
			return p.Path, nil
		}
	}
	return "", fmt.Errorf("preflight: project %q not found", g.project)
}

func (g *gate) delegation(r reply) (step, error) {
	var observed []string
	for _, s := range r.Process.Steps {
		if s.Kind == "delegate" && s.State == "done" && s.Agent == g.targetAgent && s.Node == g.targetNode && strings.TrimPrefix(s.ID, "#") != "" && s.Attempt != "" {
			return s, nil
		}
		observed = append(observed, fmt.Sprintf("step=%s kind=%s state=%s agent=%s node=%s attempt=%s answer=%q", s.ID, s.Kind, s.State, s.Agent, s.Node, s.Attempt, clip(s.Answer)))
	}
	return step{}, fmt.Errorf("reply=%s: no completed delegate with an attempt on %s/%s; process.steps=[%s]; reply.text=%q", r.ID, g.targetNode, g.targetAgent, strings.Join(observed, "; "), clip(r.Text))
}

func (g *gate) checkTask(t task) error {
	if t.ID != g.taskID || t.Parent == "" || t.State != "done" || t.Project != g.project || t.Channel != g.conversation || t.Member != g.targetAgent || t.Node != g.targetNode {
		return fmt.Errorf("task #%s: expected done child in conversation=%s project=%s on %s/%s; got %+v", g.taskID, g.conversation, g.project, g.targetNode, g.targetAgent, t)
	}
	return nil
}

func (g *gate) usage(s state) (tokens, error) {
	for _, t := range s.Tasks {
		if t.ID != g.taskID {
			continue
		}
		if err := g.checkTask(t); err != nil {
			return tokens{}, err
		}
		for _, row := range t.AttemptRows {
			if row.Agent == g.targetAgent && row.Node == g.targetNode && row.Outcome == "ok" && row.Reported && row.Tokens.present() {
				return row.Tokens, nil
			}
		}
		return tokens{}, fmt.Errorf("/state task #%s attempt=%s: expected successful attempt_rows with tokens and reported=true on %s/%s; got %+v", g.taskID, g.attemptID, g.targetNode, g.targetAgent, t.AttemptRows)
	}
	return tokens{}, fmt.Errorf("/state: task #%s attempt=%s is missing", g.taskID, g.attemptID)
}

// A failed send can still have created a task. Report only this run's IDs,
// within the remaining total deadline; never retry or cancel the work.
func (g *gate) diagnostics(ctx context.Context) {
	if ctx.Err() != nil || g.conversation == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var s state
	if err := g.request(ctx, http.MethodGet, "/state", nil, &s); err != nil {
		g.log("DIAG state: %v", err)
		return
	}
	for _, t := range s.Tasks {
		if t.Channel != g.conversation {
			continue
		}
		g.log("DIAG task=#%s parent=%s state=%s agent=%s node=%s", t.ID, t.Parent, t.State, t.Member, t.Node)
		var detail struct {
			Attempts []attempt `json:"attempts"`
		}
		if err := g.request(ctx, http.MethodGet, "/console/tasks/"+url.PathEscape(t.ID), nil, &detail); err != nil {
			g.log("DIAG task=#%s: %v", t.ID, err)
			continue
		}
		for _, a := range detail.Attempts {
			g.log("DIAG task=#%s attempt=%s kind=%s state=%s agent=%s node=%s", t.ID, a.ID, a.Kind, a.State, a.Agent, a.Node)
		}
	}
	var transcript struct {
		Replies []reply `json:"replies"`
	}
	if err := g.request(ctx, http.MethodGet, "/console/replies?conversation="+url.QueryEscape(g.conversation), nil, &transcript); err != nil {
		g.log("DIAG replies: %v", err)
		return
	}
	for _, r := range transcript.Replies {
		if r.Conversation == g.conversation && r.Kind == "reply" {
			g.log("DIAG reply=%s at=%s error=%q", r.ID, r.At.Format(time.RFC3339Nano), clip(r.Error))
		}
	}
}

func clip(s string) string {
	const limit = 600
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
