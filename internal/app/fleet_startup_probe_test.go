package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Registration is not execution consent. Keep the assembly from wiring an
// automatic model-session probe; the explicit probe path remains available.
func TestFleetWorkersDoNotProbeEveryConfiguredAgentAtStartup(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "assemble_fleet_workers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "ProbeAll" {
			t.Error("startup starts configured agent sessions without an explicit probe or job")
		}
		return true
	})
}
