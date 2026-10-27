package transcriptparse_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePrefix = "github.com/dkroshow/nano/internal/transcriptparse"

func TestImportBoundary(t *testing.T) {
	output, err := exec.Command("go", "list", "std").Output()
	if err != nil {
		t.Fatal(err)
	}
	standard := map[string]bool{}
	for _, path := range strings.Fields(string(output)) {
		standard[path] = true
	}
	for _, root := range []string{".", "../../cmd/transcript-dump"} {
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "doc.go")); err != nil {
					// Intermediate directories such as internal contain no package.
					files, _ := filepath.Glob(filepath.Join(path, "*.go"))
					if len(files) > 0 {
						t.Errorf("%s: package must have doc.go", path)
					}
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, problem := range checkSource(path, content, standard) {
				t.Errorf("%s: %s", path, problem)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestBoundaryRejectsForbiddenDependenciesAndEffects(t *testing.T) {
	standard := map[string]bool{"encoding/json": true, "time": true, "os": true, "math/rand": true, "database/sql": true}
	for _, tc := range []struct{ name, path, source string }{
		{"nano", "codex/reader.go", `package codex; import "github.com/dkroshow/nano/internal/history"`},
		{"third_party", "codex/reader.go", `package codex; import "example.com/parser"`},
		{"sibling_provider", "codex/reader.go", `package codex; import "github.com/dkroshow/nano/internal/transcriptparse/claude"`},
		{"model_outward", "model/types.go", `package model; import "github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"`},
		{"filesystem", "codex/reader.go", `package codex; import "os"`},
		{"database", "codex/reader.go", `package codex; import "database/sql"`},
		{"random", "codex/reader.go", `package codex; import "math/rand"`},
		{"clock_alias", "codex/reader.go", `package codex; import clock "time"; var x = clock.Now()`},
		{"clock_value", "codex/reader.go", `package codex; import "time"; var clock = time.Now`},
		{"dot_import", "codex/reader.go", `package codex; import . "time"; var x = Now()`},
		{"dump_external", "../../cmd/transcript-dump/main.go", `package main; import "github.com/dkroshow/nano/internal/history"`},
		{"dump_private", "../../cmd/transcript-dump/main.go", `package main; import "github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"`},
		{"test_external", "model/types_test.go", `package model; import "github.com/dkroshow/nano/internal/history"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if problems := checkSource(tc.path, []byte(tc.source), standard); len(problems) == 0 {
				t.Fatal("guard accepted a forbidden dependency or effect")
			}
		})
	}
	for _, tc := range []struct{ path, source string }{
		{"codex/reader.go", `package codex; import "github.com/dkroshow/nano/internal/transcriptparse/model"`},
		{"model/time.go", `package model; import "time"; var parse = time.Parse`},
		{"../../cmd/transcript-dump/main.go", `package main; import "os"; import "github.com/dkroshow/nano/internal/transcriptparse/codex"`},
		{"fixtures_test.go", `package transcriptparse_test; import "os"`},
	} {
		if problems := checkSource(tc.path, []byte(tc.source), standard); len(problems) != 0 {
			t.Errorf("allowed source rejected: %v", problems)
		}
	}
}

func checkSource(path string, content []byte, standard map[string]bool) []string {
	file, err := parser.ParseFile(token.NewFileSet(), path, content, 0)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	testFile := strings.HasSuffix(path, "_test.go")
	commandFile := strings.HasPrefix(filepath.ToSlash(filepath.Clean(path)), "../../cmd/transcript-dump/")
	imports := map[string]string{}
	for _, spec := range file.Imports {
		dependency, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			problems = append(problems, "invalid import string")
			continue
		}
		local := dependency == modulePrefix || strings.HasPrefix(dependency, modulePrefix+"/")
		if !standard[dependency] && !local {
			problems = append(problems, "dependency outside parsing module and standard library: "+dependency)
		}
		name := filepath.Base(dependency)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = dependency
		if testFile {
			continue
		}
		if name == "." {
			problems = append(problems, "dot imports conceal dependency use")
		}
		if local && !allowedLayer(filepath.ToSlash(filepath.Dir(path)), strings.TrimPrefix(dependency, modulePrefix+"/")) {
			problems = append(problems, "dependency points out of its layer: "+dependency)
		}
		if commandFile {
			continue
		}
		for _, forbidden := range []string{"os", "io/fs", "path/filepath", "net", "syscall", "unsafe", "crypto/rand", "math/rand", "plugin", "database/sql"} {
			if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
				problems = append(problems, "effectful dependency in pure parser: "+dependency)
			}
		}
	}
	if !testFile && !commandFile {
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name, ok := selector.X.(*ast.Ident)
			if !ok || imports[name.Name] != "time" {
				return true
			}
			switch selector.Sel.Name {
			case "Now", "Since", "Until", "Sleep", "After", "AfterFunc", "Tick", "NewTicker", "NewTimer", "Local", "LoadLocation":
				problems = append(problems, "clock or environment access: time."+selector.Sel.Name)
			}
			return true
		})
	}
	return problems
}

func allowedLayer(from, to string) bool {
	switch from {
	case "../../cmd/transcript-dump":
		return to == "codex" || to == "claude" || to == "grok" || to == "model"
	case "codex", "claude", "grok":
		return to == "model" || to == "internal/assemble" || to == "internal/jsonl"
	case "internal/assemble":
		return to == "model"
	default:
		return false
	}
}
