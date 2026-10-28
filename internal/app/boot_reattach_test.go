package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/httpapi/remoteauth"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Boot re-attach: a nano restart resumes every stored attachment by itself
// through the normal resume ladder (boot_reattach.go). These tests drive
// reattachOnBoot directly — main only wraps it in a goroutine.

// bootReattachServer is claudeResumeServer plus a codex daemon, so one boot
// can walk records of both providers.
func bootReattachServer(t *testing.T, shimURL, codexURL string) (*server, *attachmentStore) {
	t.Helper()
	s, store := claudeResumeServer(t, shimURL)
	if codexURL != "" {
		s.codex = newCodexDaemon(codexURL, supervisedDaemonLabel)
		s.codex.supervisedJobLoaded = func() bool { return true }
	}
	return s, store
}

// openTurnsForBoot gives every stored thread an open turn, so the boot loop
// queues each one whatever the sidebar shows; tests of the loop's own limits
// use it.
func openTurnsForBoot(t *testing.T, s *server) {
	t.Helper()
	for _, record := range s.attachments.records() {
		if _, _, err := s.agentExecutions.beginTurn(record.HostedThreadID, "nano:test:open-turn", "hash"); err != nil {
			t.Fatal(err)
		}
	}
}

func seedBootAttachments(t *testing.T, s *server) {
	t.Helper()
	for _, record := range s.attachments.records() {
		if record.HostedThreadID == "" {
			record.HostedThreadID = "nht_boot_" + agentHash([]byte(record.Provider + "\x00" + record.NativeThreadID))[:16]
			s.attachments.remember(record)
		}
		ensureClaudeTestSpool(t, s, record.Provider, record.HostedThreadID)
		if _, err := s.agentExecutions.ensureSessionWithIdentity(record.HostedThreadID, record.Provider, presentationShared, record.InvokerHostedThreadID, record.InvokerNativeThreadID, record.InvokerProvider); err != nil {
			t.Fatal(err)
		}
		if err := s.controlMetadata.claimNativeID(record.HostedThreadID, record.Provider, record.NativeThreadID, record.provisional()); err != nil {
			t.Fatal(err)
		}
		generation, err := s.controlMetadata.beginTransportBinding(record.HostedThreadID)
		if err != nil {
			t.Fatal(err)
		}
		fixtureCwd := record.Cwd
		if fixtureCwd == "" {
			// The JSON record is transport-only after the cutover. Test setup
			// supplies a canonical cwd explicitly rather than restoring a read.
			fixtureCwd = s.defaults.cwd
		}
		fixtureModel := record.Model
		if fixtureModel == "" {
			// These record factories represent the legacy file before startup
			// migration. remember() has already applied the post-cutover scrub.
			if record.Provider == "codex" {
				fixtureModel = "gpt-test"
			} else if record.Provider == "claude" {
				fixtureModel = "sonnet"
			}
		}
		_, fixturePermission, _ := argvStripPair(record.Args, "--permission-mode")
		for _, fact := range []postureFieldValue{{field: "cwd", value: fixtureCwd}, {field: postureFieldModel, value: fixtureModel}, {field: postureFieldPermissionMode, value: fixturePermission}} {
			if fact.value == "" {
				continue
			}
			key := fmt.Sprintf("boot-fixture:%s:%d:%s:%s", record.HostedThreadID, generation, fact.field, fact.value)
			if _, err := s.controlMetadata.appendEvent(metadataEvent{HostedThreadID: record.HostedThreadID, Field: fact.field, Value: fact.value, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, AttachmentGeneration: generation, SourceEventKey: key}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func bootCodexRecord(t *testing.T) attachmentRecord {
	t.Helper()
	return attachmentRecord{
		Provider:       "codex",
		NativeThreadID: "codex-thread-1",
		Cwd:            t.TempDir(),
		Model:          "gpt-test",
		AttachedAt:     "2026-07-28T10:00:00Z",
	}
}

// A claude record whose shim child is still alive comes back attached to it —
// the same rung-1 path a sidebar click takes — with the record retained.
func TestBootReattachResumesClaudeThroughLiveShim(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "turnActive": true, "lastSeq": 1,
		})
		c.line(1, "recv", claudeInitLine)
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
	}))
	s, store := bootReattachServer(t, shim.wsURL(), "")
	rec := rememberedClaudeSession(t, "/stored/claude")
	rec.HostedThreadID = "nht_worker"
	rec.InvokerHostedThreadID = "nht_parent"
	rec.InvokerNativeThreadID = "native-parent"
	rec.InvokerProvider = "codex"
	store.remember(rec)

	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.reattachOnBoot()

	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	if len(sessions) != 1 {
		t.Fatalf("sessions after boot = %d, want 1", len(sessions))
	}
	sum := waitForSessionStatus(t, s, sessions[0].id, "ready")
	if sum.Provider != "claude" || sum.ThreadID != claudeResumeThreadID || sum.Status != "ready" ||
		sum.InvokerHostedThreadID != "nht_parent" || sum.InvokerProvider != "codex" {
		t.Fatalf("summary = %#v, want the shim thread attached and ready", sum)
	}
	waitForClaude(t, "boot re-attach to restore the shim's active turn", func() bool {
		return sessions[0].summary().TurnActive
	})
	if attaches := shim.callsOf(shimMethodAttach); len(attaches) != 1 {
		t.Fatalf("session/attach calls = %d, want 1", len(attaches))
	}
	if starts := shim.callsOf(shimMethodStart); len(starts) != 0 {
		t.Fatalf("boot re-attach spawned a child: %+v", starts)
	}
	remembered, ok := store.get("claude", claudeResumeThreadID)
	if !ok {
		t.Fatal("boot re-attach dropped the record it attached to")
	}
	if remembered.InvokerHostedThreadID != "nht_parent" || remembered.InvokerNativeThreadID != "native-parent" {
		t.Fatalf("boot re-attach lost invocation edge: %#v", remembered)
	}
}

// Process liveness and turn activity are separate shim facts. An idle child is
// still state=running because it accepts another prompt; boot re-attach must
// not turn that into a false working indicator.
func TestBootReattachKeepsRunningButIdleClaudeSessionIdle(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-idle", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "turnActive": false, "lastSeq": 2,
		})
		c.line(1, "send", `{"type":"user","message":{"role":"user","content":"old"}}`)
		c.line(2, "recv", `{"type":"result","subtype":"success"}`)
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-idle", "lastSeq": 2})
	}))
	s, store := bootReattachServer(t, shim.wsURL(), "")
	rec := rememberedClaudeSession(t, "/stored/claude")
	store.remember(rec)

	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.reattachOnBoot()
	s.mu.Lock()
	var sess *session
	for _, candidate := range s.sessions {
		sess = candidate
	}
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("boot did not register the idle Claude session")
	}
	if sum := waitForSessionStatus(t, s, sess.id, "ready"); sum.TurnActive {
		t.Fatalf("idle running shim child became working: %+v", sum)
	}
}

// An idle thread whose spool ends in a finished turn needs nothing replayed:
// its content is saved history. The stored turn anchor, stale or not, plays
// no part in the floor.
func TestBootReattachUsesRetainedClaudeHistorySeam(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-offset", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "turnActive": false, "lastSeq": 8,
		})
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-offset", "lastSeq": 8})
	}))
	s, store := bootReattachServer(t, shim.wsURL(), "")
	record := rememberedClaudeSession(t, "/stored/claude")
	record.TurnStartedAtMs, record.TurnAnchorSeq = 1789766804441, 2 // stale anchor
	store.remember(record)
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	stored, _ := s.attachmentFor("claude", claudeResumeThreadID)
	writeClaudeTestSpool(t, s.defaults.claudeShimSpools, stored.HostedThreadID,
		recvFrame(1, claudeInitLine), sendFrame(2, testUserPrompt), recvFrame(3, messageStart("a")),
		recvFrame(4, textDelta(0, "done")), recvFrame(5, testMessageStop), recvFrame(6, `{"type":"assistant","message":{"id":"msg_a","content":[]}}`),
		recvFrame(7, `{"type":"system","subtype":"status"}`), recvFrame(8, claudeReplayResultLine))

	s.reattachOnBoot()
	var sess *session
	waitForClaude(t, "retained-history Claude attach", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, candidate := range s.sessions {
			sess = candidate
		}
		return sess != nil && sess.summary().Status == "ready"
	})
	var params struct {
		FromSeq int64 `json:"fromSeq"`
	}
	calls := shim.callsOf(shimMethodAttach)
	if len(calls) != 1 || json.Unmarshal(calls[0].Params, &params) != nil || params.FromSeq != 8 {
		t.Fatalf("session/attach calls = %+v, params = %+v; want the finished spool's end", calls, params)
	}
}

// A fresh Claude child has no native id until its first prompt. If Nano
// restarts in that window, the provisional attachment must carry the New
// Window title without prematurely writing it against the shim handle.
func TestBootReattachPreservesProvisionalClaudeNameUntilRealBind(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-pending", "nativeSessionId": "",
			"state": "running", "lastSeq": 0,
		})
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-pending", "lastSeq": 0})
	}))
	s, store := bootReattachServer(t, shim.wsURL(), "")
	store.remember(attachmentRecord{
		Provider: "claude", NativeThreadID: "shim-pending", ShimSessionID: "shim-pending",
		Cwd: t.TempDir(), Bin: "/stored/claude", PendingName: "nano api fable",
	})

	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.reattachOnBoot()
	s.mu.Lock()
	var sess *session
	for _, candidate := range s.sessions {
		sess = candidate
	}
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("boot did not register the provisional Claude session")
	}
	waitForSessionStatus(t, s, sess.id, "ready")
	if sum := sess.summary(); sum.Name != "nano api fable" {
		t.Fatalf("summary name = %q, want restored pending title", sum.Name)
	}
	sess.stateMu.Lock()
	pending := sess.pendingName
	sess.stateMu.Unlock()
	if pending != "nano api fable" {
		t.Fatalf("pending name = %q, want it held for the real native id", pending)
	}

	var namedID, namedValue string
	sess.nameThread = func(_ string, nativeID string, name string) {
		namedID, namedValue = nativeID, name
	}
	sess.applyPendingName("native-real")
	if namedID != "native-real" || namedValue != "nano api fable" {
		t.Fatalf("real bind named %q as %q", namedID, namedValue)
	}
}

// Older attachment records can lack PendingName even though the hosted
// creation request supplied one. Boot must reload the canonical creation fact
// before the resume bind and must still preserve an operator's later rename.
func TestBootReattachReloadsCanonicalCreationNameWhenAttachmentDroppedIt(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-nameless", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "lastSeq": 0,
		})
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-nameless", "lastSeq": 0})
	}))
	s, store := bootReattachServer(t, shim.wsURL(), "")
	rec := rememberedClaudeSession(t, "/stored/claude")
	rec.HostedThreadID = "nht_creation_name_boot"
	rec.PendingName = ""
	store.remember(rec)
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	if err := s.controlMetadata.recordCreationName(rec.HostedThreadID, "creation survives boot"); err != nil {
		t.Fatal(err)
	}
	catalog := &fakeThreadCatalog{names: map[string]string{}}
	s.catalog = catalog

	s.reattachOnBoot()
	s.mu.Lock()
	var sess *session
	for _, candidate := range s.sessions {
		sess = candidate
	}
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("boot did not register the named Claude session")
	}
	waitForSessionStatus(t, s, sess.id, "ready")
	key := "claude\x00" + claudeResumeThreadID
	if got := catalog.names[key]; got != "creation survives boot" {
		t.Fatalf("boot bind name = %q, want canonical creation name", got)
	}

	catalog.names[key] = "operator rename"
	sess.applyPendingName(claudeResumeThreadID)
	if got := catalog.names[key]; got != "operator rename" {
		t.Fatalf("later bind overwrote operator rename with %q", got)
	}
}

// A provisional thread can be renamed after creation but before its real
// native id arrives. Boot must keep that pending operator choice separate from
// the immutable creation fact: pending wins at bind and creation stays intact.
func TestBootReattachPendingRenameDoesNotBecomeCreationName(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-renamed", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "lastSeq": 0,
		})
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-renamed", "lastSeq": 0})
	}))
	s, store := bootReattachServer(t, shim.wsURL(), "")
	rec := rememberedClaudeSession(t, "/stored/claude")
	rec.HostedThreadID = "nht_creation_then_rename"
	rec.ShimSessionID = "shim-renamed"
	rec.PendingName = "operator rename"
	store.remember(rec)
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	if err := s.controlMetadata.recordCreationName(rec.HostedThreadID, "creation name"); err != nil {
		t.Fatal(err)
	}
	catalog := &fakeThreadCatalog{names: map[string]string{}}
	s.catalog = catalog

	s.reattachOnBoot()
	s.mu.Lock()
	var sess *session
	for _, candidate := range s.sessions {
		sess = candidate
	}
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("boot left renamed thread detached")
	}
	waitForSessionStatus(t, s, sess.id, "ready")
	key := "claude\x00" + claudeResumeThreadID
	if got := catalog.names[key]; got != "operator rename" {
		t.Fatalf("boot bind name = %q, want pending operator rename", got)
	}
	if got, ok := s.controlMetadata.creationName(rec.HostedThreadID); !ok || got != "creation name" {
		t.Fatalf("creation fact = %q, %v; pending rename must not rewrite it", got, ok)
	}
}

// A codex record re-binds to the app-server daemon's thread via thread/resume.
func TestBootReattachResumesCodexThread(t *testing.T) {
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, "", daemon.wsURL())
	rec := bootCodexRecord(t)
	store.remember(rec)

	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.reattachOnBoot()

	s.mu.Lock()
	count := len(s.sessions)
	s.mu.Unlock()
	if count != 1 {
		t.Fatalf("sessions after boot = %d, want 1", count)
	}
	waitForClaude(t, "thread/resume call", func() bool { return daemon.resumeCount() == 1 })
}

// A hidden thread's boot re-attach holds its saved file in the catalog's
// background queue, so the visible threads catch up first; a visible thread,
// and a hidden one the operator opened, hold theirs in front. Boundary:
// reattachStored through the session's catalog bind.
func TestBootReattachHoldsOnlyHiddenThreadsInTheBackground(t *testing.T) {
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, "", daemon.wsURL())
	s.restart = newRestartTracker(filepath.Join(t.TempDir(), restartsFileName), "test-stamp", "boot-test", time.Now())
	sources := &recordingSources{}
	s.catalogSources = sources
	records := map[string]attachmentRecord{}
	for _, native := range []string{"visible", "hidden", "opened"} {
		rec := bootCodexRecord(t)
		rec.NativeThreadID, rec.HostedThreadID = native, "nht_"+native
		store.remember(rec)
		records[native] = rec
	}
	seedBootAttachments(t, s)
	s.prepareBootReattach()
	s.restart.rank(map[attachmentKey]bootRank{records["visible"].key(): {tier: bootTierStarred, visible: true}}, s.boot.order)

	s.reattachStored(records["visible"], false)
	s.reattachStored(records["hidden"], false)
	s.reattachStored(records["opened"], true)

	background := map[string]bool{}
	for _, bind := range sources.binds {
		for native := range records {
			if strings.HasPrefix(bind, "codex/"+native+"@") {
				background[native] = strings.HasSuffix(bind, "|background")
			}
		}
	}
	if len(background) != 3 || background["visible"] || !background["hidden"] || background["opened"] {
		t.Fatalf("background binds = %v from %q; want only the hidden boot thread", background, sources.binds)
	}
}

// The app-server can refuse a boot resume while its own listener for the
// thread is still closing ("thread listener command channel is closed"), and
// a later resume succeeds. Boot must retry it through session recovery's
// bounded backoff instead of leaving the thread exited until the next deploy.
// If the refusal outlasts every retry, the thread parks; a prompt then re-arms
// it and is delivered exactly once, never left waiting on a parked thread.
func TestBootReattachRetriesCodexResumeRefusedByAClosedListener(t *testing.T) {
	closedListener := "failed to enqueue running thread resume for thread %s: thread listener command channel is closed"
	// Seen at a 2026-10-09 boot: the app server still closed the thread.
	closing := "thread %s is closing; retry thread/resume after the thread is closed"
	for _, tc := range []struct {
		name     string
		refusal  string
		refusals int
	}{
		{"clears on retry", closedListener, 1},
		{"outlasts the retries", closedListener, 1 + codexReconnectAttempts},
		{"closing thread clears on retry", closing, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newFakeAppServer(t)
			daemon.setTurnStartResult(map[string]any{"turn": map[string]any{"id": "after-refusal"}})
			s, store := bootReattachServer(t, "", daemon.wsURL())
			s.codex.reconnectBackoff = time.Millisecond
			rec := bootCodexRecord(t)
			store.remember(rec)
			refusals := make([]string, tc.refusals)
			for i := range refusals {
				refusals[i] = fmt.Sprintf(tc.refusal, rec.NativeThreadID)
			}
			daemon.setResumeErrorSequence(refusals...)

			seedBootAttachments(t, s)
			openTurnsForBoot(t, s)
			s.reattachOnBoot()

			s.mu.Lock()
			var sess *session
			for _, candidate := range s.sessions {
				sess = candidate
			}
			s.mu.Unlock()
			if sess == nil {
				t.Fatal("boot registered no session")
			}
			if tc.refusals > 1 {
				waitForSessionStatus(t, s, sess.id, "reconnect_parked")
				for repeat := 0; repeat < 2; repeat++ {
					if response := postTurn(t, sess, context.Background(), "after-parking", "prompt to a parked thread"); response.Code != http.StatusAccepted && response.Code != http.StatusOK {
						t.Fatalf("parked prompt: %d %s", response.Code, response.Body.String())
					}
				}
				waitTurnSendDone(t, sess, "after-parking")
			}
			waitForSessionStatus(t, s, sess.id, "ready")
			if got := daemon.resumeCount(); got != tc.refusals+1 {
				t.Fatalf("thread/resume calls = %d, want %d refused and one that succeeds", got, tc.refusals)
			}
			daemon.mu.Lock()
			starts := daemon.turnStarts
			daemon.mu.Unlock()
			if want := map[bool]int{true: 1, false: 0}[tc.refusals > 1]; starts != want {
				t.Fatalf("turn/start calls = %d, want %d: the parked prompt is delivered once", starts, want)
			}
		})
	}
}

// A restart reconnects only threads with work not yet settled; every other
// stored thread rests until something needs it. Every stored thread used to
// reconnect at boot, hundreds of hidden subagents included, so the boot loop
// ran for minutes. Failure caught: a thread with open work of any kind (an
// operation, a queued browser prompt, a provider turn started and not
// finished) left resting, so its work
// never settles; a thread the sidebar shows (starred, or a visible top-level
// row) left waiting for a browser to ask, so what the operator sees comes back
// late; an idle hidden subagent reconnected at boot; a prompt or an idle-only
// wake to a resting thread refused or lost. Boundary: the boot loop over the
// stores and the materialized sidebar, then the hosted turn API.
func TestBootReattachRegistersOpenWorkAndRestsTheRest(t *testing.T) {
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, "", daemon.wsURL())
	s.completedTurns = newCompletedTurnSource(&completedTurnStore{db: s.queuedTurns.db})
	now := time.Now().UTC()
	natives := []string{"starred-idle", "root-idle", "child-idle", "child-wake", "child-quiet", "operation-open", "prompt-queued", "provider-turn-open"}
	for i, native := range natives {
		rec := bootCodexRecord(t)
		rec.NativeThreadID, rec.HostedThreadID = native, "nht_"+native
		if strings.HasPrefix(native, "child-") {
			rec.InvokerHostedThreadID, rec.InvokerNativeThreadID, rec.InvokerProvider = "nht_root-idle", "root-idle", "codex"
		}
		rec.AttachedAt = now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339)
		store.remember(rec)
	}
	seedBootAttachments(t, s)
	ctx := context.Background()
	if _, _, err := s.agentExecutions.beginTurn("nht_operation-open", "nano:test:open-turn", "hash"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.turns.Submit(ctx, turns.Submission{
		HostedThreadID: "nht_prompt-queued", Provider: model.ProviderCodex, NativeThreadID: "prompt-queued", ActionID: "browser-queued",
		ProviderText: "queued in the browser", PresentationText: "queued in the browser", Origin: turns.HumanOrigin,
		TextHash: sendTextHash("queued in the browser"), RequestHash: sendTextHash("queued in the browser"),
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := s.completedTurns.store.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := completed.Started(ctx, coreturns.Start{HostedThreadID: "nht_provider-turn-open", Provider: "codex", NativeThreadID: "provider-turn-open", TurnID: "turn-running", At: now, Source: "test"}); err != nil {
		t.Fatal(err)
	}
	s.prepareBootReattach()
	updated := func(ago time.Duration) *string {
		value := now.Add(-ago).Format(time.RFC3339Nano)
		return &value
	}
	threads := []catalogThread{
		{ID: "nth_starred", Provider: "codex", NativeID: "starred-idle", HostedThreadID: "nht_starred-idle", Starred: true, UpdatedAt: updated(40 * 24 * time.Hour)},
		{ID: "nth_root", Provider: "codex", NativeID: "root-idle", HostedThreadID: "nht_root-idle", UpdatedAt: updated(time.Hour)},
		{ID: "nth_child", Provider: "codex", NativeID: "child-idle", HostedThreadID: "nht_child-idle", UpdatedAt: updated(time.Minute)},
		{ID: "nth_child_wake", Provider: "codex", NativeID: "child-wake", HostedThreadID: "nht_child-wake", UpdatedAt: updated(time.Minute)},
		{ID: "nth_child_quiet", Provider: "codex", NativeID: "child-quiet", HostedThreadID: "nht_child-quiet", UpdatedAt: updated(time.Minute)},
	}
	s.attachRuntimeOverlayBatch(ctx, threads, s.sidebarRuntimeSnapshot())
	query := sidebarDefaultQuery(now)
	active, nodes := projectSidebar(threads, query, 1)
	s.sidebar = newSidebarMaterializedState()
	s.sidebar.replace(threads, query, active, nodes, 1)
	var logs syncBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })

	s.reattachOnBoot()

	var got []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if _, after, ok := strings.Cut(line, "boot re-attach codex/"); ok && strings.Contains(after, " registered session ") {
			got = append(got, strings.Fields(after)[0])
		}
	}
	slices.Sort(got)
	if want := []string{"operation-open", "prompt-queued", "provider-turn-open", "root-idle", "starred-idle"}; !slices.Equal(got, want) {
		t.Fatalf("boot registered %v, want %v", got, want)
	}
	for _, native := range []string{"child-idle", "child-wake", "child-quiet"} {
		if state := s.boot.state(attachmentKey{provider: "codex", nativeThreadID: native}); state != "resting" {
			t.Fatalf("%s after boot = %q, want resting", native, state)
		}
	}
	waitForClaude(t, "the boot loop's thread/resume calls", func() bool { return daemon.resumeCount() == 5 })

	body := `{"prompt":"sent while resting","clientActionId":"nano:test:resting","origin":"system"}`
	recorder := httptest.NewRecorder()
	s.handleAgentTurnSubmit(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/nht_child-idle/turns", strings.NewReader(body)), "nht_child-idle")
	if recorder.Code != http.StatusAccepted && recorder.Code != http.StatusOK {
		t.Fatalf("prompt to a resting thread = %d %s", recorder.Code, recorder.Body.String())
	}
	// The prompt leaves the queue for the provider once its thread attaches
	// (the queued browser prompt above has reached it too).
	waitForClaude(t, "the prompt to reach the provider", func() bool {
		turn, ok := s.agentExecutions.turnForAction("nht_child-idle", "nano:test:resting")
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		return ok && turn.Status != agentTurnQueued && daemon.turnStarts == 2
	})

	// An idle-only wake (a heartbeat) to a resting thread is admitted while
	// the thread attaches, not refused as not ready.
	daemon.setReplyDelay("thread/resume", time.Second)
	if _, _, err := s.submitHostedTurnWithOptions("nht_child-wake", "nano:test:idle-wake", "scheduled wake", true); err != nil {
		t.Fatalf("idle-only wake to a resting thread = %v", err)
	}
	if state := s.boot.state(attachmentKey{provider: "codex", nativeThreadID: "child-quiet"}); state != "resting" {
		t.Fatalf("an unrelated resting thread after the prompts = %q, want resting", state)
	}
}

// A slow fleet snapshot must not hold the boot registration queue. The runtime
// subscriber must still receive every attached row after the snapshot resumes.
func TestBootReattachSlowRuntimePublishDoesNotHoldRegistration(t *testing.T) {
	daemon := newFakeAppServer(t)
	daemon.setReplyDelay("thread/resume", 10*time.Second)
	s, store := bootReattachServer(t, "", daemon.wsURL())
	const sessionCount = 8
	for i := range sessionCount {
		rec := bootCodexRecord(t)
		rec.NativeThreadID = fmt.Sprintf("snapshot-thread-%d", i)
		store.remember(rec)
	}
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.prepareBootReattach()
	updates := s.runtimeEvents.subscribe()
	defer s.runtimeEvents.unsubscribe(updates)

	s.runtimeSnapshotMu.Lock()
	held := true
	defer func() {
		if held {
			s.runtimeSnapshotMu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		s.reattachOnBoot()
		close(done)
	}()
	registered := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		registered = len(s.sessions)
		s.mu.Unlock()
		if registered == sessionCount {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var blockedStacks string
	if registered != sessionCount {
		stacks := make([]byte, 1<<20)
		blockedStacks = string(stacks[:runtime.Stack(stacks, true)])
	}
	held = false
	s.runtimeSnapshotMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("boot registration did not finish after the runtime snapshot resumed")
	}
	if registered != sessionCount {
		t.Fatalf("only %d/%d sessions registered while the runtime snapshot was held:\n%s", registered, sessionCount, blockedStacks)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case delta := <-updates:
			attached := 0
			for _, row := range delta.Rows {
				if row.Provider == "codex" && strings.HasPrefix(row.NativeID, "snapshot-thread-") && row.Runtime != nil && row.Runtime.Attached {
					attached++
				}
			}
			if attached == sessionCount {
				return
			}
		case <-time.After(time.Until(deadline)):
		}
	}
	t.Fatal("runtime subscriber did not receive every boot registration")
}

// A blocked fleet snapshot must not stop reconnects at their status publishes.
// Codex's real handshake crosses starting and ready with no replay fixture.
func TestBootReattachSlowRuntimePublishDoesNotHoldHandshake(t *testing.T) {
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, "", daemon.wsURL())
	const sessionCount = 8
	for i := range sessionCount {
		rec := bootCodexRecord(t)
		rec.NativeThreadID = fmt.Sprintf("handshake-thread-%d", i)
		store.remember(rec)
	}
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.prepareBootReattach()
	updates := s.runtimeEvents.subscribe()
	defer s.runtimeEvents.unsubscribe(updates)

	s.runtimeSnapshotMu.Lock()
	held := true
	defer func() {
		if held {
			s.runtimeSnapshotMu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		s.reattachOnBoot()
		close(done)
	}()
	ready := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ready = 0
		for _, sum := range s.volatileSessionSummaries() {
			if sum.Provider == "codex" && strings.HasPrefix(sum.ThreadID, "handshake-thread-") && sum.Status == "ready" {
				ready++
			}
		}
		if ready == sessionCount {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var blockedStacks string
	if ready != sessionCount {
		stacks := make([]byte, 1<<20)
		blockedStacks = string(stacks[:runtime.Stack(stacks, true)])
	}
	held = false
	s.runtimeSnapshotMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("boot did not finish after the runtime snapshot resumed")
	}
	if ready != sessionCount {
		t.Fatalf("only %d/%d reconnects ready while the runtime snapshot was held:\n%s", ready, sessionCount, blockedStacks)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		select {
		case delta := <-updates:
			ready = 0
			for _, row := range delta.Rows {
				if row.Provider == "codex" && strings.HasPrefix(row.NativeID, "handshake-thread-") && row.Runtime != nil && row.Runtime.Attached && row.Runtime.Status == "ready" {
					ready++
				}
			}
			if ready == sessionCount {
				goto published
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("runtime subscriber did not receive every ready reconnect")
		}
	}
published:
	// Once the boot publisher exits, there is no timer to deliver a deferred status.
	sess := s.sessionForThread("codex", "handshake-thread-0")
	sess.observeConnection(lifecycle.EventLinkLost)
	deadline = time.Now().Add(time.Second)
	for {
		select {
		case delta := <-updates:
			for _, row := range delta.Rows {
				if row.Provider == "codex" && row.NativeID == "handshake-thread-0" && row.Runtime != nil && row.Runtime.Status == "reconnecting" {
					return
				}
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("status change after boot did not publish immediately")
		}
	}
}

// Ordinary boot notices coalesce without losing refresh, while an opened
// thread must appear before its deliberately delayed provider handshake.
func TestBootReattachRuntimeGateKeepsRefreshAndPrioritizedRegistration(t *testing.T) {
	s, daemon, rec := startingCodexServer(t)
	daemon.setReplyDelay("thread/resume", 10*time.Second)
	updates := s.runtimeEvents.subscribe()
	defer s.runtimeEvents.unsubscribe(updates)
	s.publishRuntime(false)
	<-updates
	// The prepared starting thread is opened while the boot publisher is active.
	s.boot.mu.Lock()
	s.boot.runtimeActive = true
	s.boot.mu.Unlock()
	s.notifyRuntimeRefresh()
	s.notifyRuntime()
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
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 0})
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 0})
	}))
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, shim.wsURL(), daemon.wsURL())
	codex := bootCodexRecord(t)
	codex.AttachedAt = "2026-07-28T10:00:00Z"
	store.remember(codex)
	claude := rememberedClaudeSession(t, "/stored/claude")
	claude.AttachedAt = "2026-07-28T10:09:00Z" // stored order: the Codex thread first
	store.remember(claude)
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	var logs syncBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })

	codexLock := s.resumeSpawnLock("codex")
	codexLock.Lock() // a Codex pre-claim that cannot finish
	done := make(chan struct{})
	go func() {
		s.reattachOnBoot()
		close(done)
	}()
	waitForClaude(t, "the Claude thread's registration while Codex is held", func() bool {
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
	hidden := attachmentRecord{Provider: "codex", NativeThreadID: "hidden"}
	visible := attachmentRecord{Provider: "codex", NativeThreadID: "visible"}
	ranks := map[attachmentKey]bootRank{
		hidden.key():  {tier: bootTierTopLevel},
		visible.key(): {tier: bootTierTopLevel, visible: true},
	}
	if got := bootOrder([]attachmentRecord{hidden, visible}, ranks); got[0].NativeThreadID != "visible" {
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
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 0})
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 0})
	}))
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, shim.wsURL(), daemon.wsURL())
	for _, native := range []string{"codex-visible", "codex-hidden"} {
		rec := bootCodexRecord(t)
		rec.NativeThreadID, rec.HostedThreadID = native, "nht_"+native
		store.remember(rec)
	}
	store.remember(rememberedClaudeSession(t, "/stored/claude"))
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.prepareBootReattach()
	now := time.Now().UTC()
	updated := now.Format(time.RFC3339Nano)
	threads := []catalogThread{
		{ID: "nth_codex_visible", Provider: "codex", NativeID: "codex-visible", Starred: true, UpdatedAt: &updated},
		{ID: "nth_claude_visible", Provider: "claude", NativeID: claudeResumeThreadID, Starred: true, UpdatedAt: &updated},
	}
	s.attachRuntimeOverlayBatch(context.Background(), threads, s.sidebarRuntimeSnapshot())
	query := sidebarDefaultQuery(now)
	active, nodes := projectSidebar(threads, query, 1)
	s.sidebar = newSidebarMaterializedState()
	s.sidebar.replace(threads, query, active, nodes, 1)
	var logs syncBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })
	registered := func(key string) bool { return strings.Contains(logs.String(), "boot re-attach "+key+" registered") }

	claudeLock := s.resumeSpawnLock("claude")
	claudeLock.Lock() // the visible Claude thread cannot register yet
	done := make(chan struct{})
	go func() {
		s.reattachOnBoot()
		close(done)
	}()
	waitForClaude(t, "the visible Codex thread's registration", func() bool { return registered("codex/codex-visible") })
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if registered("codex/codex-hidden") {
			claudeLock.Unlock()
			t.Fatal("the hidden Codex thread registered while the visible Claude thread waited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	claudeLock.Unlock()
	waitForClaude(t, "the hidden Codex thread's registration once Claude's visible one registered", func() bool {
		return registered("claude/"+claudeResumeThreadID) && registered("codex/codex-hidden")
	})
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("boot did not finish")
	}
}

// Session shells are the boot-time UI contract: /api/sessions must expose all
// remembered attachments immediately, even if one provider handshake stalls.
// The oldest attachment runs first, so a synchronous Claude shim attach used
// to hide every newer Codex session behind it.
func TestBootReattachRegistersAllShellsBeforeSlowClaudeHandshake(t *testing.T) {
	releaseAttach := make(chan struct{})
	attachStarted := make(chan struct{}, 1)
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		select {
		case attachStarted <- struct{}{}:
		default:
		}
		<-releaseAttach
		c.reply(call, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "lastSeq": 0,
		})
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 0})
	}))
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, shim.wsURL(), daemon.wsURL())
	claude := rememberedClaudeSession(t, "/stored/claude")
	claude.AttachedAt = "2026-07-28T10:00:00Z"
	store.remember(claude)
	codex := bootCodexRecord(t)
	codex.AttachedAt = "2026-07-28T11:00:00Z"
	store.remember(codex)

	done := make(chan struct{})
	go func() {
		seedBootAttachments(t, s)
		openTurnsForBoot(t, s)
		s.reattachOnBoot()
		close(done)
	}()

	select {
	case <-attachStarted:
	case <-time.After(time.Second):
		close(releaseAttach)
		t.Fatal("Claude boot attach never started")
	}
	allRegistered := false
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		allRegistered = len(s.sessions) == 2
		s.mu.Unlock()
		if allRegistered {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseAttach)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("boot re-attach did not finish after Claude resumed")
	}
	if !allRegistered {
		t.Fatal("slow Claude attach blocked the newer Codex session shell")
	}
	// Let the async attach finish before test cleanup closes its session.
	deadline = time.Now().Add(time.Second)
	allReady := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		allReady = len(s.sessions) == 2
		for _, sess := range s.sessions {
			allReady = allReady && sess.summary().Status == "ready"
		}
		s.mu.Unlock()
		if allReady {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !allReady {
		t.Fatal("boot session handshakes did not finish after the Claude attach was released")
	}
}

func TestBootReattachLimitsConcurrentGrokHandshakes(t *testing.T) {
	const sessionCount = 5
	stateDir := t.TempDir()
	bin := writeBlockingGrokACP(t, stateDir)
	s, store := bootReattachServer(t, "", "")
	s.defaults.grokBin = bin
	for i := 0; i < sessionCount; i++ {
		store.remember(attachmentRecord{
			Provider:       "grok",
			NativeThreadID: fmt.Sprintf("grok-boot-%d", i),
			Cwd:            stateDir,
			AttachedAt:     fmt.Sprintf("2026-09-11T02:%02d:00Z", i),
		})
	}
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s) // queued at boot though the sidebar does not show them
	s.reattachOnBoot()
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(stateDir, "release"), []byte("release"), 0600) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && readGrokCounter(stateDir, "maximum") < 4 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	if len(sessions) != sessionCount {
		t.Fatalf("registered Grok shells = %d, want %d", len(sessions), sessionCount)
	}
	for _, sess := range sessions {
		if got := sess.summary().Status; got != "starting" {
			t.Fatalf("waiting Grok shell %s status = %q, want starting", sess.id, got)
		}
	}
	maximum := readGrokCounter(stateDir, "maximum")
	if err := os.WriteFile(filepath.Join(stateDir, "release"), []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ready := 0
		for _, sess := range sessions {
			if sess.summary().Status == "ready" {
				ready++
			}
		}
		if ready == sessionCount {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for _, sess := range sessions {
		if got := sess.summary().Status; got != "ready" {
			t.Fatalf("released Grok shell %s status = %q, want ready", sess.id, got)
		}
	}
	if maximum != 4 {
		t.Fatalf("maximum concurrent Grok handshakes = %d, want 4", maximum)
	}
}

func TestBootReattachLimitsConcurrentClaudeHandshakes(t *testing.T) {
	const sessionCount = claudeBootHandshakeLimit + 1
	var mu sync.Mutex
	active, maximum := 0, 0
	started := make(chan struct{}, sessionCount)
	release := make(chan struct{})
	shim := newFakeShim(t, func(c *shimConn) {
		call, ok := c.next()
		if !ok {
			return
		}
		var params struct {
			NativeSessionID string `json:"nativeSessionId"`
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Errorf("decode attach: %v", err)
			return
		}
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		mu.Unlock()
		started <- struct{}{}
		<-release
		// An idle child whose spool holds nothing past the scanned floor.
		c.reply(call, map[string]any{
			"shimSessionId": "shim-" + params.NativeSessionID, "nativeSessionId": params.NativeSessionID,
			"state": "running", "turnActive": false, "lastSeq": 0,
		})
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-" + params.NativeSessionID, "lastSeq": 0})
		mu.Lock()
		active--
		mu.Unlock()
		c.answerRest()
	})
	s, store := bootReattachServer(t, shim.wsURL(), "")
	for i := 0; i < sessionCount; i++ {
		id := fmt.Sprintf("claude-boot-%d", i)
		store.remember(attachmentRecord{
			Provider: "claude", NativeThreadID: id, ShimSessionID: "shim-" + id,
			Bin: "/stored/claude", Args: storedPosture,
			AttachedAt: fmt.Sprintf("2026-09-11T03:%02d:00Z", i),
		})
	}
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.reattachOnBoot()

	for i := 0; i < claudeBootHandshakeLimit; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatalf("%d Claude boot handshakes did not start", claudeBootHandshakeLimit)
		}
	}
	select {
	case <-started:
		close(release)
		t.Fatal("one more Claude handshake exceeded the bound")
	case <-time.After(75 * time.Millisecond):
	}
	s.mu.Lock()
	shells := len(s.sessions)
	s.mu.Unlock()
	if shells != sessionCount {
		close(release)
		t.Fatalf("registered Claude shells = %d, want %d", shells, sessionCount)
	}
	mu.Lock()
	gotMaximum := maximum
	mu.Unlock()
	close(release)
	if gotMaximum != claudeBootHandshakeLimit {
		t.Fatalf("maximum concurrent Claude handshakes = %d, want %d", gotMaximum, claudeBootHandshakeLimit)
	}
	waitForClaude(t, "all bounded Claude handshakes", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.sessions) != sessionCount {
			return false
		}
		for _, sess := range s.sessions {
			if sess.summary().Status != "ready" {
				return false
			}
		}
		return true
	})
}

func writeBlockingGrokACP(t *testing.T, stateDir string) string {
	t.Helper()
	bin := filepath.Join(stateDir, "blocking-grok-acp")
	script := "#!/bin/sh\nstate_dir=" + strconv.Quote(stateDir) + `
IFS= read -r request || exit 1
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
IFS= read -r request || exit 1
lock="$state_dir/handshake.lock"
while ! mkdir "$lock" 2>/dev/null; do sleep 0.01; done
touch "$state_dir/active.$$"
set -- "$state_dir"/active.*
active=$#
maximum=0
if [ -f "$state_dir/maximum" ]; then read -r maximum < "$state_dir/maximum"; fi
if [ "$active" -gt "$maximum" ]; then printf '%s\n' "$active" > "$state_dir/maximum"; fi
rmdir "$lock"
while [ ! -f "$state_dir/release" ]; do sleep 0.01; done
while ! mkdir "$lock" 2>/dev/null; do sleep 0.01; done
rm "$state_dir/active.$$"
rmdir "$lock"
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"_meta":{"x.ai/sessionConfig":{"options":[{"category":"mode","id":"high","selected":true}]}}}}'
while IFS= read -r request; do :; done
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func readGrokCounter(stateDir, name string) int {
	raw, err := os.ReadFile(filepath.Join(stateDir, name))
	if err != nil {
		return 0
	}
	value, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return value
}

// Codex does not write a rollout until the first turn. A nano restart in the
// gap must keep the daemon-resident thread routed and usable so a delayed
// first turn (background tagging is the real case) can make it durable.
func TestBootReattachKeepsZeroTurnCodexThreadUsable(t *testing.T) {
	daemon := newFakeAppServer(t)
	daemon.setResumeErrorSequence(
		"no rollout found for thread id codex-thread-1",
		"no rollout found for thread id codex-thread-1",
		"",
	)
	daemon.setTurnStartResult(map[string]any{"turn": map[string]any{"id": "turn-zero-1"}})
	daemon.setResumeTurns(json.RawMessage(`[{"id":"turn-zero-1","status":"inProgress","startedAt":1787600000,"items":[{"id":"user-zero-1","type":"userMessage","text":"tag this thread"}]}]`))
	s, store := bootReattachServer(t, "", daemon.wsURL())
	rec := bootCodexRecord(t)
	store.remember(rec)

	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.reattachOnBoot()

	s.mu.Lock()
	var sess *session
	for _, candidate := range s.sessions {
		sess = candidate
	}
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("zero-turn attachment did not create a boot session")
	}
	sum := waitForSessionStatus(t, s, sess.id, "ready")
	if sum.ThreadID != rec.NativeThreadID || sum.Model != rec.Model || sum.Error != "" {
		t.Fatalf("summary = %#v, want the stored zero-turn thread ready", sum)
	}
	s.codex.mu.Lock()
	routed := s.codex.sessions[rec.NativeThreadID]
	s.codex.mu.Unlock()
	if routed != sess {
		t.Fatal("zero-turn fallback released the routing claim")
	}
	// The recovered zero-turn target has no provider-native identity yet, so it
	// cannot truthfully identify itself as the sender. Use a separate confirmed
	// hosted sender to prove the target remains reachable through the public
	// agent route without weakening sender admission.
	sender, _, err := s.agentExecutions.beginSession("boot-sender", "boot-sender", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.bindSession(sender.ID, "nano-boot-sender", "sender-native", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	s.catalog = renamedCatalog{name: "Boot sender"}

	turn := httptest.NewRecorder()
	s.handleAgentTurnSubmit(turn, httptest.NewRequest(http.MethodPost,
		"/api/v1/hosted-threads/"+sess.hostedThreadID+"/turns",
		bytes.NewBufferString(`{"clientActionId":"zero-turn-action","prompt":"tag this thread","senderThreadRef":"`+sender.ID+`"}`)), sess.hostedThreadID)
	if turn.Code != http.StatusAccepted {
		t.Fatalf("first turn status = %d, body = %s", turn.Code, turn.Body.String())
	}
	var operation agentTurnView
	if err := json.Unmarshal(turn.Body.Bytes(), &operation); err != nil {
		t.Fatalf("decode first-turn operation: %v", err)
	}
	waitForClaude(t, "post-start resume on the replacement connection", func() bool {
		sess.stateMu.Lock()
		finished := !sess.codexZeroTurnResume && sess.workActiveLocked()
		sess.stateMu.Unlock()
		return daemon.resumeCount() == 3 && finished
	})
	sess.stateMu.Lock()
	stillPending := sess.codexZeroTurnResume
	turnActive := sess.workActiveLocked()
	sess.stateMu.Unlock()
	if stillPending {
		t.Fatal("successful post-start resume did not finish zero-turn recovery")
	}
	if !turnActive {
		t.Fatal("post-start resume did not restore the in-flight turn")
	}

	daemon.emitThreadNotification(rec.NativeThreadID, map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"threadId": rec.NativeThreadID, "turnId": "turn-zero-1",
			"item": map[string]any{"id": "final-zero-1", "type": "agentMessage", "phase": "final_answer", "text": "tagged"},
		},
	})
	daemon.emitThreadNotification(rec.NativeThreadID, map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"threadId": rec.NativeThreadID,
			"turn":     map[string]any{"id": "turn-zero-1", "status": "completed"},
		},
	})
	defer func() {
		if t.Failed() {
			got, _ := s.agentExecutions.turn(operation.ID)
			t.Logf("first-turn operation: %+v; lifecycle: %+v; projection: %+v", got, sess.summary(), projectionStateForTest(sess.projection).Turns)
		}
	}()
	// The prompt went out with its own client id, so the wire's completion
	// alone does not finish its operation: only its own saved prompt does.
	ctx := context.Background()
	completed, err := sess.completedTurns.store.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitForClaude(t, "the completed-turn record", func() bool {
		record, err := completed.Get(ctx, sess.hostedThreadID, "turn-zero-1")
		return err == nil && record.Outcome == agentTurnCompleted
	})
	open, _ := s.agentExecutions.turn(operation.ID)
	if agentTurnTerminal(open.Status) || open.ProviderTurnID != "turn-zero-1" || open.CandidateItemID != "final-zero-1" {
		t.Fatalf("operation after the wire completed its turn = %+v; want it open with the turn's final answer as candidate", open)
	}
	prompt, _, err := sess.turns.Get(ctx, open.TransportActionID)
	if err != nil {
		t.Fatal(err)
	}
	binding := prompt.Outbox.Binding
	if prompt.Outbox.HostedThreadID != sess.hostedThreadID || binding == nil || binding.Scope.Provider != model.ProviderCodex ||
		binding.Scope.NativeThreadID == nil || *binding.Scope.NativeThreadID == "" || *binding.Scope.NativeThreadID != rec.NativeThreadID ||
		binding.Scope.NativeTurnID == nil || *binding.Scope.NativeTurnID != "turn-zero-1" ||
		binding.EventID != submissionEventID(sess.hostedThreadID, open.TransportActionID) {
		t.Fatalf("prompt binding = %+v; want this thread, turn-zero-1 and the prompt's own client id", prompt.Outbox)
	}
	// Saved-proof input: this wire-only fixture has no saved transcript, so it
	// supplies the fact a saved row of this prompt would prove. It is not
	// evidence from a saved file or its callback.
	if _, settled, err := sess.turns.Settle(ctx, open.TransportActionID); err != nil || !settled {
		t.Fatalf("settle the prompt: settled=%v err=%v", settled, err)
	}
	sess.finishJoinedAgentTurns(ctx, "turn-zero-1")
	waitForClaude(t, "hosted first-turn operation completion", func() bool {
		got, ok := s.agentExecutions.turn(operation.ID)
		return ok && got.Status == agentTurnCompleted && got.ProviderTurnID == "turn-zero-1" &&
			got.FinalItemID == "final-zero-1" && got.FinalResponse == "tagged"
	})
	// The stored record remains the durability anchor for later restarts.
	if got, ok := store.get("codex", rec.NativeThreadID); !ok || got.Model != "" || got.Cwd != "" || got.TurnServiceTier != "" {
		encoded, _ := json.Marshal(got)
		t.Fatalf("attachment after first turn = %s, present=%v", encoded, ok)
	}
}

// One record failing must not stop the walk or forget the record: the broken
// (older, so earlier) claude record stays detached while the codex record
// still comes back live.
func TestBootReattachFailureLeavesRecordDetachedAndContinues(t *testing.T) {
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, "ws://127.0.0.1:1", daemon.wsURL()) // no shim listening
	broken := rememberedClaudeSession(t, filepath.Join(t.TempDir(), "missing-claude"))
	store.remember(broken)
	store.remember(bootCodexRecord(t))

	seedBootAttachments(t, s)
	openTurnsForBoot(t, s)
	s.reattachOnBoot()

	var sessions []*session
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		sessions = sessions[:0]
		for _, sess := range s.sessions {
			sessions = append(sessions, sess)
		}
		s.mu.Unlock()
		if len(sessions) == 1 && sessions[0].provider == "codex" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(sessions) != 1 || sessions[0].provider != "codex" {
		t.Fatalf("sessions after boot = %+v, want only the codex thread", sessions)
	}
	if _, ok := store.get("claude", claudeResumeThreadID); !ok {
		t.Fatal("failed re-attach forgot its record; it must stay detached")
	}
}

func TestClaudeColdCatalogCallbackAfterReattachDoesNotConfirmOrScrubHistoricalSubmission(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-cold-callback", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "turnActive": false, "lastSeq": 3,
		})
		c.line(1, "send", `{"type":"user","message":{"role":"user","content":"synthetic cold-callback-action"}}`)
		c.line(2, "recv", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"historical"}]}}`)
		c.line(3, "recv", `{"type":"result","subtype":"success"}`)
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-cold-callback", "lastSeq": 3})
	}))
	server, store := bootReattachServer(t, shim.wsURL(), "")
	attempts, err := newSendAttemptStore(server.queuedTurns.db, server.queuedTurns.path, "cold-callback-run", testPromptStore(t, server.queuedTurns.db))
	if err != nil {
		t.Fatal(err)
	}
	turnStore, err := turns.OpenStore(context.Background(), server.queuedTurns.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turnStore.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	server.turns, server.queuedTurns.turns, attempts.turns, server.sendAttempts = turnStore, turnStore, turnStore, attempts
	store.remember(rememberedClaudeSession(t, "/stored/claude"))
	seedBootAttachments(t, server)
	openTurnsForBoot(t, server)
	remembered, ok := store.get("claude", claudeResumeThreadID)
	if !ok || remembered.HostedThreadID == "" {
		t.Fatalf("seeded attachment = %#v, found=%v", remembered, ok)
	}
	text := "synthetic cold-callback-action"
	seedAcceptedTransactionalTurn(t, turnStore, remembered.HostedThreadID, model.ProviderClaude, claudeResumeThreadID, "cold-callback-action", "claude:1", text)

	ctx, cancel := context.WithCancel(context.Background())
	service := &threadCatalogService{transcriptChanges: make(map[string]*pendingTranscriptChange)}
	service.transcriptChangedCond = sync.NewCond(&service.transcriptChangedMu)
	service.transcriptChangeWorkerWg.Add(1)
	go service.runTranscriptChangeWorker(ctx)
	t.Cleanup(func() {
		cancel()
		service.transcriptChangedMu.Lock()
		service.transcriptChangeStopping = true
		service.transcriptChangedCond.Broadcast()
		service.transcriptChangedMu.Unlock()
		service.transcriptChangeWorkerWg.Wait()
	})
	service.notifyTranscriptChanged(transcriptChange{
		ThreadID: "nth-cold-callback", Provider: "claude", NativeThreadID: claudeResumeThreadID,
	})
	// This is the production order: the catalog's catch-up queues changes,
	// boot reattachment reconstructs the shim spool, and only then can a worker
	// invoke the callback main registered.
	server.reattachOnBoot()
	server.mu.Lock()
	var restarted *session
	for _, candidate := range server.sessions {
		restarted = candidate
	}
	server.mu.Unlock()
	if restarted == nil {
		t.Fatal("boot did not register the Claude session")
	}
	waitForSessionStatus(t, server, restarted.id, "ready")
	callbackDone := make(chan struct{})
	service.setTranscriptChanged(func(ctx context.Context, change transcriptChange) {
		server.confirmDurableThread(ctx, change)
		close(callbackDone)
	})
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("cold catalog callback did not run after reattach")
	}

	if attempt, ok := attempts.get("cold-callback-action"); !ok || attempt.Confirmed {
		t.Fatalf("cold catalog callback changed historical journal row: %#v, found=%v", attempt, ok)
	}
	if _, ok := server.queuedTurns.get("cold-callback-action"); !ok {
		t.Fatal("cold catalog callback scrubbed historical outbox row")
	}
}

// startingCodexServer is a restarted server with one stored Codex thread that
// the boot has recorded as starting but not registered yet.
func startingCodexServer(t *testing.T) (*server, *fakeAppServer, attachmentRecord) {
	t.Helper()
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, "", daemon.wsURL())
	store.remember(bootCodexRecord(t))
	seedBootAttachments(t, s)
	openTurnsForBoot(t, s) // the boot loop's own thread, not left resting
	s.prepareBootReattach()
	return s, daemon, store.records()[0]
}

// TestServeAnswersWhileStoredThreadsStillReattach catches the minutes-long
// closed port after a restart, and a waiting thread reported as detached or
// ready instead of starting. Boundary: the real serve order on a loopback
// listener, while the boot loop's first registration is held; then the one
// summary line the deploy slot waits for.
func TestServeAnswersWhileStoredThreadsStillReattach(t *testing.T) {
	s, daemon, rec := startingCodexServer(t)
	var logs syncBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })
	authStore, err := remoteauth.OpenStore(filepath.Join(t.TempDir(), "remote-auth.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authStore.Close() })
	app := &application{server: s, remoteAuth: remoteauth.New(authStore, "nano.example.ts.net:7690")}
	local, remote := app.mount()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	s.resumeSpawnLock("codex").Lock() // the boot loop's first registration waits here
	held := true
	t.Cleanup(func() {
		if held {
			s.resumeSpawnLock("codex").Unlock()
		}
	})
	go func() { _ = app.serve(listener, local, remote) }()
	get := func(path string, into any) int {
		t.Helper()
		response, err := http.Get("http://" + listener.Addr().String() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if into != nil {
			if err := json.NewDecoder(response.Body).Decode(into); err != nil {
				t.Fatal(err)
			}
		}
		return response.StatusCode
	}
	if status := get("/api/v1/build", nil); status != http.StatusOK {
		t.Fatalf("build while reattaching = %d", status)
	}
	var hosted struct {
		Status          string               `json:"status"`
		Attached        bool                 `json:"attached"`
		Availability    string               `json:"availability"`
		AttachmentState string               `json:"attachmentState"`
		Attachment      hostedAttachmentView `json:"attachment"`
	}
	if status := get("/api/v1/hosted-threads/"+rec.HostedThreadID, &hosted); status != http.StatusOK ||
		hosted.Status != "starting" || hosted.Attached || hosted.AttachmentState != "starting" || hosted.Attachment.State != "starting" || hosted.Availability != availabilityBusy {
		t.Fatalf("hosted view while reattaching = %d %+v, want starting", status, hosted)
	}
	var sessions struct {
		Detached []struct {
			NativeThreadID string `json:"nativeThreadId"`
			Status         string `json:"status"`
		} `json:"detached"`
	}
	if get("/api/sessions", &sessions); len(sessions.Detached) != 1 || sessions.Detached[0].Status != "starting" {
		t.Fatalf("session list while reattaching = %+v, want one starting row", sessions.Detached)
	}
	if runtime, _ := s.RuntimeFor(rec.Provider, rec.NativeThreadID); runtime.Status != "starting" {
		t.Fatalf("runtime overlay while reattaching = %+v", runtime)
	}

	held = false
	s.resumeSpawnLock("codex").Unlock()
	waitForClaude(t, "boot re-attach to finish", func() bool {
		return strings.Contains(logs.String(), "boot re-attach finished")
	})
	if count := strings.Count(logs.String(), "boot re-attach finished"); count != 1 || !strings.Contains(logs.String(), "1 threads") {
		t.Fatalf("boot summary lines = %d:\n%s", count, logs.String())
	}
	waitForClaude(t, "thread/resume call", func() bool { return daemon.resumeCount() == 1 })
	if runtime, _ := s.RuntimeFor(rec.Provider, rec.NativeThreadID); !runtime.Attached {
		t.Fatalf("runtime overlay after reattach = %+v", runtime)
	}
}

// TestPromptToAStartingThreadRegistersItOnceAndQueues catches a prompt that
// is refused or lost because its thread has not been reattached yet, and a
// second provider session when the boot loop reaches the thread later.
// Boundary: the hosted turn API, the provider daemon and the boot loop.
func TestPromptToAStartingThreadRegistersItOnceAndQueues(t *testing.T) {
	s, daemon, rec := startingCodexServer(t)
	body := `{"prompt":"sent while starting","clientActionId":"nano:test:starting","origin":"system"}`
	recorder := httptest.NewRecorder()
	s.handleAgentTurnSubmit(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+rec.HostedThreadID+"/turns", strings.NewReader(body)), rec.HostedThreadID)
	if recorder.Code != http.StatusAccepted && recorder.Code != http.StatusOK {
		t.Fatalf("prompt to a starting thread = %d %s", recorder.Code, recorder.Body.String())
	}
	waitForClaude(t, "the prompt to reach the provider", func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		return daemon.turnStarts == 1
	})
	s.reattachOnBoot()
	daemon.mu.Lock()
	turnStarts := daemon.turnStarts
	daemon.mu.Unlock()
	s.mu.Lock()
	count := len(s.sessions)
	s.mu.Unlock()
	if resumes := daemon.resumeCount(); resumes != 1 || turnStarts != 1 || count != 1 {
		t.Fatalf("resumes = %d, turn starts = %d, sessions = %d; want one of each", resumes, turnStarts, count)
	}
}

// TestOpeningAStartingThreadRegistersItFirst catches an opened thread that
// waits its turn in the boot queue: the operator is looking at it and cannot
// type until every thread before it has registered. Boundary: the route the
// browser calls when a pane opens a starting thread, before the boot loop
// reaches it; then the boot loop, which must not register it a second time.
func TestOpeningAStartingThreadRegistersItFirst(t *testing.T) {
	s, daemon, rec := startingCodexServer(t)
	daemon.setReplyDelay("thread/resume", 10*time.Second)
	updates := s.runtimeEvents.subscribe()
	defer s.runtimeEvents.unsubscribe(updates)
	handler := applicationHandler(s)
	prioritize := func() string {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+rec.HostedThreadID+"/prioritize", nil))
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("prioritize = %d %s", recorder.Code, recorder.Body.String())
		}
		return strings.TrimSpace(recorder.Body.String())
	}
	if got := prioritize(); got != `{"starting":true}` {
		t.Fatalf("prioritize a starting thread = %s", got)
	}
	// The boot loop has not started and the handshake cannot be ready yet:
	// the opened shell must publish its own starting row immediately.
	publishDeadline := time.After(2 * time.Second)
waitingForPublish:
	for {
		select {
		case delta := <-updates:
			for _, row := range delta.Rows {
				if row.NativeID == rec.NativeThreadID && row.Runtime != nil && row.Runtime.Attached && row.Runtime.Status == "starting" {
					break waitingForPublish
				}
			}
		case <-publishDeadline:
			t.Fatal("prioritized registration did not publish its attached starting row before the boot loop")
		}
	}
	waitForClaude(t, "the opened thread's registration", func() bool {
		_, waiting := s.boot.waitingHosted(rec.HostedThreadID)
		return waiting == "" && s.sessionForHostedThread(rec.HostedThreadID) != nil
	})
	waitForClaude(t, "thread/resume call", func() bool { return daemon.resumeCount() == 1 })
	if got := prioritize(); got != `{"starting":false}` {
		t.Fatalf("prioritize a registered thread = %s", got)
	}
	s.reattachOnBoot()
	s.mu.Lock()
	count := len(s.sessions)
	s.mu.Unlock()
	if resumes := daemon.resumeCount(); resumes != 1 || count != 1 {
		t.Fatalf("provider resumes = %d, sessions = %d; want one of each", resumes, count)
	}
}

// TestResumeOfALiveCodexThreadKeepsItsBinding catches a second resume that
// rebinds the hosted record to a session it then discards: Codex refuses a
// duplicate only after its shell registers. Boundary: the resume API and the
// hosted record, after the boot loop registered the thread.
func TestResumeOfALiveCodexThreadKeepsItsBinding(t *testing.T) {
	s, daemon, rec := startingCodexServer(t)
	s.reattachOnBoot()
	live := s.sessionForHostedThread(rec.HostedThreadID)
	if live == nil {
		t.Fatal("boot loop did not register the thread")
	}
	waitForClaude(t, "thread/resume call", func() bool { return daemon.resumeCount() == 1 })
	generation := func() (value int64) {
		t.Helper()
		if err := s.controlMetadata.db.QueryRow(`SELECT attachment_generation FROM hosted_threads WHERE hosted_thread_id=?`, rec.HostedThreadID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := generation()
	recorder := httptest.NewRecorder()
	s.handleSessionCreate(recorder, httptest.NewRequest(http.MethodPost, "/api/sessions",
		strings.NewReader(`{"provider":"codex","mode":"resume","threadId":"`+rec.NativeThreadID+`"}`)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("resume of a live thread = %d %s, want 409", recorder.Code, recorder.Body.String())
	}
	if stored, _ := s.agentExecutions.session(rec.HostedThreadID); stored.NanoSessionID != live.id {
		t.Fatalf("hosted record now names session %q, want the live %q", stored.NanoSessionID, live.id)
	}
	if after := generation(); after != before {
		t.Fatalf("attachment generation moved from %d to %d; the live session's evidence is now stale", before, after)
	}
	if resumes := daemon.resumeCount(); resumes != 1 {
		t.Fatalf("provider resumes = %d, want 1", resumes)
	}
}

// TestBootLoopSkipsAThreadForgottenWhileStarting catches the boot loop
// bringing back a thread the operator detached or closed while it waited.
// Boundary: the attachment store and the boot loop.
func TestBootLoopSkipsAThreadForgottenWhileStarting(t *testing.T) {
	s, daemon, rec := startingCodexServer(t)
	s.forgetAttachment(rec.Provider, rec.NativeThreadID)
	s.reattachOnBoot()
	s.mu.Lock()
	count := len(s.sessions)
	s.mu.Unlock()
	if count != 0 || daemon.resumeCount() != 0 {
		t.Fatalf("sessions = %d, provider resumes = %d; want the forgotten thread left alone", count, daemon.resumeCount())
	}
	if _, waiting := s.boot.waitingHosted(rec.HostedThreadID); waiting != "" {
		t.Fatal("the forgotten thread still reports starting")
	}
}

// TestCodexTurnFinishedWhileNanoWasDownReachesTheCompletedTurnFeed catches a
// Codex turn that ends while Nano is stopped completing its hosted operation
// but never reaching the completed-turn feed, so a subscribed parent is never
// told. Boundary: boot reattach with the provider's resume answer, then the
// feed that completion subscriptions read.
func TestCodexTurnFinishedWhileNanoWasDownReachesTheCompletedTurnFeed(t *testing.T) {
	s, daemon, rec := startingCodexServer(t)
	op, _, err := s.agentExecutions.beginTurn(rec.HostedThreadID, "action-down", "finish while Nano is down")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.markTurnDelivery(op.ID, "turn-down", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	// An older finished turn comes first: the reply's items are parsed only
	// from the earliest turn whose operation is still live.
	daemon.setResumeTurns(json.RawMessage(`[{"id":"turn-older","status":"completed","items":[{"id":"answer-older","type":"agentMessage","phase":"final_answer","text":"older"}]},{"id":"turn-down","status":"completed","completedAt":1787600200,"items":[{"id":"answer-down","type":"agentMessage","phase":"final_answer","text":"done while down"}]}]`))

	s.reattachOnBoot()
	sess := s.sessionForHostedThread(rec.HostedThreadID)
	if sess == nil {
		t.Fatal("boot loop did not register the thread")
	}
	waitForClaude(t, "the hosted operation to complete", func() bool {
		latest, _ := s.agentExecutions.turn(op.ID)
		return latest.Status == agentTurnCompleted
	})
	if latest, _ := s.agentExecutions.turn(op.ID); latest.FinalResponse != "done while down" {
		t.Fatalf("operation's final response = %q, want the resumed turn's answer", latest.FinalResponse)
	}
	var feed []completedTurn
	waitForClaude(t, "the completed-turn feed", func() bool {
		feed, err = sess.completedTurns.after(rec.HostedThreadID, "", 10)
		return err == nil && len(feed) > 0
	})
	if len(feed) != 1 || feed[0].TurnID != "turn-down" || feed[0].Outcome != agentTurnCompleted {
		t.Fatalf("completed-turn feed = %+v, want turn-down once", feed)
	}
	if completedAt, err := time.Parse(time.RFC3339Nano, feed[0].CompletedAt); err != nil || !completedAt.Equal(time.Unix(1787600200, 0)) {
		t.Fatalf("completed at %q (%v), want the provider's completion time, not the reattach time", feed[0].CompletedAt, err)
	}
}

// feedSubscriptionClient serves the subscription poller from the server's own
// completed-turn feed handler and records what it delivers.
type feedSubscriptionClient struct {
	s        *server
	mu       sync.Mutex
	reads    int
	messages []interact.SendMessageRequest
	prompts  []interact.SubmitPromptRequest
}

func (c *feedSubscriptionClient) counts() (reads, messages int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads, len(c.messages)
}

func (c *feedSubscriptionClient) SubscribeCompletedTurns(_ context.Context, request interact.PageRequest) (interact.CompletedTurnPage, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	recorder := httptest.NewRecorder()
	target := "/api/v1/hosted-threads/" + request.ThreadRef + "/completed-turns?after=" + url.QueryEscape(request.Cursor) + "&limit=" + strconv.Itoa(max(request.Limit, 1))
	c.s.handleCompletedTurns(recorder, httptest.NewRequest(http.MethodGet, target, nil), request.ThreadRef)
	var page interact.CompletedTurnPage
	if recorder.Code != http.StatusOK {
		return page, fmt.Errorf("completed-turn feed = %d %s", recorder.Code, recorder.Body.String())
	}
	return page, json.Unmarshal(recorder.Body.Bytes(), &page)
}

func (c *feedSubscriptionClient) SendMessage(_ context.Context, request interact.SendMessageRequest) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, request)
	return json.RawMessage(`{"messageId":"message"}`), nil
}

func (c *feedSubscriptionClient) SubmitPrompt(_ context.Context, request interact.SubmitPromptRequest) (interact.Turn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prompts = append(c.prompts, request)
	return interact.Turn{ID: "turn"}, nil
}

// subscribedCodexServer is startingCodexServer whose stored Codex thread is a
// child that tells its parent's inbox when a turn completes.
func subscribedCodexServer(t *testing.T, resumeTurns string) (*server, *fakeAppServer, attachmentRecord, *feedSubscriptionClient, *subscriptionDispatcher) {
	t.Helper()
	daemon := newFakeAppServer(t)
	daemon.setResumeTurns(json.RawMessage(resumeTurns))
	s, store := bootReattachServer(t, "", daemon.wsURL())
	s.completedTurns = newCompletedTurnSource(&completedTurnStore{db: s.queuedTurns.db})
	parent, _, err := s.agentExecutions.beginSession("parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := s.agentExecutions.beginSessionWithSubscriptions("child", "child-hash", "codex", presentationBackground, parent, subscriptions.Choices{Inbox: subscriptions.Send})
	if err != nil {
		t.Fatal(err)
	}
	rec := bootCodexRecord(t)
	rec.HostedThreadID = child.ID
	store.remember(rec)
	seedBootAttachments(t, s)
	s.prepareBootReattach()
	client := &feedSubscriptionClient{s: s}
	return s, daemon, rec, client, subscriptions.NewDispatcher(s.agentExecutions.control.db, client, nil, nil)
}

// TestResumeAnswerWithOlderTerminalTurnsNotifiesNobody catches the first boot
// after this change flooding subscribers with turns that ended long before
// the restart. Boundary: boot reattach, the completed-turn feed and the
// subscription poller.
func TestResumeAnswerWithOlderTerminalTurnsNotifiesNobody(t *testing.T) {
	s, _, rec, client, dispatcher := subscribedCodexServer(t, `[
		{"id":"turn-old","status":"completed","items":[{"id":"answer-old","type":"agentMessage","text":"old"}]},
		{"id":"turn-settled","status":"completed","items":[{"id":"answer-settled","type":"agentMessage","text":"settled"}]}]`)
	settled, _, err := s.agentExecutions.beginTurn(rec.HostedThreadID, "action-settled", "reported before the stop")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.markTurnDelivery(settled.ID, "turn-settled", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.markTurnTerminal("codex", rec.NativeThreadID, "turn-settled", agentTurnCompleted, nil, "settled", "answer-settled"); err != nil {
		t.Fatal(err)
	}
	dispatcher.Sweep("") // start the child's cursor where a running Nano would have it

	s.reattachOnBoot()
	s.reattachWaitingHosted(rec.HostedThreadID) // no work open, so it rests until needed
	waitForClaude(t, "the reattached session", func() bool {
		sess := s.sessionForHostedThread(rec.HostedThreadID)
		return sess != nil && sess.summary().Status == "ready"
	})
	dispatcher.Sweep("")
	if feed, err := s.completedTurns.after(rec.HostedThreadID, "", 10); err != nil || len(feed) != 0 || len(client.messages) != 0 {
		t.Fatalf("feed = %+v (%v), deliveries = %d; want nothing for turns that ended before the stop", feed, err, len(client.messages))
	}
}

// TestCodexTurnCompletedLiveAndSeenAgainOnResumeIsDeliveredOnce catches a
// completion reported twice, or refused as a conflict, when resume sees a turn
// the live wire already completed. Boundary: the live turn/completed path, the
// resume path recording the same turn again, the completed-turn feed and the
// subscription poller.
func TestCodexTurnCompletedLiveAndSeenAgainOnResumeIsDeliveredOnce(t *testing.T) {
	s, daemon, rec, client, dispatcher := subscribedCodexServer(t, `[{"id":"turn-live","status":"inProgress","startedAt":1787600000}]`)
	op, _, err := s.agentExecutions.beginTurn(rec.HostedThreadID, "action-live", "complete while live")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.markTurnDelivery(op.ID, "turn-live", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	dispatcher.Sweep("")
	s.reattachOnBoot()
	var sess *session
	waitForClaude(t, "the reattached turn to be active", func() bool {
		sess = s.sessionForHostedThread(rec.HostedThreadID)
		if sess == nil {
			return false
		}
		sess.stateMu.Lock()
		defer sess.stateMu.Unlock()
		return sess.workActiveLocked()
	})
	daemon.emitThreadNotification(rec.NativeThreadID, map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": rec.NativeThreadID, "turnId": "turn-live",
		"item": map[string]any{"id": "final-live", "type": "agentMessage", "phase": "final_answer", "text": "done live"},
	}})
	daemon.emitThreadNotification(rec.NativeThreadID, map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": rec.NativeThreadID, "turn": map[string]any{"id": "turn-live", "status": "completed", "completedAt": 1787600100},
	}})
	var live []completedTurn
	waitForClaude(t, "the live completion in the feed", func() bool {
		live, err = s.completedTurns.after(rec.HostedThreadID, "", 10)
		return err == nil && len(live) == 1
	})
	dispatcher.Sweep("")

	var logs syncBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })
	resumed := `{"thread":{"id":"` + rec.NativeThreadID + `","turns":[{"id":"turn-live","status":"completed","completedAt":1787600100,"items":[{"id":"final-live","type":"agentMessage","phase":"final_answer","text":"done live"}]}]}}`
	sess.observeCodexResume(decodedCodexResume([]byte(resumed)))
	// A resume that races the live completion records the same turn again.
	sess.observeCodexResumedTurnFinish(rec.NativeThreadID, codexResumeProjectionTurn{ID: "turn-live", Status: agentTurnCompleted})
	dispatcher.Sweep("")

	feed, err := s.completedTurns.after(rec.HostedThreadID, "", 10)
	if err != nil || len(feed) != 1 || feed[0].Outcome != live[0].Outcome || feed[0].FinalMessageID != live[0].FinalMessageID {
		t.Fatalf("feed after resume = %+v (%v), want the one live record %+v", feed, err, live)
	}
	if strings.Contains(logs.String(), "ended while Nano was down") {
		t.Fatalf("recording the same turn again was refused:\n%s", logs.String())
	}
	if len(client.messages) != 1 {
		t.Fatalf("parent inbox deliveries = %d, want 1", len(client.messages))
	}
}
