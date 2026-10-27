package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForSkillsCacheIdle(t *testing.T, cache *skillsListCache, count int) {
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

func TestSkillsListFanoutCollapsesFortySessionsByCwd(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.setReplyDelay("skills/list", 100*time.Millisecond)
	daemon := fake.daemon()
	cache := newSkillsListCache(func(ctx context.Context, cwd string) (json.RawMessage, error) {
		var result json.RawMessage
		err := daemon.Call(ctx, "skills/list", map[string]any{"cwds": []string{cwd}}, &result)
		return result, err
	})

	cwds := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	sessions := make([]*session, 40)
	for i := range sessions {
		sessions[i] = &session{
			id:       newSessionID(),
			provider: "codex",
			cwd:      cwds[i%len(cwds)],
			codex:    daemon,
			skills:   cache,
			state:    map[string]json.RawMessage{},
		}
	}

	var wg sync.WaitGroup
	for _, sess := range sessions {
		wg.Add(1)
		go func(sess *session) {
			defer wg.Done()
			response := httptest.NewRecorder()
			sess.handleSkills(response, httptest.NewRequest("GET", "/skills", nil))
			if response.Code != 200 {
				t.Errorf("skills response = %d, want 200", response.Code)
			}
		}(sess)
	}
	wg.Wait()
	waitForSkillsCacheIdle(t, cache, len(cwds))

	calls, maxInFlight, requested := fake.skillsListCalls()
	if calls != len(cwds) {
		t.Errorf("skills/list calls = %d, want %d distinct cwds", calls, len(cwds))
	}
	if maxInFlight > 2 {
		t.Errorf("skills/list max in flight = %d, want at most 2", maxInFlight)
	}
	sort.Strings(requested)
	sort.Strings(cwds)
	for i := range cwds {
		if i >= len(requested) || requested[i] != cwds[i] {
			t.Fatalf("skills/list cwds = %v, want %v", requested, cwds)
		}
	}

	for _, sess := range sessions {
		response := httptest.NewRecorder()
		sess.handleSkills(response, httptest.NewRequest("GET", "/skills", nil))
		if !strings.Contains(response.Body.String(), `"name":"some-skill"`) {
			t.Fatalf("cached skills read omitted the answer: %s", response.Body.String())
		}
	}
	if callsAfterHits, _, _ := fake.skillsListCalls(); callsAfterHits != len(cwds) {
		t.Errorf("cache hits made %d skills/list calls, want %d", callsAfterHits, len(cwds))
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
	cache := newSkillsListCache(func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		fetchMu.Lock()
		defer fetchMu.Unlock()
		return append(json.RawMessage(nil), answer...), fetchErr
	})
	cache.now = func() time.Time { return time.Unix(0, now.Load()) }
	cache.logf = func(string, ...any) {}
	cwd := t.TempDir()

	if got := string(cache.current(cwd)); got != string(emptySkillsList) {
		t.Fatalf("cold cache = %s, want empty list while refresh runs", got)
	}
	waitForSkillsCacheIdle(t, cache, 1)
	if got := string(cache.current(cwd)); got != string(answer) {
		t.Fatalf("first cached answer = %s, want %s", got, answer)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fresh cache calls = %d, want 1", got)
	}

	now.Store(base.Add(skillsListTTL + time.Second).UnixNano())
	fetchMu.Lock()
	fetchErr = errors.New("provider unavailable")
	fetchMu.Unlock()
	if got := string(cache.current(cwd)); got != string(answer) {
		t.Fatalf("stale answer during failed refresh = %s, want %s", got, answer)
	}
	waitForSkillsCacheIdle(t, cache, 1)
	if got := string(cache.current(cwd)); got != string(answer) {
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
	if got := string(cache.current(cwd)); got != `{"data":[{"name":"first"}]}` {
		t.Fatalf("expired cache stopped serving last good answer: %s", got)
	}
	waitForSkillsCacheIdle(t, cache, 1)
	if got := string(cache.current(cwd)); got != string(second) {
		t.Fatalf("refetched answer = %s, want %s", got, second)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("post-expiry calls = %d, want 3", got)
	}
}

func TestSkillsListCacheColdMissDoesNotWaitForProvider(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cache := newSkillsListCache(func(context.Context, string) (json.RawMessage, error) {
		close(started)
		<-release
		return json.RawMessage(`{"data":[]}`), nil
	})

	if got := string(cache.current(t.TempDir())); got != string(emptySkillsList) {
		t.Fatalf("cold cache = %s, want immediate empty list", got)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not start")
	}
	close(release)
}
