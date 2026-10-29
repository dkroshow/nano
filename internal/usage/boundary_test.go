package usage

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestUsageDependencyDirection(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			dependency, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			// The read API uses the shared HTTP encoding, and the store shares
			// the single 0600 SQLite file rule; it
			// still cannot depend on application state.
			if path == "http.go" && dependency == "github.com/dkroshow/nano/internal/httpapi" {
				continue
			}
			if path == "store.go" && dependency == "github.com/dkroshow/nano/internal/state/db" {
				continue
			}
			if strings.HasPrefix(dependency, "github.com/dkroshow/nano/") {
				t.Errorf("%s imports Nano package %s", path, dependency)
			}
		}
	}
}
