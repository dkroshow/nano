package history

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	parsegrok "github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const maxDiagnosticDetails = 25

// DiscoverRoot walks exactly one configured provider root. It does not follow
// symlinks, so a directory link cannot escape the discovery boundary.
func DiscoverRoot(ctx context.Context, root SourceRoot) (DiscoveryResult, error) {
	result := DiscoveryResult{Root: root}
	info, err := os.Stat(root.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("stat history root %s: %w", root.Path, err)
	}
	if !info.IsDir() {
		return result, fmt.Errorf("history root is not a directory: %s", root.Path)
	}
	result.Present = true

	err = filepath.WalkDir(root.Path, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			addDiagnostic(&result.Diagnostics, &result.Summary, Diagnostic{
				Kind: DiagnosticFilesystem, Message: "history path could not be inspected",
			})
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root.Path || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !matchesRootFile(root, path) {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			addDiagnostic(&result.Diagnostics, &result.Summary, Diagnostic{
				Kind: DiagnosticFilesystem, Message: "history file metadata could not be read",
			})
			return nil
		}
		relative, err := filepath.Rel(root.Path, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			addDiagnostic(&result.Diagnostics, &result.Summary, Diagnostic{
				Kind: DiagnosticFilesystem, Message: "history file resolved outside its source root",
			})
			return nil
		}
		result.Candidates = append(result.Candidates, discoveredCandidate(root, path, relative, fileInfo))
		return nil
	})
	if err != nil {
		return result, err
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		return result.Candidates[i].Path < result.Candidates[j].Path
	})
	if root.Kind == RootGrokSessions {
		applyGrokSubagentLineage(&result)
	}
	return result, nil
}

// DiscoverFile inspects one known path without walking its root. Missing,
// unrelated, directory, and symlink paths are successful non-matches.
func DiscoverFile(ctx context.Context, root SourceRoot, path string) (SourceCandidate, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceCandidate{}, false, err
	}
	relative, err := filepath.Rel(root.Path, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return SourceCandidate{}, false, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return SourceCandidate{}, false, nil
	}
	if err != nil {
		return SourceCandidate{}, false, fmt.Errorf("stat history file %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !matchesRootFile(root, path) {
		return SourceCandidate{}, false, nil
	}
	return discoveredCandidate(root, path, relative, info), true, nil
}

func discoveredCandidate(root SourceRoot, path, relative string, info fs.FileInfo) SourceCandidate {
	device, inode := fileIdentity(info)
	return SourceCandidate{
		Root: root, Path: path, RelativePath: relative,
		Kind: classifyPath(root, relative), Archived: root.Archived,
		Size: info.Size(), ModTime: info.ModTime(), Device: device, Inode: inode,
	}
}

// applyGrokSubagentLineage joins parent-owned sidecars to the ordinary child
// sources discovered above. The child's own summary/updates file stays the
// transcript source; this only enriches its existing metadata projection.
func applyGrokSubagentLineage(result *DiscoveryResult) {
	relations := map[string]string{}
	conflicts := map[string]bool{}
	_ = filepath.WalkDir(result.Root.Path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry == nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Base(path) != "meta.json" || filepath.Base(filepath.Dir(filepath.Dir(path))) != "subagents" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil || len(raw) > 64<<10 {
			return nil
		}
		_, batch, parseErr := parsegrok.ParseFile(parsegrok.State{}, model.Input{
			Bytes: raw, Format: "json", Closed: true, MaxRowBytes: 64 << 10,
			Source: model.SourceRef{Label: path, Incarnation: "discovery"},
		})
		if parseErr != nil {
			return nil
		}
		var parent, child string
		for _, observation := range batch.Observations {
			fields := observation.Metadata
			if fields[model.MetadataSourceKind].Value != string(ThreadSubagent) {
				continue
			}
			if value := fields[model.MetadataParentNativeID]; value.State == model.MetadataKnown {
				parent = value.Value
			}
			if value := fields[model.MetadataNativeThreadID]; value.State == model.MetadataKnown {
				child = value.Value
			}
		}
		if strings.TrimSpace(parent) == "" || strings.TrimSpace(child) == "" {
			return nil
		}
		if existing, ok := relations[child]; ok && existing != parent {
			addDiagnostic(&result.Diagnostics, &result.Summary, Diagnostic{
				Kind: DiagnosticIdentityConflict, Message: "Grok child sidecars claim different native parents",
			})
			delete(relations, child)
			conflicts[child] = true
			return nil
		}
		if conflicts[child] {
			return nil
		}
		relations[child] = parent
		return nil
	})
	for i := range result.Candidates {
		candidate := &result.Candidates[i]
		child := filepath.Base(filepath.Dir(candidate.Path))
		if parent := relations[child]; parent != "" {
			candidate.Kind, candidate.ParentNativeID = ThreadSubagent, parent
		}
	}
}

func matchesRootFile(root SourceRoot, path string) bool {
	base := filepath.Base(path)
	switch root.Kind {
	case RootCodexSessions, RootCodexArchived:
		return strings.HasPrefix(base, "rollout-") && strings.EqualFold(filepath.Ext(base), ".jsonl")
	case RootClaudeProjects:
		return strings.EqualFold(filepath.Ext(base), ".jsonl")
	case RootGrokSessions:
		// Grok's durable session directory is <encoded-cwd>/<session-id>/,
		// containing summary.json and an optional sibling updates.jsonl. Use
		// the append-only update stream as the source whenever it exists so
		// transcript checkpoints share its byte offsets. Older metadata-only
		// sessions fall back to summary.json, which is rewritten whole.
		if base == "updates.jsonl" {
			return true
		}
		if base != "summary.json" {
			return false
		}
		_, err := os.Stat(filepath.Join(filepath.Dir(path), "updates.jsonl"))
		return errors.Is(err, fs.ErrNotExist)
	default:
		return false
	}
}

func classifyPath(root SourceRoot, relative string) ThreadKind {
	if root.Provider != ProviderClaude {
		return ThreadMain
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	base := parts[len(parts)-1]
	for _, part := range parts[:len(parts)-1] {
		if part == "subagents" && strings.HasPrefix(base, "agent-") {
			return ThreadSubagent
		}
	}
	return ThreadMain
}

func fileIdentity(info fs.FileInfo) (uint64, uint64) {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return 0, 0
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, 0
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 0, 0
	}
	return uintField(value, "Dev"), uintField(value, "Ino")
}

func uintField(value reflect.Value, name string) uint64 {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Int() >= 0 {
			return uint64(field.Int())
		}
	}
	return 0
}

func addDiagnostic(details *[]Diagnostic, summary *DiagnosticSummary, diagnostic Diagnostic) {
	summary.Total++
	switch diagnostic.Kind {
	case DiagnosticMalformed:
		summary.Malformed++
	case DiagnosticUnknownRow:
		summary.UnknownRows++
	case DiagnosticMissingIdentity:
		summary.MissingIdentity++
	case DiagnosticIdentityConflict:
		summary.IdentityConflicts++
	case DiagnosticOversizedLine:
		summary.OversizedLines++
	case DiagnosticFilesystem:
		summary.Filesystem++
	}
	if len(*details) < maxDiagnosticDetails {
		*details = append(*details, diagnostic)
		return
	}
	summary.Dropped++
}
