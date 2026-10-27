// instruction_files.go — policy-wrapped access to the instruction source
// files shown in the Responses Input panel: list, read, and (where safe)
// write. The allowlist is derived per request from session state — Codex's
// authoritative thread instructionSources, Claude's discovered-on-disk files —
// so the UI can never turn this into a general file browser. Reads and writes
// use direct Go IO (not app-server fs/*); successful writes publish a
// synthetic wire event. Decision: docs/decisions/instruction-file-access.md.
package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/setup"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const maxInstructionFileBytes = 1 << 20 // files beyond this are view/edit-rejected

// instructionFilePolicy holds canonical (symlink-resolved) roots: writes must
// land under a writeRoot; protectedRoots are always view-only, enforcing the
// repo rule that nano never writes under provider-owned ~/.codex, ~/.claude,
// or ~/.grok trees.
type instructionFilePolicy struct {
	writeRoots     []string
	protectedRoots []string
}

type instructionFileMeta struct {
	Path           string `json:"path"`
	Basename       string `json:"basename"`
	Provider       string `json:"provider"`
	SourceKind     string `json:"sourceKind"`
	SourceScope    string `json:"sourceScope,omitempty"`
	SourceLoad     string `json:"sourceLoad,omitempty"`
	ProvenLoaded   bool   `json:"provenLoaded"`
	Evidence       string `json:"evidence"`
	Exists         bool   `json:"exists"`
	Editable       bool   `json:"editable"`
	ReadOnlyReason string `json:"readOnlyReason,omitempty"`
	SizeBytes      int64  `json:"sizeBytes"`
	// Mtime is Unix seconds; a save rejects an on-disk drift greater than 0.01s.
	Mtime float64 `json:"mtime"`
}

// canonicalPath cleans, absolutizes, and resolves symlinks so policy checks
// compare real filesystem locations, not spellings.
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// statMtime returns a file's modification time as unix seconds,
// the unit the write-conflict contract compares.
func statMtime(fi os.FileInfo) float64 {
	return float64(fi.ModTime().UnixNano()) / 1e9
}

func underRoot(path, root string) bool {
	return root != "" && (path == root || strings.HasPrefix(path, root+string(os.PathSeparator)))
}

func protectedInstructionRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		canonicalPath(filepath.Join(home, ".codex")),
		canonicalPath(filepath.Join(home, ".claude")),
		canonicalPath(filepath.Join(home, ".grok")),
	}
}

func instructionEvidence(src setup.InstructionFileSource) string {
	if src.Evidence != "" {
		return src.Evidence
	}
	if src.ProvenLoaded {
		return setup.InstructionEvidenceWire
	}
	return setup.InstructionEvidencePresent
}

// instructionFileSources derives the session's instruction-file allowlist and
// write policy. Codex paths are wire-authoritative (the thread response reports
// them); Claude paths are documented-expected from Claude Code's memory loading
// rules, but not wire-attested as exact prompt input.
func (sess *session) instructionFileSources() ([]setup.InstructionFileSource, instructionFilePolicy) {
	pol := instructionFilePolicy{protectedRoots: protectedInstructionRoots()}
	var sources []setup.InstructionFileSource
	switch sess.provider {
	case "codex":
		sess.stateMu.Lock()
		raw := sess.state["thread"]
		cwd := sess.cwd // the background resume handshake may still be updating it
		sess.stateMu.Unlock()
		var thread struct {
			Cwd                   string   `json:"cwd"`
			InstructionSources    []string `json:"instructionSources"`
			RuntimeWorkspaceRoots []string `json:"runtimeWorkspaceRoots"`
		}
		if len(raw) > 0 {
			json.Unmarshal(raw, &thread)
		}
		for _, p := range thread.InstructionSources {
			sources = append(sources, setup.InstructionFileSource{
				Path:         p,
				SourceKind:   "codex-thread-instruction-source",
				ProvenLoaded: true,
				Evidence:     setup.InstructionEvidenceWire,
			})
		}
		roots := append([]string(nil), thread.RuntimeWorkspaceRoots...)
		if thread.Cwd != "" {
			roots = append(roots, thread.Cwd)
		}
		if len(roots) == 0 {
			roots = []string{cwd}
		}
		for _, r := range roots {
			pol.writeRoots = append(pol.writeRoots, canonicalPath(r))
		}
	case "claude":
		sess.stateMu.Lock()
		initRaw := sess.state["init"]
		sess.stateMu.Unlock()
		sources = append(sources, setup.ClaudeInstructionSources(sess.cwd, initRaw)...)
		pol.writeRoots = []string{canonicalPath(sess.cwd)}
	}
	return sources, pol
}

func findInstructionSource(sources []setup.InstructionFileSource, path string) *setup.InstructionFileSource {
	for i := range sources {
		if sources[i].Path == path { // exact allowlist match, no normalization
			return &sources[i]
		}
	}
	return nil
}

// inspectInstructionFile stats, reads, and classifies one allowlisted file.
// content is nil whenever the file cannot be shown as text (missing,
// oversized, binary, unreadable); meta.ReadOnlyReason then says why. A
// non-nil content with Editable=false is viewable but write-protected.
func inspectInstructionFile(provider string, src setup.InstructionFileSource, pol instructionFilePolicy) (instructionFileMeta, []byte) {
	meta := instructionFileMeta{
		Path:         src.Path,
		Basename:     filepath.Base(src.Path),
		Provider:     provider,
		SourceKind:   src.SourceKind,
		SourceScope:  src.SourceScope,
		SourceLoad:   src.SourceLoad,
		ProvenLoaded: src.ProvenLoaded,
		Evidence:     instructionEvidence(src),
	}
	fi, err := os.Stat(src.Path)
	if err != nil {
		meta.ReadOnlyReason = "file does not exist"
		return meta, nil
	}
	meta.Exists = true
	meta.SizeBytes = fi.Size()
	meta.Mtime = statMtime(fi)
	if !fi.Mode().IsRegular() {
		meta.ReadOnlyReason = "not a regular file"
		return meta, nil
	}
	if fi.Size() > maxInstructionFileBytes {
		meta.ReadOnlyReason = fmt.Sprintf("file exceeds %d bytes", maxInstructionFileBytes)
		return meta, nil
	}
	content, err := os.ReadFile(src.Path)
	if err != nil {
		meta.ReadOnlyReason = "unreadable: " + err.Error()
		return meta, nil
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		meta.ReadOnlyReason = "not UTF-8 text"
		return meta, nil
	}

	canon := canonicalPath(src.Path)
	for _, root := range pol.protectedRoots {
		if underRoot(canon, root) {
			meta.ReadOnlyReason = "view-only: nano is read-only toward " + root
			return meta, content
		}
	}
	inWriteRoot := false
	for _, root := range pol.writeRoots {
		if underRoot(canon, root) {
			inWriteRoot = true
			break
		}
	}
	if !inWriteRoot {
		meta.ReadOnlyReason = "outside the session cwd and workspace roots"
		return meta, content
	}
	meta.Editable = true
	return meta, content
}

func (sess *session) handleInstructionFiles(w http.ResponseWriter, r *http.Request) {
	sources, pol := sess.instructionFileSources()
	files := make([]instructionFileMeta, 0, len(sources))
	for _, src := range sources {
		meta, _ := inspectInstructionFile(sess.provider, src, pol)
		files = append(files, meta)
	}
	httpapi.WriteJSON(w, map[string]any{"files": files})
}

func (sess *session) handleInstructionFileRead(w http.ResponseWriter, r *http.Request) {
	sources, pol := sess.instructionFileSources()
	src := findInstructionSource(sources, r.URL.Query().Get("path"))
	if src == nil {
		httpapi.WriteHTTPError(w, "path is not an instruction source of this session", http.StatusNotFound)
		return
	}
	meta, content := inspectInstructionFile(sess.provider, *src, pol)
	if !meta.Exists {
		httpapi.WriteHTTPError(w, meta.ReadOnlyReason, http.StatusNotFound)
		return
	}
	if content == nil {
		httpapi.WriteHTTPError(w, "not readable as text: "+meta.ReadOnlyReason, http.StatusUnprocessableEntity)
		return
	}
	lineCount := strings.Count(string(content), "\n")
	if len(content) > 0 && !bytes.HasSuffix(content, []byte("\n")) {
		lineCount++
	}
	httpapi.WriteJSON(w, map[string]any{
		"file": meta, "encoding": "utf-8", "content": string(content), "lineCount": lineCount,
	})
}

// handleInstructionFileMtime reports the current on-disk mtime for an
// allowlisted file so the viewer can poll for external changes cheaply.
func (sess *session) handleInstructionFileMtime(w http.ResponseWriter, r *http.Request) {
	sources, _ := sess.instructionFileSources()
	src := findInstructionSource(sources, r.URL.Query().Get("path"))
	if src == nil {
		httpapi.WriteHTTPError(w, "path is not an instruction source of this session", http.StatusNotFound)
		return
	}
	fi, err := os.Stat(src.Path)
	if err != nil || !fi.Mode().IsRegular() {
		httpapi.WriteHTTPError(w, "file not found", http.StatusNotFound)
		return
	}
	httpapi.WriteJSON(w, map[string]any{"path": src.Path, "mtime": statMtime(fi)})
}

func (sess *session) handleInstructionFileWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string   `json:"path"`
		Content string   `json:"content"`
		Mtime   *float64 `json:"mtime"` // omitted = force overwrite (UI sends it only after the user confirms)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		httpapi.WriteHTTPError(w, "body must be {\"path\": \"...\", \"content\": \"...\", \"mtime\"?: <unix seconds>}", http.StatusBadRequest)
		return
	}
	if len(req.Content) > maxInstructionFileBytes {
		httpapi.WriteHTTPError(w, fmt.Sprintf("content exceeds %d bytes", maxInstructionFileBytes), http.StatusRequestEntityTooLarge)
		return
	}
	sources, pol := sess.instructionFileSources()
	src := findInstructionSource(sources, req.Path)
	if src == nil {
		httpapi.WriteHTTPError(w, "path is not an instruction source of this session", http.StatusNotFound)
		return
	}
	meta, _ := inspectInstructionFile(sess.provider, *src, pol)
	if !meta.Exists {
		httpapi.WriteHTTPError(w, "file does not exist (nano does not create instruction files)", http.StatusNotFound)
		return
	}
	if !meta.Editable {
		httpapi.WriteHTTPError(w, "read-only: "+meta.ReadOnlyReason, http.StatusForbidden)
		return
	}
	if req.Mtime != nil && absFloat(meta.Mtime-*req.Mtime) > mtimeConflictEpsilon {
		httpapi.WriteErrorEnvelope(w, http.StatusConflict, httpapi.ErrorEnvelope{Error: httpapi.Error{
			Code: "mtime_conflict", Message: "File changed on disk since it was read", Details: map[string]float64{"mtime": meta.Mtime},
		}})

		return
	}

	canon := canonicalPath(src.Path) // write the symlink target, never replace a link
	fi, err := os.Stat(canon)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := writeFileAtomic(canon, []byte(req.Content), fi.Mode().Perm()); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	meta, _ = inspectInstructionFile(sess.provider, *src, pol)
	sess.recordNanoEvent("nano/instructionFile/written", map[string]any{
		"path": src.Path, "mtime": meta.Mtime, "sizeBytes": meta.SizeBytes,
	})
	httpapi.WriteJSON(w, map[string]any{"file": meta})
}

const mtimeConflictEpsilon = 0.01 // seconds; below this, treat mtimes as equal

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".nano-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
