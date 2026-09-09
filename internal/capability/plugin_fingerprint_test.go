package capability

import (
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/home"
)

type changingSkills struct{ value string }

func (s *changingSkills) Fingerprint() string { return s.value }

func TestPinnedRuntimeKeepsSkillFingerprintWhileNewSessionsRefresh(t *testing.T) {
	skills := &changingSkills{value: "original"}
	assembler := NewAssembler(nil).SetSkills(skills)
	selected := agent.Agent{ID: "worker", Config: agent.Config{Harness: "mock"}}
	first, err := assembler.AssembleExtra(selected, home.ModeNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	skills.value = "updated"
	pinned, err := assembler.AssembleExtraPinned(selected, home.ModeNone, nil, first.SkillsFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	current, err := assembler.AssembleExtra(selected, home.ModeNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Fingerprint != first.Fingerprint || pinned.SessionFingerprint != first.SessionFingerprint {
		t.Fatal("old runtime drifted with global skills")
	}
	if current.SessionFingerprint == pinned.SessionFingerprint {
		t.Fatal("new session missed updated skills")
	}
	selected.SystemPrompt = "changed agent instructions"
	changed, err := assembler.AssembleExtraPinned(selected, home.ModeNone, nil, first.SkillsFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SessionFingerprint == first.SessionFingerprint {
		t.Fatal("pinning skills hid a different agent configuration")
	}
}
