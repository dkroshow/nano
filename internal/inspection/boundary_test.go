package inspection

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInspectionDependencyDirection(t *testing.T) {
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
			// Direction now lives at its canonical owner in wire; wire/codex already depended on it.
			// files is a standard-library leaf: git details read through its protected-folder walk.
			if strings.HasPrefix(dependency, "github.com/dkroshow/nano/") && dependency != "github.com/dkroshow/nano/internal/wire/codex" && dependency != "github.com/dkroshow/nano/internal/wire" && dependency != "github.com/dkroshow/nano/internal/files" {
				t.Errorf("%s imports forbidden dependency %s", path, dependency)
			}
		}
	}
}
