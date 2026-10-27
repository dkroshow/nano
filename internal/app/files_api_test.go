package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	filemodel "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/protocol"
)

func TestGeneralFileReadIsGlobalAndProtectedProviderFilesAreViewOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	global := filepath.Join(t.TempDir(), "artifact.md")
	protected := filepath.Join(home, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(protected), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, global, "global\n")
	writeTestFile(t, protected, "protected\n")
	server := &server{}

	read := func(path string) filemodel.Meta {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/files/read?path="+url.QueryEscape(path), nil)
		server.handleGeneralFileRead(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("read %s: %d %s", path, recorder.Code, recorder.Body.String())
		}
		var result filemodel.Meta
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := read(global); !result.Editable || result.Content != "global\n" {
		t.Fatalf("global file = %+v", result)
	}
	if result := read(protected); result.Editable || !strings.Contains(result.ReadOnlyReason, "read-only") {
		t.Fatalf("protected file = %+v", result)
	}
}

func TestGeneralFileReadRejectsNonTextLargeAndNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "binary.txt")
	invalidUTF8 := filepath.Join(root, "invalid.txt")
	large := filepath.Join(root, "large.txt")
	writeTestFile(t, binary, "before\x00after")
	if err := os.WriteFile(invalidUTF8, []byte{0xff, 0xfe}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(large, bytes.Repeat([]byte("x"), generalFileMaxBytes+1), 0644); err != nil {
		t.Fatal(err)
	}
	server := &server{}
	for _, test := range []struct {
		path string
		code string
	}{
		{binary, "not_text"},
		{invalidUTF8, "not_text"},
		{large, "too_large"},
		{root, "not_regular"},
	} {
		recorder := httptest.NewRecorder()
		server.handleGeneralFileRead(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/files/read?path="+url.QueryEscape(test.path), nil))
		if !strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) {
			t.Fatalf("read %s = %d %s, want %s", test.path, recorder.Code, recorder.Body.String(), test.code)
		}
		if recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("read %s cache control = %q", test.path, recorder.Header().Get("Cache-Control"))
		}
	}
}

func TestGeneralFileReadCanonicalizesSymlinkButMutationRequiresReturnedPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.md")
	alias := filepath.Join(root, "alias.md")
	writeTestFile(t, target, "target\n")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	server := &server{}
	readRecorder := httptest.NewRecorder()
	server.handleGeneralFileRead(readRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/files/read?path="+url.QueryEscape(alias), nil))
	if readRecorder.Code != http.StatusOK {
		t.Fatalf("read alias = %d %s", readRecorder.Code, readRecorder.Body.String())
	}
	var meta filemodel.Meta
	if err := json.Unmarshal(readRecorder.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Path != canonicalPath(target) {
		t.Fatalf("canonical path = %q, want %q", meta.Path, canonicalPath(target))
	}
	mtimeRecorder := httptest.NewRecorder()
	server.handleGeneralFileMtime(mtimeRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/files/mtime?path="+url.QueryEscape(alias), nil))
	if mtimeRecorder.Code != http.StatusBadRequest || !strings.Contains(mtimeRecorder.Body.String(), "invalid_path") {
		t.Fatalf("alias mtime = %d %s", mtimeRecorder.Code, mtimeRecorder.Body.String())
	}
}

func TestGeneralFileWriteConflictForcePermissionsAndNoCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.md")
	writeTestFile(t, path, "v1\n")
	path = canonicalPath(path)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	mtime := statMtime(info)
	server := &server{}
	put := func(path, content string, mtime *float64) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"path": path, "content": content, "mtime": mtime})
		recorder := httptest.NewRecorder()
		server.handleGeneralFileWrite(recorder, httptest.NewRequest(http.MethodPut, "/api/v1/files/write", bytes.NewReader(body)))
		return recorder
	}

	stale := mtime - 5
	if recorder := put(path, "v2\n", &stale); recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "mtime_conflict") {
		t.Fatalf("stale write = %d %s", recorder.Code, recorder.Body.String())
	}
	if got, _ := os.ReadFile(path); string(got) != "v1\n" {
		t.Fatalf("conflict wrote %q", got)
	}
	if recorder := put(path, "v2\n", &mtime); recorder.Code != http.StatusOK {
		t.Fatalf("matching write = %d %s", recorder.Code, recorder.Body.String())
	}
	if mode := func() os.FileMode { info, _ := os.Stat(path); return info.Mode().Perm() }(); mode != 0640 {
		t.Fatalf("mode = %o, want 640", mode)
	}
	if recorder := put(path, "v3\n", nil); recorder.Code != http.StatusOK {
		t.Fatalf("force write = %d %s", recorder.Code, recorder.Body.String())
	}
	missing := filepath.Join(filepath.Dir(path), "missing.md")
	if recorder := put(missing, "new\n", nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("creation = %d %s", recorder.Code, recorder.Body.String())
	}
}

// liveEvidenceSession is a Codex session whose live text shows each text as a
// finished reply, as the wire delivered it.
func liveEvidenceSession(texts ...string) *session {
	sess := &session{id: "session-1", provider: "codex", projection: newLiveProjection("codex")}
	sess.liveText = newLiveTextLoop(&server{}, sess)
	sess.projection.liveText = sess.liveText
	for _, text := range texts {
		addLiveEvidence(sess, text)
	}
	return sess
}

// addLiveEvidence streams one reply: live text shows a reply that started
// streaming, not one that arrived only finished.
func addLiveEvidence(sess *session, text string) {
	id := "reply-" + sendTextHash(text)
	for _, method := range []string{"item/started", "item/completed"} {
		frame, _ := json.Marshal(map[string]any{"method": method, "params": map[string]any{"turnId": "turn-1",
			"item": map[string]any{"id": id, "type": "agentMessage", "text": text}}})
		sess.projection.applyWire(protocol.Recv, frame, time.Now())
	}
}

func TestGeneralFileResolutionUsesOnlyCurrentThreadLiveEvidence(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	relative := filepath.Join("reports", "pulse.md")
	first := filepath.Join(firstRoot, relative)
	second := filepath.Join(secondRoot, relative)
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, path, path+"\n")
	}
	sess := liveEvidenceSession("See " + first)
	server := &server{sessions: map[string]*session{"session-1": sess}}

	path, by, err := server.resolveGeneralFile(context.Background(), filepath.ToSlash(relative), "", "", "session-1")
	if err != nil || path != canonicalPath(first) || by != "thread-evidence" {
		t.Fatalf("live evidence = %q, %q, %v", path, by, err)
	}

	addLiveEvidence(sess, "Also "+second)
	_, _, err = server.resolveGeneralFile(context.Background(), filepath.ToSlash(relative), "", "", "session-1")
	if err == nil || err.Code != "ambiguous_path" || len(err.Candidates) != 2 {
		t.Fatalf("ambiguous evidence = %#v", err)
	}
}

func TestGeneralFileEvidenceUsesPathBoundariesAndDedupesSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real", "reports", "pulse.md")
	alias := filepath.Join(root, "alias", "reports", "pulse.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(alias), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, target, "pulse\n")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	sess := liveEvidenceSession("real " + target + " alias " + alias + " false " + filepath.Join(root, "notreports", "pulse.md"))
	server := &server{sessions: map[string]*session{"session-1": sess}}

	path, _, err := server.resolveGeneralFile(context.Background(), "reports/pulse.md", "", "", "session-1")
	if err != nil || path != canonicalPath(target) {
		t.Fatalf("deduped evidence = %q, %v; want %q", path, err, canonicalPath(target))
	}
}

// fileEvidenceTestCatalog answers the saved-history file-evidence search from
// fixed blobs per catalog thread and records which threads were searched.
type fileEvidenceTestCatalog struct {
	fakeThreadCatalog
	blobs    map[string][]string
	searched []string
}

func (c *fileEvidenceTestCatalog) FindFileEvidenceBlobs(_ context.Context, threadID, _ string, _, _ int) ([]string, error) {
	c.searched = append(c.searched, threadID)
	return c.blobs[threadID], nil
}

func (c *fileEvidenceTestCatalog) ResolveThreadReference(_ context.Context, reference string) (string, error) {
	if reference == "native-1" {
		return "thread-1", nil
	}
	return "", errors.New("unknown thread")
}

// The live rows hold only recent turns. A file named only in an older saved
// row must still resolve through the saved-history search. Boundary:
// resolveGeneralFile with live rows that never mention the file.
func TestGeneralFileResolvesFromSavedHistoryOlderThanTheLiveRows(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "reports", "older.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, target, "older\n")
	catalog := &fileEvidenceTestCatalog{blobs: map[string][]string{"thread-1": {"Earlier turn: " + target}}}
	server := &server{catalog: catalog, sessions: map[string]*session{
		"session-1": {id: "session-1", projection: newLiveProjection("codex")},
	}}

	path, by, err := server.resolveGeneralFile(context.Background(), "reports/older.md", "", "thread-1", "session-1")
	if err != nil || path != canonicalPath(target) || by != "thread-evidence" {
		t.Fatalf("saved evidence = %q, %q, %v; want %q by thread-evidence", path, by, err, canonicalPath(target))
	}
}

// A request can name only its session. The saved-history search must still
// run, on the session's own catalog thread, or older rows stop resolving.
// Boundary: resolveGeneralFile with no thread id and a session whose native
// thread the catalog knows.
func TestGeneralFileSearchesTheSessionsSavedHistoryWithoutAThreadID(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "reports", "older.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, target, "older\n")
	catalog := &fileEvidenceTestCatalog{blobs: map[string][]string{"thread-1": {"Earlier turn: " + target}}}
	server := &server{catalog: catalog, sessions: map[string]*session{
		"session-1": {id: "session-1", threadID: "native-1", projection: newLiveProjection("codex")},
	}}

	path, _, err := server.resolveGeneralFile(context.Background(), "reports/older.md", "", "", "session-1")
	if err != nil || path != canonicalPath(target) || !reflect.DeepEqual(catalog.searched, []string{"thread-1"}) {
		t.Fatalf("session-only request = %q, %v; searched %v, want thread-1", path, err, catalog.searched)
	}
}

// repoFixture builds a git-marked checkout with a nested document directory,
// the shape that broke: a skill file deep in the tree linking a path that is
// relative to the repository root.
func repoFixture(t *testing.T) (repo, docDir string) {
	t.Helper()
	repo = t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	docDir = filepath.Join(repo, ".agents", "skills", "orchestrate")
	if err := os.MkdirAll(docDir, 0755); err != nil {
		t.Fatal(err)
	}
	return repo, docDir
}

func TestGeneralFileResolvesRepoRelativeLinkFromNestedDocument(t *testing.T) {
	repo, docDir := repoFixture(t)
	target := filepath.Join(repo, "analysis", "model-routing.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, target, "routing\n")
	// A build-output copy must not make the link ambiguous.
	vendored := filepath.Join(repo, "node_modules", "pkg", "analysis", "model-routing.md")
	if err := os.MkdirAll(filepath.Dir(vendored), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, vendored, "vendored\n")

	server := &server{}
	path, by, err := server.resolveGeneralFile(context.Background(), "analysis/model-routing.md", docDir, "", "")
	if err != nil || path != canonicalPath(target) || by != "suffix" {
		t.Fatalf("suffix resolve = %q, %q, %v; want %q by suffix", path, by, err, canonicalPath(target))
	}
}

func TestGeneralFileResolvesBareFilenameFromAncestorDirectory(t *testing.T) {
	repo, docDir := repoFixture(t)
	target := filepath.Join(repo, "AGENTS.md")
	writeTestFile(t, target, "adapter\n")

	server := &server{}
	path, by, err := server.resolveGeneralFile(context.Background(), "AGENTS.md", docDir, "", "")
	if err != nil || path != canonicalPath(target) || by != "ancestor" {
		t.Fatalf("ancestor resolve = %q, %q, %v; want %q by ancestor", path, by, err, canonicalPath(target))
	}
}

func TestGeneralFileSuffixAmbiguityListsCandidatesShortestFirst(t *testing.T) {
	repo, docDir := repoFixture(t)
	near := filepath.Join(repo, "docs", "notes.md")
	far := filepath.Join(repo, "packages", "worker", "docs", "notes.md")
	for _, path := range []string{near, far} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, path, path+"\n")
	}

	server := &server{}
	_, _, err := server.resolveGeneralFile(context.Background(), "docs/notes.md", docDir, "", "")
	if err == nil || err.Code != "ambiguous_path" || len(err.Candidates) != 2 {
		t.Fatalf("ambiguous suffix = %#v", err)
	}
	if err.Candidates[0] != canonicalPath(near) || err.Candidates[1] != canonicalPath(far) {
		t.Fatalf("candidate order = %v; want %q first", err.Candidates, canonicalPath(near))
	}
}

func TestGeneralFileSearchStaysInsideTheDocumentRepository(t *testing.T) {
	_, docDir := repoFixture(t)
	otherRepo := t.TempDir()
	stranger := filepath.Join(otherRepo, "analysis", "model-routing.md")
	if err := os.MkdirAll(filepath.Dir(stranger), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, stranger, "other checkout\n")

	server := &server{}
	if _, _, err := server.resolveGeneralFile(context.Background(), "analysis/model-routing.md", docDir, "", ""); err == nil || err.Code != "not_found" {
		t.Fatalf("cross-repo resolve = %#v, want not_found", err)
	}
}

func TestGeneralFileFallsBackToTheThreadWorkingDirectory(t *testing.T) {
	repo, docDir := repoFixture(t)
	// The document lives in a different checkout from the running thread; the
	// thread's cwd still resolves a link the document's own repo cannot.
	threadRepo := t.TempDir()
	target := filepath.Join(threadRepo, "analysis", "routing.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, target, "routing\n")
	_ = repo

	server := &server{sessions: map[string]*session{"session-1": {id: "session-1", cwd: threadRepo}}}
	path, by, err := server.resolveGeneralFile(context.Background(), "analysis/routing.md", docDir, "", "session-1")
	if err != nil || path != canonicalPath(target) || by != "thread-cwd" {
		t.Fatalf("thread-cwd resolve = %q, %q, %v; want %q by thread-cwd", path, by, err, canonicalPath(target))
	}
}

func TestGeneralFileEvidenceOutranksAnAmbiguousFilesystemGuess(t *testing.T) {
	repo, docDir := repoFixture(t)
	near := filepath.Join(repo, "docs", "notes.md")
	far := filepath.Join(repo, "packages", "worker", "docs", "notes.md")
	for _, path := range []string{near, far} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, path, path+"\n")
	}
	server := &server{sessions: map[string]*session{"session-1": liveEvidenceSession("wrote " + far)}}

	path, by, err := server.resolveGeneralFile(context.Background(), "docs/notes.md", docDir, "", "session-1")
	if err != nil || path != canonicalPath(far) || by != "thread-evidence" {
		t.Fatalf("evidence over ambiguity = %q, %q, %v; want %q by thread-evidence", path, by, err, canonicalPath(far))
	}
}
