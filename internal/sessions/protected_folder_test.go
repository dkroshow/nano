package sessions

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	filemodel "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/wire"
)

// Catches: Nano reading inside a folder macOS guards with a privacy prompt
// because a thread works there. Each read (git for the branch, CLAUDE.md
// discovery, an instruction file's stat) made macOS ask whether
// "codex-nano-bin" may access the Documents folder, again after every build.
// Boundary: the session state and instruction-file list a pane reads on open.
func TestProtectedFolderSessionReadsNothingThere(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS privacy folders exist only on darwin")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "Documents", "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	// A real repository with instructions, so any read would find something.
	if out, err := exec.Command("git", "init", "-q", "-b", "main", cwd).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	agents := filepath.Join(cwd, "AGENTS.md")
	writeTestFile(t, agents, "project instructions\n")
	writeTestFile(t, filepath.Join(cwd, "CLAUDE.md"), "project instructions\n")

	claude := sessionWithOwners(&Session{
		state: State{
			cwd:            cwd,
			state:          map[string]json.RawMessage{},
			lifecycleState: testLifecycleSnapshot("ready", false)},
		wire:     Wire{subs: map[chan wire.Event]struct{}{}},
		identity: attachmentIdentity{id: "claude-test", provider: "claude"},
	})
	for _, sess := range []*Session{newTestCodexSession(t, cwd, []string{agents}), claude} {
		provider := sess.identity.provider
		rec := httptest.NewRecorder()
		sess.state.HandleState(rec, httptest.NewRequest(http.MethodGet, "/state", nil))
		var state struct {
			GitBranch        string   `json:"gitBranch"`
			ProtectedFolder  string   `json:"protectedFolder"`
			InstructionFiles []string `json:"instructionFiles"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
			t.Fatalf("%s state: %v: %s", provider, err, rec.Body.String())
		}
		if state.GitBranch != "" {
			t.Errorf("%s: read the repository's branch %q in a protected folder", provider, state.GitBranch)
		}
		if state.ProtectedFolder != filemodel.ProtectedNotice {
			t.Errorf("%s: protectedFolder = %q, want the notice", provider, state.ProtectedFolder)
		}
		for _, path := range state.InstructionFiles {
			if strings.HasPrefix(path, cwd) {
				t.Errorf("%s: discovered %s in a protected folder", provider, path)
			}
		}

		rec = httptest.NewRecorder()
		sess.view.HandleInstructionFiles(rec, httptest.NewRequest(http.MethodGet, "/instruction-files", nil))
		var list struct {
			Files []instructionFileMeta `json:"files"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("%s instruction files: %v", provider, err)
		}
		for _, file := range list.Files {
			if strings.HasPrefix(file.Path, cwd) && (file.Exists || file.ReadOnlyReason != filemodel.ProtectedNotice) {
				t.Errorf("%s: inspected %s in a protected folder: %+v", provider, file.Path, file)
			}
		}
	}

	// A thread outside the folder whose instruction file is a link into it
	// (seen in review: the read served the protected file's content).
	safe := filepath.Join(home, "Code", "project")
	if err := os.MkdirAll(safe, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(safe, "AGENTS.md")
	if err := os.Symlink(agents, link); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	newTestCodexSession(t, safe, []string{link}).view.HandleInstructionFileRead(rec, httptest.NewRequest(http.MethodGet, "/read?path="+link, nil))
	if rec.Code == http.StatusOK {
		t.Errorf("served %s through a link into a protected folder: %s", link, rec.Body.String())
	}

	if got := GrokProbeCwd(cwd); got == cwd {
		t.Errorf("Grok's probe chose the protected folder %s", got)
	}
}
