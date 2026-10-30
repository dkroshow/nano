package files

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxBytes      = 1 << 20
	MaxCandidates = 20
	maxVisited    = 20_000
	maxMatches    = 20
	maxAscent     = 64
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true,
	"target": true, "__pycache__": true, ".venv": true,
}

type Meta struct {
	RequestedPath  string  `json:"requestedPath,omitempty"`
	Path           string  `json:"path"`
	Basename       string  `json:"basename"`
	Content        string  `json:"content,omitempty"`
	Encoding       string  `json:"encoding,omitempty"`
	SizeBytes      int64   `json:"sizeBytes"`
	LineCount      int     `json:"lineCount,omitempty"`
	Mtime          float64 `json:"mtime"`
	Editable       bool    `json:"editable"`
	ReadOnlyReason string  `json:"readOnlyReason,omitempty"`
	ResolvedBy     string  `json:"resolvedBy,omitempty"`
}

type Error struct {
	Code       string
	Message    string
	Status     int
	Candidates []string
}

func (err *Error) Error() string { return err.Message }

// Resolve turns an authored link into a file on disk. Documents address each
// other loosely, so it widens one step at a time: the authoring directory, the
// thread directory, their repository roots, then transcript evidence. Roots
// stay narrow so a link cannot resolve into an unrelated checkout.
func Resolve(requested, cwd, threadCWD string, evidence func(string) []string) (string, string, *Error) {
	if requested == "" || strings.ContainsRune(requested, 0) {
		return "", "", fileError("invalid_path", "Path is required")
	}
	requested = ExpandHome(requested)
	if filepath.IsAbs(requested) {
		path, err := CanonicalExisting(requested)
		return path, "absolute", err
	}
	if cwd != "" {
		if path, err := CanonicalExisting(filepath.Join(cwd, requested)); err == nil {
			return path, "cwd", nil
		}
	}
	if threadCWD != "" && threadCWD != CleanDir(cwd) {
		if path, err := CanonicalExisting(filepath.Join(threadCWD, requested)); err == nil {
			return path, "thread-cwd", nil
		}
	}

	roots := searchRoots(cwd, threadCWD)
	if path, ok := resolveAncestor(requested, cwd, roots); ok {
		return path, "ancestor", nil
	}
	matches := suffixMatches(requested, roots)
	if len(matches) == 1 {
		return matches[0], "suffix", nil
	}
	ambiguous := &Error{Code: "ambiguous_path", Message: "More than one file matches this reference", Status: http.StatusConflict, Candidates: matches}
	relative := evidenceRelativePath(requested)
	if relative == "" {
		if len(matches) > 1 {
			return "", "", ambiguous
		}
		return "", "", fileError("not_found", "File not found")
	}
	var blobs []string
	if evidence != nil {
		blobs = evidence(relative)
	}
	candidates := VerifiedEvidenceCandidates(blobs, relative)
	switch {
	case len(candidates) == 1:
		return candidates[0], "thread-evidence", nil
	case len(candidates) > 1:
		return "", "", &Error{Code: "ambiguous_path", Message: "More than one file matches this thread reference", Status: http.StatusConflict, Candidates: candidates}
	case len(matches) > 1:
		return "", "", ambiguous
	default:
		return "", "", fileError("not_found", "File not found")
	}
}

func searchRoots(dirs ...string) []string {
	var roots []string
	seen := make(map[string]bool)
	add := func(dir string) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			roots = append(roots, dir)
		}
	}
	for _, dir := range dirs {
		dir = CleanDir(dir)
		if dir != "" {
			add(repositoryRoot(dir))
			add(dir)
		}
	}
	return roots
}

func CleanDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = ExpandHome(path)
	if Protected(path) {
		return ""
	}
	canonical := CanonicalPath(path)
	if info, err := os.Stat(canonical); err != nil || !info.IsDir() {
		return ""
	}
	return canonical
}

// An unversioned directory is its own boundary, not permission to search its
// parents or the entire home directory.
func repositoryRoot(dir string) string {
	current := dir
	for index := 0; index < maxAscent; index++ {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return dir
}

// Bare filenames may climb only to the outermost root already derived from
// the authoring directory.
func resolveAncestor(requested, cwd string, roots []string) (string, bool) {
	if requested != filepath.Base(requested) || requested == "." || requested == ".." {
		return "", false
	}
	dir := CleanDir(cwd)
	if dir == "" {
		return "", false
	}
	boundary := ""
	for _, root := range roots {
		if dir != root && !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
			continue
		}
		if boundary == "" || len(root) < len(boundary) {
			boundary = root
		}
	}
	if boundary == "" {
		return "", false
	}
	for {
		if path, err := CanonicalExisting(filepath.Join(dir, requested)); err == nil {
			return path, true
		}
		if dir == boundary {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// suffixMatches ignores copied dependency and build trees. Visit and result
// caps keep a mistyped link from walking a large checkout indefinitely.
func suffixMatches(requested string, roots []string) []string {
	want := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(requested)), "./")
	want = strings.TrimPrefix(want, "/")
	if want == "" || want == "." || strings.HasPrefix(want, "../") {
		return nil
	}
	var matches []string
	seen := make(map[string]bool)
	visited := 0
	// The walk never follows a link, so the spelling of each directory is
	// enough to keep it out of the folders macOS protects.
	protected := protectedRoots()
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				if path != root && (skipDirs[entry.Name()] || spelledProtected(path, protected)) {
					return filepath.SkipDir
				}
				return nil
			}
			visited++
			if visited > maxVisited {
				return filepath.SkipAll
			}
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return nil
			}
			relative = filepath.ToSlash(relative)
			if relative != want && !strings.HasSuffix("/"+relative, "/"+want) {
				return nil
			}
			resolved, fileErr := CanonicalExisting(path)
			if fileErr != nil || seen[resolved] {
				return nil
			}
			seen[resolved] = true
			matches = append(matches, resolved)
			if len(matches) >= maxMatches {
				return filepath.SkipAll
			}
			return nil
		})
	}
	sort.Slice(matches, func(left, right int) bool {
		if len(matches[left]) != len(matches[right]) {
			return len(matches[left]) < len(matches[right])
		}
		return matches[left] < matches[right]
	})
	return matches
}

func Inspect(path string, protectedRoots []string) (Meta, []byte, *Error) {
	path, err := CanonicalExistingAbsolute(path)
	if err != nil {
		return Meta{}, nil, err
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		return Meta{}, nil, fileError("not_found", "File not found")
	}
	if !info.Mode().IsRegular() {
		return Meta{}, nil, fileError("not_regular", "Path is not a regular file")
	}
	if info.Size() > MaxBytes {
		return Meta{}, nil, fileError("too_large", "File exceeds 1 MiB")
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return Meta{}, nil, fileError("read_only", "Cannot read file")
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return Meta{}, nil, fileError("not_text", "File is not UTF-8 text")
	}
	meta := Meta{
		Path: path, Basename: filepath.Base(path), SizeBytes: info.Size(),
		Mtime: ModificationTime(info), Editable: true,
	}
	for _, root := range protectedRoots {
		if UnderRoot(path, root) {
			meta.Editable = false
			meta.ReadOnlyReason = "Nano is read-only toward " + root
			break
		}
	}
	return meta, content, nil
}

func CanonicalExistingAbsolute(path string) (string, *Error) {
	if !filepath.IsAbs(path) {
		return "", fileError("invalid_path", "Canonical absolute path required")
	}
	canonical, err := CanonicalExisting(path)
	if err != nil {
		return "", err
	}
	if filepath.Clean(path) != canonical {
		return "", fileError("invalid_path", "Canonical absolute path required")
	}
	return canonical, nil
}

func CanonicalExisting(path string) (string, *Error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fileError("invalid_path", "Invalid file path")
	}
	if Protected(absolute) {
		return "", fileError("read_only", ProtectedNotice)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fileError("not_found", "File not found")
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fileError("not_found", "File not found")
	}
	if !info.Mode().IsRegular() {
		return "", fileError("not_regular", "Path is not a regular file")
	}
	return filepath.Clean(canonical), nil
}

func ExpandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func evidenceRelativePath(path string) string {
	path = filepath.ToSlash(strings.TrimSpace(path))
	for strings.HasPrefix(path, "./") {
		path = strings.TrimPrefix(path, "./")
	}
	if path == "" || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~") || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return ""
	}
	return path
}

func VerifiedEvidenceCandidates(blobs []string, relative string) []string {
	found := make(map[string]struct{})
	for _, blob := range blobs {
		collectEvidenceCandidates(blob, relative, found)
		if len(found) >= MaxCandidates {
			break
		}
	}
	var candidates []string
	for candidate := range found {
		if canonical, err := CanonicalExisting(candidate); err == nil {
			candidates = append(candidates, canonical)
		}
	}
	sort.Strings(candidates)
	return dedupe(candidates)
}

func collectEvidenceCandidates(blob, relative string, found map[string]struct{}) {
	needle := "/" + filepath.ToSlash(relative)
	for offset := 0; offset < len(blob); {
		index := strings.Index(blob[offset:], needle)
		if index < 0 {
			return
		}
		index += offset
		end := index + len(needle)
		if end == len(blob) || !pathByte(blob[end]) {
			start := index
			for start > 0 && pathByte(blob[start-1]) {
				start--
			}
			if start < len(blob) && blob[start] == '/' {
				found[blob[start:end]] = struct{}{}
			}
		}
		offset = index + 1
	}
}

func pathByte(value byte) bool {
	return value == '/' || value == '_' || value == '@' || value == '.' || value == '+' || value == '-' || value == '~' ||
		(value >= '0' && value <= '9') || (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z')
}

func dedupe(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

// UnderRoot reports whether a canonical path lies at or below a canonical root.
func UnderRoot(path, root string) bool {
	return root != "" && (path == root || strings.HasPrefix(path, root+string(os.PathSeparator)))
}

func fileError(code, message string) *Error {
	status := http.StatusInternalServerError
	switch code {
	case "invalid_path":
		status = http.StatusBadRequest
	case "not_found":
		status = http.StatusNotFound
	case "read_only":
		status = http.StatusForbidden
	case "not_regular", "not_text":
		status = http.StatusUnprocessableEntity
	case "too_large":
		status = http.StatusRequestEntityTooLarge
	}
	return &Error{Code: code, Message: message, Status: status}
}
