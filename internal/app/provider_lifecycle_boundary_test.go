package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"testing"
)

func TestLifecycleMovedFunctionsHaveNoProviderDispatch(t *testing.T) {
	functions := map[string][]string{
		"agent_execution_api.go":          {"nativeThreadIDProvisional"},
		"boot_reattach.go":                {"reattachOnBoot"},
		"codex.go":                        {"startCodexZeroTurnRecovery"},
		"codex_reconnect_state.go":        {"codexZeroTurnNativeMissing"},
		"hosted_native_recovery_store.go": {"nativeRecoveryResumeGuard"},
		"live_projection.go":              {"applyLiveRuntimeWire"},
		"thread_reference_api.go":         {"resolveNativeThreadReference"},
		"turn_send.go":                    {"runTransactionalTurnSend", "drainQueuedTurn"},
		"session.go":                      {"recordWireAt"},
		"provider_lifecycle.go":           nil,
		"session_lifecycle.go":            nil,
		"runtime.go":                      nil,
	}
	for path, names := range functions {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			selected := names == nil
			for _, name := range names {
				if fn.Name.Name == name {
					selected = true
				}
			}
			if !selected {
				continue
			}
			found[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.BinaryExpr:
					if (value.Op == token.EQL || value.Op == token.NEQ) && (lifecycleProviderName(value.X) || lifecycleProviderName(value.Y)) {
						t.Errorf("provider dispatch returned to %s", fset.Position(value.Pos()))
					}
				case *ast.CaseClause:
					for _, expr := range value.List {
						if lifecycleProviderName(expr) {
							t.Errorf("provider switch returned to %s", fset.Position(expr.Pos()))
						}
					}
				}
				return true
			})
		}
		for _, name := range names {
			if !found[name] {
				t.Errorf("update the disposition of removed function %s.%s", path, name)
			}
		}
	}
	for _, path := range []string{"delegation_state.go", "delegation_reconciliation.go"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("retired delegating source exists or is unreadable: %s: %v", path, err)
		}
	}
}

func lifecycleProviderName(expr ast.Expr) bool {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return false
		}
		name, _ := strconv.Unquote(value.Value)
		return name == "codex" || name == "claude" || name == "grok"
	case *ast.Ident:
		return value.Name == "ProviderCodex" || value.Name == "ProviderClaude" || value.Name == "ProviderGrok"
	case *ast.SelectorExpr:
		return lifecycleProviderName(value.Sel)
	}
	return false
}
