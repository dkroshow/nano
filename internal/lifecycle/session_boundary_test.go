package lifecycle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The coordinator may initialize from New or install Transition's result.
// Copies of that snapshot cannot become a second mutable status/work owner.
func TestSessionLifecycleHasOneMutationBoundary(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fs := token.NewFileSet()
		file, err := parser.ParseFile(fs, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			if typeDecl, ok := declaration.(*ast.GenDecl); ok {
				for _, spec := range typeDecl.Specs {
					typ, ok := spec.(*ast.TypeSpec)
					if !ok || typ.Name.Name != "session" {
						continue
					}
					for _, field := range typ.Type.(*ast.StructType).Fields.List {
						for _, name := range field.Names {
							switch name.Name {
							case "status", "turnActive", "threadStatus":
								t.Errorf("mutable session state alias %s at %s", name.Name, fs.Position(name.Pos()))
							}
						}
					}
				}
			}
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(node ast.Node) bool {
				assignment, ok := node.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range assignment.Lhs {
					if !writesLifecycleSnapshot(lhs) {
						continue
					}
					if filepath.Base(path) == "session.go" && fn.Name.Name == "spawnSession" && len(assignment.Rhs) == 1 {
						if call, ok := assignment.Rhs[0].(*ast.CallExpr); ok {
							if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
								if name, ok := selector.X.(*ast.Ident); ok && name.Name == "lifecycle" && selector.Sel.Name == "New" {
									continue
								}
							}
						}
					}
					if filepath.Base(path) == "session_lifecycle.go" && fn.Name.Name == "applyLifecycleObservationLocked" {
						writes++
					} else {
						t.Errorf("snapshot mutation outside lifecycle boundary at %s", fs.Position(lhs.Pos()))
					}
				}
				return true
			})
		}
	}
	if writes != 1 {
		t.Fatalf("live snapshot assignments = %d, want one", writes)
	}
}

func writesLifecycleSnapshot(expr ast.Expr) bool {
	switch node := expr.(type) {
	case *ast.SelectorExpr:
		return node.Sel.Name == "lifecycleState" || writesLifecycleSnapshot(node.X)
	case *ast.IndexExpr:
		return writesLifecycleSnapshot(node.X)
	}
	return false
}
