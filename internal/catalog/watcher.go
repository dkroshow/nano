package catalog

// WatchStats is the file watcher's ownership state at one instant. It counts
// watches, not kernel memory: the kernel does not report per-watch cost.
type WatchStats struct {
	// Paths is the number of distinct expected files held by attachments.
	Paths int `json:"paths"`
	// Files is the number of expected files watched directly.
	Files int `json:"files"`
	// Directories is the number of shared temporary directory watches that
	// wait for absent or replaced expected files.
	Directories int `json:"directories"`
	// Waiting is the number of expected files that do not exist yet.
	Waiting int `json:"waiting"`
	// Failed is the number of expected files whose watch could not be
	// established; they are not monitored until they are held again.
	Failed int `json:"failed"`
	// PathBytes is the bytes of path text the watcher holds.
	PathBytes int64 `json:"pathBytes"`
	// LoopFailed reports that the event loop stopped after a kernel error.
	LoopFailed bool `json:"loopFailed"`
}
