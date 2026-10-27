package remoteauth

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPackageImportsOnlyEdgeDependencies(t *testing.T) {
	allowed := map[string]bool{
		"github.com/dkroshow/nano/internal/configpaths": true, // shared default paths; standard library only
		"github.com/dkroshow/nano/internal/files":       true,
		"github.com/dkroshow/nano/internal/httpapi":     true,
	}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			name, _ := strconv.Unquote(spec.Path.Value)
			// Only the store may share the single 0600 SQLite file rule.
			if path == "store.go" && name == "github.com/dkroshow/nano/internal/state/db" {
				continue
			}
			if strings.HasPrefix(name, "github.com/dkroshow/nano/") && !allowed[name] {
				t.Fatalf("%s imports %s", path, name)
			}
		}
	}
}
