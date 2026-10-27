package threads

import (
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestStorageDependencyBoundary(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".go") || strings.HasSuffix(file.Name(), "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), file.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range source.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == "modernc.org/sqlite" || path == "github.com/dkroshow/nano/internal/state/db" {
				continue
			}
			dependency, err := build.Default.Import(path, "", build.FindOnly)
			if err != nil || !dependency.Goroot {
				t.Errorf("%s imports forbidden dependency %s", file.Name(), path)
			}
		}
	}
}

func TestThreadsOwnsOnlyThreadStateTables(t *testing.T) {
	forbidden := []string{
		"queued_turns", "send_attempts", "completed_turns",
		"agent_messages", "agent_message_events", "agent_message_refs", "agent_message_seq",
		"hosted_native_recovery_grants", "hosted_native_recoveries", "hosted_native_recovery_events",
		"hosted_permission_events",
		"browser_duplicate_incidents",
		"subscription_completed_cursors",
	}
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".go") || strings.HasSuffix(file.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range forbidden {
			if strings.Contains(string(body), table) {
				t.Errorf("%s still owns lodger table %s", file.Name(), table)
			}
		}
	}
}
