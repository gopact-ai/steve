package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
)

func transferFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitignore"), []byte("ignored.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, "ignored.txt"), []byte("must move too"), 0600)
	os.WriteFile(filepath.Join(home, "work.txt"), []byte("current uncommitted content"), 0600)
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects := project.Open(book)
	projects.SetHubID("source")
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: home}}, {ID: "private-other", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Create(task.Task{ProjectID: "p", Goal: "project history"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Create(task.Task{ProjectID: "private-other", Goal: "DO NOT EXPORT THIS"}); err != nil {
		t.Fatal(err)
	}
	return dir, home
}
func TestOfflineProjectTransferCarriesCurrentFilesAndRejectsSourceExecution(t *testing.T) {
	source, home := transferFixture(t)
	path := filepath.Join(t.TempDir(), "project.steve")
	b, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Evidence: "service stopped and no writer remains", Output: path})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Tasks.Tasks) != 1 {
		t.Fatalf("cross-project tasks: %+v", b.Tasks.Tasks)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "DO NOT EXPORT THIS") {
		t.Fatal("other project data leaked")
	}
	target := t.TempDir()
	targetHome := filepath.Join(t.TempDir(), "restored")
	p, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Home: targetHome, Input: path})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ignored.txt", "work.txt"} {
		a, _ := os.ReadFile(filepath.Join(home, name))
		got, err := os.ReadFile(filepath.Join(targetHome, name))
		if err != nil || string(got) != string(a) {
			t.Fatalf("file %s=%q %v", name, got, err)
		}
	}
	if p.ID != "p" {
		t.Fatal(p)
	}
	if _, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Home: targetHome, Input: path}); err != nil {
		t.Fatal("replay", err)
	}
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects := project.Open(book)
	projects.SetHubID("source")
	if _, _, err := projects.Get(t.Context(), "p"); !errors.Is(err, project.ErrNotOwner) {
		t.Fatal("source can execute released project", err)
	}
}
func TestExportRefusesLiveHubAndWrongTarget(t *testing.T) {
	source, _ := transferFixture(t)
	unlock, err := steveruntime.AcquireLock(source)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = Export(context.Background(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Evidence: "claim", Output: filepath.Join(t.TempDir(), "bundle")})
	if err == nil {
		t.Fatal("export ran alongside hub")
	}
}

func TestTransferIntoHubWithExistingNumericIDsPreservesBothProjects(t *testing.T) {
	source, _ := transferFixture(t)
	sourceBook, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.OpenLedger(sourceBook, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = plans.Create(plan.Plan{ProjectID: "p", TaskID: "1", Goal: "original task #1 remains prose", Steps: []plan.Step{{ID: "work", Agent: "worker", Goal: "build", Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"}}}})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := schedule.OpenLedger(sourceBook, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Create(schedule.Job{ProjectID: "p", ConversationID: "console:main", Prompt: "scheduled p", Spec: schedule.Spec{Kind: schedule.KindOnce, At: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	sourceBook.Close()
	path := filepath.Join(t.TempDir(), "bundle")
	b, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	sourceBook, err = ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sealedTasks, _ := task.OpenLedger(sourceBook, "")
	sealedTask, _ := sealedTasks.Get("1")
	if sealedTask.State != task.StateCancelled {
		t.Fatal("source task can resume migrated project")
	}
	sourceJobs, _ := schedule.OpenLedger(sourceBook, "")
	if due, err := sourceJobs.Due(time.Now().Add(2 * time.Hour)); err != nil || len(due) != 0 {
		t.Fatal("source still dispatches transferred schedule")
	}
	sourceProjects := project.Open(sourceBook)
	sourceProjects.SetHubID("source")
	if _, ok, err := sourceProjects.Get(t.Context(), "private-other"); err != nil || !ok {
		t.Fatal("source stopped unrelated project", err)
	}
	sourceBook.Close()
	target := t.TempDir()
	book, err := ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	targetProjects := project.Open(book)
	targetProjects.SetHubID("target")
	if err := targetProjects.Declare(t.Context(), []project.Project{{ID: "q", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	tasks, _ := task.OpenLedger(book, "")
	other, err := tasks.Create(task.Task{ProjectID: "q", Goal: "keep me"})
	if err != nil {
		t.Fatal(err)
	}
	otherPlans, _ := plan.OpenLedger(book, "")
	if _, err := otherPlans.Create(plan.Plan{ProjectID: "q", TaskID: other.ID, Goal: "q", Steps: []plan.Step{{ID: "work", Agent: "worker", Goal: "q", Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"}}}}); err != nil {
		t.Fatal(err)
	}
	otherJobs, _ := schedule.OpenLedger(book, "")
	if _, err := otherJobs.Create(schedule.Job{ProjectID: "q", ConversationID: "console:q", Prompt: "q", Spec: schedule.Spec{Kind: schedule.KindOnce, At: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	book.Close()
	if _, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: path, Home: filepath.Join(t.TempDir(), "p")}); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	tasks, _ = task.OpenLedger(book, "")
	if got, ok := tasks.Get(other.ID); !ok || got.Goal != "keep me" {
		t.Fatalf("q task overwritten: %+v", got)
	}
	id := namespace(b.Owner.HubID, "1")
	if got, ok := tasks.Get(id); !ok || got.ProjectID != "p" {
		t.Fatalf("p task missing: %+v", got)
	}
	movedPlans, err := plan.ExportProject(book.Document("plans"), "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, revs := range movedPlans.Plans {
		if revs[0].TaskID != id || revs[0].Goal != "original task #1 remains prose" {
			t.Fatalf("bad structured remap: %+v", revs[0])
		}
	}
	movedJobs, err := schedule.ExportProject(book.Document("schedules"), "p")
	if err != nil || len(movedJobs.Jobs) != 1 {
		t.Fatalf("p schedule=%+v %v", movedJobs, err)
	}
}

func TestImportRejectsTamperedForeignFactsBeforeWritingFiles(t *testing.T) {
	source, _ := transferFixture(t)
	path := filepath.Join(t.TempDir(), "bundle")
	b, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	b.Facts.Bindings["credentials"] = map[string]json.RawMessage{"secret": json.RawMessage(`{"token":"injected"}`)}
	raw, _ := json.Marshal(b)
	wrapped, _ := json.Marshal(Envelope{Digest: digest(raw), Bundle: raw})
	os.WriteFile(path, wrapped, 0600)
	home := filepath.Join(t.TempDir(), "must-not-exist")
	if _, err := Import(t.Context(), ImportOptions{StateDir: t.TempDir(), HubID: "target", ExpectedSource: "source", Input: path, Home: home}); err == nil {
		t.Fatal("foreign facts admitted")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("rejected import wrote workspace")
	}
}

func TestGitHistoryAndDirtyIgnoredFilesMoveWithoutSourceGitMutation(t *testing.T) {
	source, home := transferFixture(t)
	if _, err := git(t.Context(), home, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(t.Context(), home, "add", "work.txt", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(t.Context(), home, "commit", "--quiet", "-m", "original history"); err != nil {
		t.Fatal(err)
	}
	originalHead, _ := git(t.Context(), home, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(home, "work.txt"), []byte("dirty current work"), 0600)
	beforeIndex, err := os.ReadFile(filepath.Join(home, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, ".git", "hooks", "not-transferred"), []byte("private hook"), 0700)
	path := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "all stopped"}); err != nil {
		t.Fatal(err)
	}
	afterIndex, _ := os.ReadFile(filepath.Join(home, ".git", "index"))
	if string(afterIndex) != string(beforeIndex) {
		t.Fatal("export changed source index")
	}
	head, _ := git(t.Context(), home, "rev-parse", "HEAD")
	if string(head) != string(originalHead) {
		t.Fatal("export moved source HEAD")
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := Import(t.Context(), ImportOptions{StateDir: t.TempDir(), HubID: "target", ExpectedSource: "source", Input: path, Home: restored}); err != nil {
		t.Fatal(err)
	}
	head, err = git(t.Context(), restored, "rev-parse", "HEAD")
	if err != nil || string(head) != string(originalHead) {
		t.Fatal("original git history missing", err)
	}
	current, _ := os.ReadFile(filepath.Join(restored, "work.txt"))
	if string(current) != "dirty current work" {
		t.Fatal("dirty work lost")
	}
	ignored, _ := os.ReadFile(filepath.Join(restored, "ignored.txt"))
	if string(ignored) != "must move too" {
		t.Fatal("ignored content lost")
	}
	if _, err := os.Stat(filepath.Join(restored, ".git", "hooks", "not-transferred")); !os.IsNotExist(err) {
		t.Fatal("source hooks transferred")
	}
}

func TestInterruptedInstallCanResumeWithoutOverwritingAnotherProject(t *testing.T) {
	source, _ := transferFixture(t)
	path := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	home := filepath.Join(t.TempDir(), "restored")
	blocker := filepath.Join(target, "materials")
	if err := os.WriteFile(blocker, []byte("temporarily unavailable blob directory"), 0600); err != nil {
		t.Fatal(err)
	}
	options := ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: path, Home: home}
	if _, err := Import(t.Context(), options); err == nil {
		t.Fatal("blocked material directory did not fail import")
	}
	book, err := ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	projects.SetHubID("target")
	if _, ok, err := projects.Lookup(t.Context(), "p"); err != nil || ok {
		t.Fatal("partial import activated project")
	}
	book.Close()
	if _, err := os.Stat(filepath.Join(home, "work.txt")); err != nil {
		t.Fatal("fixture did not reach file install", err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(t.Context(), options); err != nil {
		t.Fatal("same-payload recovery failed", err)
	}
	b, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	b.CreatedAt = b.CreatedAt.Add(time.Second)
	raw, _ := json.Marshal(b)
	wrapped, _ := json.Marshal(Envelope{Digest: digest(raw), Bundle: raw})
	os.WriteFile(path, wrapped, 0600)
	if _, err := Import(t.Context(), options); err == nil {
		t.Fatal("changed payload reused accepted transfer")
	}
}

func TestPublicTargetRejectsRestrictedProjectBeforeExtraction(t *testing.T) {
	source, _ := transferFixture(t)
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	projects.SetHubID("source")
	p, _, _ := projects.Get(t.Context(), "p")
	p.Level = project.LevelRestricted
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	book.Close()
	path := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "unused-state")
	home := filepath.Join(t.TempDir(), "unused-home")
	if _, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: path, Home: home, TargetLevel: project.LevelPublic}); err == nil {
		t.Fatal("low-level target accepted restricted data")
	}
	for _, name := range []string{target, home} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("rejected target wrote %s", name)
		}
	}
}

func TestRichProjectTransferPreservesMaterialsQuestionsReceiptsAndArtifacts(t *testing.T) {
	source, _ := transferFixture(t)
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	projects.SetHubID("source")
	if _, err := projects.Bind(t.Context(), "console:a", "p", "owner"); err != nil {
		t.Fatal(err)
	}
	tasks, _ := task.OpenLedger(book, "")
	title := "important work"
	labels := []string{"keep"}
	if _, err := tasks.SetMeta("1", task.MetaPatch{Title: &title, Labels: &labels}); err != nil {
		t.Fatal(err)
	}
	mats, err := material.Open(filepath.Join(source, "materials"), book)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := mats.Capture(t.Context(), material.CaptureInput{Project: "p", Title: "retained reply", MIME: "text/plain", Source: material.Source{Kind: "reply", Conversation: "console:a", ReplyID: "r1"}, Data: []byte("quoted source material")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mats.SaveAnnotation(t.Context(), "owner", material.AnnotationInput{ID: "note1", Project: "p", Ref: material.Ref{ID: captured.ID}, Body: "retain my annotation"}); err != nil {
		t.Fatal(err)
	}
	mats.Close()
	now := time.Now().UTC()
	transcript := map[string]any{"replies": map[string][]consoleapi.Reply{"console:a": {{ID: "r1", Conversation: "console:a", ProjectID: "p", At: now, Kind: "reply", Text: "task #1 is original prose", Refs: []material.Ref{{ID: captured.ID}}}}}, "exchanges": map[string][]console.TransferExchange{"console:a": {{Exchange: consoleapi.Exchange{ID: "e1", Conversation: "console:a", ExpectedProject: "p", Input: "next", State: "queued", Key: "client:stable", EnqueuedAt: now}, PayloadHash: "originalhash"}}}, "questions": map[string]consoleapi.PendingQuestion{"q1": {ID: "q1", Project: "p", TaskID: "1", Conversation: "console:a", ExchangeID: "e1", Principal: "owner", State: "pending", CreatedAt: now, Deadline: now.Add(time.Hour)}}}
	raw, _ := json.Marshal(transcript)
	if err := book.Document("console").Save(raw); err != nil {
		t.Fatal(err)
	}
	art := artifact.New(filepath.Join(source, "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	ws, err := art.Materialize(t.Context(), project.Request{Project: "p", Isolated: true, Owner: "att-source"})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws.Path, "result.txt"), []byte("versioned result"), 0600)
	result, _, err := art.Publish(t.Context(), ws, ws.Base, "att-source", "result")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := art.Bind(t.Context(), "steve/1/work", 0, result.ID); err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	record, err := attempts.Open(t.Context(), attempt.Spec{ID: "att-source", TaskID: "1", Kind: attempt.KindStep, Project: "p", Workspace: ws, Scope: attempt.ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []attempt.State{attempt.Prepared, attempt.Running, attempt.BindReady} {
		if _, err := attempts.Advance(t.Context(), record.ID, state, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := attempts.Complete(t.Context(), record.ID, "test", attempt.Completion{Result: attempt.Result{Artifact: result.ID, Summary: "finished"}, Usage: &attempt.Usage{Input: 123, Reported: true}}); err != nil {
		t.Fatal(err)
	}
	book.Close()
	path := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	if _, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Home: home, Input: path}); err != nil {
		t.Fatal(err)
	}
	targetBook, err := ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer targetBook.Close()
	importedMats, err := material.Open(filepath.Join(target, "materials"), targetBook)
	if err != nil {
		t.Fatal(err)
	}
	defer importedMats.Close()
	notes, err := importedMats.Annotations(t.Context(), "p")
	if err != nil || len(notes) != 1 || notes[0].Body != "retain my annotation" {
		t.Fatalf("annotations=%+v %v", notes, err)
	}
	frozen, err := importedMats.ResolveRefs(t.Context(), "p", []material.Ref{{ID: captured.ID}})
	if err != nil || len(frozen) != 1 || frozen[0].Material.Source.Conversation != "console:a" {
		t.Fatalf("immutable provenance changed: %+v %v", frozen, err)
	}
	moved, err := console.ExportProject(targetBook.Document("console"), "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := consoleNamespace("source", "p", "console:a")
	if len(moved.Replies[conv]) != 1 || moved.Replies[conv][0].Text != "task #1 is original prose" {
		t.Fatalf("reply moved incorrectly: %+v", moved.Replies)
	}
	if moved.Questions["q1"].State != "interrupted" || moved.Questions["q1"].TaskID != namespace("source", "1") {
		t.Fatalf("question reference=%+v", moved.Questions["q1"])
	}
	if moved.Exchanges[conv][0].Key != "client:stable" || moved.Exchanges[conv][0].PayloadHash != "originalhash" {
		t.Fatal("keyed receipt identity lost")
	}
	targetAttempts := attempt.New(targetBook)
	got, err := targetAttempts.Get(t.Context(), "att-source")
	if err != nil || got.TaskID != namespace("source", "1") || got.Usage.Input != 123 || len(got.Leases) != 0 {
		t.Fatalf("attempt transfer=%+v %v", got, err)
	}
	targetProjects := project.Open(targetBook)
	targetProjects.SetHubID("target")
	targetArt := artifact.New(filepath.Join(target, "artifacts"), targetBook, targetProjects, artifact.LocalNodes{Dir: t.TempDir()})
	repo, err := targetArt.Repo(t.Context(), "p")
	if err != nil || !repo.Has(t.Context(), result.ID) {
		t.Fatal("historical artifact object missing", err)
	}
}
