package providers_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/providers"
)

func TestAdapterHasFiveTurnVerbsAndLifecycleEvidence(t *testing.T) {
	contract := reflect.TypeOf((*providers.Adapter)(nil)).Elem()
	want := map[string]bool{"Capabilities": true, "Current": true, "TurnCapabilities": true, "Start": true, "Resume": true, "Submit": true, "Interrupt": true, "RespondInteraction": true}
	for i := 0; i < contract.NumMethod(); i++ {
		name := contract.Method(i).Name
		if !want[name] {
			t.Errorf("unexpected turn-adapter method %s", name)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("missing contract methods: %v", want)
	}
}

func TestProviderDependencyDirection(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			dependency, _ := strconv.Unquote(spec.Path.Value)
			allowed := !strings.HasPrefix(dependency, "github.com/dkroshow/nano/internal/") || dependency == "github.com/dkroshow/nano/internal/lifecycle" || dependency == "github.com/dkroshow/nano/internal/core/prompts" || dependency == "github.com/dkroshow/nano/internal/transcriptparse/model"
			if filepath.Dir(path) != "." && dependency == "github.com/dkroshow/nano/internal/providers" {
				allowed = true
			}
			if !allowed {
				t.Errorf("%s imports %s outside the provider boundary", path, dependency)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSharedTurnVerbsDoNotDispatchOnProviderNames(t *testing.T) {
	targets := map[string]map[string]bool{
		"../app/interactions.go": {"respondInteraction": true, "closeInteractions": true, "handleInteraction": true},
		"../app/session.go":      {"handleInterrupt": true},
		"../app/turn_send.go":    {"runTransactionalTurnSend": true},
	}
	for path, functions := range targets {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !functions[function.Name.Name] {
				continue
			}
			seen[function.Name.Name] = true
			ast.Inspect(function.Body, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, _ := strconv.Unquote(literal.Value)
				if value == "codex" || value == "claude" || value == "grok" {
					t.Errorf("%s dispatches on provider %q at %s", function.Name.Name, value, fset.Position(literal.Pos()))
				}
				return true
			})
		}
		for name := range functions {
			if !seen[name] {
				t.Errorf("%s no longer contains %s; update the adapter boundary test", path, name)
			}
		}
	}
}
