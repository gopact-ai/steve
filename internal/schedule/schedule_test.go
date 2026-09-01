package schedule

import (
	"path/filepath"
	"testing"
	"time"
)

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, time.Local)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// The parser's real job is deciding where the schedule stops and the
// instruction starts. Everything else is arithmetic.
func TestParseEverySplitsSpecFromPrompt(t *testing.T) {
	now := at(t, "2026-08-28 09:30") // a Friday
	for _, tc := range []struct {
		in     string
		kind   Kind
		prompt string
		next   string
	}{
		{"30m 看一眼 CI", KindEvery, "看一眼 CI", "2026-08-28 10:00"},
		{"2小时 巡检", KindEvery, "巡检", "2026-08-28 11:30"},
		{"09:00 汇报昨天进展", KindDaily, "汇报昨天进展", "2026-08-29 09:00"},
		{"每天 09:00 汇报", KindDaily, "汇报", "2026-08-29 09:00"},
		{"day 10:00 report", KindDaily, "report", "2026-08-28 10:00"},
		{"周一 09:00 周会材料", KindDaily, "周会材料", "2026-08-31 09:00"},
		{"mon 09:00 weekly", KindDaily, "weekly", "2026-08-31 09:00"},
	} {
		spec, prompt, err := ParseEvery(tc.in, now)
		if err != nil {
			t.Errorf("ParseEvery(%q): %v", tc.in, err)
			continue
		}
		if spec.Kind != tc.kind || prompt != tc.prompt {
			t.Errorf("ParseEvery(%q) = %s/%q; want %s/%q", tc.in, spec.Kind, prompt, tc.kind, tc.prompt)
		}
		if got := spec.Next(now); !got.Equal(at(t, tc.next)) {
			t.Errorf("ParseEvery(%q).Next = %s; want %s", tc.in, got, tc.next)
		}
	}
}

func TestParseAtResolvesOneMoment(t *testing.T) {
	now := at(t, "2026-08-28 09:30")
	for _, tc := range []struct {
		in     string
		prompt string
		when   string
	}{
		{"10:00 提醒我站会", "提醒我站会", "2026-08-28 10:00"},
		{"09:00 明早的事", "明早的事", "2026-08-29 09:00"},
		{"明天 09:00 复盘", "复盘", "2026-08-29 09:00"},
		{"30m 回来看结果", "回来看结果", "2026-08-28 10:00"},
		{"2026-09-01 09:00 月初检查", "月初检查", "2026-09-01 09:00"},
	} {
		spec, prompt, err := ParseAt(tc.in, now)
		if err != nil {
			t.Errorf("ParseAt(%q): %v", tc.in, err)
			continue
		}
		if spec.Kind != KindOnce || prompt != tc.prompt {
			t.Errorf("ParseAt(%q) = %s/%q; want once/%q", tc.in, spec.Kind, prompt, tc.prompt)
		}
		if !spec.At.Equal(at(t, tc.when)) {
			t.Errorf("ParseAt(%q).At = %s; want %s", tc.in, spec.At, tc.when)
		}
	}
}

// A schedule that fires faster than once a minute is a loop with a chat
// window attached, and a schedule with no instruction has nothing to run.
func TestParseRejectsWhatCannotWork(t *testing.T) {
	now := at(t, "2026-08-28 09:30")
	for _, in := range []string{"10s 太快了", "30m", "yesterday 09:00 nope", "", "看一眼 CI"} {
		if _, _, err := ParseEvery(in, now); err == nil {
			t.Errorf("ParseEvery(%q) was accepted", in)
		}
	}
}

func TestDueClaimsAndAdvancesInOneWrite(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := at(t, "2026-08-28 09:30")
	store.now = func() time.Time { return now }

	recurring, err := store.Create(Job{
		ConversationID: "chat", AnchorMessage: "om_a", Member: "codex",
		Prompt: "check CI", Spec: Spec{Kind: KindEvery, Every: 30 * time.Minute},
	})
	if err != nil {
		t.Fatalf("create recurring: %v", err)
	}
	if _, err := store.Create(Job{
		ConversationID: "chat", AnchorMessage: "om_b", Member: "codex",
		Prompt: "one shot", Spec: Spec{Kind: KindOnce, At: now.Add(10 * time.Minute)},
	}); err != nil {
		t.Fatalf("create one-shot: %v", err)
	}

	if due, err := store.Due(now.Add(5 * time.Minute)); err != nil || len(due) != 0 {
		t.Fatalf("Due before anything is due = %v, %v", due, err)
	}
	due, err := store.Due(now.Add(31 * time.Minute))
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %d jobs; want both", len(due))
	}
	// Claiming and advancing in one write is what stops a slow run from
	// being fired again by the next tick.
	if again, err := store.Due(now.Add(31 * time.Minute)); err != nil || len(again) != 0 {
		t.Fatalf("Due fired the same jobs twice: %v, %v", again, err)
	}
	remaining := store.List("chat")
	if len(remaining) != 1 || remaining[0].ID != recurring.ID {
		t.Fatalf("remaining = %+v; want only the recurring job", remaining)
	}
	if remaining[0].Runs != 1 || !remaining[0].NextAt.Equal(now.Add(61*time.Minute)) {
		t.Fatalf("recurring job did not advance: %+v", remaining[0])
	}
}

func TestStoreSurvivesReopenAndBoundsOneConversation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	store, _ := Open(path)
	now := at(t, "2026-08-28 09:30")
	store.now = func() time.Time { return now }
	for i := 0; i < MaxPerConversation; i++ {
		if _, err := store.Create(Job{
			ConversationID: "chat", AnchorMessage: "om_a", Prompt: "work",
			Spec: Spec{Kind: KindEvery, Every: time.Hour},
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := store.Create(Job{
		ConversationID: "chat", AnchorMessage: "om_a", Prompt: "one too many",
		Spec: Spec{Kind: KindEvery, Every: time.Hour},
	}); err == nil {
		t.Fatal("a conversation took more schedules than its ceiling")
	}
	// Another conversation is unaffected: the ceiling is per chat.
	if _, err := store.Create(Job{
		ConversationID: "other", AnchorMessage: "om_b", Prompt: "work",
		Spec: Spec{Kind: KindEvery, Every: time.Hour},
	}); err != nil {
		t.Fatalf("other conversation: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := len(reopened.List("chat")); got != MaxPerConversation {
		t.Fatalf("after reopen chat holds %d schedules; want %d", got, MaxPerConversation)
	}
	if _, ok := reopened.Get("1"); !ok {
		t.Fatal("reopened store lost job #1")
	}
	removed, ok, err := reopened.Delete("1")
	if err != nil || !ok || removed.ID != "1" {
		t.Fatalf("Delete = %+v, %t, %v", removed, ok, err)
	}
	if _, ok := reopened.Get("1"); ok {
		t.Fatal("cancelled schedule survived")
	}
}

// A gateway that was off overnight must not open the morning by running every
// job it slept through — but the jobs still have to move on to their next
// moment rather than staying permanently overdue.
func TestDueSkipsFiringsItSleptThrough(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := at(t, "2026-08-28 09:00")
	store.now = func() time.Time { return now }
	if _, err := store.Create(Job{
		ConversationID: "chat", AnchorMessage: "om_a", Prompt: "daily report",
		Spec: Spec{Kind: KindDaily, Hour: 9, Minute: 0, Weekday: anyDay},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Create(Job{
		ConversationID: "chat", AnchorMessage: "om_b", Prompt: "one shot",
		Spec: Spec{Kind: KindOnce, At: at(t, "2026-08-29 09:00")},
	}); err != nil {
		t.Fatalf("create one-shot: %v", err)
	}

	// Back up at 15:00 the next day: six hours late for both.
	due, err := store.Due(at(t, "2026-08-29 15:00"))
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("fired %d jobs it slept through", len(due))
	}
	jobs := store.List("chat")
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v; want the stale one-shot dropped", jobs)
	}
	if want := at(t, "2026-08-30 09:00"); !jobs[0].NextAt.Equal(want) {
		t.Fatalf("next = %s; want the skipped job moved on to %s", jobs[0].NextAt, want)
	}
	if jobs[0].Runs != 0 {
		t.Fatalf("runs = %d; a skipped firing is not a run", jobs[0].Runs)
	}
}
