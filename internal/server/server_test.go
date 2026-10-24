package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/approvals"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/work"
)

var (
	mockOnce sync.Once
	mockBin  string
	mockErr  error
)

func mockAgent(t *testing.T) string {
	t.Helper()
	mockOnce.Do(func() {
		dir, err := os.MkdirTemp("", "steve-mockagent")
		if err != nil {
			mockErr = err
			return
		}
		mockBin = filepath.Join(dir, "mockagent")
		out, err := exec.Command("go", "build", "-o", mockBin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput()
		if err != nil {
			mockErr = fmt.Errorf("build mockagent: %v %s", err, out)
		}
	})
	if mockErr != nil {
		t.Fatal(mockErr)
	}
	return mockBin
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

type rig struct {
	t     *testing.T
	srv   *Server
	http  *httptest.Server
	mu    sync.Mutex
	seen  []sseEvent
	added chan struct{}
}

type sseEvent struct {
	Type string
	Data json.RawMessage
}

func start(t *testing.T) *rig {
	t.Helper()
	state := t.TempDir()
	raw := fmt.Sprintf(`{
		"agents": {"mock": {"harness": "mock", "default": true}},
		"harnesses": {"mock": {"command": %q}},
		"projects": {"repo": {"home": {"path": %q}}},
		"mcp_servers": {},
		"gateway": {"owner_id": "owner", "state_path": %q, "read_model_token": "secret"}
	}`, mockAgent(t), gitRepo(t), filepath.Join(state, "state.json"))
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), cfg, Options{ApprovalTimeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	h := &rig{t: t, srv: srv, http: ts, added: make(chan struct{}, 1)}
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		srv.Close()
	})
	return h
}

func (h *rig) do(method, path string, body any, into any) int {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.http.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

// follow reads the stream from the cursor into h.seen.
func (h *rig) follow(ctx context.Context, cursor string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.http.URL+"/v2/stream", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Last-Event-ID", cursor)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	go func() {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		var event sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event.Type = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				event.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			case line == "" && event.Type != "":
				h.mu.Lock()
				h.seen = append(h.seen, event)
				h.mu.Unlock()
				select {
				case h.added <- struct{}{}:
				default:
				}
				event = sseEvent{}
			}
		}
	}()
}

// await waits for a streamed event that matches.
func (h *rig) await(what string, match func(sseEvent) bool) sseEvent {
	h.t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		h.mu.Lock()
		for _, e := range h.seen {
			if match(e) {
				h.mu.Unlock()
				return e
			}
		}
		h.mu.Unlock()
		select {
		case <-h.added:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			h.mu.Lock()
			defer h.mu.Unlock()
			var all []string
			for _, e := range h.seen {
				if e.Type != "heartbeat" {
					all = append(all, e.Type+" "+string(e.Data))
				}
			}
			h.t.Fatalf("never saw %s; saw:\n%s", what, strings.Join(all, "\n"))
		}
	}
}

func last(events []sseEvent) string {
	if len(events) == 0 {
		return "none"
	}
	e := events[len(events)-1]
	return e.Type + " " + string(e.Data)
}

func taskIn(e sseEvent, id string, states ...string) (work.Card, bool) {
	var c work.Card
	if e.Type != work.EventTask || json.Unmarshal(e.Data, &c) != nil || c.ID != id {
		return c, false
	}
	for _, s := range states {
		if c.State == s {
			return c, true
		}
	}
	return c, false
}

func TestATaskRunsOnARealAgentAsksForApprovalAndFinishes(t *testing.T) {
	h := start(t)
	var snap Snapshot
	if code := h.do("GET", "/v2/snapshot", nil, &snap); code != 200 || snap.Cursor == "" || len(snap.Tasks) != 0 {
		t.Fatalf("snapshot %d %+v", code, snap)
	}
	h.follow(t.Context(), snap.Cursor)

	var card work.Card
	if code := h.do("POST", "/v2/tasks", map[string]string{"client_nonce": "n-1", "goal": "perm plan fix the parser"}, &card); code != http.StatusAccepted || card.ID == "" || card.Agent != "mock" || card.Project != "repo" {
		t.Fatalf("submit %d %+v", code, card)
	}
	opened := h.await("an approval card", func(e sseEvent) bool { return e.Type == approvals.EventOpened })
	var ask approvals.Card
	json.Unmarshal(opened.Data, &ask)
	if ask.TaskID != card.ID || ask.Title != "dangerous operation" {
		t.Fatalf("approval = %+v", ask)
	}
	h.await("the task waiting on the person", func(e sseEvent) bool { _, ok := taskIn(e, card.ID, work.StateWaiting); return ok })
	var open struct{ Approvals []approvals.Card }
	if h.do("GET", "/v2/approvals", nil, &open); len(open.Approvals) != 1 {
		t.Fatalf("open approvals = %+v", open)
	}
	if code := h.do("POST", "/v2/approvals/"+ask.ID, map[string]string{"decision": "approve"}, nil); code != 200 {
		t.Fatalf("approve %d", code)
	}
	if code := h.do("POST", "/v2/approvals/"+ask.ID, map[string]string{"decision": "reject"}, nil); code != http.StatusConflict {
		t.Fatalf("second answer %d", code)
	}
	done := h.await("the task done", func(e sseEvent) bool { _, ok := taskIn(e, card.ID, work.StateDone, work.StateFailed); return ok })
	final, _ := taskIn(done, card.ID, work.StateDone, work.StateFailed)
	if final.State != work.StateDone || !strings.Contains(final.Answer, "[permission: selected/allow]") {
		t.Fatalf("final card = %+v", final)
	}
	h.await("plan progress on the card", func(e sseEvent) bool {
		c, ok := taskIn(e, card.ID, work.StateRunning, work.StateWaiting)
		return ok && c.Activity.Stage != nil && c.Activity.Stage.Total > 0
	})

	var again work.Card
	if code := h.do("POST", "/v2/tasks", map[string]string{"client_nonce": "n-1", "goal": "perm plan fix the parser"}, &again); code != http.StatusAccepted || again.ID != card.ID {
		t.Fatalf("resubmit %d %+v", code, again)
	}
	var list struct{ Tasks []work.Card }
	if h.do("GET", "/v2/tasks", nil, &list); len(list.Tasks) != 1 {
		t.Fatalf("tasks = %+v", list)
	}
}

func TestAStoppedTaskEndsStopped(t *testing.T) {
	h := start(t)
	var snap Snapshot
	h.do("GET", "/v2/snapshot", nil, &snap)
	h.follow(t.Context(), snap.Cursor)
	var card work.Card
	h.do("POST", "/v2/tasks", map[string]string{"client_nonce": "n-2", "goal": "slow migration"}, &card)
	h.await("the task running", func(e sseEvent) bool { _, ok := taskIn(e, card.ID, work.StateRunning); return ok })
	time.Sleep(300 * time.Millisecond) // let the prompt reach the agent
	var stopped work.Card
	if code := h.do("POST", "/v2/tasks/"+card.ID+"/stop", nil, &stopped); code != 200 {
		t.Fatalf("stop %d %+v", code, stopped)
	}
	h.await("the task stopped", func(e sseEvent) bool { _, ok := taskIn(e, card.ID, work.StateStopped); return ok })
	h.srv.Work.Wait()
	if got, _ := h.srv.Work.Get(card.ID); got.State != work.StateStopped {
		t.Fatalf("after the run returned = %+v", got)
	}
}

func TestTheAPIRefusesWithoutTheTokenAndWithoutANonce(t *testing.T) {
	h := start(t)
	resp, err := http.Get(h.http.URL + "/v2/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d", resp.StatusCode)
	}
	if code := h.do("POST", "/v2/tasks", map[string]string{"goal": "x"}, nil); code != http.StatusBadRequest {
		t.Fatalf("no nonce = %d", code)
	}
	if len(h.srv.Work.List()) != 0 {
		t.Fatal("a task was created without a nonce")
	}
}

func messageIn(e sseEvent) (m struct {
	ID     string   `json:"id"`
	Role   string   `json:"role"`
	Text   string   `json:"text"`
	TaskID string   `json:"task_id"`
	Tasks  []string `json:"tasks"`
}, ok bool) {
	if e.Type != "message" {
		return m, false
	}
	return m, json.Unmarshal(e.Data, &m) == nil
}

// G1 in miniature: the owner says one sentence; the chief of staff answers
// and starts a task; the task asks for approval, is approved and finishes;
// its outcome comes back to the conversation without the owner asking.
func TestTheOwnerTalksToSteveAndTheWorkComesBack(t *testing.T) {
	h := start(t)
	var snap Snapshot
	h.do("GET", "/v2/snapshot", nil, &snap)
	h.follow(t.Context(), snap.Cursor)

	var accepted struct {
		Accepted bool
		Message  struct{ ID string }
	}
	if code := h.do("POST", "/v2/messages", map[string]string{"client_nonce": "m-1", "text": "delegate: perm plan fix issue 7"}, &accepted); code != http.StatusAccepted || !accepted.Accepted {
		t.Fatalf("post %d %+v", code, accepted)
	}
	if code := h.do("POST", "/v2/messages", map[string]string{"client_nonce": "m-1", "text": "delegate: perm plan fix issue 7"}, nil); code != http.StatusAccepted {
		t.Fatalf("resend %d", code)
	}
	var status struct{ Accepted bool }
	if h.do("GET", "/v2/messages/status?client_nonce=m-1", nil, &status); !status.Accepted {
		t.Fatal("status does not know the nonce")
	}
	reply := h.await("Steve's reply", func(e sseEvent) bool { m, ok := messageIn(e); return ok && m.Role == "steve" })
	m, _ := messageIn(reply)
	// The chief's own permission request ("perm" is in what it read) was
	// refused at once, not put in front of the owner.
	if !strings.HasSuffix(m.Text, "chief: on it") || !strings.Contains(m.Text, "selected/reject") || len(m.Tasks) != 1 {
		t.Fatalf("reply = %+v", m)
	}
	taskID := m.Tasks[0]
	opened := h.await("the task's approval card", func(e sseEvent) bool { return e.Type == approvals.EventOpened })
	var ask approvals.Card
	json.Unmarshal(opened.Data, &ask)
	if ask.TaskID != taskID {
		t.Fatalf("approval for %s, task is %s", ask.TaskID, taskID)
	}
	h.do("POST", "/v2/approvals/"+ask.ID, map[string]string{"decision": "approve"}, nil)
	notice := h.await("the outcome in the conversation", func(e sseEvent) bool { m, ok := messageIn(e); return ok && m.Role == "notice" && m.TaskID == taskID })
	n, _ := messageIn(notice)
	if !strings.Contains(n.Text, "任务 #"+taskID+" 已完成") || !strings.Contains(n.Text, "[permission: selected/allow]") {
		t.Fatalf("notice = %+v", n)
	}
	var after Snapshot
	h.do("GET", "/v2/snapshot", nil, &after)
	roles := []string{}
	for _, m := range after.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "owner,steve,notice" || len(after.Tasks) != 1 || after.Tasks[0].State != work.StateDone {
		t.Fatalf("snapshot roles=%v tasks=%+v", roles, after.Tasks)
	}
}
