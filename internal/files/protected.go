package files

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ProtectedNotice is what a surface shows in place of a detail it did not read.
const ProtectedNotice = "Not read: macOS asks permission before any app reads this folder (Desktop, Documents, Downloads, iCloud Drive, cloud storage, other apps' data or an external volume), so Nano does not look inside it."

// The folders behind the file TCC services tccd names: Desktop, Documents and
// Downloads; iCloud Drive (Ubiquity) and File Provider cloud folders; other
// apps' data (AppData); removable and network volumes.
var protectedHomeFolders = []string{
	"Desktop",
	"Documents",
	"Downloads",
	filepath.Join("Library", "Mobile Documents"),
	filepath.Join("Library", "CloudStorage"),
	filepath.Join("Library", "Containers"),
	filepath.Join("Library", "Group Containers"),
}

// Protected reports whether path is one of the folders macOS guards with a
// privacy prompt, or inside one. Any read there, even a stat or a git status,
// asks the user whether "codex-nano-bin" may access it, and the answer belongs
// to one build of the binary, so Nano reads nothing there. Only the spelling
// is compared: resolving a link would itself read inside the folder.
func Protected(path string) bool {
	if runtime.GOOS != "darwin" || path == "" {
		return false
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	roots := []string{"/Volumes"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, folder := range protectedHomeFolders {
			roots = append(roots, filepath.Join(home, folder))
		}
	}
	for _, root := range roots {
		// The default macOS volume ignores case, so ~/documents is ~/Documents.
		if len(path) >= len(root) && strings.EqualFold(path[:len(root)], root) &&
			(len(path) == len(root) || path[len(root)] == '/') {
			return true
		}
	}
	return false
}
