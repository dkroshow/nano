//go:build race

package history

// raceEnabled reports whether the race detector is compiled in. The bounded
// collector test drains a multi-thread fixture through thirteen per-table
// batched deletes; under -race with modernc sqlite that takes minutes, so it
// runs only in the non-race suite where it finishes in under thirty seconds.
const raceEnabled = true
