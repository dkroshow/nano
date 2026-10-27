package diagnostics

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPackageHasNoNanoDependencies(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			name, _ := strconv.Unquote(spec.Path.Value)
			if name != "" && len(name) >= len("github.com/dkroshow/nano/") && name[:len("github.com/dkroshow/nano/")] == "github.com/dkroshow/nano/" {
				t.Fatalf("%s imports %s", path, name)
			}
		}
	}
}
