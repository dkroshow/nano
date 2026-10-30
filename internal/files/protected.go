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

// maxLinkHops bounds a symlink chain, as the kernel's MAXSYMLINKS does. A
// longer chain counts as protected: refusing is the safe answer.
const maxLinkHops = 32

// Protected reports whether path is one of the folders macOS guards with a
// privacy prompt, or leads into one through a symlink. Any read there, even a
// stat or a git status, asks the user whether "codex-nano-bin" may access it,
// and the answer belongs to one build of the binary, so Nano reads nothing
// there. The path is walked the way the kernel walks it, one component at a
// time, following each link and applying ".." to where the walk really is,
// and every step is checked before it is looked at.
func Protected(path string) bool {
	roots := protectedRoots()
	if roots == nil || path == "" {
		return false
	}
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return true
		}
		path = wd + "/" + path
	}
	if spelledProtected(filepath.Clean(path), roots) {
		return true
	}
	current, pending := "/", strings.Split(path, "/")
	for hops := 0; len(pending) > 0; {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, name)
		if spelledProtected(next, roots) {
			return true
		}
		info, err := os.Lstat(next)
		if err != nil {
			// Nothing further exists to follow; the rest is only a spelling.
			return spelledProtected(filepath.Join(append([]string{next}, pending...)...), roots)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		target, err := os.Readlink(next)
		if hops++; err != nil || hops > maxLinkHops {
			return true
		}
		if filepath.IsAbs(target) {
			current = "/"
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	return false
}

// protectedRoots spells each folder both ways when the home path itself goes
// through a link, so a resolved path still matches.
func protectedRoots() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
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
