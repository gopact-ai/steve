package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
)

func TestIncompleteDraftDoesNotReportIdentitySaved(t *testing.T) {
	for _, output := range []string{"已记下。\n===SOUL.md===\n# Soul\n你是用户的可靠助手，需要维护项目。\n", "已记下。\n===USER.md===\n# User\n- name\n", "已记下。\n===SOUL.md===\n<full SOUL.md>\n===USER.md===\n<full USER.md>"} {
		written := false
		reply, saved, err := ApplyWith(output, func(string, string) error { written = true; return nil })
		if err == nil || saved || written || reply != "" {
			t.Fatalf("malformed draft reported success: %q %v %v", reply, saved, err)
		}
	}
	reply, saved, err := ApplyWith("请先告诉我你的时区。", func(string, string) error { t.Fatal("normal question wrote identity"); return nil })
	if err != nil || saved || reply != "请先告诉我你的时区。" {
		t.Fatalf("normal question was rejected: %q %v %v", reply, saved, err)
	}
}

func TestParseDraftRejectsInstructionEcho(t *testing.T) {
	if _, err := ParseDraft(Continue(i18n.LocaleZH, "/tmp/home")); err == nil {
		t.Fatal("instruction echo should not parse as a draft")
	}
}

func TestParseDraftAndApply(t *testing.T) {
	got, err := ParseDraft("先记下。\n===SOUL.md===\n# Soul\nYou are Steve, the owner's assistant.\n===USER.md===\n# User\nLee\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Soul, "# Soul") || !strings.Contains(got.User, "Lee") {
		t.Fatalf("parsed = %#v", got)
	}
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_owner"); err != nil {
		t.Fatal(err)
	}
	reply, written, err := Apply(dir, "已记下，李总。\n\n===SOUL.md===\n# Soul\n你是李总的个人助手，说话短而可执行。\n===USER.md===\n# User\n- 称呼：李总\n")
	if err != nil || !written {
		t.Fatalf("apply written=%v err=%v", written, err)
	}
	if reply != "已记下，李总。" || strings.Contains(reply, soulFence) {
		t.Fatalf("reply = %q", reply)
	}
	user, err := os.ReadFile(filepath.Join(dir, home.FileUser))
	if err != nil {
		t.Fatal(err)
	}
	if home.IsTemplate(string(user)) || !strings.Contains(string(user), "李总") {
		t.Fatalf("user = %s", user)
	}
	if home.NeedsInit(dir) {
		t.Fatal("home still needs init after apply")
	}
}

func TestApplyWithoutDraftKeepsFiles(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_owner"); err != nil {
		t.Fatal(err)
	}
	const existingSoul = "# Soul\nYou are Steve. The owner prefers concise explanations.\n"
	if err := os.WriteFile(filepath.Join(dir, home.FileSoul), []byte(existingSoul), 0o600); err != nil {
		t.Fatal(err)
	}
	reply, written, err := Apply(dir, "先问问称呼。")
	if err != nil || written || reply != "先问问称呼。" {
		t.Fatalf("reply=%q written=%v err=%v", reply, written, err)
	}
	if !home.NeedsInit(dir) {
		t.Fatal("template should remain")
	}
	soul, err := os.ReadFile(filepath.Join(dir, home.FileSoul))
	if err != nil || string(soul) != existingSoul {
		t.Fatalf("ordinary reply changed existing identity: %q %v", soul, err)
	}
}

func TestContinueMakesProfileOptionalAndKeepsOwnerFacts(t *testing.T) {
	for _, locale := range []i18n.Locale{i18n.LocaleZH, i18n.LocaleEN} {
		for _, prompt := range []string{Continue(locale, "/tmp/home"), ContinueShared(locale)} {
			for _, required := range []string{
				"## Optional profile setup",
				"Reply directly to greetings and ordinary questions",
				"Only produce a profile when the owner supplies durable facts",
				"Preserve any existing non-template identity and user facts",
				"Tools remain available for the user's actual request",
				"Do not scan local sessions to complete a profile",
			} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("missing profile decision rule %q: %s", required, prompt)
				}
			}
			if strings.Contains(prompt, "Build their profile now.") || strings.Contains(prompt, "- Do not use tools.") {
				t.Fatalf("profile setup still overrides ordinary conversation: %s", prompt)
			}
		}
	}
}

func TestBuildingSkipsKickoffConversation(t *testing.T) {
	if Building(PendingID("ou"), true, true) {
		t.Fatal("kickoff conversation should not write files")
	}
	if !Building("oc_dm", true, true) {
		t.Fatal("owner follow-up should write files")
	}
	if Building("oc_dm", false, true) {
		t.Fatal("guest should not write files")
	}
}
