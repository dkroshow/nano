package transcript

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPackageDependencyDirection(t *testing.T) {
	for _, root := range []string{".", "../core/turns"} {
		files, err := filepath.Glob(filepath.Join(root, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, "doc.go")); err != nil {
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
				if !allowedImport(root, dependency) {
					t.Errorf("%s imports forbidden dependency %s", path, dependency)
				}
			}
		}
	}
}

func TestBoundaryRejectsCyclesAndTranscriptEffects(t *testing.T) {
	for _, test := range []struct{ root, dependency string }{
		{".", "github.com/dkroshow/nano/internal/app"}, {".", "github.com/dkroshow/nano/internal/threads"},
		{".", "github.com/dkroshow/nano/internal/providers/codex"}, {".", "github.com/dkroshow/nano/internal/history"},
		{".", "database/sql"}, {".", "net/http"}, {".", "os"}, {".", "time"}, {".", "crypto/rand"},
		{"../core/turns", "github.com/dkroshow/nano/internal/transcript"}, {"../core/turns", "github.com/dkroshow/nano/internal/app"},
	} {
		if allowedImport(test.root, test.dependency) {
			t.Errorf("guard allowed %s to import %s", test.root, test.dependency)
		}
	}
}

func allowedImport(root, path string) bool {
	if path == "github.com/dkroshow/nano/internal/transcriptparse/model" {
		return true
	}
	if root == "." {
		switch path {
		case "github.com/dkroshow/nano/internal/core/prompts", "encoding/json", "crypto/sha256", "encoding/hex", "fmt", "errors", "sort", "strconv", "strings", "bytes", "container/heap":
			return true
		}
		return false
	}
	switch path {
	case "github.com/dkroshow/nano/internal/threads", "github.com/dkroshow/nano/internal/lifecycle", "github.com/dkroshow/nano/internal/state/db", "context", "database/sql", "errors", "fmt", "sort", "sync", "time", "crypto/sha256", "encoding/hex", "encoding/json", "strings", "strconv", "bytes", "io", "os", "path/filepath":
		return true
	}
	return false
}
