package sessions

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	filemodel "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/wire"
)

func inspectOne(path string, pol instructionFilePolicy) (instructionFileMeta, []byte) {
	src := setup.InstructionFileSource{Path: path, SourceKind: "test", ProvenLoaded: true}
	return inspectInstructionFile("codex", src, pol)
}

func TestInstructionFileEditableUnderWriteRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	writeTestFile(t, path, "# hello\n")
	pol := instructionFilePolicy{writeRoots: []string{filemodel.CanonicalPath(dir)}}

	meta, content := inspectOne(path, pol)
	if !meta.Exists || !meta.Editable {
		t.Fatalf("want exists+editable, got %+v", meta)
	}
	if string(content) != "# hello\n" {
		t.Fatalf("content = %q", content)
	}
	if meta.Mtime <= 0 {
		t.Fatalf("want a positive mtime, got %v", meta.Mtime)
	}
}

func TestInstructionFileProtectedRootViewOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	writeTestFile(t, path, "global instructions\n")
	// Protected wins even when the file is also under a write root.
	pol := instructionFilePolicy{
		writeRoots:     []string{filemodel.CanonicalPath(dir)},
		protectedRoots: []string{filemodel.CanonicalPath(dir)},
	}

	meta, content := inspectOne(path, pol)
	if meta.Editable {
		t.Fatalf("protected file must not be editable: %+v", meta)
	}
	if content == nil {
		t.Fatal("protected file must still be viewable")
	}
	if !strings.Contains(meta.ReadOnlyReason, "read-only") {
		t.Fatalf("reason = %q", meta.ReadOnlyReason)
	}
}

func TestInstructionFileOutsideRootsViewOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	writeTestFile(t, path, "elsewhere\n")
	pol := instructionFilePolicy{writeRoots: []string{filemodel.CanonicalPath(t.TempDir())}}

	meta, content := inspectOne(path, pol)
	if meta.Editable {
		t.Fatalf("file outside write roots must not be editable: %+v", meta)
	}
	if content == nil {
		t.Fatal("file outside write roots must still be viewable")
	}
}

func TestInstructionFileSymlinkEscape(t *testing.T) {
	inside := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "real.md")
	writeTestFile(t, target, "outside\n")
	link := filepath.Join(inside, "AGENTS.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	pol := instructionFilePolicy{writeRoots: []string{filemodel.CanonicalPath(inside)}}

	meta, _ := inspectOne(link, pol)
	if meta.Editable {
		t.Fatal("symlink escaping the write root must not be editable")
	}
}

func TestInstructionFileBinaryAndOversized(t *testing.T) {
	dir := t.TempDir()
	pol := instructionFilePolicy{writeRoots: []string{filemodel.CanonicalPath(dir)}}

	binary := filepath.Join(dir, "bin.md")
	if err := os.WriteFile(binary, []byte{'a', 0x00, 'b'}, 0644); err != nil {
		t.Fatal(err)
	}
	meta, content := inspectOne(binary, pol)
	if meta.Editable || content != nil {
		t.Fatalf("binary file must be rejected: %+v", meta)
	}

	oversized := filepath.Join(dir, "big.md")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte("x"), maxInstructionFileBytes+1), 0644); err != nil {
		t.Fatal(err)
	}
	meta, content = inspectOne(oversized, pol)
	if meta.Editable || content != nil {
		t.Fatalf("oversized file must be rejected without reading: %+v", meta)
	}
}

// newTestCodexSession fabricates a session whose thread state lists the given
// instruction sources — the handlers only touch state and the filesystem.
func newTestCodexSession(t *testing.T, cwd string, sources []string) *Session {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"cwd":                   cwd,
		"instructionSources":    sources,
		"runtimeWorkspaceRoots": []string{cwd},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Session{id: "test",
		provider:       "codex",
		cwd:            cwd,
		subs:           map[chan wire.Event]struct{}{},
		state:          map[string]json.RawMessage{"thread": raw},
		lifecycleState: testLifecycleSnapshot("ready", false)}
}

func TestInstructionFileReadRequiresAllowlist(t *testing.T) {
	dir := t.TempDir()
	listed := filepath.Join(dir, "AGENTS.md")
	unlisted := filepath.Join(dir, "secret.md")
	writeTestFile(t, listed, "listed\n")
	writeTestFile(t, unlisted, "unlisted\n")
	sess := newTestCodexSession(t, dir, []string{listed})

	for path, want := range map[string]int{listed: http.StatusOK, unlisted: http.StatusNotFound} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/read?path="+path, nil)
		sess.HandleInstructionFileRead(rec, req)
		if rec.Code != want {
			t.Fatalf("read %s: status %d, want %d (%s)", path, rec.Code, want, rec.Body.String())
		}
	}
}

func TestInstructionFileMtimeRequiresAllowlist(t *testing.T) {
	dir := t.TempDir()
	listed := filepath.Join(dir, "AGENTS.md")
	unlisted := filepath.Join(dir, "secret.md")
	writeTestFile(t, listed, "listed\n")
	writeTestFile(t, unlisted, "unlisted\n")
	sess := newTestCodexSession(t, dir, []string{listed})

	for path, want := range map[string]int{listed: http.StatusOK, unlisted: http.StatusNotFound} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/mtime?path="+path, nil)
		sess.HandleInstructionFileMtime(rec, req)
		if rec.Code != want {
			t.Fatalf("mtime %s: status %d, want %d (%s)", path, rec.Code, want, rec.Body.String())
		}
	}
}

func TestInstructionFileWriteHappyPathConflictAndForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	writeTestFile(t, path, "v1\n")
	sess := newTestCodexSession(t, dir, []string{path})

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	goodMtime := filemodel.ModificationTime(fi)

	putBody := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/write", bytes.NewReader(raw))
		sess.HandleInstructionFileWrite(rec, req)
		return rec
	}

	// Stale mtime → 409, no write.
	if rec := putBody(map[string]any{"path": path, "content": "v2\n", "mtime": goodMtime - 5}); rec.Code != http.StatusConflict {
		t.Fatalf("stale mtime: status %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if got, _ := os.ReadFile(path); string(got) != "v1\n" {
		t.Fatalf("conflict must not write; file = %q", got)
	}

	// Correct mtime → write succeeds.
	if rec := putBody(map[string]any{"path": path, "content": "v2\n", "mtime": goodMtime}); rec.Code != http.StatusOK {
		t.Fatalf("write: status %d (%s)", rec.Code, rec.Body.String())
	}
	if got, _ := os.ReadFile(path); string(got) != "v2\n" {
		t.Fatalf("file after write = %q", got)
	}

	// Omitted mtime → force overwrite regardless of on-disk state.
	if rec := putBody(map[string]any{"path": path, "content": "v3\n"}); rec.Code != http.StatusOK {
		t.Fatalf("forced write: status %d (%s)", rec.Code, rec.Body.String())
	}
	if got, _ := os.ReadFile(path); string(got) != "v3\n" {
		t.Fatalf("file after forced write = %q", got)
	}

	// Both successful writes must be visible in the session feed.
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.events) != 2 || !bytes.Contains(sess.events[0].Msg, []byte("nano/instructionFile/written")) {
		t.Fatalf("expected two nano/instructionFile/written events, got %v", sess.events)
	}
}

func TestInstructionFileWriteDeniedOutsideRoots(t *testing.T) {
	home := t.TempDir()
	protected := filepath.Join(home, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(protected), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, protected, "global\n")
	pol := instructionFilePolicy{
		writeRoots:     []string{filemodel.CanonicalPath(t.TempDir())},
		protectedRoots: []string{filemodel.CanonicalPath(filepath.Join(home, ".codex"))},
	}
	meta, _ := inspectOne(protected, pol)
	if meta.Editable {
		t.Fatal("~/.codex file must never be editable")
	}
}

func TestClaudeInstructionFilesUseDocumentedLoadingRules(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(t.TempDir(), "repo")
	cwd := filepath.Join(root, "pkg")
	if err := os.MkdirAll(filepath.Join(cwd, ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(home, ".claude", "CLAUDE.md")
	userRule := filepath.Join(home, ".claude", "rules", "preferences.md")
	projectRule := filepath.Join(cwd, ".claude", "rules", "testing.md")
	for _, path := range []string{
		filepath.Join(root, "CLAUDE.md"),
		filepath.Join(cwd, "CLAUDE.md"),
		filepath.Join(cwd, ".claude", "CLAUDE.md"),
		filepath.Join(cwd, "CLAUDE.local.md"),
		userFile,
		userRule,
		projectRule,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, path, "instructions\n")
	}

	sess := &Session{provider: "claude",
		cwd:            cwd,
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sources, _ := sess.instructionFileSources()
	got := map[string]setup.InstructionFileSource{}
	for _, src := range sources {
		got[src.Path] = src
	}
	want := map[string]string{
		filepath.Join(root, "CLAUDE.md"):           "claude-md",
		filepath.Join(cwd, "CLAUDE.md"):            "claude-md",
		filepath.Join(cwd, ".claude", "CLAUDE.md"): "claude-md",
		filepath.Join(cwd, "CLAUDE.local.md"):      "claude-md",
		userFile:                                   "claude-md",
		userRule:                                   "claude-rule",
		projectRule:                                "claude-rule",
	}
	for path, kind := range want {
		src, ok := got[path]
		if !ok {
			t.Fatalf("missing claude instruction source %s; got %#v", path, sources)
		}
		if src.SourceKind != kind || src.Evidence != setup.InstructionEvidenceDocumented || src.ProvenLoaded {
			t.Fatalf("source %s = %+v, want documented-expected and not proven-loaded", path, src)
		}
	}
	wantScopes := map[string]string{
		userFile:                              "user",
		userRule:                              "user",
		filepath.Join(root, "CLAUDE.md"):      "project",
		filepath.Join(cwd, "CLAUDE.md"):       "project",
		filepath.Join(cwd, "CLAUDE.local.md"): "local",
		projectRule:                           "project",
	}
	for path, scope := range wantScopes {
		if got[path].SourceScope != scope {
			t.Fatalf("source %s scope = %q, want %q", path, got[path].SourceScope, scope)
		}
	}
	wantOrder := []string{
		userFile,
		userRule,
		filepath.Join(root, "CLAUDE.md"),
		filepath.Join(cwd, "CLAUDE.md"),
		filepath.Join(cwd, ".claude", "CLAUDE.md"),
		filepath.Join(cwd, "CLAUDE.local.md"),
		projectRule,
	}
	var gotOrder []string
	for _, src := range sources {
		if _, ok := want[src.Path]; ok {
			gotOrder = append(gotOrder, src.Path)
		}
	}
	if strings.Join(gotOrder, "\n") != strings.Join(wantOrder, "\n") {
		t.Fatalf("claude source order:\ngot:\n%s\nwant:\n%s", strings.Join(gotOrder, "\n"), strings.Join(wantOrder, "\n"))
	}
}
