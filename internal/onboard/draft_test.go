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

func TestAllowScan(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "default", input: "叫我李总，时区对的", want: true},
		{name: "explicit allow", input: "允许扫描", want: true},
		{name: "deny chinese", input: "不允许扫描会话", want: false},
		{name: "deny english", input: "please do not scan", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AllowScan(tt.input); got != tt.want {
				t.Fatalf("AllowScan(%q)=%v want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseDraftRejectsInstructionEcho(t *testing.T) {
	if _, err := ParseDraft(Continue(i18n.LocaleZH, "/tmp/home", "")); err == nil {
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
	reply, written, err := Apply(dir, "先问问称呼。")
	if err != nil || written || reply != "先问问称呼。" {
		t.Fatalf("reply=%q written=%v err=%v", reply, written, err)
	}
	if !home.NeedsInit(dir) {
		t.Fatal("template should remain")
	}
}

func TestContinueMentionsExcerpts(t *testing.T) {
	got := Continue(i18n.LocaleZH, "/tmp/home", "codex: likes tea")
	if !strings.Contains(got, "likes tea") || !strings.Contains(got, soulFence) {
		t.Fatalf("continue = %s", got)
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
