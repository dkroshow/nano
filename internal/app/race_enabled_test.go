//go:build race

package app

// raceEnabled reports whether the race detector is compiled in. Wall-clock
// bounds that prove a whole-store scan is gone hold in the plain suite; under
// -race on a loaded machine the same work can take several times longer.
const raceEnabled = true
