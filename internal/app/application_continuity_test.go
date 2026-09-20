package app

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/state"
)

// The fixture's memory belongs to an exact ACP session, not to Steve's
// transcript. A different session or a replay cannot satisfy this test.
func TestApplicationRestartPreservesNativeMemory(t *testing.T) {
	root := ClusterPeerTestDir(t)
	bin := filepath.Join(root, "mockagent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated agent: %v %s", err, out)
	}
	for _, name := range []string{"recall", "load_failure_does_not_open_fresh", "repair_archived_revoked_credential"} {
		t.Run(name, func(t *testing.T) {
			rejectLoad := name == "load_failure_does_not_open_fresh"
			repairCredential := name == "repair_archived_revoked_credential"
			dir := filepath.Join(root, name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", dir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
			memory := filepath.Join(dir, "native-memory")
			if err := os.Mkdir(memory, 0o700); err != nil {
				t.Fatal(err)
			}
			options, installed := testPeerOptions(t, filepath.Join(dir, "app"), nil)
			shortenRecoveryQuiet(t, options.ConfigPath)
			cfg, err := cluster.LoadClusterPeerConfig(options.ClusterPath)
			if err != nil {
				t.Fatal(err)
			}
			token, err := cluster.ClusterRandomToken()
			if err != nil {
				t.Fatal(err)
			}
			worker := node.ServerConfig{
				Name: cfg.NodeID, Listen: "127.0.0.1:0", Token: token,
				Hubs: map[string]string{cfg.ClusterID: token}, StateDir: filepath.Join(cfg.DataDir, "node"),
				WorkspaceRoot: installed.Paths.Root,
				Harnesses: map[string]node.HarnessSpec{"mock": {
					Command: bin, Env: []string{"MOCKAGENT_MEMORY_DIR=" + memory},
				}},
			}
			if err := cluster.SaveClusterJSON(cfg.WorkerConfigFile, worker, true); err != nil {
				t.Fatal(err)
			}
			first := StartTestPeer(t, options)
			WaitPeerReady(t, first)
			continuityRequest(t, first, http.MethodPost, "/console/agents", consoleapi.AddAgentRequest{
				ID: "worker", Harness: "mock", Node: cfg.NodeID,
			})
			const conversation = "console:restart-memory"
			continuitySend(t, first, conversation, "/project use workspace", "select-project")
			// A user title prevents a separate title-generation ACP session.
			continuityRequest(t, first, http.MethodPut, "/console/conversations/"+conversation, map[string]string{"title": "Native continuity fixture"})
			preferences := map[string]any{
				"conversation": conversation, "agent": "worker",
				"patch": map[string]string{"model": "mock-deep", "mode": "read-only"},
			}
			continuityRequest(t, first, http.MethodPut, "/console/preferences", preferences)
			var random [24]byte
			if _, err := rand.Read(random[:]); err != nil {
				t.Fatal(err)
			}
			marker := hex.EncodeToString(random[:])
			reply := continuitySend(t, first, conversation, "fixture-remember "+marker, "remember-once")
			if reply.Error != "" || !strings.Contains(reply.Text, "memory: stored") {
				t.Fatalf("first turn did not store native memory: %s %s", reply.Error, reply.Text)
			}
			before := continuityConversation(t, first, conversation)
			saved := before.Sessions["worker"]
			if saved.AgentToken == "" || saved.UpstreamID == "" || saved.NodeID != cfg.NodeID || saved.ProjectID != "workspace" {
				t.Fatal("first turn lacks durable session/token/project/node binding")
			}
			original, err := attempt.New(first.Runtime.Load().Ledger()).Get(t.Context(), reply.AttemptID)
			if err != nil || original.NativeContext == "" || original.Unsettled || original.Result == nil {
				t.Fatalf("first execution did not settle with native context: %v", err)
			}
			eventsBefore := continuityEvents(t, memory)
			if len(eventsBefore) != 2 || eventsBefore[0].Kind != "new" || eventsBefore[1].Kind != "prompt" {
				t.Fatalf("expected exactly new+first prompt, got %+v", eventsBefore)
			}
			native := eventsBefore[0].Session
			if native == "" || eventsBefore[1].Session != native || !strings.Contains(eventsBefore[1].Input, marker) {
				t.Fatal("first prompt was not delivered to its new native session")
			}
			replacementToken := ""
			if repairCredential {
				// Historical repair only: reproduce an already-damaged pointer.
				// Normal restart above never requires /new or /history.
				if reset := continuitySend(t, first, conversation, "/new", "historical-reset"); reset.Error != "" {
					t.Fatalf("archive original context: %s", reset.Error)
				}
				replacement := continuitySend(t, first, conversation, "fixture-remember replacement-context", "replacement-input")
				if replacement.Error != "" || !strings.Contains(replacement.Text, "memory: stored") {
					t.Fatalf("create replacement context: %s %s", replacement.Error, replacement.Text)
				}
				replacementToken = continuityConversation(t, first, conversation).Sessions["worker"].AgentToken
				if replacementToken == "" || replacementToken == saved.AgentToken || !continuityGrant(t, first, saved.AgentToken).Revoked {
					t.Fatal("replacement did not revoke the original native credential")
				}
				if restored := continuitySend(t, first, conversation, "/history 1", "restore-old-pointer"); restored.Error != "" {
					t.Fatalf("restore archived pointer: %s", restored.Error)
				}
				before = continuityConversation(t, first, conversation)
				if !reflect.DeepEqual(before.Sessions["worker"], saved) {
					t.Fatal("history did not select the original native pointer and revoked credential")
				}
				eventsBefore = continuityEvents(t, memory)
				if len(eventsBefore) != 4 || eventsBefore[2].Kind != "new" || eventsBefore[3].Kind != "prompt" ||
					eventsBefore[2].Session == native || eventsBefore[3].Session != eventsBefore[2].Session {
					t.Fatal("historical damage fixture did not create exactly one distinct replacement context")
				}
			}

			// Close waits for both the coordinator and embedded worker to stop.
			// Restart only from their durable files, not from a retained process.
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			if rejectLoad {
				if err := os.WriteFile(filepath.Join(memory, "reject-load"), []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			second := StartTestPeer(t, options)
			WaitPeerReady(t, second)
			continuityRequest(t, second, http.MethodPut, "/console/preferences", preferences)
			afterPrefs := continuityConversation(t, second, conversation)
			if !reflect.DeepEqual(before, afterPrefs) {
				t.Fatal("restart or identical model/mode PUT changed conversation bindings")
			}

			// No marker, transcript, quote or replay is supplied on turn two.
			if rejectLoad {
				body := continuityRequest(t, second, http.MethodPost, "/console/queue", consoleapi.Submission{
					Conversation: conversation, Input: "fixture-recall", CommandID: "recall-only",
				})
				var submitted consoleapi.Exchange
				if err := json.Unmarshal(body, &submitted); err != nil {
					t.Fatal(err)
				}
				question := awaitPeerExchangeQuestion(t, second, conversation, "", submitted.ID, 20*time.Second)
				if question.Kind != "recovery" {
					t.Fatalf("native load failure not surfaced as recovery: kind=%s", question.Kind)
				}
				events := continuityEvents(t, memory)
				if len(events) != 3 || events[2].Kind != "load" || events[2].Session != native {
					t.Fatalf("failed load fell back to new session or prompt: %+v", events)
				}
			} else {
				recall := continuitySend(t, second, conversation, "fixture-recall", "recall-only")
				events := continuityEvents(t, memory)
				if recall.Error != "" || !strings.Contains(recall.Text, "memory: "+marker) ||
					!strings.Contains(recall.Text, "model=mock-deep mode=read-only mcp=ok") {
					t.Fatalf("native memory/preferences/MCP did not continue: %s %s", recall.Error, recall.Text)
				}
				base := len(eventsBefore)
				if len(events) != base+2 || events[base].Kind != "load" || events[base+1].Kind != "prompt" ||
					events[base].Session != native || events[base+1].Session != native ||
					events[base].PID == eventsBefore[0].PID {
					t.Fatalf("restart did not load the exact native session in a new process: %+v", events)
				}
				if strings.Contains(events[base+1].Input, marker) || strings.Contains(events[base+1].Input, "fixture-remember") {
					t.Fatal("recall prompt replayed the marker instead of using native memory")
				}
				next, err := attempt.New(second.Runtime.Load().Ledger()).Get(t.Context(), recall.AttemptID)
				if err != nil || next.ID == original.ID || next.NativeContext != original.NativeContext || next.Unsettled {
					t.Fatalf("second input lost original native context or reused original attempt: %v", err)
				}
				after := continuityConversation(t, second, conversation)
				current := after.Sessions["worker"]
				if current.ProjectID != saved.ProjectID ||
					current.ProjectVersion != saved.ProjectVersion || current.NodeID != saved.NodeID ||
					current.HarnessID != saved.HarnessID || current.Workspace != saved.Workspace ||
					!reflect.DeepEqual(after.Preferences, before.Preferences) || len(after.Archived) != len(before.Archived) {
					t.Fatal("continued turn replaced token, project, node, harness or conversation context")
				}
				if repairCredential {
					if current.AgentToken == "" || current.AgentToken == saved.AgentToken || current.AgentToken == replacementToken {
						t.Fatal("historical repair did not mint a fresh credential")
					}
					old, fresh := continuityGrant(t, second, saved.AgentToken), continuityGrant(t, second, current.AgentToken)
					if !old.Revoked || fresh.Revoked || old.Binding != fresh.Binding {
						t.Fatal("historical repair revived the old grant or changed its logical binding")
					}
					second.Mu.RLock()
					registry := second.Application.Admin.Nodes
					second.Mu.RUnlock()
					endpoint, err := registry.MCPEndpoint(t.Context(), cfg.NodeID)
					if err != nil {
						t.Fatal(err)
					}
					checkRetainedMCP(t, endpoint, saved.AgentToken, http.StatusUnauthorized)
				} else if current.AgentToken != saved.AgentToken {
					t.Fatal("normal restart rotated a valid credential")
				}
			}
			records, err := attempt.New(second.Runtime.Load().Ledger()).ForTask(t.Context(), original.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, record := range records {
				if record.ID == original.ID {
					found++
					if !reflect.DeepEqual(record, original) {
						t.Fatal("original completed attempt was mutated")
					}
				}
			}
			wantRecords := 2
			if repairCredential {
				wantRecords = 1 // /new closed the old task; recall belongs to the new task.
			}
			if found != 1 || len(records) != wantRecords {
				t.Fatalf("original task attempt history changed unexpectedly: original=%d total=%d want=%d", found, len(records), wantRecords)
			}
		})
	}
}

type continuityGrantState struct {
	Binding agentmcp.Binding `json:"binding"`
	Revoked bool             `json:"revoked"`
}

func continuityGrant(t *testing.T, peer *cluster.Peer, token string) continuityGrantState {
	t.Helper()
	hash := sha256.Sum256([]byte(token))
	var raw string
	err := peer.Runtime.Load().Ledger().Read(t.Context(), func(tx *ledger.ReadTx) error {
		return tx.QueryRow("SELECT data FROM bindings WHERE kind=? AND id=?", "agent-mcp-grant", hex.EncodeToString(hash[:])).Scan(&raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	var grant continuityGrantState
	if err := json.Unmarshal([]byte(raw), &grant); err != nil {
		t.Fatal(err)
	}
	return grant
}

type continuityEvent struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
	PID     int    `json:"pid"`
	Input   string `json:"input,omitempty"`
}

func continuityEvents(t *testing.T, dir string) []continuityEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var events []continuityEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event continuityEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func continuityConversation(t *testing.T, peer *cluster.Peer, conversation string) state.Conversation {
	t.Helper()
	store, err := state.OpenLedger(peer.Runtime.Load().Ledger(), "")
	if err != nil {
		t.Fatal(err)
	}
	return store.Conversation(conversation)
}

func continuityRequest(t *testing.T, peer *cluster.Peer, method, path string, request any) []byte {
	t.Helper()
	status, body := PeerRequest(t, peer, method, path, request)
	if status != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, status, body)
	}
	return body
}

func continuitySend(t *testing.T, peer *cluster.Peer, conversation, input, command string) consoleapi.Reply {
	t.Helper()
	body := continuityRequest(t, peer, http.MethodPost, "/console/send", consoleapi.Submission{
		Conversation: conversation, Input: input, CommandID: command,
	})
	var response struct {
		Reply consoleapi.Reply `json:"reply"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	return response.Reply
}
