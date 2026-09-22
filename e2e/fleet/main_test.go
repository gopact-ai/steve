package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
	tasks "github.com/gopact-ai/steve/internal/task"
)

// Exercise the whole client against the HTTP contract. Only the fake remote
// delegate writes the file; a green card alone must never make the gate pass.
func TestGate(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"pass", ""},
		{"failed_card", "no completed delegate"},
		{"wrong_node", "no completed delegate"},
		{"no_attempt_id", "no completed delegate"},
		{"stale_reply", "missing from persisted transcript"},
		{"wrong_project", "expected project=scratch"},
		{"wrong_task", "expected done child"},
		{"no_attempt", "delegate attempt child-attempt missing"},
		{"no_change", "changes index must contain A"},
		{"deleted_file", "changes index must contain A"},
		{"wrong_index", "changes index must contain A"},
		{"unreported", "expected successful accounting row"},
		{"empty_tokens", "expected successful accounting row"},
		{"parent_tokens_only", "expected successful accounting row"},
		{"usage_failed", "/usage: ledger offline"},
		{"usage_partial", "/usage: incomplete history"},
		{"usage_unwired", "/usage: ledger-usage is not wired"},
		{"usage_missing_source", "/usage: missing ledger-usage source"},
		{"usage_missing", "/usage: missing usage"},
		{"usage_zero", "/usage: missing reported tokens for agent shipper"},
		{"usage_other_agent", "/usage: missing reported tokens for agent shipper"},
		{"usage_unreported", "/usage: missing reported tokens for agent shipper"},
		{"usage_incomplete_tokens", "/usage: missing reported tokens for agent shipper"},
		{"usage_http_error", "GET /usage: HTTP 503"},
		{"missing_file", "landing: stat"},
		{"wrong_content", "has wrong content"},
		{"symlink", "must be a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			var output bytes.Buffer
			g := newGate(options{token: "secret-token", project: "scratch", agent: "claude", targetNode: "node-b", targetAgent: "shipper", timeout: time.Second}, &output)
			var sent int
			var usageReads int
			var stored reply
			used := make(map[string]bool)
			// The task detail and the attempt page are served as the hub
			// serves them — the read model's own types — so the client is
			// checked against the contract, not against a copy of it.
			child := func() readmodel.TaskDetail {
				taskID := "child"
				if tc.name == "wrong_task" {
					taskID = "unrelated"
				}
				row := readmodel.AttemptRow{Agent: "shipper", Node: "node-b", Outcome: "ok", Reported: tc.name != "unreported", Tokens: readmodel.Tokens{Input: 10, Output: 2, Total: 12}}
				if tc.name == "empty_tokens" {
					row.Tokens = readmodel.Tokens{}
				}
				tsk := readmodel.Task{ID: taskID, Parent: "parent", State: tasks.StateDone, Member: "shipper", NodeID: "node-b", ProjectID: "scratch", Channel: g.conversation}
				accounting := readmodel.AccountingPage{Items: []readmodel.AccountingItem{{AttemptRow: row}}, Total: 1}
				if tc.name == "parent_tokens_only" {
					accounting = readmodel.AccountingPage{Items: []readmodel.AccountingItem{}}
				}
				return readmodel.TaskDetail{Task: tsk, Children: readmodel.TaskPage{Items: []readmodel.Task{}}, Accounting: accounting}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer secret-token" || r.URL.Query().Has("token") {
					t.Error("credential must be sent only in the Authorization header")
				}
				var response any
				switch r.Method + " " + r.URL.Path {
				case "GET /state":
					s := state{Agents: []agent{{ID: "claude", Node: "hub", Eligible: true}, {ID: "shipper", Node: "node-b", Eligible: true}}, Projects: []project{{ID: "scratch", Node: "hub", Path: home}}}
					s.Hub.Node, s.Hub.Version = "hub", "test-version"
					// Base state is not the usage contract. Child evidence must
					// come from task detail, aggregates from /usage.
					s.Tasks = []task{{ID: "parent"}, {ID: "child"}}
					response = s
				case "GET /usage":
					usageReads++
					if tc.name == "usage_http_error" {
						http.Error(w, "usage unavailable", http.StatusServiceUnavailable)
						return
					}
					health := map[string]any{"name": "ledger-usage", "wired": tc.name != "usage_unwired"}
					sources := []any{health}
					if tc.name == "usage_failed" {
						health["error"] = "ledger offline"
					}
					if tc.name == "usage_partial" {
						health["error"] = "incomplete history"
					}
					if tc.name == "usage_missing_source" {
						sources = nil
					}
					total := tokens{Input: 100, Output: 20, Total: 120}
					if tc.name == "usage_zero" {
						total = tokens{}
					}
					row := map[string]any{"key": "shipper", "attempts": 1, "tokens": total}
					switch tc.name {
					case "usage_other_agent":
						row["key"] = "coordinator"
					case "usage_unreported":
						row["unreported"] = 1
					case "usage_incomplete_tokens":
						row["tokens"] = tokens{Input: 9, Output: 2, Total: 11}
					}
					response = map[string]any{"at": time.Now(), "sources": sources, "usage": map[string]any{
						"total":    map[string]any{"attempts": 2, "tokens": total},
						"by_agent": []any{row},
					}}
					if tc.name == "usage_missing" || tc.name == "usage_failed" {
						delete(response.(map[string]any), "usage")
					}
				case "POST /console/send":
					var req struct {
						Conversation string `json:"conversation"`
						Input        string `json:"input"`
						CommandID    string `json:"command_id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						return
					}
					if req.Conversation != g.conversation || !strings.HasPrefix(req.Conversation, "console:e2e-fleet-") || req.CommandID == "" || used[req.CommandID] {
						t.Errorf("bad conversation/command identity: %+v", req)
					}
					used[req.CommandID] = true
					sent++
					rpl := reply{At: time.Now().UTC(), Conversation: req.Conversation, Kind: "reply"}
					switch sent {
					case 1:
						if req.Input != "/project use scratch" {
							t.Errorf("first command: %q", req.Input)
						}
					case 2:
						if req.Input != "/use claude" {
							t.Errorf("second command: %q", req.Input)
						}
					case 3:
						for _, part := range []string{"steve_delegate", "shipper", "node-b", g.filename, "must not git init or git commit"} {
							if !strings.Contains(req.Input, part) {
								t.Errorf("delegation prompt missing %q", part)
							}
						}
						rpl.Injected.Project, rpl.Injected.Agent = "scratch", "claude"
						s := step{ID: "#child", Kind: "delegate", State: "done", Agent: "shipper", Node: "node-b", Attempt: "child-attempt"}
						switch tc.name {
						case "failed_card":
							s.State, s.Answer = "failed", "snapshot failed"
						case "wrong_node":
							s.Node = "hub"
						case "no_attempt_id":
							s.Attempt = ""
						case "wrong_project":
							rpl.Injected.Project = "another-project"
						}
						rpl.Process.Steps = []step{s}
						stored = rpl
						stored.ID = "final-reply"
						if tc.name == "stale_reply" {
							stored.At = stored.At.Add(-time.Hour)
						}
						content := "fleet e2e " + strings.TrimSuffix(strings.TrimPrefix(g.filename, "e2e-fleet-"), ".txt") + "\n"
						if tc.name == "wrong_content" {
							content = strings.Replace(content, "fleet", "wrong", 1)
						}
						path := filepath.Join(home, g.filename)
						if tc.name == "symlink" {
							if err := os.Symlink("elsewhere", path); err != nil {
								t.Error(err)
							}
						} else if tc.name != "missing_file" {
							if err := os.WriteFile(path, []byte(content), 0600); err != nil {
								t.Error(err)
							}
						}
					default:
						t.Error("unexpected extra send")
					}
					// Like the deployed hub, the synchronous response has no ID.
					response = map[string]any{"reply": rpl}
				case "GET /console/replies":
					if r.URL.Query().Get("conversation") != g.conversation {
						t.Error("wrong transcript")
					}
					milestone := stored
					milestone.ID, milestone.Kind = "milestone", "milestone"
					response = map[string]any{"replies": []reply{milestone, stored}}
				case "GET /console/tasks/child":
					response = child()
				case "GET /console/tasks/child/attempts":
					page := consoleapi.AttemptHistoryPage{Items: []consoleapi.AttemptHistoryItem{{TaskID: "child", Project: "scratch",
						AttemptView: consoleapi.AttemptView{ID: "child-attempt", Kind: "delegate", State: "bound", Agent: "shipper", Node: "node-b", Artifact: "snapshot"}}}}
					if tc.name == "no_attempt" {
						page.Items = nil
					}
					response = page
				case "GET /console/attempts/child-attempt/changes":
					name, status, attemptID := g.filename, "A", "child-attempt"
					switch tc.name {
					case "no_change":
						name = "unrelated.txt"
					case "deleted_file":
						status = "D"
					case "wrong_index":
						attemptID = "parent-attempt"
					}
					response = map[string]any{"attempt": attemptID, "project": "scratch", "changes": []map[string]string{{"path": name, "status": status}}}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
					return
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			g.hub = srv.URL
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := g.run(ctx)
			if tc.want == "" {
				if err != nil || !strings.Contains(output.String(), "FLEET PASS") {
					t.Fatalf("run: %v\n%s", err, &output)
				}
				if usageReads != 1 {
					t.Fatalf("usage must be read independently once, got %d reads", usageReads)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(output.String(), "FLEET PASS") {
				t.Fatalf("want failure containing %q, got %v\n%s", tc.want, err, &output)
			}
			if sent != 3 || strings.Contains(output.String(), "secret-token") {
				t.Fatalf("sent=%d output=%s", sent, &output)
			}
		})
	}
}

func TestSendDeadlineAndRedaction(t *testing.T) {
	var output bytes.Buffer
	g := newGate(options{token: "private-token"}, &output)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/failure" {
			http.Error(w, "rejected private-token", http.StatusUnauthorized)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()
	g.hub = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := g.send(ctx, "delegate", "wait")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(g.started) > 500*time.Millisecond {
		t.Fatalf("send did not respect total deadline: %v", err)
	}
	err = g.request(context.Background(), http.MethodGet, "/failure", nil, nil)
	g.log("FAIL: %v", err)
	if !strings.Contains(output.String(), "HTTP 401") || strings.Contains(output.String(), "private-token") {
		t.Fatalf("diagnostic leaked token or lost HTTP status: %s", &output)
	}
}

func TestFailureDiagnosticsStayInThisConversationAndDeadline(t *testing.T) {
	var output bytes.Buffer
	g := newGate(options{token: "private-token"}, &output)
	g.conversation = "console:this-run"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var response any
		switch r.URL.Path {
		case "/state":
			response = state{Tasks: []task{
				{ID: "own-task", Channel: g.conversation, Member: "claude", State: "running"},
				{ID: "other-task", Channel: "console:someone-else"},
			}}
		case "/console/tasks/own-task/attempts":
			response = consoleapi.AttemptHistoryPage{Items: []consoleapi.AttemptHistoryItem{{TaskID: "own-task", AttemptView: consoleapi.AttemptView{ID: "own-attempt", State: "failed"}}}}
		case "/console/replies":
			response = map[string]any{"replies": []reply{{ID: "own-reply", Conversation: g.conversation, Kind: "reply", Error: "private-token rejected"}}}
		default:
			t.Errorf("unexpected diagnostic request: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	g.hub = srv.URL
	g.diagnostics(context.Background())
	for _, want := range []string{"task=#own-task", "attempt=own-attempt", "reply=own-reply", "[REDACTED] rejected"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("diagnostic missing %q: %s", want, &output)
		}
	}
	if strings.Contains(output.String(), "other-task") || strings.Contains(output.String(), "private-token") {
		t.Fatalf("unrelated data in diagnostics: %s", &output)
	}
	output.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.diagnostics(ctx)
	if output.Len() != 0 {
		t.Fatal("diagnostics ran after total deadline")
	}
}

func TestOptions(t *testing.T) {
	for _, key := range []string{"HUB", "TOKEN", "PROJECT", "AGENT", "TARGET_NODE", "TARGET_AGENT"} {
		t.Setenv(key, "")
	}
	config := filepath.Join(t.TempDir(), "config.e2e.json")
	if err := os.WriteFile(config, []byte(`{"gateway":{"read_model_addr":"0.0.0.0:7710","read_model_token":"config-token"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o, err := parseOptions([]string{"-config", config}, io.Discard)
	if err != nil || o.hub != "http://127.0.0.1:7710" || o.token != "config-token" || o.project != "scratch" || o.agent != "claude" || o.targetNode != "node-b" || o.targetAgent != "shipper" || o.timeout != 10*time.Minute {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	t.Setenv("HUB", "https://env.example")
	t.Setenv("TOKEN", "env-token")
	o, err = parseOptions([]string{"-config", config}, io.Discard)
	if err != nil || o.hub != "https://env.example" || o.token != "env-token" {
		t.Fatalf("environment must override config: %+v %v", o, err)
	}
	o, err = parseOptions([]string{"-hub", "http://flag.example", "-token", "flag-token", "-config", "absent.json"}, io.Discard)
	if err != nil || o.hub != "http://flag.example" || o.token != "flag-token" {
		t.Fatalf("flags must override environment without needing config: %+v %v", o, err)
	}
	var help bytes.Buffer
	_, _ = parseOptions([]string{"-help"}, &help)
	if strings.Contains(help.String(), "env-token") {
		t.Fatal("help leaked TOKEN")
	}
	for _, args := range [][]string{{"-timeout", "11m"}, {"-timeout", "0s"}, {"-project", "scratch\n/cancel"}, {"-hub", "http://hub?token=secret"}} {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid arguments %q", args)
		}
	}
	t.Setenv("TOKEN", "")
	if _, err := parseOptions([]string{"-config", "absent.json"}, io.Discard); err == nil || !strings.Contains(err.Error(), "missing token") {
		t.Fatalf("missing token diagnostic: %v", err)
	}
}
