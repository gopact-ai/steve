package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/gopact-ai/acp"
)

type result struct {
	TaskID                     string `json:"task_id"`
	Agent, Node, State, Answer string
}

var oneTask = regexp.MustCompile(`delegate one task to agent "([^"]+)" on node "([^"]+)": create the new file "([^"]+)".*containing exactly the single line "([^"]+)"`)
var releaseName = regexp.MustCompile(`kvtool/(release-[A-Za-z0-9-]+)/`)
var readyAgent = regexp.MustCompile(`(?m)^- (\S+) on (\S+) \([^\n]+\) — ready; meets the requirement`)

func (a *agent) delegateOne(ctx context.Context, id acp.SessionID, s *session, input string) (string, error) {
	m := oneTask.FindStringSubmatch(input)
	if len(m) != 5 {
		return "", errors.New("unrecognized fleet delegation brief")
	}
	goal, err := encodeJob(job{Kind: "file", Path: m[3], Content: m[4] + "\n"})
	if err != nil {
		return "", err
	}
	text, err := a.tool(ctx, id, s, "steve_delegate", map[string]any{"agent": m[1], "goal": goal})
	if err != nil {
		return "", err
	}
	for {
		var r result
		if err = json.Unmarshal([]byte(text), &r); err != nil {
			return "", err
		}
		if r.Agent != m[1] || r.Node != m[2] {
			return "", fmt.Errorf("delegation selected unexpected participant %s on %s", r.Agent, r.Node)
		}
		if r.State == "done" {
			return fmt.Sprintf("Task %s by %s on %s: %s", r.TaskID, r.Agent, r.Node, r.Answer), nil
		}
		if r.State != "running" {
			return "", fmt.Errorf("delegation ended %s: %s", r.State, text)
		}
		text, err = a.tool(ctx, id, s, "steve_await", map[string]any{"task_id": r.TaskID, "wait_seconds": 50})
		if err != nil {
			return "", err
		}
	}
}

func (a *agent) autonomous(ctx context.Context, id acp.SessionID, s *session, input string) (string, error) {
	m := releaseName.FindStringSubmatch(input)
	if len(m) != 2 {
		return "", errors.New("release directory is missing")
	}
	barrier, token := os.Getenv("STEVE_LAB_BARRIER"), os.Getenv("STEVE_LAB_BARRIER_TOKEN")
	if barrier == "" || token == "" {
		return "", errors.New("lab concurrency barrier was not configured")
	}
	selected := map[string]bool{}
	var tasks []string
	for _, kind := range []string{"build", "docs"} {
		fleet, err := a.tool(ctx, id, s, "steve_fleet", map[string]any{"requires": []string{kind}})
		if err != nil {
			return "", err
		}
		candidate := ""
		for _, line := range readyAgent.FindAllStringSubmatch(fleet, -1) {
			if !selected[line[1]] {
				candidate = line[1]
				break
			}
		}
		if candidate == "" {
			return "", fmt.Errorf("no distinct ready agent offers %s: %s", kind, fleet)
		}
		selected[candidate] = true
		goal, err := encodeJob(job{Kind: kind, Release: m[1], Barrier: barrier, Token: token})
		if err != nil {
			return "", err
		}
		text, err := a.tool(ctx, id, s, "steve_delegate", map[string]any{"agent": candidate, "requires": []string{kind}, "goal": goal})
		if err != nil {
			return "", err
		}
		var r result
		if err = json.Unmarshal([]byte(text), &r); err != nil {
			return "", err
		}
		if r.State != "running" && r.State != "done" {
			return "", fmt.Errorf("child could not start: %s", text)
		}
		tasks = append(tasks, fmt.Sprintf("%s on %s (task %s)", r.Agent, r.Node, r.TaskID))
	}
	// Return immediately. The real delivery queue resumes the coordinator
	// after each child completes; there is no test-side polling or result injection.
	return "Dispatched " + strings.Join(tasks, ", ") + "; awaiting Steve's delivery.", nil
}

func encodeJob(j job) (string, error) {
	raw, err := json.Marshal(j)
	return "FLEETLAB_JOB=" + base64.RawURLEncoding.EncodeToString(raw), err
}
