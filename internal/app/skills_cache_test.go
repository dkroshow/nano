package app

import (
	"context"
	"encoding/json"
	"github.com/dkroshow/nano/internal/setup"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSkillsListFanoutCollapsesFortySessionsByCwd(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.setReplyDelay("skills/list", 100*time.Millisecond)
	daemon := fake.daemon()
	cache := setup.NewSkillsListCache(func(ctx context.Context, cwd string) (json.RawMessage, error) {
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
	// Wait at the HTTP boundary until every workspace has its completed answer.
	deadline := time.Now().Add(5 * time.Second)
	for _, sess := range sessions {
		for {
			response := httptest.NewRecorder()
			sess.handleSkills(response, httptest.NewRequest("GET", "/skills", nil))
			if strings.Contains(response.Body.String(), `"name":"some-skill"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("skills cache did not publish all workspace answers")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

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
