package db

import (
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestStateLayerDependencyDirection(t *testing.T) {
	root := filepath.Clean("../../..")
	assertNoUpwardImports := func(packagePath string) {
		t.Helper()
		err := filepath.WalkDir(filepath.Join(root, packagePath), func(path string, entry os.DirEntry, err error) error {
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
				if dependency == "github.com/dkroshow/nano/internal/diagnostics" || strings.HasPrefix(dependency, "github.com/dkroshow/nano/internal/features/") {
					t.Errorf("%s imports upward dependency %s", strings.TrimPrefix(path, root+string(filepath.Separator)), dependency)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertImports := func(packagePath string, allowed map[string]bool) map[string]bool {
		t.Helper()
		imports := map[string]bool{}
		entries, err := os.ReadDir(filepath.Join(root, packagePath))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, packagePath, entry.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, spec := range file.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				dependency, stdErr := build.Default.Import(path, "", build.FindOnly)
				if stdErr == nil && dependency.Goroot {
					continue
				}
				imports[path] = true
				if !allowed[path] {
					t.Errorf("%s imports forbidden dependency %s", packagePath, path)
				}
			}
		}
		return imports
	}
	stateEntries, err := os.ReadDir(filepath.Join(root, "internal/state"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range stateEntries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			t.Errorf("internal/state contains layer-level package file %s; composition belongs above state/db", entry.Name())
		}
	}
	assertImports("internal/state/db", map[string]bool{"modernc.org/sqlite": true})
	assertImports("internal/lifecycle", nil)
	assertNoUpwardImports("internal/app")
	threadImports := assertImports("internal/threads", map[string]bool{
		"github.com/dkroshow/nano/internal/state/db": true,
		"modernc.org/sqlite":                         true,
	})
	if threadImports["github.com/dkroshow/nano/internal/core/prompts"] {
		t.Fatal("threads imports turns")
	}
	promptImports := assertImports("internal/core/prompts", map[string]bool{
		"github.com/dkroshow/nano/internal/state/db":              true,
		"github.com/dkroshow/nano/internal/threads":               true,
		"github.com/dkroshow/nano/internal/transcriptparse/model": true,
		"modernc.org/sqlite":                                      true,
	})
	if !promptImports["github.com/dkroshow/nano/internal/threads"] {
		t.Fatal("prompts no longer imports threads for shared attachment and backup state")
	}
	assertImports("internal/core/turns", map[string]bool{
		"github.com/dkroshow/nano/internal/threads": true,
		"modernc.org/sqlite":                        true,
	})
}
