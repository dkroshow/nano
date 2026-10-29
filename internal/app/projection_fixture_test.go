package app

import (
	"time"

	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/sessions"
)

func newLiveProjection(provider string, ids ...string) *sessions.LiveProjection {
	id := ""
	if len(ids) > 0 {
		id = ids[0]
	}
	return sessions.NewLiveProjection(provider, id, false)
}
func newLiveProjectionWithDeltaSetting(provider string, drop bool) *sessions.LiveProjection {
	return sessions.NewLiveProjection(provider, "", drop)
}
func applyProjectionWire(p *sessions.LiveProjection, dir protocol.Direction, line []byte, at time.Time) {
	p.ApplyWireLive(dir, line, at, true, false)
}
