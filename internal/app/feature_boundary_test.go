package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFeaturePackagesUseOnlyInteractAndStoreUtilitiesFromNano(t *testing.T) {
	var files []string
	for _, root := range []string{"../features"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && filepath.Ext(path) == ".go" {
				files = append(files, path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			// Store owners share one home for the 0600 file rule and connection
			// settings, not core state access.
			if name == "github.com/dkroshow/nano/internal/state/db" && (path == "../features/heartbeat/store.go" || path == "../features/mail/store.go" || path == "../features/feedback/store.go") {
				alias := "db"
				if spec.Name != nil {
					alias = spec.Name.Name
				}
				if alias == "." || alias == "_" {
					t.Errorf("%s must import state/db with a named alias", path)
				}
				ast.Inspect(file, func(node ast.Node) bool {
					selector, ok := node.(*ast.SelectorExpr)
					if ok {
						if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == alias && selector.Sel.Name != "Open" && selector.Sel.Name != "URI" {
							t.Errorf("%s uses state/db.%s; only Open and URI are store policy", path, selector.Sel.Name)
						}
					}
					return true
				})
				continue
			}
			// Default paths share no application state and depend only on the standard library.
			if name == "github.com/dkroshow/nano/internal/configpaths" {
				continue
			}
			if strings.HasPrefix(name, "github.com/dkroshow/nano/internal/") && name != "github.com/dkroshow/nano/internal/interact" {
				t.Errorf("%s imports private package %s", path, name)
			}
		}
	}
}
