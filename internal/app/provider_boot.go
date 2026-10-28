package app

import (
	"sync"
	"time"
)

type bootProviderControl struct {
	handshakeSlots chan struct{}
	// Tests shorten session/load and backoff without changing production values.
	// Zero always selects the provider default.
	loadTimeout      time.Duration
	retryBackoffBase time.Duration
}

func newBootProviderControl(limit int) *bootProviderControl {
	control := &bootProviderControl{}
	if limit > 0 {
		control.handshakeSlots = make(chan struct{}, limit)
	}
	return control
}

// holdHandshakeSlot takes one of the provider's boot handshake slots for
// sess's attach and returns its release. The slot goes back once: when the
// attach starts waiting for its saved catch-up, or when it ends.
func (c *bootProviderControl) holdHandshakeSlot(sess *session) func() {
	if c == nil || c.handshakeSlots == nil {
		return func() {}
	}
	c.handshakeSlots <- struct{}{}
	var once sync.Once
	release := func() { once.Do(func() { <-c.handshakeSlots }) }
	sess.holdHandshakeRelease(release)
	return release
}
