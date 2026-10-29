package sessions

import (
	"time"

	"github.com/dkroshow/nano/internal/protocol"
)

func newLiveProjection(provider string, ids ...string) *LiveProjection {
	id := ""
	if len(ids) > 0 {
		id = ids[0]
	}
	return NewLiveProjection(provider, id, false)
}
func projectionStateForTest(p *LiveProjection) liveProjectionSnapshot {
	status := p.status()
	p.mu.Lock()
	native := p.nativeThread
	p.mu.Unlock()
	return liveProjectionSnapshot{NativeThreadID: native, Turns: p.turnList(), Runtime: status.Runtime, Interactions: status.Interactions}
}

func (p *LiveProjection) applyWire(dir protocol.Direction, line []byte, at time.Time) {
	p.applyWireLive(dir, line, at, true, false)
}
