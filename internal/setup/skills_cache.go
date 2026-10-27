package setup

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"sync"
	"time"
)

const (
	skillsListTTL     = 10 * time.Minute
	skillsListTimeout = 10 * time.Second
	skillsListWorkers = 2
)

var emptySkillsList = json.RawMessage(`{"data":[]}`)

type skillsListEntry struct {
	value      json.RawMessage
	checkedAt  time.Time
	refreshing bool
}

// SkillsListCache keeps the app-server's workspace-scoped skills answer off
// the session state path. A miss starts one background refresh per cwd while
// every caller immediately receives the last good answer (or an empty list).
type SkillsListCache struct {
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time
	fetch   func(context.Context, string) (json.RawMessage, error)
	logf    func(string, ...any)
	slots   chan struct{}

	mu      sync.Mutex
	entries map[string]*skillsListEntry
}

// NewSkillsListCache creates a cache using the supplied provider read function.
func NewSkillsListCache(fetch func(context.Context, string) (json.RawMessage, error)) *SkillsListCache {
	return &SkillsListCache{
		ttl: skillsListTTL, timeout: skillsListTimeout, now: time.Now,
		fetch: fetch, logf: log.Printf,
		slots: make(chan struct{}, skillsListWorkers), entries: map[string]*skillsListEntry{},
	}
}

// Current returns the cached answer and starts a bounded refresh when stale.
func (c *SkillsListCache) Current(cwd string) json.RawMessage {
	if c == nil || c.fetch == nil || cwd == "" {
		return append(json.RawMessage(nil), emptySkillsList...)
	}
	cwd = filepath.Clean(cwd)
	now := c.now()

	c.mu.Lock()
	entry := c.entries[cwd]
	if entry == nil {
		entry = &skillsListEntry{}
		c.entries[cwd] = entry
	}
	value := entry.value
	fresh := !entry.checkedAt.IsZero() && now.Sub(entry.checkedAt) < c.ttl
	if !fresh && !entry.refreshing {
		entry.refreshing = true
		go c.refresh(cwd)
	}
	c.mu.Unlock()

	if len(value) == 0 {
		value = emptySkillsList
	}
	return append(json.RawMessage(nil), value...)
}

func (c *SkillsListCache) refresh(cwd string) {
	c.slots <- struct{}{}
	defer func() { <-c.slots }()

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	value, err := c.fetch(ctx, cwd)
	cancel()

	c.mu.Lock()
	entry := c.entries[cwd]
	if entry == nil {
		entry = &skillsListEntry{}
		c.entries[cwd] = entry
	}
	entry.refreshing = false
	entry.checkedAt = c.now()
	hadGood := len(entry.value) != 0
	if err == nil && len(value) != 0 {
		entry.value = append(json.RawMessage(nil), value...)
	}
	c.mu.Unlock()

	if err != nil {
		fallback := "empty list"
		if hadGood {
			fallback = "last good answer"
		}
		c.logf("skills/list cwd %q: %v; serving %s", cwd, err, fallback)
	} else if len(value) == 0 {
		c.logf("skills/list cwd %q: empty response; serving last good answer", cwd)
	}
}
