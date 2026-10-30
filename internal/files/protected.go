package files

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ProtectedNotice is what a surface shows in place of a detail it did not read.
const ProtectedNotice = "Not read: macOS protects this folder, and reading it would ask you for permission."

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

// maxLinkHops bounds a symlink loop, as the kernel's MAXSYMLINKS does.
const maxLinkHops = 32

// Protected reports whether path is one of the folders macOS guards with a
// privacy prompt, or leads into one through a symlink. Any read there, even a
// stat or a git status, asks the user whether "codex-nano-bin" may access it,
// and the answer belongs to one build of the binary, so Nano reads nothing
// there. Links are followed one step at a time and each step is checked
// before it is looked at, so following a link never reads inside a folder.
func Protected(path string) bool {
	if runtime.GOOS != "darwin" || path == "" {
		return false
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	roots := protectedRoots()
	if spelledProtected(path, roots) {
		return true
	}
	current, pending := "/", splitPath(path)
	for hops := 0; len(pending) > 0; {
		next := filepath.Join(current, pending[0])
		pending = pending[1:]
		if spelledProtected(next, roots) {
			return true
		}
		info, err := os.Lstat(next)
		if err != nil {
			return false // nothing further exists to lead anywhere
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		target, err := os.Readlink(next)
		if hops++; err != nil || hops > maxLinkHops {
			return false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(current, target)
		}
		current, pending = "/", append(splitPath(target), pending...)
	}
	return false
}

// protectedRoots spells each folder both ways when the home path itself goes
// through a link, so a resolved path still matches.
func protectedRoots() []string {
	roots := []string{"/Volumes"}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return roots
	}
	homes := []string{home}
	if resolved, err := filepath.EvalSymlinks(home); err == nil && resolved != home {
		homes = append(homes, resolved)
	}
	for _, home := range homes {
		for _, folder := range protectedHomeFolders {
			roots = append(roots, filepath.Join(home, folder))
		}
	}
	return roots
}

// spelledProtected compares the spelling only. The default macOS volume
// ignores case, so ~/documents is ~/Documents.
func spelledProtected(path string, roots []string) bool {
	for _, root := range roots {
		if len(path) >= len(root) && strings.EqualFold(path[:len(root)], root) &&
			(len(path) == len(root) || path[len(root)] == '/') {
			return true
		}
	}
	return false
}

func splitPath(path string) []string {
	path = strings.Trim(filepath.Clean(path), "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}
