package historical

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"

	"github.com/dkroshow/nano/internal/threadindex"
)

type fakePassiveCodex struct {
	mu      sync.Mutex
	methods []string
	block   <-chan struct{}
}

func (fake *fakePassiveCodex) Call(_ context.Context, method string, _ any, result any) error {
	if fake.block != nil {
		<-fake.block
	}
	fake.mu.Lock()
	fake.methods = append(fake.methods, method)
	fake.mu.Unlock()
	var payload any
	switch method {
	case "config/read":
		payload = map[string]any{"config": map[string]any{
			"model": "configured-model", "model_provider": "openai",
			"model_reasoning_effort": "medium", "approval_policy": "never",
			"sandbox_mode": "workspace-write", "default_permissions": ":workspace",
			"mcp_servers": map[string]any{
				"docs": map[string]any{"bearer_token": "must-not-be-cached"},
			},
			"project_doc_max_bytes": 32768,
		}}
	case "skills/list":
		payload = map[string]any{"data": []any{map[string]any{
			"skills": []any{map[string]any{
				"name": "example", "description": "Example skill", "path": "/skills/example/SKILL.md",
				"scope": "user", "enabled": true,
			}},
			"errors": []any{},
		}}}
	case "permissionProfile/list":
		payload = map[string]any{"data": []any{
			map[string]any{"id": ":workspace", "allowed": true},
			map[string]any{"id": "blocked", "allowed": false},
		}}
	case "model/list":
		payload = map[string]any{"data": []any{map[string]any{
			"id": "configured-model", "model": "configured-model", "isDefault": true,
			"defaultReasoningEffort": "medium", "serviceTiers": []any{},
		}}}
	default:
		tPayload, _ := json.Marshal(map[string]any{})
		return json.Unmarshal(tPayload, result)
	}
	encoded, _ := json.Marshal(payload)
	return json.Unmarshal(encoded, result)
}

func TestHistoricalResolverPreparedReadDoesNotWaitForColdResolution(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	cwd := filepath.Join(root, "repo")
	for _, directory := range []string{codexHome, filepath.Join(cwd, ".git")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store, err := history.Open(filepath.Join(root, "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	block := make(chan struct{})
	fake := &fakePassiveCodex{block: block}
	resolver := NewCodexResolver(fake, store, codexHome)
	thread := testCodexCatalogThread(cwd, "recorded-model")

	started := time.Now()
	first := resolver.ResolvePrepared(context.Background(), thread)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("prepared read blocked for %s", elapsed)
	}
	if first.State != "resolving" {
		t.Fatalf("cold prepared state = %s", first.State)
	}

	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for {
		resolution := resolver.ResolvePrepared(context.Background(), thread)
		if resolution.State == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background resolution did not become ready: %#v", resolution)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHistoricalResolverSharesWorkspaceWorkAndOverlaysThreadSettings(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	repo := filepath.Join(root, "repo")
	child := filepath.Join(repo, "nested")
	for _, directory := range []string{codexHome, filepath.Join(repo, ".git"), child} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(codexHome, "AGENTS.md"), []byte("global"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "AGENTS.override.md"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := history.Open(filepath.Join(root, "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fake := &fakePassiveCodex{}
	resolver := NewCodexResolver(fake, store, codexHome)
	resolver.now = func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) }

	first := testCodexCatalogThread(child, "thread-model-one")
	second := testCodexCatalogThread(child, "thread-model-two")
	firstResolution := resolver.Resolve(context.Background(), first)
	secondResolution := resolver.Resolve(context.Background(), second)

	if firstResolution.State != "ready" || secondResolution.State != "ready" {
		t.Fatalf("resolution states: %s, %s", firstResolution.State, secondResolution.State)
	}
	if got := firstResolution.Values["model"].Value; got != "thread-model-one" {
		t.Fatalf("first model = %v", got)
	}
	if got := secondResolution.Values["model"].Value; got != "thread-model-two" {
		t.Fatalf("second model = %v", got)
	}
	instructions := firstResolution.Values["instructionSources"].Value.([]resolvedInstructionSource)
	if len(instructions) != 3 {
		t.Fatalf("instruction sources = %#v", instructions)
	}
	if got := firstResolution.Values["mcpServers"].Value; len(got.([]string)) != 1 || got.([]string)[0] != "docs" {
		t.Fatalf("MCP identities = %#v", got)
	}
	if roots := firstResolution.Values["workspaceRoots"]; roots.State != "unknown" {
		t.Fatalf("workspace roots = %#v, want unknown without an exact app-server resolver", roots)
	}

	fake.mu.Lock()
	methods := append([]string(nil), fake.methods...)
	fake.mu.Unlock()
	if strings.Join(methods, ",") != "config/read,skills/list,permissionProfile/list,model/list" {
		t.Fatalf("passive calls = %v", methods)
	}
	for _, method := range methods {
		if strings.HasPrefix(method, "thread/") || strings.HasPrefix(method, "turn/") || strings.HasPrefix(method, "mcpServer") {
			t.Fatalf("resolver made active call %q", method)
		}
	}

	row, ok, err := store.LoadCodexWorkspaceResolution(context.Background(), child, historicalResolverVersion)
	if err != nil || !ok {
		t.Fatalf("load cached resolution: ok=%v err=%v", ok, err)
	}
	if strings.Contains(string(row.Payload), "must-not-be-cached") {
		t.Fatal("cached payload contains an MCP credential")
	}
}

func TestHistoricalResolverLeavesProviderFilesUntouched(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	cwd := filepath.Join(root, "repo")
	for _, directory := range []string{codexHome, filepath.Join(cwd, ".git")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{filepath.Join(codexHome, "AGENTS.md"), filepath.Join(cwd, "AGENTS.md")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := history.Open(filepath.Join(root, "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolver := NewCodexResolver(&fakePassiveCodex{}, store, codexHome)
	if got := resolver.Resolve(context.Background(), testCodexCatalogThread(cwd, "recorded-model")); got.State != "ready" {
		t.Fatalf("resolution = %#v", got)
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "original\n" {
			t.Fatalf("resolver modified provider file %s: %q", path, body)
		}
	}
}

func TestHistoricalResolverMarksPreparedSnapshotStaleWhenInputsChange(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	cwd := filepath.Join(root, "repo")
	for _, directory := range []string{codexHome, filepath.Join(cwd, ".git")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	instruction := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(instruction, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(root, "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolver := NewCodexResolver(&fakePassiveCodex{}, store, codexHome)
	resolver.ttl = time.Hour
	thread := testCodexCatalogThread(cwd, "recorded-model")
	if got := resolver.Resolve(context.Background(), thread); got.State != "ready" {
		t.Fatalf("initial resolution = %#v", got)
	}
	// The fingerprint includes resolved instruction paths, so this is stale
	// immediately rather than waiting for the one-hour test TTL.
	time.Sleep(2 * time.Millisecond)
	if err := os.WriteFile(instruction, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared := resolver.ResolvePrepared(context.Background(), thread)
	if prepared.State != "stale" || !prepared.Stale {
		t.Fatalf("prepared state = %#v, want stale after instruction change", prepared)
	}
}

func TestResolveCodexInstructionsHonorsPrecedenceAndLimit(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "home")
	repo := filepath.Join(root, "repo")
	child := filepath.Join(repo, "child")
	for _, directory := range []string{codexHome, filepath.Join(repo, ".git"), child} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(codexHome, "AGENTS.override.md"): "global override",
		filepath.Join(codexHome, "AGENTS.md"):          "shadowed global",
		filepath.Join(repo, "AGENTS.md"):               "12345",
		filepath.Join(child, "NOTES.md"):               "67890",
	}
	for path, contents := range files {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	sources, diagnostics := resolveCodexInstructions(codexHome, child, []string{".git"}, []string{"NOTES.md"}, 8)
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	if len(sources) != 3 {
		t.Fatalf("sources = %#v", sources)
	}
	if filepath.Base(sources[0].Path) != "AGENTS.override.md" || filepath.Dir(sources[0].Path) != codexHome {
		t.Fatalf("global source = %#v", sources[0])
	}
	if sources[2].BytesRead != 3 || !sources[2].Truncated {
		t.Fatalf("limited child source = %#v", sources[2])
	}
}

func TestReadCodexResolutionTargetUsesSessionMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	body := strings.Join([]string{
		`{"type":"session_meta","payload":{"cwd":"/recorded/session","model_provider":"openai"}}`,
		`{"type":"turn_context","payload":{"cwd":"/later/turn","approvals_reviewer":"user"}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"approvals_reviewer":"auto_review"}}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	later := "/catalog/latest-turn"
	sources := []threadindex.SourceDetail{{Path: path}}
	thread := threadindex.Thread{Metadata: threadindex.Metadata{CWD: threadindex.Value{State: "known", Value: &later}}, Sources: &sources}
	target := readCodexResolutionTarget(thread)
	if target.CWD != "/recorded/session" || target.ModelProvider != "openai" || target.ApprovalsReviewer != "auto_review" {
		t.Fatalf("target = %#v", target)
	}
}

func TestNonProjectRootMarkersIgnoreProjectLayer(t *testing.T) {
	project := codexConfigLayer{Config: map[string]json.RawMessage{
		"project_root_markers": json.RawMessage(`["PROJECT"]`),
	}}
	project.Name.Type = "project"
	user := codexConfigLayer{Config: map[string]json.RawMessage{
		"project_root_markers": json.RawMessage(`["USER"]`),
	}}
	user.Name.Type = "user"
	system := codexConfigLayer{Config: map[string]json.RawMessage{
		"project_root_markers": json.RawMessage(`["SYSTEM"]`),
	}}
	system.Name.Type = "system"

	markers, ok := nonProjectRootMarkers([]codexConfigLayer{project, user, system})
	if !ok || len(markers) != 1 || markers[0] != "USER" {
		t.Fatalf("markers = %v, %v", markers, ok)
	}
}

func testCodexCatalogThread(cwd, model string) threadindex.Thread {
	evidence := "session_meta"
	return threadindex.Thread{Provider: "codex", Metadata: threadindex.Metadata{
		CWD:    threadindex.Value{State: "known", Value: &cwd, Evidence: &evidence},
		Model:  threadindex.Value{State: "known", Value: &model, Evidence: &evidence},
		Effort: threadindex.Value{State: "unknown"},
	}}
}
