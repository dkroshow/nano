package catalog

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCatalogDependencyDirection(t *testing.T) {
	allowed := map[string]bool{
		"github.com/dkroshow/nano/internal/history":   true,
		"github.com/dkroshow/nano/internal/lifecycle": true,
		"github.com/fsnotify/fsevents":                true,
	}
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
			first, _, _ := strings.Cut(dependency, "/")
			if (strings.Contains(first, ".") || strings.HasPrefix(dependency, "github.com/dkroshow/nano/")) && !allowed[dependency] {
				t.Errorf("%s imports forbidden dependency %s", path, dependency)
			}
		}
	}
}
