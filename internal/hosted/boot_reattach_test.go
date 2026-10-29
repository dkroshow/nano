package hosted

import (
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/testsupport"
	shimfixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
	codexfixture "github.com/dkroshow/nano/internal/testsupport/codexserver"
	"github.com/dkroshow/nano/internal/threadindex"
	shim "github.com/dkroshow/nano/internal/wire/claude"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A hidden thread's boot re-attach holds its saved file in the catalog's
// background queue, so the visible threads catch up first; a visible thread,
// and a hidden one the operator opened, hold theirs in front. Boundary:
// reattachStored through the session's catalog bind.
func TestBootReattachHoldsOnlyHiddenThreadsInTheBackground(t *testing.T) {
	daemon := codexfixture.NewServer(t)
	s, store := newBootCoordinator(t, "", daemon.WSURL())
	s.Restart = NewRestartTracker(filepath.Join(t.TempDir(), RestartsFileName), "test-stamp", "boot-test", time.Now())
	sources := &testsupport.Sources{}
	s.CatalogSources = sources
	records := map[string]hostedstore.Attachment{}
	for _, native := range []string{"visible", "hidden", "opened"} {
		rec := bootCodexRecord(t)
		rec.NativeThreadID, rec.HostedThreadID = native, "nht_"+native
		store.Remember(rec)
		records[native] = rec
	}
	seedBootAttachments(t, s)
	s.PrepareBoot()
	s.Restart.rank(map[hostedstore.AttachmentKey]sidebar.BootRank{records["visible"].Key(): {Tier: 0, Visible: true}}, s.boot.order)

	s.reattachStored(records["visible"], false)
	s.reattachStored(records["hidden"], false)
	s.reattachStored(records["opened"], true)

	background := map[string]bool{}
	for _, bind := range sources.Binds {
		for native := range records {
			if strings.HasPrefix(bind, "codex/"+native+"@") {
				background[native] = strings.HasSuffix(bind, "|background")
			}
		}
	}
	if len(background) != 3 || background["visible"] || !background["hidden"] || background["opened"] {
		t.Fatalf("background binds = %v from %q; want only the hidden boot thread", background, sources.Binds)
	}
}

// Ordinary boot notices coalesce without losing refresh, while an opened
// thread must appear before its deliberately delayed provider handshake.
func TestBootReattachRuntimeGateKeepsRefreshAndPrioritizedRegistration(t *testing.T) {
	s, daemon, rec := startingCodexCoordinator(t)
	daemon.SetReplyDelay("thread/resume", 10*time.Second)
	updates := s.events.Subscribe()
	defer s.events.Unsubscribe(updates)
	s.publishRuntime(false)
	<-updates
	// The prepared starting thread is opened while the boot publisher is active.
	s.boot.mu.Lock()
	s.boot.runtimeActive = true
	s.boot.mu.Unlock()
	s.NotifyRuntimeRefresh()
	s.NotifyRuntime()
	select {
	case delta := <-updates:
		t.Fatalf("ordinary boot notice published immediately: %+v", delta)
	default:
	}

	s.reattachWaitingHosted(rec.HostedThreadID)
	select {
	case delta := <-updates:
		attached := false
		for _, row := range delta.Rows {
			if row.Provider == rec.Provider && row.NativeID == rec.NativeThreadID && row.Runtime != nil && row.Runtime.Attached && row.Runtime.Status == "starting" {
				attached = true
			}
		}
		if !attached {
			t.Fatalf("prioritized thread missing before its resume reply: %+v", delta)
		}
	default:
		t.Fatal("prioritized registration did not publish immediately during boot")
	}
	s.publishBootRuntime(false)
	select {
	case delta := <-updates:
		if !delta.Refresh {
			t.Fatal("coalesced publish lost the refresh requested before priority registration")
		}
	default:
		t.Fatal("boot publisher lost its queued notice")
	}
	s.publishBootRuntime(false)
	select {
	case delta := <-updates:
		t.Fatalf("clean boot publisher emitted another snapshot: %+v", delta)
	default:
	}
	s.publishBootRuntime(true)
}

// One provider's held pre-claim used to hold every provider's boot: a Codex
// daemon that accepts connections but never answers keeps its mutex during
// the dial, so the next Codex thread's pre-claim waited while holding the one
// resume lock, and every Claude and Grok thread queued behind it waited too
// (about 60 s in the runner's degraded check). Each provider now has its own
// boot queue and resume lock. Boundary: the real boot loop while the Codex
// resume lock is held, with Codex first in the stored order.
func TestBootReattachStuckProviderDoesNotHoldAnother(t *testing.T) {
	shim := shimfixture.NewServer(t, bootResumeScript(func(c *shimfixture.Conn, call shimfixture.Call) {
		c.Reply(call, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 0})
		c.Notify(shim.MethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 0})
	}))
	daemon := codexfixture.NewServer(t)
	s, store := newBootCoordinator(t, shim.WSURL(), daemon.WSURL())
	codex := bootCodexRecord(t)
	codex.AttachedAt = "2026-07-28T10:00:00Z"
	store.Remember(codex)
	claude := rememberedClaudeSession(t, "/stored/claude")
	claude.AttachedAt = "2026-07-28T10:09:00Z" // stored order: the Codex thread first
	store.Remember(claude)
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	var logs testsupport.LogBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })

	codexLock := s.resumeSpawnLock("codex")
	codexLock.Lock() // a Codex pre-claim that cannot finish
	done := make(chan struct{})
	go func() {
		s.ReattachOnBoot()
		close(done)
	}()
	shimfixture.WaitFor(t, "the Claude thread's registration while Codex is held", func() bool {
		return strings.Contains(logs.String(), "boot re-attach claude/"+claudeResumeThreadID+" registered")
	})
	if strings.Contains(logs.String(), "boot re-attach codex/") {
		t.Fatal("the Codex thread registered while its resume lock was held")
	}
	codexLock.Unlock()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("boot did not finish once the Codex lock was released")
	}
}

// Failure caught: two top-level threads with the same updated time kept their
// stored order, so a visible one could follow a hidden one and wait with the
// hidden threads for every queue's visible registrations. Boundary: bootOrder.
func TestBootOrderPutsVisibleThreadsBeforeTiedHiddenOnes(t *testing.T) {
	hidden := hostedstore.Attachment{Provider: "codex", NativeThreadID: "hidden"}
	Visible := hostedstore.Attachment{Provider: "codex", NativeThreadID: "visible"}
	ranks := map[hostedstore.AttachmentKey]sidebar.BootRank{
		hidden.Key():  {Tier: 1},
		Visible.Key(): {Tier: 1, Visible: true},
	}
	if got := bootOrder([]hostedstore.Attachment{hidden, Visible}, ranks); got[0].NativeThreadID != "visible" {
		t.Fatalf("boot order = %s, %s; want the visible thread first", got[0].NativeThreadID, got[1].NativeThreadID)
	}
}

// Failure caught: one provider's queue reached its hidden threads while
// another provider still registered its visible ones. At boot Grok's single
// visible thread registered at once and its 31 hidden replays then competed
// with the visible Claude and Codex registrations. The boot loop queues only
// threads with open work, so every thread here has a turn open. Boundary: the
// boot loop, across provider queues.
func TestBootReattachHoldsHiddenThreadsUntilEveryVisibleOneRegisters(t *testing.T) {
	shim := shimfixture.NewServer(t, bootResumeScript(func(c *shimfixture.Conn, call shimfixture.Call) {
		c.Reply(call, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 0})
		c.Notify(shim.MethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 0})
	}))
	daemon := codexfixture.NewServer(t)
	s, store := newBootCoordinator(t, shim.WSURL(), daemon.WSURL())
	for _, native := range []string{"codex-visible", "codex-hidden"} {
		rec := bootCodexRecord(t)
		rec.NativeThreadID, rec.HostedThreadID = native, "nht_"+native
		store.Remember(rec)
	}
	store.Remember(rememberedClaudeSession(t, "/stored/claude"))
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.PrepareBoot()
	now := time.Now().UTC()
	Updated := now.Format(time.RFC3339Nano)
	threads := []sidebar.Thread{
		{Thread: threadindex.Thread{ID: "nth_codex_visible", Provider: "codex", NativeID: "codex-visible", UpdatedAt: &Updated}, Starred: true},
		{Thread: threadindex.Thread{ID: "nth_claude_visible", Provider: "claude", NativeID: claudeResumeThreadID, UpdatedAt: &Updated}, Starred: true},
	}
	materializeBootSidebar(t, s, threads, now)
	var logs testsupport.LogBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })
	registered := func(key string) bool { return strings.Contains(logs.String(), "boot re-attach "+key+" registered") }

	claudeLock := s.resumeSpawnLock("claude")
	claudeLock.Lock() // the visible Claude thread cannot register yet
	done := make(chan struct{})
	go func() {
		s.ReattachOnBoot()
		close(done)
	}()
	shimfixture.WaitFor(t, "the visible Codex thread's registration", func() bool { return registered("codex/codex-visible") })
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if registered("codex/codex-hidden") {
			claudeLock.Unlock()
			t.Fatal("the hidden Codex thread registered while the visible Claude thread waited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	claudeLock.Unlock()
	shimfixture.WaitFor(t, "the hidden Codex thread's registration once Claude's visible one registered", func() bool {
		return registered("claude/"+claudeResumeThreadID) && registered("codex/codex-hidden")
	})
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("boot did not finish")
	}
}
