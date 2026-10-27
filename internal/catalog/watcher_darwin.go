//go:build darwin

package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const (
	// A file's own events are hints to re-read it: an append, a truncation
	// (attributes), or a replacement of the name it was opened by.
	fileWatchFlags = syscall.NOTE_WRITE | syscall.NOTE_EXTEND | syscall.NOTE_ATTRIB |
		syscall.NOTE_DELETE | syscall.NOTE_RENAME | syscall.NOTE_REVOKE
	// A directory's NOTE_WRITE means an entry was added, removed or renamed.
	dirWatchFlags = syscall.NOTE_WRITE | syscall.NOTE_DELETE | syscall.NOTE_RENAME | syscall.NOTE_REVOKE
	goneFlags     = syscall.NOTE_DELETE | syscall.NOTE_RENAME | syscall.NOTE_REVOKE
	// O_EVTONLY keeps a watch from pinning its volume; O_CLOEXEC keeps the
	// descriptor out of the provider processes Nano spawns.
	watchOpenFlags = syscall.O_EVTONLY | syscall.O_CLOEXEC | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
	// A name that keeps changing between open and registration is retried a
	// few times, then left waiting on its directory, whose next event retries.
	maxBindAttempts = 4
	watchEventBatch = 64
)

// FileWatcher watches a known set of files with one kqueue. Its caller owns
// which files are wanted and for how long. It never lists a
// directory: a file that does not exist yet, or was just replaced, is waited
// for through a temporary watch on its nearest existing ancestor, shared by
// every expected file below it and released once none needs it. Each event is
// only a hint that the named file may have changed; the caller re-reads it.
//
// Only the event loop closes descriptors. A released watch leaves the
// descriptor table at once but keeps its number open until the loop has
// finished the events in hand, so a reused number cannot carry a stale event
// to a different file.
type FileWatcher struct {
	kq      int
	changed func(string)
	logf    func(string, ...any)
	done    chan struct{}

	mu       sync.Mutex
	paths    map[string]*expectedFile
	fds      map[int]*vnodeWatch
	dirs     map[string]*vnodeWatch
	released []int
	closed   bool
	loopErr  error
}

type expectedFile struct {
	path string
	home string
	file *vnodeWatch
	dir  *vnodeWatch
	err  error
}

type vnodeWatch struct {
	fd   int
	path string
	// waiters is non-nil only for a directory watch.
	waiters map[*expectedFile]struct{}
}

// StartFileWatcher starts the event loop. changed is called from that loop,
// once per file per event batch, and must not block.
func StartFileWatcher(changed func(string), logf func(string, ...any)) (*FileWatcher, error) {
	syscall.ForkLock.RLock()
	kq, err := syscall.Kqueue()
	if err == nil {
		syscall.CloseOnExec(kq)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("create kqueue: %w", err)
	}
	wake := syscall.Kevent_t{Filter: syscall.EVFILT_USER, Flags: syscall.EV_ADD | syscall.EV_CLEAR}
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{wake}, nil, nil); err != nil {
		syscall.Close(kq)
		return nil, fmt.Errorf("register kqueue wake: %w", err)
	}
	w := &FileWatcher{
		kq: kq, changed: changed, logf: logf, done: make(chan struct{}),
		paths: map[string]*expectedFile{}, fds: map[int]*vnodeWatch{}, dirs: map[string]*vnodeWatch{},
	}
	go w.run()
	return w, nil
}

// Watch starts watching path. home bounds the directories that may be
// watched while the file is absent. Watching a watched path retries it only
// if its watch failed.
func (w *FileWatcher) Watch(path, home string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.loopErr != nil {
		return
	}
	p := w.paths[path]
	if p == nil {
		p = &expectedFile{path: path, home: home}
		w.paths[path] = p
	} else if p.err == nil {
		return
	}
	w.bind(p)
	if len(w.released) > 0 {
		w.wake()
	}
}

// Unwatch removes every watch path used.
func (w *FileWatcher) Unwatch(path string) {
	w.mu.Lock()
	p := w.paths[path]
	if p == nil {
		w.mu.Unlock()
		return
	}
	delete(w.paths, path)
	if p.file != nil {
		w.retire(p.file)
		p.file = nil
	}
	w.setDir(p, nil)
	w.mu.Unlock()
	w.wake()
}

func (w *FileWatcher) Stats() WatchStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	stats := WatchStats{Paths: len(w.paths), Directories: len(w.dirs), LoopFailed: w.loopErr != nil}
	for path, p := range w.paths {
		stats.PathBytes += int64(len(path))
		switch {
		case w.loopErr != nil:
			stats.Failed++
		case p.file != nil:
			stats.Files++
		case p.err != nil:
			stats.Failed++
		default:
			stats.Waiting++
		}
	}
	return stats
}

// Close stops the loop and closes every descriptor.
func (w *FileWatcher) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()
	w.wake()
	<-w.done
	syscall.Close(w.kq)
}

func (w *FileWatcher) wake() {
	trigger := syscall.Kevent_t{Filter: syscall.EVFILT_USER, Fflags: syscall.NOTE_TRIGGER}
	_, _ = syscall.Kevent(w.kq, []syscall.Kevent_t{trigger}, nil, nil)
}

func (w *FileWatcher) run() {
	defer close(w.done)
	events := make([]syscall.Kevent_t, watchEventBatch)
	for {
		w.mu.Lock()
		w.closeReleased()
		if w.closed {
			w.closeAllLocked()
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()
		n, err := syscall.Kevent(w.kq, nil, events, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			w.mu.Lock()
			w.loopErr = err
			w.closeAllLocked()
			w.mu.Unlock()
			w.logf("attached-file watcher stopped: %v", err)
			return
		}
		w.mu.Lock()
		hints := w.dispatch(events[:n])
		w.mu.Unlock()
		for _, path := range hints {
			w.changed(path)
		}
	}
}

// dispatch turns one batch into the expected files to re-read.
func (w *FileWatcher) dispatch(events []syscall.Kevent_t) []string {
	var hints []string
	seen := map[string]bool{}
	hint := func(path string) {
		if !seen[path] {
			seen[path] = true
			hints = append(hints, path)
		}
	}
	for _, event := range events {
		if event.Filter == syscall.EVFILT_USER {
			continue
		}
		v := w.fds[int(event.Ident)]
		if v == nil {
			continue
		}
		if v.waiters == nil {
			p := w.paths[v.path]
			if p == nil || p.file != v {
				continue
			}
			if event.Fflags&goneFlags != 0 {
				w.retire(v)
				p.file = nil
				w.bind(p)
			}
			hint(p.path)
			continue
		}
		waiters := make([]*expectedFile, 0, len(v.waiters))
		for p := range v.waiters {
			waiters = append(waiters, p)
		}
		if event.Fflags&goneFlags != 0 {
			for _, p := range waiters {
				p.dir = nil
			}
			delete(w.dirs, v.path)
			w.retire(v)
		}
		for _, p := range waiters {
			w.bind(p)
			if p.file != nil {
				hint(p.path)
			}
		}
	}
	return hints
}

// bind watches p's file when it exists, otherwise its nearest existing
// ancestor. After a directory watch is registered the name is checked again,
// so a file or directory created during registration is not missed.
func (w *FileWatcher) bind(p *expectedFile) {
	var last *vnodeWatch
	for attempt := 0; attempt < maxBindAttempts; attempt++ {
		file, err := w.openFile(p.path)
		if err != nil {
			w.fail(p, err)
			return
		}
		if file != nil {
			w.setDir(p, nil)
			p.file, p.err = file, nil
			return
		}
		dir, err := w.ancestorWatch(p)
		if err != nil {
			w.fail(p, err)
			return
		}
		w.setDir(p, dir)
		p.err = nil
		if dir == last {
			return
		}
		last = dir
	}
}

// openFile watches the file now at path. It returns nil without an error when
// nothing is there, and retries when the name is replaced between the open and
// the registration, which would leave the watch on an unlinked file.
func (w *FileWatcher) openFile(path string) (*vnodeWatch, error) {
	for attempt := 0; attempt < maxBindAttempts; attempt++ {
		fd, err := openWatch(path)
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("open: %w", err)
		}
		var opened syscall.Stat_t
		if err := syscall.Fstat(fd, &opened); err != nil {
			syscall.Close(fd)
			return nil, fmt.Errorf("stat: %w", err)
		}
		if opened.Mode&syscall.S_IFMT != syscall.S_IFREG {
			syscall.Close(fd)
			return nil, errors.New("not a regular file")
		}
		if err := w.register(fd, fileWatchFlags); err != nil {
			syscall.Close(fd)
			return nil, err
		}
		v := &vnodeWatch{fd: fd, path: path}
		w.fds[fd] = v
		var current syscall.Stat_t
		if syscall.Lstat(path, &current) == nil && current.Dev == opened.Dev && current.Ino == opened.Ino {
			return v, nil
		}
		w.retire(v)
	}
	return nil, nil
}

// ancestorWatch returns the shared watch on the nearest existing directory
// above p, never above p's home.
func (w *FileWatcher) ancestorWatch(p *expectedFile) (*vnodeWatch, error) {
	for dir := filepath.Dir(p.path); within(dir, p.home); dir = filepath.Dir(dir) {
		if v := w.dirs[dir]; v != nil {
			return v, nil
		}
		fd, err := openWatch(dir)
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open directory: %w", err)
		}
		if err := w.register(fd, dirWatchFlags); err != nil {
			syscall.Close(fd)
			return nil, err
		}
		v := &vnodeWatch{fd: fd, path: dir, waiters: map[*expectedFile]struct{}{}}
		w.fds[fd], w.dirs[dir] = v, v
		return v, nil
	}
	return nil, errors.New("no existing directory within the provider home")
}

func (w *FileWatcher) register(fd int, flags uint32) error {
	change := syscall.Kevent_t{
		Ident: uint64(fd), Filter: syscall.EVFILT_VNODE,
		Flags: syscall.EV_ADD | syscall.EV_CLEAR, Fflags: flags,
	}
	if _, err := syscall.Kevent(w.kq, []syscall.Kevent_t{change}, nil, nil); err != nil {
		return fmt.Errorf("register kqueue watch: %w", err)
	}
	return nil
}

// setDir moves p to dir's waiters, releasing a directory nothing waits on.
func (w *FileWatcher) setDir(p *expectedFile, dir *vnodeWatch) {
	if p.dir == dir {
		return
	}
	if old := p.dir; old != nil {
		delete(old.waiters, p)
		if len(old.waiters) == 0 {
			delete(w.dirs, old.path)
			w.retire(old)
		}
	}
	p.dir = dir
	if dir != nil {
		dir.waiters[p] = struct{}{}
	}
}

func (w *FileWatcher) fail(p *expectedFile, err error) {
	w.setDir(p, nil)
	if p.file != nil {
		w.retire(p.file)
		p.file = nil
	}
	if p.err == nil {
		w.logf("attached-file watch failed for %s: %v", p.path, err)
	}
	p.err = err
}

func (w *FileWatcher) retire(v *vnodeWatch) {
	delete(w.fds, v.fd)
	w.released = append(w.released, v.fd)
}

func (w *FileWatcher) closeReleased() {
	for _, fd := range w.released {
		syscall.Close(fd)
	}
	w.released = w.released[:0]
}

func (w *FileWatcher) closeAllLocked() {
	for fd := range w.fds {
		syscall.Close(fd)
	}
	w.closeReleased()
	w.fds, w.dirs = map[int]*vnodeWatch{}, map[string]*vnodeWatch{}
	for _, p := range w.paths {
		p.file, p.dir = nil, nil
	}
}

func openWatch(path string) (int, error) {
	for {
		fd, err := syscall.Open(path, watchOpenFlags, 0)
		if !errors.Is(err, syscall.EINTR) {
			return fd, err
		}
	}
}

func within(path, home string) bool {
	return home != "" && (path == home || strings.HasPrefix(path, home+string(os.PathSeparator)))
}
