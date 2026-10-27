package setup

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForSkillsCacheIdle(t *testing.T, cache *SkillsListCache, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cache.mu.Lock()
		entries, refreshing := len(cache.entries), false
		for _, entry := range cache.entries {
			refreshing = refreshing || entry.refreshing
		}
		cache.mu.Unlock()
		if entries == count && !refreshing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("skills cache did not settle: entries=%d refreshing=%v", entries, refreshing)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSkillsListCacheReturnsImmediatelyAndKeepsLastGoodAnswer(t *testing.T) {
	base := time.Date(2026, 9, 11, 14, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(base.UnixNano())
	var calls atomic.Int32
	var fetchMu sync.Mutex
	answer := json.RawMessage(`{"data":[{"name":"first"}]}`)
	fetchErr := error(nil)
	cache := NewSkillsListCache(func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		fetchMu.Lock()
		defer fetchMu.Unlock()
		return append(json.RawMessage(nil), answer...), fetchErr
	})
	cache.now = func() time.Time { return time.Unix(0, now.Load()) }
	cache.logf = func(string, ...any) {}
	cwd := t.TempDir()

	if got := string(cache.Current(cwd)); got != string(emptySkillsList) {
		t.Fatalf("cold cache = %s, want empty list while refresh runs", got)
	}
	waitForSkillsCacheIdle(t, cache, 1)
	if got := string(cache.Current(cwd)); got != string(answer) {
		t.Fatalf("first cached answer = %s, want %s", got, answer)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fresh cache calls = %d, want 1", got)
	}

	now.Store(base.Add(skillsListTTL + time.Second).UnixNano())
	fetchMu.Lock()
	fetchErr = errors.New("provider unavailable")
	fetchMu.Unlock()
	if got := string(cache.Current(cwd)); got != string(answer) {
		t.Fatalf("stale answer during failed refresh = %s, want %s", got, answer)
	}
	waitForSkillsCacheIdle(t, cache, 1)
	if got := string(cache.Current(cwd)); got != string(answer) {
		t.Fatalf("failed refresh replaced last good answer with %s", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("failed refresh calls = %d, want 2", got)
	}

	second := json.RawMessage(`{"data":[{"name":"second"}]}`)
	now.Store(base.Add(2*skillsListTTL + 2*time.Second).UnixNano())
	fetchMu.Lock()
	answer, fetchErr = second, nil
	fetchMu.Unlock()
	if got := string(cache.Current(cwd)); got != `{"data":[{"name":"first"}]}` {
		t.Fatalf("expired cache stopped serving last good answer: %s", got)
	}
	waitForSkillsCacheIdle(t, cache, 1)
	if got := string(cache.Current(cwd)); got != string(second) {
		t.Fatalf("refetched answer = %s, want %s", got, second)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("post-expiry calls = %d, want 3", got)
	}
}

func TestSkillsListCacheColdMissDoesNotWaitForProvider(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cache := NewSkillsListCache(func(context.Context, string) (json.RawMessage, error) {
		close(started)
		<-release
		return json.RawMessage(`{"data":[]}`), nil
	})

	if got := string(cache.Current(t.TempDir())); got != string(emptySkillsList) {
		t.Fatalf("cold cache = %s, want immediate empty list", got)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not start")
	}
	close(release)
}
