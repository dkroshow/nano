// instruction_files.go — policy-wrapped access to the instruction source
// files shown in the Responses Input panel: list, read, and (where safe)
// write. The allowlist is derived per request from session state — Codex's
// authoritative thread instructionSources, Claude's discovered-on-disk files —
// so the UI can never turn this into a general file browser. Reads and writes
// use direct Go IO (not app-server fs/*); successful writes publish a
// synthetic wire event. Decision: docs/decisions/instruction-file-access.md.
package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	filemodel "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/setup"
)

const maxInstructionFileBytes = filemodel.MaxBytes // files beyond this are view/edit-rejected

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
func (sess *State) instructionFileSources() ([]setup.InstructionFileSource, instructionFilePolicy) {
	pol := instructionFilePolicy{protectedRoots: setup.ProtectedProviderRoots()}
	var sources []setup.InstructionFileSource
	switch sess.identity.provider {
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
			pol.writeRoots = append(pol.writeRoots, filemodel.CanonicalPath(r))
		}
	case "claude":
		sess.stateMu.Lock()
		initRaw := sess.state["init"]
		sess.stateMu.Unlock()
		sources = append(sources, setup.ClaudeInstructionSources(sess.cwd, initRaw)...)
		pol.writeRoots = []string{filemodel.CanonicalPath(sess.cwd)}
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
	meta.Mtime = filemodel.ModificationTime(fi)
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

	canon := filemodel.CanonicalPath(src.Path)
	for _, root := range pol.protectedRoots {
		if filemodel.UnderRoot(canon, root) {
			meta.ReadOnlyReason = "view-only: nano is read-only toward " + root
			return meta, content
		}
	}
	inWriteRoot := false
	for _, root := range pol.writeRoots {
		if filemodel.UnderRoot(canon, root) {
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

// HandleInstructionFiles lists instruction files visible to the attachment.
func (sess *View) HandleInstructionFiles(w http.ResponseWriter, r *http.Request) {
	sources, pol := sess.stateOwner.instructionFileSources()
	files := make([]instructionFileMeta, 0, len(sources))
	for _, src := range sources {
		meta, _ := inspectInstructionFile(sess.identity.provider, src, pol)
		files = append(files, meta)
	}
	httpapi.WriteJSON(w, map[string]any{"files": files})
}

// HandleInstructionFileRead reads an allowed instruction file.
func (sess *View) HandleInstructionFileRead(w http.ResponseWriter, r *http.Request) {
	sources, pol := sess.stateOwner.instructionFileSources()
	src := findInstructionSource(sources, r.URL.Query().Get("path"))
	if src == nil {
		httpapi.WriteHTTPError(w, "path is not an instruction source of this session", http.StatusNotFound)
		return
	}
	meta, content := inspectInstructionFile(sess.identity.provider, *src, pol)
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

// HandleInstructionFileMtime reports the current on-disk mtime for an
// allowlisted file so the viewer can poll for external changes cheaply.
func (sess *View) HandleInstructionFileMtime(w http.ResponseWriter, r *http.Request) {
	sources, _ := sess.stateOwner.instructionFileSources()
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
	httpapi.WriteJSON(w, map[string]any{"path": src.Path, "mtime": filemodel.ModificationTime(fi)})
}

// HandleInstructionFileWrite writes an editable instruction file with conflict checks.
func (sess *View) HandleInstructionFileWrite(w http.ResponseWriter, r *http.Request) {
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
	sources, pol := sess.stateOwner.instructionFileSources()
	src := findInstructionSource(sources, req.Path)
	if src == nil {
		httpapi.WriteHTTPError(w, "path is not an instruction source of this session", http.StatusNotFound)
		return
	}
	meta, _ := inspectInstructionFile(sess.identity.provider, *src, pol)
	if !meta.Exists {
		httpapi.WriteHTTPError(w, "file does not exist (nano does not create instruction files)", http.StatusNotFound)
		return
	}
	if !meta.Editable {
		httpapi.WriteHTTPError(w, "read-only: "+meta.ReadOnlyReason, http.StatusForbidden)
		return
	}
	if req.Mtime != nil && math.Abs(meta.Mtime-*req.Mtime) > filemodel.ModificationTimeTolerance {
		httpapi.WriteErrorEnvelope(w, http.StatusConflict, httpapi.ErrorEnvelope{Error: httpapi.Error{
			Code: "mtime_conflict", Message: "File changed on disk since it was read", Details: map[string]float64{"mtime": meta.Mtime},
		}})

		return
	}

	canon := filemodel.CanonicalPath(src.Path) // write the symlink target, never replace a link
	fi, err := os.Stat(canon)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := filemodel.WriteAtomic(canon, []byte(req.Content), fi.Mode().Perm()); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	meta, _ = inspectInstructionFile(sess.identity.provider, *src, pol)
	sess.wire.recordNanoEvent("nano/instructionFile/written", map[string]any{
		"path": src.Path, "mtime": meta.Mtime, "sizeBytes": meta.SizeBytes,
	})
	httpapi.WriteJSON(w, map[string]any{"file": meta})
}
