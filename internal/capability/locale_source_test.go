package capability

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/home"
)

func TestDynamicLocaleKeepsNativeSessionFingerprint(t *testing.T) {
	files := func() (map[string]string, error) {
		return map[string]string{home.FileSoul: "soul", home.FileUser: "private user", home.FileMemory: "private memory"}, nil
	}
	for _, mode := range []home.Mode{home.ModeNone, home.ModeOwner, home.ModeGuest} {
		for _, pinned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pinned=%t", mode, pinned), func(t *testing.T) {
				locale, reads := home.LocaleZH, 0
				servers := map[string]MCPServer{"tool": {Type: "http", URL: "http://localhost:1234/mcp"}}
				skills := &fakeSkills{hash: "live-skills"}
				a := NewAssembler(servers).SetLocale(home.LocaleEN).SetSkills(skills).
					SetHome(home.Reader{ReadFiles: files, LocaleSource: func() home.Locale { return locale }}).
					SetLocaleSource(func() home.Locale { reads++; return locale })
				selected := agent.Agent{ID: "agent", Config: agent.Config{SystemPrompt: "agent instructions", MCPServers: []string{"tool"}}}
				extras := []Extra{{Name: "guidance", Instructions: "platform guidance", Memory: "project memory"}}
				assemble := func(a *Assembler) Capabilities {
					t.Helper()
					var got Capabilities
					var err error
					if pinned {
						got, err = a.AssembleExtraPinned(selected, mode, extras, "pinned-skills")
					} else {
						got, err = a.AssembleExtra(selected, mode, extras)
					}
					if err != nil {
						t.Fatal(err)
					}
					return got
				}
				var previous Capabilities
				for _, next := range []home.Locale{home.LocaleZH, home.LocaleEN, ""} {
					locale = next
					before := reads
					got := assemble(a)
					want := assemble(NewAssembler(servers).SetSkills(skills).SetLocale(next).
						SetHome(home.Reader{ReadFiles: files, Locale: next}))
					if reads != before+1 || !reflect.DeepEqual(got, want) {
						t.Fatalf("assembly differs from sampled locale %q: reads=%d\ngot=%+v\nwant=%+v", next, reads-before, got, want)
					}
					if previous.SessionFingerprint != "" && (got.SessionFingerprint != previous.SessionFingerprint || got.Fingerprint == previous.Fingerprint) {
						t.Fatal("locale must refresh capability guidance without changing native session identity")
					}
					previous = got
				}
			})
		}
	}
}

func TestAssemblerSamplesLocaleAtEntry(t *testing.T) {
	locale, reads := home.LocaleZH, 0
	a := NewAssembler(nil).SetLocaleSource(func() home.Locale { reads++; return locale }).
		SetHome(home.Reader{ReadFiles: func() (map[string]string, error) {
			locale = home.LocaleEN
			return map[string]string{home.FileSoul: "soul"}, nil
		}})
	for _, want := range []home.Locale{home.LocaleZH, home.LocaleEN} {
		before := reads
		got, err := a.AssembleMode(agent.Agent{ID: "agent"}, home.ModeGuest)
		if err != nil {
			t.Fatal(err)
		}
		rule := home.LanguageRule(want)
		if reads != before+1 || !strings.Contains(got.Instructions, rule) {
			t.Fatalf("entry locale %q was not retained: reads=%d instructions=%q", want, reads-before, got.Instructions)
		}
		found := false
		for _, section := range got.Sections {
			if section.Kind == SectionLanguage {
				found = true
				if section.Name != string(want) || section.Bytes != len(rule) {
					t.Fatalf("language section disagrees with sampled instructions: %+v", section)
				}
			}
		}
		if !found {
			t.Fatal("language section missing")
		}
	}
}

func TestAssemblerNilLocaleSourceKeepsStaticLocale(t *testing.T) {
	got, err := NewAssembler(nil).SetLocale(home.LocaleEN).SetLocaleSource(nil).Assemble(agent.Agent{})
	if err != nil || !strings.Contains(got.Instructions, home.LanguageRule(home.LocaleEN)) {
		t.Fatalf("nil source lost the configured locale: %+v %v", got, err)
	}
}
