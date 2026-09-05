package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The autonomous scenario tells the coordinator only the goal. It has to
// look the fleet up, split the work, place each piece where its needs are
// met, let the children run in parallel, and report — with Steve
// delivering each child's result into the conversation as it ends, so
// the root task spans several exchanges. Everything is checked from the
// read model and the main directory, never from what the agent says.

const autonomousTimeout = 20 * time.Minute

type fleetState struct {
	Hub struct {
		Node    string `json:"node"`
		Version string `json:"version"`
	} `json:"hub"`
	Nodes []struct {
		Name         string   `json:"name"`
		Up           bool     `json:"up"`
		Capabilities []string `json:"capabilities"`
	} `json:"nodes"`
	Projects []project `json:"projects"`
	Tasks    []task    `json:"tasks"`
}

type taskDetail struct {
	Task     task   `json:"task"`
	Children []task `json:"children"`
	Attempts []struct {
		ID        string    `json:"id"`
		Kind      string    `json:"kind"`
		State     string    `json:"state"`
		Agent     string    `json:"agent"`
		Node      string    `json:"node"`
		Artifact  string    `json:"artifact"`
		StartedAt time.Time `json:"started_at"`
		EndedAt   time.Time `json:"ended_at"`
	} `json:"attempts"`
}

type toolCall struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type fullReply struct {
	reply
	Input   string `json:"input"`
	Process struct {
		Tools []toolCall `json:"tools"`
		Steps []step     `json:"steps"`
	} `json:"process"`
}

func finished(state string) bool { return state == "done" || state == "failed" || state == "cancelled" }

func toolName(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

func (g *gate) runAutonomous(ctx context.Context) error {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("create run ID: %w", err)
	}
	id := g.started.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(nonce[:])
	g.conversation = "console:e2e-auto-" + id
	release := "release-" + id
	g.log("AUTONOMOUS START hub=%s project=%s agent=%s timeout=%s", g.hub, g.project, g.agent, g.timeout)
	g.log("RUN conversation=%s release=%s", g.conversation, release)

	var initial fleetState
	if err := g.request(ctx, http.MethodGet, "/state", nil, &initial); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	home := ""
	for _, p := range initial.Projects {
		if p.ID == g.project && p.Node == initial.Hub.Node {
			home = p.Path
		}
	}
	if home == "" {
		return fmt.Errorf("preflight: project %q has no main directory on hub node %q", g.project, initial.Hub.Node)
	}
	if _, err := os.Stat(filepath.Join(home, "kvtool", "main.go")); err != nil {
		return fmt.Errorf("preflight: %s/kvtool/main.go must exist (the kvtool demo leaves it): %w", home, err)
	}
	builders := map[string]bool{}
	for _, n := range initial.Nodes {
		for _, c := range n.Capabilities {
			if c == "build" && n.Up && n.Name != initial.Hub.Node {
				builders[n.Name] = true
			}
		}
	}
	if len(builders) == 0 {
		return errors.New("preflight: no remote node advertises the build capability")
	}
	releaseDir := filepath.Join(home, "kvtool", release)
	g.log("PASS preflight hub_node=%s hub_version=%s home=%s builders=%v", initial.Hub.Node, initial.Hub.Version, home, keys(builders))

	if _, err := g.send(ctx, "project", "/project use "+g.project); err != nil {
		return err
	}
	if _, err := g.send(ctx, "agent", "/use "+g.agent); err != nil {
		return err
	}
	prompt := fmt.Sprintf("把 kvtool 做成可发布的样子，你只负责协调，两件事都不要自己动手：(1) 在有 build 能力的机器上编译 linux/amd64 二进制到 kvtool/%s/ 并在同一目录生成 SHA256SUMS（用 sha256sum 生成，条目里的文件名不带路径）；"+
		"(2) 文档：给 kvtool/README.md 补一节「安装与校验」（含 sha256sum -c 的步骤），并写 kvtool/%s/RELEASE.md（版本说明）——交给另一个 agent 去写，不要和编译的是同一个。"+
		"先用 steve_fleet 查一下集群里有哪些机器和能力，自己拆解任务、按能力挑合适的 agent 委派：编译必须委派到申报了 build 能力的机器，不要在 hub 上编译；两件事并行。"+
		"子任务完成后 Steve 会把结果送回这个会话，不必用 steve_await 等。都齐了之后汇总谁在哪台机器做了什么。", release, release)
	if _, err := g.send(ctx, "goal", prompt); err != nil {
		return err
	}

	root, err := g.rootTask(ctx)
	if err != nil {
		return err
	}
	g.taskID = root.ID
	g.log("PASS root task=#%s member=%s", root.ID, root.Member)

	// The task spans exchanges: wait until every child has ended, nothing
	// waits in the conversation's queue, and the coordinator has answered
	// after the last delivery.
	var detail taskDetail
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("total deadline while waiting for task #%s: %w", root.ID, err)
		}
		if err := g.request(ctx, http.MethodGet, "/console/tasks/"+url.PathEscape(root.ID), nil, &detail); err != nil {
			return err
		}
		allEnded := len(detail.Children) > 0
		for _, c := range detail.Children {
			if !finished(c.State) {
				allEnded = false
			}
		}
		if allEnded && g.quiet(ctx) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	g.log("PASS children ended count=%d", len(detail.Children))

	replies, err := g.transcript(ctx)
	if err != nil {
		return err
	}
	// 1. The fleet was looked up before anything was delegated.
	var order []string
	awaits := 0
	for _, r := range replies {
		if r.Kind != "reply" {
			continue
		}
		for _, t := range r.Process.Tools {
			name := toolName(t.Name)
			order = append(order, name)
			if name == "steve_await" {
				awaits++
			}
		}
	}
	fleetAt, delegateAt := index(order, "steve_fleet"), index(order, "steve_delegate")
	if fleetAt < 0 || delegateAt < 0 || fleetAt > delegateAt {
		return fmt.Errorf("tools: steve_fleet must be called before the first steve_delegate; order=%v", order)
	}
	g.log("PASS fleet-first tools=%d fleet_at=%d delegate_at=%d", len(order), fleetAt, delegateAt)

	// 2. At least two children done, one off the hub, two overlapping in time.
	type run struct {
		task, agent, node, attempt string
		start, end                 time.Time
	}
	var runs []run
	for _, c := range detail.Children {
		if c.State != "done" {
			return fmt.Errorf("child task #%s ended %s, not done", c.ID, c.State)
		}
		var cd taskDetail
		if err := g.request(ctx, http.MethodGet, "/console/tasks/"+url.PathEscape(c.ID), nil, &cd); err != nil {
			return err
		}
		for _, a := range cd.Attempts {
			if a.Kind == "delegate" && !a.StartedAt.IsZero() {
				runs = append(runs, run{task: c.ID, agent: a.Agent, node: a.Node, attempt: a.ID, start: a.StartedAt, end: a.EndedAt})
			}
		}
	}
	if len(runs) < 2 {
		return fmt.Errorf("expected at least two delegated children with attempts; got %d (children=%d)", len(runs), len(detail.Children))
	}
	remote := false
	for _, r := range runs {
		if r.node != "" && r.node != initial.Hub.Node {
			remote = true
		}
	}
	if !remote {
		return fmt.Errorf("no child ran off the hub: %+v", runs)
	}
	overlap := false
	for i := range runs {
		for j := i + 1; j < len(runs); j++ {
			a, b := runs[i], runs[j]
			if a.start.Before(endOr(b)) && b.start.Before(endOr(a)) {
				overlap = true
			}
		}
	}
	if !overlap {
		return fmt.Errorf("children ran one after another, not in parallel: %+v", runs)
	}
	g.log("PASS parallel children=%d remote=true overlap=true", len(runs))

	// 3. Whoever produced SHA256SUMS ran on a machine that advertises build.
	sumsRel := filepath.Join("kvtool", release, "SHA256SUMS")
	producer := ""
	for _, r := range runs {
		var idx struct {
			Changes []struct {
				Path string `json:"path"`
			} `json:"changes"`
		}
		if err := g.request(ctx, http.MethodGet, "/console/attempts/"+url.PathEscape(r.attempt)+"/changes", nil, &idx); err != nil {
			return err
		}
		for _, ch := range idx.Changes {
			if ch.Path == sumsRel {
				producer = r.node
				g.log("PASS producer task=#%s agent=%s node=%s attempt=%s", r.task, r.agent, r.node, r.attempt)
			}
		}
	}
	if producer == "" {
		return fmt.Errorf("no child's changes include %s", sumsRel)
	}
	if !builders[producer] {
		return fmt.Errorf("%s was produced on %s, which does not advertise build (builders=%v)", sumsRel, producer, keys(builders))
	}

	// 4. The files are in the main directory and hold together.
	for _, name := range []string{"SHA256SUMS", "RELEASE.md"} {
		if _, err := os.Stat(filepath.Join(releaseDir, name)); err != nil {
			return fmt.Errorf("landing: %s: %w", filepath.Join(releaseDir, name), err)
		}
	}
	readme, err := os.ReadFile(filepath.Join(home, "kvtool", "README.md"))
	if err != nil || !strings.Contains(string(readme), "校验") {
		return fmt.Errorf("README.md lacks a 校验 section (err=%v)", err)
	}
	check := exec.CommandContext(ctx, "sha256sum", "-c", "SHA256SUMS")
	check.Dir = releaseDir
	if out, err := check.CombinedOutput(); err != nil {
		return fmt.Errorf("sha256sum -c failed in %s: %v: %s", releaseDir, err, clip(string(out)))
	}
	sums, _ := os.ReadFile(filepath.Join(releaseDir, "SHA256SUMS"))
	binary := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			binary = strings.TrimPrefix(fields[1], "*")
			break
		}
	}
	if binary == "" {
		return errors.New("SHA256SUMS names no file")
	}
	kind, err := exec.CommandContext(ctx, "file", "-b", filepath.Join(releaseDir, binary)).Output()
	if err != nil || !strings.Contains(string(kind), "ELF") || !strings.Contains(string(kind), "x86-64") {
		return fmt.Errorf("%s is not an ELF x86-64 binary: %q (err=%v)", binary, strings.TrimSpace(string(kind)), err)
	}
	g.log("PASS landing dir=%s binary=%s kind=%q checksums=ok readme=校验", releaseDir, binary, strings.TrimSpace(string(kind)))

	// 5. Every child has a successful attempt row, and the spend of at
	// least one was reported. A harness that reports no usage (grok,
	// today) is the harness's gap, recorded honestly as reported=false;
	// the coordinator picks whichever idle agent it likes, so that gap
	// is a warning here, not a failure of the platform under test.
	var current fleetState
	if err := g.request(ctx, http.MethodGet, "/state", nil, &current); err != nil {
		return err
	}
	reportedChildren := 0
	for _, r := range runs {
		var row *attemptRow
		for _, t := range current.Tasks {
			if t.ID != r.task {
				continue
			}
			for i := range t.AttemptRows {
				if t.AttemptRows[i].Outcome == "ok" {
					row = &t.AttemptRows[i]
				}
			}
		}
		if row == nil {
			return fmt.Errorf("task #%s: no successful attempt row", r.task)
		}
		if row.Reported && row.Tokens.present() {
			reportedChildren++
		} else {
			g.log("WARN usage unreported task=#%s agent=%s node=%s (the harness reports no usage)", r.task, r.agent, r.node)
		}
	}
	if reportedChildren == 0 {
		return fmt.Errorf("no child reported its usage (%d children)", len(runs))
	}
	g.log("PASS usage reported=%d/%d children", reportedChildren, len(runs))

	// 6. The coordinator did not sit polling.
	if awaits > 2*len(runs) {
		return fmt.Errorf("steve_await called %d times for %d children; the parent should not hold its turn waiting", awaits, len(runs))
	}
	g.log("PASS awaits=%d children=%d", awaits, len(runs))
	g.log("AUTONOMOUS PASS elapsed=%s conversation=%s task=#%s children=%d", time.Since(g.started).Round(time.Millisecond), g.conversation, root.ID, len(runs))
	return nil
}

// rootTask finds the task this conversation opened for the coordinator.
func (g *gate) rootTask(ctx context.Context) (task, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var s fleetState
		if err := g.request(ctx, http.MethodGet, "/state", nil, &s); err != nil {
			return task{}, err
		}
		for _, t := range s.Tasks {
			if t.Channel == g.conversation && t.Parent == "" && t.Member == g.agent {
				return t, nil
			}
		}
		if time.Now().After(deadline) {
			return task{}, fmt.Errorf("no root task for %s in /state", g.conversation)
		}
		time.Sleep(5 * time.Second)
	}
}

// quiet says nothing is queued or running in the conversation and the
// last line is the coordinator's answer, not a delivery waiting for one.
func (g *gate) quiet(ctx context.Context) bool {
	var q struct {
		Queue []struct {
			State string `json:"state"`
		} `json:"queue"`
	}
	if err := g.request(ctx, http.MethodGet, "/console/queue?conversation="+url.QueryEscape(g.conversation), nil, &q); err != nil {
		return false
	}
	for _, e := range q.Queue {
		if e.State == "queued" || e.State == "running" {
			return false
		}
	}
	replies, err := g.transcript(ctx)
	if err != nil || len(replies) == 0 {
		return false
	}
	last := replies[len(replies)-1]
	return last.Kind == "reply" && last.Error == ""
}

func (g *gate) transcript(ctx context.Context) ([]fullReply, error) {
	var t struct {
		Replies []fullReply `json:"replies"`
	}
	if err := g.request(ctx, http.MethodGet, "/console/replies?conversation="+url.QueryEscape(g.conversation), nil, &t); err != nil {
		return nil, err
	}
	sort.SliceStable(t.Replies, func(i, j int) bool { return t.Replies[i].At.Before(t.Replies[j].At) })
	return t.Replies, nil
}

func index(list []string, name string) int {
	for i, v := range list {
		if v == name {
			return i
		}
	}
	return -1
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// endOr is an attempt's end, or now for one still running.
func endOr(r struct {
	task, agent, node, attempt string
	start, end                 time.Time
}) time.Time {
	if r.end.IsZero() {
		return time.Now()
	}
	return r.end
}
