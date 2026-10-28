package app

import (
	"sync"
)

// pausedAttachedCatalog controls the readiness fact consumed by sessions without controlling the index's private worker.
type pausedAttachedCatalog struct {
	*threadCatalogService
	mu     sync.Mutex
	paused chan struct{}
}

func (c *pausedAttachedCatalog) pause() func() {
	c.mu.Lock()
	c.paused = make(chan struct{})
	ch := c.paused
	c.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); c.paused = nil; close(ch); c.mu.Unlock() }) }
}
func (c *pausedAttachedCatalog) AwaitAttachedCurrent(owner string, fresh bool, neverPrompted func() bool, cancel <-chan struct{}) bool {
	c.mu.Lock()
	paused := c.paused
	c.mu.Unlock()
	if paused != nil {
		select {
		case <-paused:
		case <-cancel:
			return false
		}
	}
	return c.threadCatalogService.AwaitAttachedCurrent(owner, fresh, neverPrompted, cancel)
}
