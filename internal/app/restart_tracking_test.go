package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

// A restart used to leave only a log line with its total time, so nobody could
// tell which step or which thread was slow. Each boot must write one record
// with every thread's steps, its sidebar place and its failure reason, and
// serve it through the API. Boundary: the real boot loop and Codex handshake
// against a fake app server, the written restarts.jsonl line, and
// GET /api/v1/restarts.
func TestRestartRecordTracksEveryThreadOfABoot(t *testing.T) {
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, "", daemon.WSURL())
	path := filepath.Join(t.TempDir(), hostedmodel.RestartsFileName)
	s.restart = hostedmodel.NewRestartTracker(path, "test-stamp", "boot-test", time.Now())
	for _, native := range []string{"starred", "plain", "forgotten"} {
		rec := bootCodexRecord(t)
		rec.NativeThreadID, rec.HostedThreadID = native, "nht_"+native
		store.Remember(rec)
	}
	seedBootAttachments(t, s)
	// The boot loop queues only threads with open work.
	for _, hosted := range []string{"nht_starred", "nht_plain", "nht_forgotten"} {
		if _, _, err := fixtureBeginTurn(s.agentExecutions, hosted, "nano:test:open-turn", "hash"); err != nil {
			t.Fatal(err)
		}
	}
	s.attachmentCoordinator().PrepareBoot()
	now := time.Now().UTC()
	updated := now.Format(time.RFC3339Nano)
	threads := []sidebar.Thread{{Thread: threadindex.Thread{ID: "nth_starred", Provider: "codex", NativeID: "starred", UpdatedAt: &updated}, HostedThreadID: "nht_starred", Starred: true}}
	materializeBootSidebar(t, s, threads, now)
	s.restart.MarkHTTPOpen()
	s.attachments.Forget("codex", "forgotten")

	s.attachmentCoordinator().ReattachOnBoot()

	var record hostedmodel.RestartRecord
	waitForClaude(t, "the written restart record", func() bool {
		data, err := os.ReadFile(path)
		return err == nil && json.Unmarshal(data, &record) == nil
	})
	if !record.Complete || record.ThreadsTotal != 3 || record.Live != 2 || record.Failed != 1 || record.Pending != 0 || record.Visible != 1 {
		t.Fatalf("restart summary = %+v", record)
	}
	if record.HTTPOpenMs == nil || record.BootLoopMs == nil || record.VisibleLiveMs == nil || record.AllSettledMs == nil {
		t.Fatalf("restart times missing: %+v", record)
	}
	// A failed thread must not let the restart claim every thread went live.
	if record.AllLiveMs != nil {
		t.Fatalf("all_live_ms = %d with a failed thread", *record.AllLiveMs)
	}
	byNative := map[string]hostedmodel.RestartThreadRecord{}
	for _, th := range record.Threads {
		byNative[th.NativeThreadID] = th
	}
	starred := byNative["starred"]
	if !starred.Visible || starred.Tier != "starred" || *record.VisibleLiveMs != *starred.LiveMs {
		t.Fatalf("starred thread = %+v, visible live %d", starred, *record.VisibleLiveMs)
	}
	for _, native := range []string{"starred", "plain"} {
		th := byNative[native]
		for name, at := range map[string]*int64{"picked": th.PickedMs, "lock": th.LockMs, "registered": th.RegisteredMs, "attached": th.AttachedMs, "live": th.LiveMs} {
			if at == nil {
				t.Fatalf("%s thread has no %s time: %+v", native, name, th)
			}
		}
		if th.FailedMs != nil || th.Reason != "" {
			t.Fatalf("%s thread failed: %+v", native, th)
		}
	}
	if plain := byNative["plain"]; plain.Visible || plain.Tier != "unseen" {
		t.Fatalf("thread outside the sidebar = %+v", plain)
	}
	if forgotten := byNative["forgotten"]; forgotten.FailedMs == nil || forgotten.Reason != "no_longer_stored" || forgotten.LiveMs != nil {
		t.Fatalf("forgotten thread = %+v", forgotten)
	}
	if codex := record.ByProvider["codex"]; codex.Threads != 3 || codex.Live != 2 || codex.Failed != 1 || codex.Ms["reconnect"].N != 2 {
		t.Fatalf("codex totals = %+v", codex)
	}

	for query, wantThreads := range map[string]int{"": 0, "?threads=1": 3} {
		rec := httptest.NewRecorder()
		s.restart.HandleRestarts(rec, httptest.NewRequest(http.MethodGet, "/api/v1/restarts"+query, nil))
		var body struct{ Restarts []hostedmodel.RestartRecord }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Restarts) != 1 {
			t.Fatalf("GET restarts%s = %d %s", query, rec.Code, rec.Body.String())
		}
		if got := body.Restarts[0]; got.RestartID != record.RestartID || len(got.Threads) != wantThreads {
			t.Fatalf("GET restarts%s = %s, %d threads", query, got.RestartID, len(got.Threads))
		}
	}
	if strings.Count(strings.TrimSpace(string(mustRead(t, path))), "\n") != 0 {
		t.Fatal("one boot wrote more than one restart line")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
