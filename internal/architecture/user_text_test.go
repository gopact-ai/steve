package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
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

// userTextExempt names, one declaration each, the literals that are not
// Steve speaking to a person: file#function, file#Type.Method, or
// file#name for a package-level constant, variable or type.
var userTextExempt = map[string]string{
	"internal/i18n/catalog.go#zh": textCatalog,
	"internal/i18n/catalog.go#en": textCatalog,

	"cmd/mockagent/main.go#mockPlan":      textDouble,
	"cmd/mockagent/main.go#scriptedReply": textDouble,

	"internal/exec/agent_runner.go#ReportingContract":               textModel,
	"internal/delegate/delegate.go#worktreeContract":                textModel,
	"internal/agentmcp/inform.go#helpTopics":                        textModel,
	"internal/planner/llm.go#renderPrompt":                          textModel,
	"internal/planner/llm.go#LLM.run":                               textModel,
	"internal/ctxpack/ctxpack.go#Context.Render":                    textModel,
	"internal/ctxpack/ctxpack.go#Bearings":                          textModel,
	"internal/exec/verify.go#verifyBrief":                           textModel,
	"internal/delegate/delegate.go#expectLine":                      textModel,
	"internal/delegate/deliver.go#Delivery.Prompt":                  textModel,
	"internal/delegate/deliver.go#writeChild":                       textModel,
	"internal/delegate/deliver.go#stateWord":                        textModel,
	"internal/delegate/deliver.go#Service.landFor":                  textModel,
	"internal/delegate/preface.go#Service.Preface":                  textModel,
	"internal/app/titler.go#titlePrompt":                            textModel,
	"internal/onboard/onboard.go#Prompt":                            textModel,
	"internal/onboard/onboard.go#sharedPrompt":                      textModel,
	"internal/console/console.go#Service.quoteBlock":                textModel,
	"internal/console/console.go#orUnknown":                         textModel,
	"internal/console/rewind.go#rewindHistory":                      textModel,
	"internal/console/relocation.go#Service.relocationInput":        textModel,
	"internal/material/types.go#Frozen.PromptText":                  textModel,
	"internal/agentmcp/inform.go#Server.steveContext":               textModel,
	"internal/agentmcp/memory.go#memoryTools":                       textModel,
	"internal/agentmcp/memory.go#Server.steveRecall":                textModel,
	"internal/turn/coordinator_inform.go#Coordinator.Where":         textModel,
	"internal/turn/coordinator_inform.go#Coordinator.WhereProjects": textModel,
	"internal/turn/coordinator_inform.go#orNone":                    textModel,
	"internal/turn/coordinator_inform.go#repoWord":                  textModel,
	"internal/turn/recovery_relocate.go#relocationPrompt":           textModel,

	"internal/schedule/spec.go#chineseDuration":        textInput,
	"internal/schedule/spec.go#parseDuration":          textInput,
	"internal/schedule/spec.go#clock":                  textInput,
	"internal/schedule/spec.go#parseClock":             textInput,
	"internal/schedule/spec.go#weekdays":               textInput,
	"internal/schedule/spec.go#isEveryDayWord":         textInput,
	"internal/schedule/spec.go#isTomorrowWord":         textInput,
	"internal/turn/commands_tasks.go#taskVerbs":        textInput,
	"internal/memory/memory.go#sectionAliases":         textInput,
	"internal/turn/commands_schedule.go#firingVerdict": textInput,
	"internal/memory/memory.go#Sections":               textInput,
	"internal/app/titler.go#cleanTitle":                textInput,

	"internal/memory/memory.go#Template":                                      textLocale,
	"internal/home/reader.go#Reader.Load":                                     textLocale,
	"internal/home/templates.go#UserLabelNameZH":                              textLocale,
	"internal/home/templates.go#UserLabelTimezoneZH":                          textLocale,
	"internal/home/templates.go#templateSoulZH":                               textLocale,
	"internal/home/templates.go#templateUserZH":                               textLocale,
	"internal/home/templates.go#templateMemoryZH":                             textLocale,
	"internal/home/templates.go#ownerWrapper":                                 textLocale,
	"internal/home/templates.go#guestWrapper":                                 textLocale,
	"internal/home/templates.go#LanguageRule":                                 textLocale,
	"internal/home/templates.go#ListenUnmentioned":                            textLocale,
	"internal/onboard/draft.go#continueProfile":                               textLocale,
	"internal/app/application_memory.go#prepareApplicationMemoryWithSettings": textLocale,
}

// exemptDeclaration is the shape of an exemption key: a Go file and one
// declaration in it.
var exemptDeclaration = regexp.MustCompile(`^[\w./-]+\.go#(\w+\.)?\w+$`)

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
		if !exemptDeclaration.MatchString(key) {
			t.Errorf("exemption %s names no single declaration: name the function, the Type.Method or the package-level value", key)
		}
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
// Only the named declaration is covered: a file, a directory or every
// package-level declaration of a file is never exempt as a whole.
func userTextExemption(rel, site string) string {
	if _, ok := userTextExempt[site]; ok {
		return site
	}
	return ""
}

// hanLiterals returns, for each string literal with a Han character, the
// declaration holding it: a function's name, a method's Type.Method, or
// the name of a package-level constant, variable or type.
func hanLiterals(syntax *ast.File) []string {
	var out []string
	visit := func(name string, node ast.Node) {
		if node == nil {
			return
		}
		ast.Inspect(node, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(lit.Value); err == nil && strings.IndexFunc(value, isHan) >= 0 {
				out = append(out, name)
			}
			return true
		})
	}
	for _, decl := range syntax.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			visit(declName(decl), decl)
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					for i, name := range spec.Names {
						if len(spec.Values) == len(spec.Names) {
							visit(name.Name, spec.Values[i])
						} else if i == 0 {
							visit(name.Name, spec)
						}
					}
				case *ast.TypeSpec:
					visit(spec.Name.Name, spec)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// declName is a function's name, or Type.Method for a method.
func declName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	switch generic := typ.(type) {
	case *ast.IndexExpr:
		typ = generic.X
	case *ast.IndexListExpr:
		typ = generic.X
	}
	if ident, ok := typ.(*ast.Ident); ok {
		return ident.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func isHan(r rune) bool { return unicode.Is(unicode.Han, r) }

// An exemption covers the one declaration it names. A literal added
// elsewhere in the same file, directory or package-level block — or in a
// function that merely shares an exempt method's name — still counts.
func TestUserTextExemptionsCoverOnlyTheNamedDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name, rel, src string
		exempt         bool
	}{
		{"a new function in an exempt file", "internal/schedule/spec.go", `package schedule
import "errors"
func probeFull() error { return errors.New("定时任务已满，请先取消一个") }`, false},
		{"a new package-level value in an exempt file", "internal/turn/commands_tasks.go", `package turn
import "errors"
var probeTaskMissing = errors.New("任务不存在或已被删除")`, false},
		{"a new file in an exempt directory", "internal/home/probe.go", `package home
func probeSave() string { return "档案保存失败，请检查磁盘权限" }`, false},
		{"a function named like an exempt method", "internal/turn/coordinator_inform.go", `package turn
func Where() string { return "当前项目不可用" }`, false},
		{"the exempt method itself", "internal/turn/coordinator_inform.go", `package turn
func (c *Coordinator) Where() string { return "项目" }`, true},
	} {
		syntax, err := parser.ParseFile(token.NewFileSet(), tc.rel, tc.src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		lits := hanLiterals(syntax)
		if len(lits) == 0 {
			t.Fatalf("%s: no literal found", tc.name)
		}
		for _, lit := range lits {
			if reason := userTextExemption(tc.rel, tc.rel+"#"+lit); (reason != "") != tc.exempt {
				t.Errorf("%s: %s#%s exempt by %q, want exempt=%v", tc.name, tc.rel, lit, reason, tc.exempt)
			}
		}
	}
}
