//go:build !darwin

package catalog

import "errors"

// FileWatcher is unavailable off macOS. Attached threads there stay current
// through provider wire hints and explicit refresh.
type FileWatcher struct{}

func StartFileWatcher(func(string), func(string, ...any)) (*FileWatcher, error) {
	return nil, errors.New("attached-file watching requires macOS kqueue")
}

func (*FileWatcher) Watch(string, string) {}
func (*FileWatcher) Unwatch(string)       {}
func (*FileWatcher) Stats() WatchStats    { return WatchStats{} }
func (*FileWatcher) Close()               {}
