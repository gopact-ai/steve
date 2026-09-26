package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// Why a Chinese literal may stay outside the i18n catalog. Everything
// else Steve says to a person — a chat reply, a card, a console answer, a
// CLI line or a tool result — is a catalog entry rendered in that
// person's language.
const (
	textCatalog = "the catalog's own tables"
	textModel   = "text an agent reads as its input; the language rule tells the agent which language to answer in"
	textInput   = "words recognised in what a person or a model writes, not text Steve says"
	textLocale  = "chosen beside its English by the locale it is given"
	textDouble  = "a scripted agent for tests"
)

// userTextExempt names, by file or by file#function, the literals that
// are not Steve speaking to a person. A package-level literal is file#.
var userTextExempt = map[string]string{
	"internal/i18n/catalog.go": textCatalog,

	"cmd/mockagent/main.go": textDouble,

	"internal/planner/llm.go#renderPrompt":                textModel,
	"internal/planner/llm.go#run":                         textModel,
	"internal/ctxpack/ctxpack.go#Render":                  textModel,
	"internal/ctxpack/ctxpack.go#Bearings":                textModel,
	"internal/exec/verify.go#verifyBrief":                 textModel,
	"internal/exec/agent_runner.go#":                      textModel,
	"internal/delegate/delegate.go#":                      textModel,
	"internal/delegate/delegate.go#expectLine":            textModel,
	"internal/delegate/deliver.go#Prompt":                 textModel,
	"internal/delegate/deliver.go#writeChild":             textModel,
	"internal/delegate/deliver.go#stateWord":              textModel,
	"internal/delegate/deliver.go#landFor":                textModel,
	"internal/delegate/preface.go#Preface":                textModel,
	"internal/app/titler.go#titlePrompt":                  textModel,
	"internal/onboard/onboard.go#Prompt":                  textModel,
	"internal/onboard/onboard.go#sharedPrompt":            textModel,
	"internal/console/console.go#quoteBlock":              textModel,
	"internal/console/console.go#orUnknown":               textModel,
	"internal/console/rewind.go#rewindHistory":            textModel,
	"internal/console/relocation.go#relocationInput":      textModel,
	"internal/material/types.go#PromptText":               textModel,
	"internal/memory/memory.go#Template":                  textModel,
	"internal/agentmcp/inform.go#":                        textModel,
	"internal/agentmcp/inform.go#steveContext":            textModel,
	"internal/agentmcp/memory.go#memoryTools":             textModel,
	"internal/agentmcp/memory.go#steveRecall":             textModel,
	"internal/turn/coordinator_inform.go#Where":           textModel,
	"internal/turn/coordinator_inform.go#WhereProjects":   textModel,
	"internal/turn/coordinator_inform.go#orNone":          textModel,
	"internal/turn/coordinator_inform.go#repoWord":        textModel,
	"internal/turn/recovery_relocate.go#relocationPrompt": textModel,

	"internal/schedule/spec.go":                        textInput,
	"internal/turn/commands_tasks.go#":                 textInput,
	"internal/turn/commands_schedule.go#firingVerdict": textInput,
	"internal/memory/memory.go#":                       textInput,
	"internal/memory/memory.go#Sections":               textInput,
	"internal/app/titler.go#cleanTitle":                textInput,

	"internal/home/": textLocale,
	"internal/onboard/draft.go#continueProfile":                               textLocale,
	"internal/app/application_memory.go#prepareApplicationMemoryWithSettings": textLocale,
}

// TestUserTextGoesThroughTheCatalog keeps what Steve says to a person in
// the catalog. A Chinese literal outside the catalog reaches an English
// reader untranslated; it is either exempt for a stated reason or listed
// in testdata/user_text.txt, which only shrinks.
func TestUserTextGoesThroughTheCatalog(t *testing.T) {
	root := repoRoot(t)
	used := map[string]bool{}
	counts := map[string]int{}
	for _, file := range sourceFiles(t, root) {
		rel, _ := filepath.Rel(root, file)
		rel = filepath.ToSlash(rel)
		syntax, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, lit := range hanLiterals(syntax) {
			site := rel + "#" + lit
			if reason := userTextExemption(rel, site); reason != "" {
				used[reason] = true
				continue
			}
			counts[site]++
		}
	}
	for key := range userTextExempt {
		if !used[key] {
			t.Errorf("exemption %s matches no literal; remove it", key)
		}
	}
	current := make([]string, 0, len(counts))
	for site, n := range counts {
		current = append(current, fmt.Sprintf("%s %d", site, n))
	}
	ratchetWith(t, "user_text", current, func(string) string {
		return "say it through an i18n.Key with zh and en entries"
	})
}

// userTextExemption returns the exemption key that covers site, or "".
func userTextExemption(rel, site string) string {
	for _, key := range []string{site, rel} {
		if _, ok := userTextExempt[key]; ok {
			return key
		}
	}
	for dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." && dir != "/"; dir = filepath.ToSlash(filepath.Dir(dir)) {
		if _, ok := userTextExempt[dir+"/"]; ok {
			return dir + "/"
		}
	}
	return ""
}

// hanLiterals returns, for each string literal with a Han character, the
// name of the function declaring it ("" at package level).
func hanLiterals(syntax *ast.File) []string {
	var out []string
	visit := func(fn string, node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(lit.Value); err == nil && strings.IndexFunc(value, isHan) >= 0 {
				out = append(out, fn)
			}
			return true
		})
	}
	for _, decl := range syntax.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			visit(fd.Name.Name, fd)
			continue
		}
		visit("", decl)
	}
	sort.Strings(out)
	return out
}

func isHan(r rune) bool { return unicode.Is(unicode.Han, r) }
