// Package historical resolves current workspace configuration for saved Codex
// and Claude conversations. It combines recorded thread evidence with current
// read-only provider/filesystem observations and caches bounded workspace
// snapshots in the existing history store. It does not attach sessions, change
// provider files, choose runtime posture, or assemble sidebar/live overlays.
package historical
