package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// The HTTP server, console, coordinator, persistence and ACP stdio are real.
// Only the model is replaced with the deterministic local mockagent process.
type interactionE2E struct {
	server    *Server
	service   *console.Service
	book      *ledger.Ledger
	materials *material.Store
	client    *http.Client
}

func newInteractionE2E(t *testing.T, bin string, noMedia bool) *interactionE2E {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	materials, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { materials.Close() })
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"test": {Harness: "mock", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	env := []string{}
	if noMedia {
		env = []string{"MOCKAGENT_NO_MEDIA=1"}
	}
	manager, err := harness.NewManager(map[string]harness.Config{"mock": {Command: bin, Permission: "read", Env: env}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "scratch", Home: project.Home{Path: t.TempDir()}, Level: project.LevelPublic}}); err != nil {
		t.Fatal(err)
	}
	coordinator := turn.New(catalog, store, capability.NewAssembler(nil), manager, 10*time.Second)
	coordinator.SetIdentity("owner", nil)
	coordinator.SetProjects(projects, "scratch", "")
	coordinator.SetAttempts(attempt.New(book))
	coordinator.SetArtifacts(artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()}))
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	coordinator.SetTasks(tasks, "")
	service := console.New(coordinator, "owner", nil)
	service.SetMaterials(materials, func(ctx context.Context, conversation, principal, projectID string) error {
		if principal != "owner" || projectID != "scratch" {
			return material.ErrScope
		}
		current, err := coordinator.Context(ctx, conversation)
		if err != nil {
			return err
		}
		if current.Project == nil || current.Project.ID != projectID {
			return material.ErrScope
		}
		return nil
	})
	if err := service.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(readmodel.New(readmodel.Sources{}), ServerConfig{Addr: "127.0.0.1:0", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	server.SetConsole(service)
	server.SetAdmin(materialAdapter{store: materials})
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { server.Close() })
	return &interactionE2E{server: server, service: service, book: book, materials: materials, client: &http.Client{Timeout: 5 * time.Second}}
}

func (f *interactionE2E) request(t *testing.T, method, path string, body []byte, mime, token string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL()+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mime)
	req.Header.Set("Accept-Language", "en")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d body=%s err=%v", method, path, res.StatusCode, want, raw, err)
	}
	return raw
}
func (f *interactionE2E) json(t *testing.T, path string, value any, want int) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return f.request(t, "POST", path, raw, "application/json", "test-token", want)
}
func (f *interactionE2E) submit(t *testing.T, conversation, input, key string, refs ...material.Ref) consoleapi.Exchange {
	t.Helper()
	var e consoleapi.Exchange
	raw := f.json(t, "/console/queue", consoleapi.Submission{Conversation: conversation, Input: input, CommandID: key, Refs: refs}, 200)
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return e
}
func (f *interactionE2E) pending(t *testing.T, exchangeID string) consoleapi.PendingQuestion {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		var data struct {
			Questions []consoleapi.PendingQuestion `json:"questions"`
		}
		_ = json.Unmarshal(f.request(t, "GET", "/console/questions", nil, "", "test-token", 200), &data)
		for _, q := range data.Questions {
			if q.ExchangeID == exchangeID && q.State == "pending" {
				return q
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no pending question: queue=%+v replies=%+v", f.service.Queue("console:e2e"), f.service.Replies("console:e2e"))
	return consoleapi.PendingQuestion{}
}
func (f *interactionE2E) reply(t *testing.T, e consoleapi.Exchange) consoleapi.Reply {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		var data struct {
			Replies []consoleapi.Reply `json:"replies"`
		}
		if err := json.Unmarshal(f.request(t, "GET", "/console/replies?conversation="+url.QueryEscape(e.Conversation), nil, "", "test-token", 200), &data); err != nil {
			t.Fatal(err)
		}
		for _, r := range data.Replies {
			if r.ExchangeID == e.ID && r.Kind == "reply" {
				return r
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("exchange %s did not complete", e.ID)
	return consoleapi.Reply{}
}

func TestConsoleMaterialsAndQuestionsACPIntegration(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v %s", err, out)
	}
	t.Run("media bytes MIME frozen identity and authorization", func(t *testing.T) {
		f := newInteractionE2E(t, bin, false)
		capabilities := f.request(t, "GET", "/console/queue", nil, "", "test-token", 200)
		if !strings.Contains(string(capabilities), `"material_refs":true`) || !strings.Contains(string(capabilities), `"interactive_requests":true`) {
			t.Fatalf("missing submission capabilities: %s", capabilities)
		}
		var imageBytes bytes.Buffer
		if err := png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			name, mime string
			data       []byte
			kind       string
		}{{"image.png", "image/png", imageBytes.Bytes(), "image"}, {"report.pdf", "application/pdf", []byte("%PDF-1.4\nfixture bytes\n%%EOF"), "resource"}, {"notes.txt", "text/plain", []byte("Frozen original notes"), ""}}
		var refs []material.Ref
		for _, c := range cases {
			var m material.Material
			raw := f.request(t, "POST", "/console/materials/upload?project=scratch&name="+c.name, c.data, c.mime, "test-token", 200)
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			refs = append(refs, material.Ref{ID: m.ID})
		}
		e := f.submit(t, "console:e2e", "inspectmedia", "media-1", refs...)
		r := f.reply(t, e)
		if r.Error != "" || r.ProjectID != "scratch" {
			t.Fatalf("reply %+v", r)
		}
		for _, c := range cases {
			want := string(c.data)
			if c.kind != "" {
				want = fmt.Sprintf("[media: %s %s %d %x]", c.kind, c.mime, len(c.data), sha256.Sum256(c.data))
			}
			if !strings.Contains(r.Text, want) {
				t.Fatalf("actual ACP prompt lost %s: %s", c.name, r.Text)
			}
		}
		if replay := f.submit(t, "console:e2e", "inspectmedia", "media-1", refs...); replay.ID != e.ID {
			t.Fatal("duplicate media exchange")
		}
		changedBrowser := i18n.WithLocale(t.Context(), i18n.LocaleZH)
		replay, err := f.service.Submit(changedBrowser, consoleapi.Submission{Conversation: "console:e2e", Input: "inspectmedia", CommandID: "media-1", Refs: refs})
		if err != nil || replay.ID != e.ID || replay.Locale != "en" {
			t.Fatalf("browser language changed a retry: %+v %v", replay, err)
		}
		imageOnly := f.reply(t, f.submit(t, "console:image-only", "", "image-only", refs[0]))
		if imageOnly.Error != "" || !strings.Contains(imageOnly.Text, "[media: image image/png") {
			t.Fatalf("image-only submission failed %+v", imageOnly)
		}
		f.json(t, "/console/queue", consoleapi.Submission{Conversation: "console:e2e", Input: "inspectmedia", CommandID: "media-1", Refs: refs[:1]}, 409)
		f.request(t, "POST", "/console/queue", []byte(`{"input":"hello"}`), "application/json", "", 401)
		other, err := f.materials.Capture(t.Context(), material.CaptureInput{Project: "other", Title: "secret", MIME: "text/plain", Source: material.Source{Kind: "upload"}, Data: []byte("secret")})
		if err != nil {
			t.Fatal(err)
		}
		f.json(t, "/console/queue", consoleapi.Submission{Conversation: "console:e2e", Input: "cross", CommandID: "cross", Refs: []material.Ref{{ID: other.ID}}}, 400)
		f.json(t, "/console/queue", consoleapi.Submission{Conversation: "console:e2e", Input: "bad", CommandID: "path", Refs: []material.Ref{{ID: "../../outside"}}}, 400)
		f.request(t, "POST", "/console/materials/upload?project=scratch&name=forged.png", []byte("<html>not a PNG</html>"), "image/png", "test-token", 400)
		f.json(t, "/console/queue", consoleapi.Submission{Conversation: "console:e2e", CommandID: "bad-range", Refs: []material.Ref{{ID: refs[2].ID, Selector: &material.Selector{Kind: "lines", Start: 999, End: 1000}}}}, 400)
	})
	t.Run("unsupported media fails before ACP prompt", func(t *testing.T) {
		f := newInteractionE2E(t, bin, true)
		m, err := f.materials.Capture(t.Context(), material.CaptureInput{Project: "scratch", Title: "report", MIME: "application/pdf", Source: material.Source{Kind: "upload"}, Data: []byte("%PDF-1.4\nbytes")})
		if err != nil {
			t.Fatal(err)
		}
		r := f.reply(t, f.submit(t, "console:e2e", "inspectmedia", "unsupported", material.Ref{ID: m.ID}))
		if !strings.Contains(r.Error, "does not support") || strings.Contains(r.Text, "[media:") {
			t.Fatalf("unsupported attachment silently executed: %+v", r)
		}
		var pngBytes bytes.Buffer
		_ = png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
		img, err := f.materials.Capture(t.Context(), material.CaptureInput{Project: "scratch", Title: "image", MIME: "image/png", Source: material.Source{Kind: "upload"}, Data: pngBytes.Bytes()})
		if err != nil {
			t.Fatal(err)
		}
		imageReply := f.reply(t, f.submit(t, "console:unsupported-image", "inspectmedia", "unsupported-image", material.Ref{ID: img.ID}))
		if !strings.Contains(imageReply.Error, "does not support") {
			t.Fatalf("unsupported image lost silently: %+v", imageReply)
		}
	})
	t.Run("human permission ask answer CAS cancel and restart", func(t *testing.T) {
		f := newInteractionE2E(t, bin, false)
		status := f.reply(t, f.submit(t, "console:status", "/status", "english-status"))
		if !strings.Contains(status.Text, "**Agent**") {
			t.Fatalf("English header did not reach coordinator: %s", status.Text)
		}
		for _, c := range []struct{ input, choice, want, kind string }{{"perm check", "allow", "[permission: selected/allow]", "permission"}, {"perm check", "reject", "[permission: selected/reject]", "permission"}, {"askme", "Blue", "[answer: accept:Blue]", "question"}} {
			e := f.submit(t, "console:e2e", c.input, c.choice)
			q := f.pending(t, e.ID)
			if q.Kind != c.kind || q.TaskID == "" || q.AttemptID == "" || q.Locale != "en" || q.Project != "scratch" || q.SessionID == "" || q.Generation == 0 {
				t.Fatalf("question identity %+v", q)
			}
			if !q.Required {
				t.Fatal("required choice was lost")
			}
			path := "/console/questions/" + q.ID + "/answer"
			f.json(t, path, consoleapi.QuestionAnswer{CommandID: "invalid", Decision: "accept", Choice: "forged"}, 400)
			f.json(t, path, consoleapi.QuestionAnswer{CommandID: "empty", Decision: "accept"}, 400)
			answer := consoleapi.QuestionAnswer{CommandID: "answer-1", Decision: "accept", Choice: c.choice}
			first := f.json(t, path, answer, 200)
			replay := f.json(t, path, answer, 200)
			if string(first) != string(replay) {
				t.Fatal("decision replay changed receipt")
			}
			answer.CommandID = "another-answer"
			f.json(t, path, answer, 409)
			if r := f.reply(t, e); !strings.Contains(r.Text, c.want) {
				t.Fatalf("agent did not get decision: %+v", r)
			}
		}
		e := f.submit(t, "console:e2e", "askme", "cancel-question")
		q := f.pending(t, e.ID)
		f.submit(t, "console:e2e", "/cancel", "cancel-control")
		_ = f.reply(t, e)
		for _, got := range f.service.Questions("") {
			if got.ID == q.ID && got.State != "cancelled" {
				t.Fatalf("question survived cancellation: %+v", got)
			}
		}
		f.json(t, "/console/questions/"+q.ID+"/answer", consoleapi.QuestionAnswer{CommandID: "late", Decision: "accept", Choice: "Blue"}, 409)
		unsupported := f.submit(t, "console:e2e", "askme-required-text", "required-schema")
		if r := f.reply(t, unsupported); !strings.Contains(r.Text, "[answer: decline]") {
			t.Fatalf("partially answered required form: %+v", r)
		}
	})
	t.Run("fresh server rejects pending callbacks from crash checkpoint", func(t *testing.T) {
		first := newInteractionE2E(t, bin, false)
		e := first.submit(t, "console:e2e", "askme", "before-crash")
		q := first.pending(t, e.ID)
		checkpoint, _, err := first.book.Document("console").Load()
		if err != nil {
			t.Fatal(err)
		}
		first.json(t, "/console/questions/"+q.ID+"/answer", consoleapi.QuestionAnswer{CommandID: "cleanup", Decision: "cancel"}, 200)
		_ = first.reply(t, e)
		restarted := newInteractionE2E(t, bin, false)
		if err := restarted.book.Document("console").Save(checkpoint); err != nil {
			t.Fatal(err)
		}
		if err := restarted.service.Persist(restarted.book.Document("console")); err != nil {
			t.Fatal(err)
		}
		list := restarted.service.Questions("")
		if len(list) != 1 || list[0].State != "interrupted" {
			t.Fatalf("restart guessed a live callback: %+v", list)
		}
		restarted.json(t, "/console/questions/"+q.ID+"/answer", consoleapi.QuestionAnswer{CommandID: "late", Decision: "accept", Choice: "Blue"}, 409)
		if got := restarted.submit(t, "console:e2e", "askme", "before-crash"); got.ID != e.ID || got.State != "failed" {
			t.Fatalf("restart replayed old work: %+v", got)
		}
	})
}
