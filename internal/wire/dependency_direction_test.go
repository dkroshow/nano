package wire

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWireDependencyDirection(t *testing.T) {
	root := filepath.Clean("../..")
	for _, provider := range []string{"codex", "claude", "grok"} {
		providerRoot := filepath.Join(root, "internal", "wire", provider)
		if _, err := os.Stat(providerRoot); err != nil {
			t.Errorf("internal/wire/%s is missing: %v", provider, err)
			continue
		}
		err := filepath.WalkDir(providerRoot, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				dependency, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if !strings.HasPrefix(dependency, "github.com/dkroshow/nano/internal/") {
					continue
				}
				isTest := strings.HasSuffix(path, "_test.go")
				if dependency == "github.com/dkroshow/nano/internal/wire" || (isTest && (dependency == "github.com/dkroshow/nano/internal/wire/wiretest" || dependency == "github.com/dkroshow/nano/internal/wire/"+provider)) {
					continue
				}
				t.Errorf("%s imports upward dependency %s", strings.TrimPrefix(path, root+string(filepath.Separator)), dependency)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
