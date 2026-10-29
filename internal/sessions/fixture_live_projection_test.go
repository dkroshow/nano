package sessions

import (
	"github.com/dkroshow/nano/internal/protocol"
	"time"
)

func newLiveProjection(provider string, ids ...string) *LiveProjection {
	id := ""
	if len(ids) > 0 {
		id = ids[0]
	}
	return NewLiveProjection(provider, id, false)
}
func projectionStateForTest(p *LiveProjection) liveProjectionSnapshot {
	status := p.Status()
	p.mu.Lock()
	native := p.nativeThread
	p.mu.Unlock()
	return liveProjectionSnapshot{NativeThreadID: native, Turns: p.TurnList(), Runtime: status.Runtime, Interactions: status.Interactions}
}

func (p *LiveProjection) applyWire(dir protocol.Direction, line []byte, at time.Time) {
	p.ApplyWireLive(dir, line, at, true, false)
}
