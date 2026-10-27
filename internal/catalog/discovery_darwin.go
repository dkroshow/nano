//go:build darwin

package catalog

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsevents"
)

const (
	// The kernel batches a root's file events over this window, so a burst
	// of writes wakes Nano once per window, not once per write.
	discoveryLatency     = 2 * time.Second
	discoveryEventBuffer = 16
)

// Discovery reports the files that changed under the provider roots, one
// FSEvents stream for all of them: the kernel records changes as part of the
// write itself, so watching costs nothing while nothing is written, and no
// descriptor is held per folder or file. It is only a trigger: the caller
// decides which paths matter and reads them. It never lists a directory; a
// dropped batch (MustScanSubDirs) is logged and left to Explorer's Refresh.
type Discovery struct {
	stream    *fsevents.EventStream
	quit      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// StartDiscovery starts one stream over roots. changed is called from its own
// goroutine, once per batch, with the batch's distinct file paths, named
// under the roots as given: FSEvents names a file by its real path, which
// differs where a root's path runs through a symlink (/var is /private/var).
func StartDiscovery(roots []string, changed func([]string), logf func(string, ...any)) (*Discovery, error) {
	names := map[string]string{}
	for _, root := range roots {
		if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
			names[real] = root
		}
	}
	stream := &fsevents.EventStream{
		Events: make(chan []fsevents.Event, discoveryEventBuffer), Paths: roots,
		Latency: discoveryLatency, Flags: fsevents.FileEvents,
	}
	if err := stream.Start(); err != nil {
		return nil, err
	}
	d := &Discovery{stream: stream, quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(d.done)
		dropped := false
		for {
			select {
			case <-d.quit:
				return
			case events, ok := <-stream.Events:
				if !ok {
					return
				}
				paths, lost := discoveryPaths(events, names)
				if lost != dropped {
					dropped = lost
					if lost {
						logf("outside-session discovery missed events (FSEvents MustScanSubDirs); Explorer's Refresh recovers them")
					}
				}
				if len(paths) > 0 {
					changed(paths)
				}
			}
		}
	}()
	return d, nil
}

// discoveryPaths keeps each changed file once, under its root's given name;
// folders are not discovery.
func discoveryPaths(events []fsevents.Event, names map[string]string) (paths []string, lost bool) {
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		if event.Flags&(fsevents.MustScanSubDirs|fsevents.UserDropped|fsevents.KernelDropped) != 0 {
			lost = true
		}
		if event.Flags&fsevents.ItemIsFile == 0 {
			continue
		}
		path := event.Path
		for real, root := range names {
			if strings.HasPrefix(path, real+string(filepath.Separator)) {
				path = root + path[len(real):]
				break
			}
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths, lost
}

func (d *Discovery) Close() {
	d.closeOnce.Do(func() {
		d.stream.Stop()
		close(d.quit)
		<-d.done
	})
}
