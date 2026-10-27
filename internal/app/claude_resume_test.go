package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// The Claude resume ladder end to end: POST /api/sessions
// {provider:"claude", mode:"resume", threadId} attaches to the live shim
// session when there is one, and otherwise respawns with `--resume`, retaining
// transport recovery arguments while canonical control metadata supplies cwd
// and desired provider settings
// (docs/decisions/claude-shim-daemon.md).

const (
	claudeResumeThreadID = "native-1"
	// successorInitLine is what a `--resume` child announces: the conversation
	// continues, but under a brand-new native session id.
	claudeSuccessorInitLine = `{"type":"system","subtype":"init","session_id":"native-2","model":"sonnet"}`
)

// storedPosture is a remembered session's argv including a stale `--resume`
// pair from an earlier fallback, so every fallback assertion also proves the
// stripping rule.
var storedPosture = []string{
	"-p", "--verbose", claudeResumeFlag, "native-0",
	"--permission-mode", "bypassPermissions",
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func claudeResumeServer(t *testing.T, shimURL string) (*server, *attachmentStore) {
	t.Helper()
	store, err := newAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	control := newTestControlMetadata(t)
	executions, err := newControlAgentExecutionStore(control, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	queue := mustQueuedTurnStore(t)
	attempts, err := newSendAttemptStore(queue.db, queue.path, "test-run", testPromptStore(t, queue.db))
	if err != nil {
		t.Fatal(err)
	}
	turnStore, err := turns.OpenStore(context.Background(), queue.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turnStore.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	queue.turns, attempts.turns = turnStore, turnStore
	s := &server{
		defaults: sessionDefaults{
			cwd:          t.TempDir(), // the New Window default a resume must never adopt
			claudeBin:    "/default/claude",
			claudeShimWS: shimURL,
			allowedTools: "Read,Grep",
			// The shim's spool directory; fixtures seed a spool per hosted id.
			claudeShimSpools: t.TempDir(),
		},
		spoolDir:        t.TempDir(),
		runtimeEvents:   newRuntimeHub(),
		attachments:     store,
		sessions:        map[string]*session{},
		controlMetadata: control,
		agentExecutions: executions,
		queuedTurns:     queue,
		sendAttempts:    attempts,
		turns:           turnStore,
	}
	s.runtime = s
	t.Cleanup(func() {
		s.mu.Lock()
		sessions := make([]*session, 0, len(s.sessions))
		for _, sess := range s.sessions {
			sessions = append(sessions, sess)
		}
		s.mu.Unlock()
		for _, sess := range sessions {
			sess.close()
		}
	})
	return s, store
}

// rememberedClaudeSession is the attachment a detached claude session leaves
// behind: its native id, the cwd it ran in, and its exact spawn posture.
func rememberedClaudeSession(t *testing.T, bin string) attachmentRecord {
	t.Helper()
	return attachmentRecord{
		Provider:       "claude",
		NativeThreadID: claudeResumeThreadID,
		Cwd:            t.TempDir(),
		Model:          "sonnet",
		Bin:            bin,
		Args:           storedPosture,
		AttachedAt:     "2026-07-27T10:00:00Z",
	}
}

func postClaudeResume(t *testing.T, s *server, body string) *httptest.ResponseRecorder {
	t.Helper()
	var request struct {
		Provider string `json:"provider"`
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal([]byte(body), &request)
	seedHostedAttachmentFixture(t, s, request.Provider, request.ThreadID)
	rec := httptest.NewRecorder()
	s.handleSessionCreate(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body)))
	return rec
}

// seedHostedAttachmentFixture is explicit test migration setup.  It converts
// a legacy attachment record into canonical hosted facts before a resume path
// is exercised; production never does this ad hoc at request time.
// ensureClaudeTestSpool gives a Claude fixture the shim spool a real shim
// always has: present, with nothing flushed yet when Nano scans it, so the
// fake shim's scripted lines all replay after the floor.
func ensureClaudeTestSpool(t *testing.T, s *server, provider, hostedID string) {
	t.Helper()
	if provider != "claude" || hostedID == "" {
		return
	}
	if s.defaults.claudeShimSpools == "" {
		s.defaults.claudeShimSpools = t.TempDir()
	}
	path := filepath.Join(s.defaults.claudeShimSpools, hostedID+".jsonl")
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedHostedAttachmentFixture(t *testing.T, s *server, provider, nativeID string) {
	t.Helper()
	record, ok := s.attachmentFor(provider, nativeID)
	if ok && record.HostedThreadID != "" {
		ensureClaudeTestSpool(t, s, provider, record.HostedThreadID)
	}
	if !ok || record.HostedThreadID != "" {
		return
	}
	hostedID := "nht_fixture_" + agentHash([]byte(provider + "\x00" + nativeID))[:16]
	ensureClaudeTestSpool(t, s, provider, hostedID)
	record.HostedThreadID = hostedID
	s.attachments.remember(record)
	if _, err := s.agentExecutions.ensureSessionWithIdentity(hostedID, provider, presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.controlMetadata.claimNativeID(hostedID, provider, nativeID, nativeThreadIDProvisional(provider, nativeID)); err != nil {
		t.Fatal(err)
	}
	generation, err := s.controlMetadata.beginTransportBinding(hostedID)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCwd := record.Cwd
	if fixtureCwd == "" {
		// Attachment JSON no longer owns cwd. This is explicit fixture setup,
		// not a production fallback to the New Window default.
		fixtureCwd = s.defaults.cwd
	}
	fixtureModel := record.Model
	if fixtureModel == "" {
		// rememberedClaudeSession is the pre-cutover source fixture. remember()
		// correctly scrubs its legacy Model field before this helper runs, so
		// reproduce the value that the startup migration imported first.
		fixtureModel = "sonnet"
	}
	_, fixturePermission, _ := argvStripPair(record.Args, "--permission-mode")
	for _, field := range []postureFieldValue{{field: "cwd", value: fixtureCwd}, {field: postureFieldModel, value: fixtureModel}, {field: postureFieldPermissionMode, value: fixturePermission}} {
		if field.value == "" {
			continue
		}
		key := fmt.Sprintf("fixture:%s:%d:%s:%s", hostedID, generation, field.field, field.value)
		if _, err := s.controlMetadata.appendEvent(metadataEvent{HostedThreadID: hostedID, Field: field.field, Value: field.value, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, AttachmentGeneration: generation, SourceEventKey: key}); err != nil {
			t.Fatal(err)
		}
	}
}

func resumeBody(threadID string) string {
	return `{"provider":"claude","mode":"resume","threadId":"` + threadID + `"}`
}

func spawnedSummary(t *testing.T, rec *httptest.ResponseRecorder) sessionSummary {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

// waitForResumedThreadID waits until the session carries the given native id.
// A claude session reaches "ready" as soon as its transport is up
// (docs/decisions/claude-ready-on-transport.md), so status no longer marks the
// handshake — a `--resume` successor's identity is the thing to wait on, and
// waiting on it also proves the child was actually spawned.
func waitForResumedThreadID(t *testing.T, s *server, sessionID, want string) sessionSummary {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		sess := s.sessions[sessionID]
		s.mu.Unlock()
		if sess == nil {
			t.Fatalf("session %s disappeared while waiting for native id %q", sessionID, want)
		}
		if sum := sess.summary(); sum.ThreadID == want {
			return sum
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s never announced native id %q", sessionID, want)
	return sessionSummary{}
}

// startArgs returns the argv of the shim's single session/start call.
func startArgs(t *testing.T, shim *fakeShim) (bin string, args []string, cwd string) {
	t.Helper()
	starts := shim.callsOf(shimMethodStart)
	if len(starts) != 1 {
		t.Fatalf("session/start calls = %d, want 1", len(starts))
	}
	var params struct {
		Bin  string   `json:"bin"`
		Args []string `json:"args"`
		Cwd  string   `json:"cwd"`
	}
	if err := json.Unmarshal(starts[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	return params.Bin, params.Args, params.Cwd
}

// seedHistoricalHostedClaudeWithoutAttachment reproduces a catalog-imported
// Claude thread: durable identity and canonical posture exist, but Nano never
// wrote an attachment record containing the transport argv. This is the exact
// shape historical threads had when the shared resume identity fix first made
// them admissible.
func seedHistoricalHostedClaudeWithoutAttachment(t *testing.T, s *server) string {
	t.Helper()
	const hostedID = "nht_historical_claude_no_attachment"
	if _, err := s.agentExecutions.ensureSessionWithIdentity(hostedID, "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.bindSession(hostedID, "old-nano", claudeResumeThreadID, sessionStatusExited, presentationShared); err != nil {
		t.Fatal(err)
	}
	for _, event := range []metadataEvent{
		{HostedThreadID: hostedID, Field: "cwd", Value: t.TempDir(), Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessHistorical, SourceEventKey: "historical-no-attachment:cwd"},
		{HostedThreadID: hostedID, Field: postureFieldModel, Value: "claude-fable-5", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessHistorical, SourceEventKey: "historical-no-attachment:model"},
		{HostedThreadID: hostedID, Field: postureFieldEffort, Value: "high", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessHistorical, SourceEventKey: "historical-no-attachment:effort"},
		{HostedThreadID: hostedID, Field: postureFieldPermissionMode, Value: "bypassPermissions", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessHistorical, SourceEventKey: "historical-no-attachment:permission"},
	} {
		if _, err := s.controlMetadata.appendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	s.defaults.apiOrigin = "http://127.0.0.1:7999"
	return hostedID
}

func assertHistoricalClaudeResumeArgs(t *testing.T, args []string, hostedID string) {
	t.Helper()
	for _, pair := range [][2]string{
		{"--input-format", "stream-json"},
		{"--output-format", "stream-json"},
		{"--permission-prompt-tool", "stdio"},
		{claudeModelFlag, "claude-fable-5"},
		{"--effort", "high"},
		{"--permission-mode", "bypassPermissions"},
		{claudeResumeFlag, claudeResumeThreadID},
	} {
		if !containsArgPair(args, pair[0], pair[1]) {
			t.Fatalf("historical resume argv missing %s %s: %#v", pair[0], pair[1], args)
		}
	}
	for _, flag := range []string{"-p", "--verbose", "--include-partial-messages"} {
		if !containsArg(args, flag) {
			t.Fatalf("historical resume argv missing %s: %#v", flag, args)
		}
	}
	identity := singletonValue(t, args, claudeIdentityFlag)
	if !strings.Contains(identity, "hostedThreadId: "+hostedID) || !strings.Contains(identity, "apiOrigin: http://127.0.0.1:7999") {
		t.Fatalf("historical resume identity = %q, want hosted identity and API origin", identity)
	}
}

func TestClaudeHistoricalHostedResumePreviewRebuildsTransportBaseline(t *testing.T) {
	s, _ := claudeResumeServer(t, "")
	hostedID := seedHistoricalHostedClaudeWithoutAttachment(t, s)
	rec := httptest.NewRecorder()
	s.handleSpawnPreview(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/preview", strings.NewReader(resumeBody(claudeResumeThreadID))))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var preview struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Argv) == 0 || preview.Argv[0] != resolveClaudeBin("/default/claude") {
		t.Fatalf("preview argv = %#v, want resolved Claude binary first", preview.Argv)
	}
	assertHistoricalClaudeResumeArgs(t, preview.Argv[1:], hostedID)
}

// The real acceptance surface is not "status ready"; Claude 2.1.220 reports
// transport readiness before its first init. Prove the historical fallback can
// accept a new prompt, receive an assistant message, and close the turn on a
// result frame while retaining the same durable hosted identity.
func TestClaudeHistoricalHostedResumeCompletesNewPrompt(t *testing.T) {
	const prompt = "continue the historical thread"
	const answer = "historical resume works"
	shim := newFakeShim(t, func(c *shimConn) {
		for {
			call, ok := c.next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:
				c.replyError(call, shimCodeSessionNotFound, "session_not_found")
			case shimMethodStart:
				c.reply(call, map[string]any{"shimSessionId": "shim-2"})
			case shimMethodInput:
				var params struct {
					Line string `json:"line"`
				}
				if err := json.Unmarshal(call.Params, &params); err != nil {
					c.t.Fatal(err)
				}
				const turnID = "claude:shim-2:1"
				c.reply(call, map[string]any{"turnId": turnID})
				if strings.Contains(params.Line, prompt) {
					time.Sleep(5 * time.Millisecond)
					c.lineForTurn(1, "recv", turnID, claudeSuccessorInitLine)
					c.lineForTurn(2, "recv", turnID, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"`+answer+`"}]}}`)
					c.lineForTurn(3, "recv", turnID, `{"type":"result","subtype":"success"}`)
				}
			default:
				c.reply(call, nil)
			}
		}
	})
	s, _ := claudeResumeServer(t, shim.wsURL())
	hostedID := seedHistoricalHostedClaudeWithoutAttachment(t, s)
	sum := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	if sum.HostedThreadID != hostedID || sum.Status != "ready" {
		t.Fatalf("resumed summary = %#v, want ready historical hosted thread %s", sum, hostedID)
	}

	turn := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(turn, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sum.ID+"/turn", strings.NewReader(`{"text":"`+prompt+`","clientActionId":"historical-resume-prompt"}`)))
	if turn.Code != http.StatusOK {
		t.Fatalf("turn status = %d, body = %s", turn.Code, turn.Body.String())
	}
	waitForResumedThreadID(t, s, sum.ID, "native-2")
	waitForClaude(t, "historical resumed prompt completion", func() bool {
		s.mu.Lock()
		sess := s.sessions[sum.ID]
		s.mu.Unlock()
		if sess == nil || sess.summary().TurnActive {
			return false
		}
		for _, event := range claudeDataWireEvents(sess) {
			if event.Dir == "recv" && strings.Contains(string(event.Msg), answer) {
				return true
			}
		}
		return false
	})
	_, args, _ := startArgs(t, shim)
	assertHistoricalClaudeResumeArgs(t, args, hostedID)
}

// resumeScript answers the whole resume conversation: attach as the test
// dictates, then session/start for the fallback rung.
func resumeScript(attach func(c *shimConn, call shimCall)) func(*shimConn) {
	return func(c *shimConn) {
		for {
			call, ok := c.next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:
				attach(c, call)
			case shimMethodStart:
				c.reply(call, map[string]any{"shimSessionId": "shim-2"})
				c.line(1, "recv", claudeSuccessorInitLine)
			default:
				c.reply(call, nil)
			}
		}
	}
}

// A shim attach that timed out used to fall back to --resume while the
// original child was still alive, leaving two claude processes on one thread.
// Once the attach reached the shim, only its answer that it holds no such
// session proves the child is gone; any other failure must leave the thread
// detached with nothing spawned. Boundary: the resume ladder against a shim
// that never answers session/attach, or drops the connection on it.
func TestClaudeResumeStaysDetachedWhenShimAttachFailsUnproven(t *testing.T) {
	for name, attach := range map[string]func(*shimConn, shimCall){
		"timeout":            func(*shimConn, shimCall) {},
		"dropped connection": func(c *shimConn, _ shimCall) { _ = c.conn.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			shim := newFakeShim(t, resumeScript(attach))
			s, store := claudeResumeServer(t, shim.wsURL())
			store.remember(rememberedClaudeSession(t, "/stored/claude"))

			resp := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
			if resp.Code < 400 || !strings.Contains(resp.Body.String(), errClaudeRecoveryUnproven.Error()) {
				t.Fatalf("resume after a failed attach = %d %s, want an unproven-recovery refusal", resp.Code, resp.Body.String())
			}
			if starts := shim.callsOf(shimMethodStart); len(starts) != 0 {
				t.Fatalf("a failed attach started a second child: %+v", starts)
			}
			if _, kept := store.get("claude", claudeResumeThreadID); !kept {
				t.Fatal("the attachment record must stay for a later retry")
			}
		})
	}
}

// Rung 1: the shim still hosts the child. Resume attaches to it — no new child,
// same native id, identity re-derived from the replayed init — and the record
// is refreshed rather than retired.
func TestClaudeResumeAttachesLiveShimSession(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "lastSeq": 1,
		})
		c.line(1, "recv", claudeInitLine)
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	rec := rememberedClaudeSession(t, "/stored/claude")
	store.remember(rec)

	sum := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	if sum.Provider != "claude" || sum.ThreadID != claudeResumeThreadID || sum.Status != "ready" {
		t.Fatalf("summary = %#v, want the attached thread already ready", sum)
	}
	if sum.Cwd != s.defaults.cwd {
		t.Fatalf("cwd = %q, want explicitly seeded canonical cwd %q", sum.Cwd, s.defaults.cwd)
	}
	if starts := shim.callsOf(shimMethodStart); len(starts) != 0 {
		t.Fatalf("attach path spawned a child: %+v", starts)
	}

	var attachParams struct {
		NativeSessionID string `json:"nativeSessionId"`
		FromSeq         int64  `json:"fromSeq"`
	}
	calls := shim.callsOf(shimMethodAttach)
	if len(calls) != 1 {
		t.Fatalf("session/attach calls = %d, want 1", len(calls))
	}
	if err := json.Unmarshal(calls[0].Params, &attachParams); err != nil {
		t.Fatal(err)
	}
	// The fixture spool is present but nothing was flushed when Nano scanned
	// it, so the floor is its end: 0. Everything the shim holds replays.
	if attachParams.NativeSessionID != claudeResumeThreadID || attachParams.FromSeq != 0 {
		t.Fatalf("session/attach params = %+v, want the idle native id attached at the scanned spool's end", attachParams)
	}

	// Re-attach re-remembers; it must not retire the record it just attached to.
	got, ok := store.get("claude", claudeResumeThreadID)
	if !ok {
		t.Fatal("attach forgot the record it re-attached to")
	}
	if got.Bin != "/stored/claude" || singletonValue(t, got.Args, claudeResumeFlag) != "native-0" || singletonValue(t, got.Args, "--permission-mode") != "bypassPermissions" || !containsArg(got.Args, "-p") || !containsArg(got.Args, "--verbose") || got.Cwd != "" || got.Model != "" || got.TurnServiceTier != "" {
		t.Fatalf("record after attach retained canonical posture fields: %+v", got)
	}
	// Attaching is not a succession — the id never changed, so there is no
	// lineage to record.
	if got.ContinuedFrom != "" {
		t.Fatalf("record after attach = %+v, want no continuedFrom", got)
	}
}

// Rung 2: the shim no longer knows the session (its own restart took the child
// with it). Resume respawns through the shim with the stored posture, the stale
// `--resume` pair stripped, in canonical cwd — and the superseded record is
// retired once the successor's init lands.
func TestClaudeResumeFallsBackToResumeFlagOnSessionNotFound(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.replyError(call, shimCodeSessionNotFound, "session_not_found")
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	rec := rememberedClaudeSession(t, "/stored/claude")
	store.remember(rec)

	sum := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	sum = waitForResumedThreadID(t, s, sum.ID, "native-2")
	if sum.Status != "ready" {
		t.Fatalf("summary = %#v, want the successor ready", sum)
	}

	bin, args, cwd := startArgs(t, shim)
	want := []string{"-p", "--verbose", "--permission-mode", "bypassPermissions", claudeResumeFlag, claudeResumeThreadID, claudeModelFlag, "sonnet"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("fallback args = %#v, want %#v", args, want)
	}
	if bin != "/stored/claude" || cwd != s.defaults.cwd {
		t.Fatalf("fallback bin = %q cwd = %q, want canonical fixture posture", bin, cwd)
	}
	// The successor's init retires the superseded record and remembers itself
	// with the argv actually used — stripping keeps that argv from stacking.
	waitForClaude(t, "superseded record retired", func() bool {
		_, stale := store.get("claude", claudeResumeThreadID)
		return !stale
	})
	successor, ok := store.get("claude", "native-2")
	if !ok {
		t.Fatal("successor session was never remembered")
	}
	if !reflect.DeepEqual(successor.Args, want) || successor.Cwd != "" || successor.Model != "" || successor.TurnServiceTier != "" {
		t.Fatalf("successor record retained canonical posture fields: %+v", successor)
	}
	// The identity fork, captured: the retired id survives on the successor as
	// the only durable trace that the two are one conversation.
	if successor.ContinuedFrom != claudeResumeThreadID {
		t.Fatalf("successor continuedFrom = %q, want the predecessor %q", successor.ContinuedFrom, claudeResumeThreadID)
	}
}

// A never-prompted thread has no native id — the shim handle in its
// provisional record (adoptDurableIdentity) is the only address it has. Resume
// attaches by it, keeps the provisional identity (an empty spool replays no
// init), and leaves the record for the real init to retire later.
func TestClaudeResumeProvisionalAttachesByShimHandle(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-7", "nativeSessionId": "",
			"state": "running", "lastSeq": 0,
		})
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-7", "lastSeq": 0})
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	rec := rememberedClaudeSession(t, "/stored/claude")
	rec.NativeThreadID = "shim-7"
	rec.ShimSessionID = "shim-7"
	store.remember(rec)

	sum := spawnedSummary(t, postClaudeResume(t, s, resumeBody("shim-7")))
	if sum.ThreadID != "shim-7" || sum.Status != "ready" {
		t.Fatalf("summary = %#v, want the provisional identity kept and ready", sum)
	}
	calls := shim.callsOf(shimMethodAttach)
	if len(calls) != 1 {
		t.Fatalf("session/attach calls = %d, want 1", len(calls))
	}
	var attachParams struct {
		ShimSessionID   string `json:"shimSessionId"`
		NativeSessionID string `json:"nativeSessionId"`
	}
	if err := json.Unmarshal(calls[0].Params, &attachParams); err != nil {
		t.Fatal(err)
	}
	if attachParams.ShimSessionID != "shim-7" {
		t.Fatalf("session/attach params = %+v, want the stored shim handle", attachParams)
	}
	if starts := shim.callsOf(shimMethodStart); len(starts) != 0 {
		t.Fatalf("spawned a child for a live provisional session: %+v", starts)
	}
	if _, ok := store.get("claude", "shim-7"); !ok {
		t.Fatal("the provisional record must survive the re-attach")
	}
}

// The shim lost the never-prompted child (its own restart took it). There is no
// transcript to `--resume`, so the resume refuses instead of spawning a child
// that could only fail — and the record is retired: an empty conversation lost
// is nothing lost, while keeping it would offer a resume that can never work.
func TestClaudeResumeProvisionalGoneRetiresTheRecord(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.replyError(call, shimCodeSessionNotFound, "session_not_found")
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	rec := rememberedClaudeSession(t, "/stored/claude")
	rec.NativeThreadID = "shim-7"
	rec.ShimSessionID = "shim-7"
	store.remember(rec)

	resp := postClaudeResume(t, s, resumeBody("shim-7"))
	if resp.Code == http.StatusOK {
		t.Fatalf("resume of a lost never-prompted thread must fail, got %s", resp.Body.String())
	}
	if starts := shim.callsOf(shimMethodStart); len(starts) != 0 {
		t.Fatalf("respawned a thread that has no transcript: %+v", starts)
	}
	if _, ok := store.get("claude", "shim-7"); ok {
		t.Fatal("the unrecoverable provisional record must be retired")
	}
}

// A provisional thread whose prompt was, or may have been, handed to the
// provider is not proven empty by its lost child: the resume fails without
// retiring the record that recovery still needs. A queued prompt counts as
// never delivered only when its outbox row and its own journal row agree.
func TestClaudeResumeProvisionalGoneKeepsARecordWhosePromptMayHaveBeenDelivered(t *testing.T) {
	for _, tc := range []struct {
		name    string
		deliver func(t *testing.T, s *server, hostedID string)
	}{
		{"claimed for delivery", func(t *testing.T, s *server, hostedID string) {
			if _, claimed, err := s.turns.Claim(context.Background(), turns.ClaimRequest{HostedThreadID: hostedID, Token: "claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
				t.Fatalf("claim = %v, %v", claimed, err)
			}
		}},
		{"queued outbox row with a retained provider turn", func(t *testing.T, s *server, hostedID string) {
			if _, err := s.queuedTurns.db.Exec(`UPDATE queued_turns SET provider_turn_id='turn-retained' WHERE client_action_id='provisional-1'`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
				c.replyError(call, shimCodeSessionNotFound, "session_not_found")
			}))
			s, store := claudeResumeServer(t, shim.wsURL())
			rec := rememberedClaudeSession(t, "/stored/claude")
			rec.NativeThreadID = "shim-7"
			rec.ShimSessionID = "shim-7"
			store.remember(rec)
			// The hosted id seedHostedAttachmentFixture gives this record.
			hostedID := "nht_fixture_" + agentHash([]byte("claude\x00shim-7"))[:16]
			if _, _, err := s.turns.Submit(context.Background(), turns.Submission{HostedThreadID: hostedID, Provider: "claude", NativeThreadID: "shim-7", ActionID: "provisional-1",
				ProviderText: "hi", PresentationText: "hi", TextHash: "text", RequestHash: "request", Origin: turns.HumanOrigin, CreatedAt: "2026-10-07T00:00:00Z"}); err != nil {
				t.Fatal(err)
			}
			tc.deliver(t, s, hostedID)

			resp := postClaudeResume(t, s, resumeBody("shim-7"))
			if resp.Code == http.StatusOK {
				t.Fatalf("resume of a lost provisional child must fail, got %s", resp.Body.String())
			}
			if starts := shim.callsOf(shimMethodStart); len(starts) != 0 {
				t.Fatalf("respawned a provisional thread: %+v", starts)
			}
			if _, ok := store.get("claude", "shim-7"); !ok {
				t.Fatal("a provisional record whose prompt may have been delivered must survive")
			}
		})
	}
}

// A provisional thread whose spool proves its own prompt's session after the
// shim lost the child: the resume records that pair at the claim owner before
// any attach or rung, then continues as that native, re-keying the attachment
// record with the same handle, so the --resume rung can start. If the pair
// cannot be recorded, nothing proceeds and the handle's record stays.
// Boundary: the resume endpoint on a schema-23 control store and a real spool.
func TestClaudeResumeAdoptsTheSpoolProvenNativeOfAProvisionalThread(t *testing.T) {
	// The last three start where a resume was interrupted after the pair
	// committed, which retires the handle, but before its attachment record
	// was re-keyed: a later resume enters through the public admission with
	// the record still under the handle, beside the native's or alone.
	for _, tc := range []struct {
		name      string
		conflict  bool
		committed bool
		both      bool
		succeeded bool
		// The native's own record resumes first, onto a live child.
		nativeFirst bool
	}{
		{name: "pair recorded"},
		{name: "pair refused", conflict: true},
		{name: "pair committed, record left under the handle", committed: true},
		{name: "pair committed, records under both", committed: true, both: true},
		{name: "pair committed, native since succeeded", committed: true, succeeded: true},
		{name: "pair committed, records under both, native resumed first", committed: true, both: true, nativeFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			// What the shim is asked to attach, and what the claim owner had
			// recorded at that moment.
			var control *controlMetadataStore
			var hostedID string
			type attachSeen struct{ handle, native, current, resolved string }
			var attaches []attachSeen
			var attachMu sync.Mutex
			// The native record as the shim was asked to attach.
			var attachStore *attachmentStore
			var nativeAtAttach attachmentRecord
			var sentLine string // the spool's first frame, which a live child replays
			shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
				var params struct {
					ShimSessionID   string `json:"shimSessionId"`
					NativeSessionID string `json:"nativeSessionId"`
				}
				_ = json.Unmarshal(call.Params, &params)
				seen := attachSeen{handle: params.ShimSessionID, native: params.NativeSessionID}
				var resolved sql.NullString
				_ = control.db.QueryRow(`SELECT current_native_id FROM hosted_threads WHERE hosted_thread_id=?`, hostedID).Scan(&seen.current)
				_ = control.db.QueryRow(`SELECT resolved_native_id FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND native_thread_id='cs_handle7'`, hostedID).Scan(&resolved)
				seen.resolved = resolved.String
				attachMu.Lock()
				attaches = append(attaches, seen)
				nativeAtAttach, _ = attachStore.get("claude", "native-real")
				attachMu.Unlock()
				if tc.nativeFirst {
					c.reply(call, map[string]any{"shimSessionId": params.ShimSessionID, "nativeSessionId": "native-real", "state": "running", "lastSeq": 2})
					c.line(1, "send", sentLine)
					c.line(2, "recv", `{"type":"system","subtype":"init","session_id":"native-real"}`)
					c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": params.ShimSessionID, "lastSeq": 2})
					return
				}
				c.replyError(call, shimCodeSessionNotFound, "session_not_found")
			}))
			s, store := claudeResumeServer(t, shim.wsURL())
			attachStore = store
			s.controlMetadata, s.agentExecutions = newTestHostedControl(t)
			control = s.controlMetadata
			rec := rememberedClaudeSession(t, "/stored/claude")
			rec.NativeThreadID, rec.ShimSessionID = "cs_handle7", "cs_handle7"
			store.remember(rec)
			seedHostedAttachmentFixture(t, s, "claude", "cs_handle7")
			hostedID = "nht_fixture_" + agentHash([]byte("claude\x00cs_handle7"))[:16]
			anchor, turn, handle := submissionEventID(hostedID, "first-prompt"), "claude:"+hostedID+":1", "cs_handle7"
			if _, _, err := s.turns.Submit(ctx, turns.Submission{HostedThreadID: hostedID, Provider: "claude", NativeThreadID: handle, ActionID: "first-prompt",
				ProviderText: "hi", PresentationText: "hi", TextHash: "text", RequestHash: "request", Origin: turns.HumanOrigin, CreatedAt: "2026-10-07T00:00:00Z"}); err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := s.turns.Claim(ctx, turns.ClaimRequest{HostedThreadID: hostedID, Token: "claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
				t.Fatalf("claim = %v, %v", claimed, err)
			}
			binding := turns.Binding{EventID: anchor, Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &handle, NativeTurnID: &turn}}
			if _, err := s.turns.Accepted(ctx, turns.Acceptance{ActionID: "first-prompt", ClaimToken: "claim", AttachmentEpoch: 1, Binding: binding}); err != nil {
				t.Fatal(err)
			}
			sentLine = `{"type":"user","uuid":"` + anchor + `","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`
			writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hostedID,
				sendFrame(1, sentLine).turn(turn),
				recvFrame(2, `{"type":"system","subtype":"init","session_id":"native-real"}`).turn(turn))
			if tc.conflict {
				if _, err := s.controlMetadata.db.Exec(`UPDATE hosted_thread_native_ids SET resolved_native_id='native-other' WHERE hosted_thread_id=? AND native_thread_id=?`, hostedID, handle); err != nil {
					t.Fatal(err)
				}
			}
			if tc.committed {
				if err := control.claimNativeIDResolving(hostedID, "claude", "native-real", false, &nativeResolution{Handle: handle, Native: "native-real"}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.both {
				kept, ok := store.get("claude", handle)
				if !ok {
					t.Fatal("handle record missing")
				}
				// The handle's record holds the first turn's anchor; the native's
				// record then saw a later turn of its own.
				kept.TurnStartedAtMs, kept.TurnAnchorSeq = 1790000000000, 7
				store.remember(kept)
				kept.NativeThreadID, kept.TurnStartedAtMs, kept.TurnAnchorSeq = "native-real", 1791000000000, 42
				store.remember(kept)
			}
			if tc.succeeded {
				if err := control.claimNativeID(hostedID, "claude", "native-2", false); err != nil {
					t.Fatal(err)
				}
			}

			if tc.both {
				// Previewing the resume resolves the same request but resumes
				// nothing, so it leaves both records where they are.
				before, err := os.ReadFile(store.path)
				if err != nil {
					t.Fatal(err)
				}
				preview := httptest.NewRecorder()
				s.handleSpawnPreview(preview, httptest.NewRequest(http.MethodPost, "/api/sessions/preview", strings.NewReader(resumeBody(handle))))
				after, err := os.ReadFile(store.path)
				if err != nil {
					t.Fatal(err)
				}
				_, handleThere := store.get("claude", handle)
				if _, nativeThere := store.get("claude", "native-real"); preview.Code != http.StatusOK || !handleThere || !nativeThere || !bytes.Equal(before, after) {
					t.Fatalf("preview %d %s changed the records: handle=%v native=%v file changed=%v", preview.Code, preview.Body.String(), handleThere, nativeThere, !bytes.Equal(before, after))
				}
			}
			var first *httptest.ResponseRecorder
			if tc.nativeFirst {
				first = postClaudeResume(t, s, resumeBody("native-real"))
			}
			resp := postClaudeResume(t, s, resumeBody(handle))
			var current string
			if err := s.controlMetadata.db.QueryRow(`SELECT current_native_id FROM hosted_threads WHERE hosted_thread_id=?`, hostedID).Scan(&current); err != nil {
				t.Fatal(err)
			}
			_, handleKept := store.get("claude", handle)
			_, nativeKept := store.get("claude", "native-real")
			starts := shim.callsOf(shimMethodStart)
			attachMu.Lock()
			defer attachMu.Unlock()
			if tc.conflict {
				if resp.Code == http.StatusOK || current != handle || !handleKept || nativeKept || len(starts) != 0 || len(attaches) != 0 {
					t.Fatalf("a refused pair proceeded: code=%d current=%q handleKept=%v nativeKept=%v starts=%d attaches=%+v", resp.Code, current, handleKept, nativeKept, len(starts), attaches)
				}
				return
			}
			if tc.nativeFirst {
				// The handle's record resumes the live native session, and the
				// re-key it was left by is finished: the record goes. The live
				// attach owns the native record's anchor from here.
				native, nativeOK := store.get("claude", "native-real")
				if first.Code != http.StatusOK || resp.Code != http.StatusConflict || handleKept || !nativeOK || native.ShimSessionID != handle || len(attaches) != 1 || len(starts) != 0 {
					t.Fatalf("native first: %d %s, then %d %s; handleKept=%v native=%+v attaches=%+v starts=%d", first.Code, first.Body.String(), resp.Code, resp.Body.String(), handleKept, native, attaches, len(starts))
				}
				return
			}
			if tc.succeeded {
				// The handle's native is no longer the thread's; the stale record
				// must not resume anything.
				if resp.Code == http.StatusOK || current != "native-2" || !handleKept || len(starts) != 0 || len(attaches) != 0 {
					t.Fatalf("a stale handle resumed: code=%d current=%q handleKept=%v starts=%d attaches=%+v", resp.Code, current, handleKept, len(starts), attaches)
				}
				return
			}
			// The pair was recorded and in effect before the shim was asked to
			// attach anything, and the attach named the proven native.
			if len(attaches) != 1 || attaches[0] != (attachSeen{handle: handle, native: "native-real", current: "native-real", resolved: "native-real"}) {
				t.Fatalf("attach saw %+v (resume %d %s); want one attach to the same child's handle as native-real after the pair was recorded", attaches, resp.Code, resp.Body.String())
			}
			var resolved sql.NullString
			if err := s.controlMetadata.db.QueryRow(`SELECT resolved_native_id FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND native_thread_id=?`, hostedID, handle).Scan(&resolved); err != nil {
				t.Fatal(err)
			}
			// The handle's resolution is permanent. The --resume child then
			// announces its own successor native, and ordinary succession makes
			// that current and retires the records before it.
			if resolved.String != "native-real" {
				t.Fatalf("resolved=%v; want the spool-proven native kept", resolved)
			}
			if handleKept {
				t.Fatal("the handle's attachment record survived the re-key")
			}
			if tc.both && (nativeAtAttach.ShimSessionID != handle || nativeAtAttach.TurnStartedAtMs != 1791000000000 || nativeAtAttach.TurnAnchorSeq != 42) {
				t.Fatalf("native record at attach = %+v; want its own handle and later anchor kept", nativeAtAttach)
			}
			waitForCatalogWithin(t, 10*time.Second, func() bool {
				var fresh string
				err := control.db.QueryRow(`SELECT current_native_id FROM hosted_threads WHERE hosted_thread_id=?`, hostedID).Scan(&fresh)
				return err == nil && fresh == "native-2"
			})
			if len(starts) != 1 {
				t.Fatalf("--resume rung starts = %d; want one start as the proven native", len(starts))
			}
			var start struct {
				Args []string `json:"args"`
			}
			if err := json.Unmarshal(starts[0].Params, &start); err != nil {
				t.Fatal(err)
			}
			if i := slices.Index(start.Args, claudeResumeFlag); i < 0 || i+1 >= len(start.Args) || start.Args[i+1] != "native-real" {
				t.Fatalf("--resume rung args = %v; want %s native-real", start.Args, claudeResumeFlag)
			}
		})
	}
}

// Rung 2 with no shim at all: both the attach and the respawn fall through to a
// direct child, which works but is warned about — that session has no
// durability, so nothing is remembered, while the superseded record still goes.
func TestClaudeResumeFallsBackToDirectChildWhenShimUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := "ws" + strings.TrimPrefix(dead.URL, "http")
	dead.Close()

	logs := &syncBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s, store := claudeResumeServer(t, deadURL)
	rec := rememberedClaudeSession(t, fakeClaudeBinary(t))
	store.remember(rec)

	sum := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	sum = waitForResumedThreadID(t, s, sum.ID, "native-direct")
	if sum.Status != "ready" {
		t.Fatalf("summary = %#v, want the direct child ready", sum)
	}
	if got := logs.String(); !strings.Contains(got, "shim attach failed") ||
		!strings.Contains(got, "WARNING: claude shim unreachable") {
		t.Fatalf("log missing the fallback warnings: %q", got)
	}
	waitForClaude(t, "superseded record retired", func() bool {
		_, stale := store.get("claude", claudeResumeThreadID)
		return !stale
	})
	if got := store.detachedExcept(nil); len(got) != 0 {
		t.Fatalf("a direct child was remembered as durable: %+v", got)
	}
}

// Two resumes of one thread: the second is the 409, naming the session
// to jump to, and never reaches the shim.
func TestClaudeResumeSecondAttachIs409(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "lastSeq": 1,
		})
		c.line(1, "recv", claudeInitLine)
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	store.remember(rememberedClaudeSession(t, "/stored/claude"))

	winner := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	second := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if second.Code != http.StatusConflict {
		t.Fatalf("second resume status = %d, body = %s", second.Code, second.Body.String())
	}
	if ct := second.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	want := `{"error":{"code":"thread_attached","message":"Thread is already attached","details":{"existingSessionId":"` + winner.ID + `"}}}`
	if got := strings.TrimSpace(second.Body.String()); got != want {
		t.Fatalf("409 body = %q, want %q", got, want)
	}
	if calls := shim.callsOf(shimMethodAttach); len(calls) != 1 {
		t.Fatalf("session/attach calls = %d, want 1 (the guard must fire before the shim)", len(calls))
	}
}

// The same guard on the rung that actually needs it. A `--resume` child
// announces its own id only after its first turn (CLI 2.1.220), so a session
// that waited for that would be invisible to the dual-attachment scan: the
// Explorer row would still offer Resume, and a second click would spawn a
// second child racing the first over one transcript. Identity at spawn closes
// that window — the session claims the thread it is resuming before any wire
// traffic, exactly as codex pre-claims its routing slot.
func TestClaudeSecondResumeOnRespawnRungIs409(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) {
		for {
			call, ok := c.next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:
				c.replyError(call, shimCodeSessionNotFound, "session_not_found")
			case shimMethodStart:
				// No init line: the child has not been sent a turn, so it has not
				// said who it is — the real CLI's behavior, and the whole window
				// this guard has to survive.
				c.reply(call, map[string]any{"shimSessionId": "shim-2"})
			default:
				c.reply(call, nil)
			}
		}
	})
	s, store := claudeResumeServer(t, shim.wsURL())
	store.remember(rememberedClaudeSession(t, "/stored/claude"))

	winner := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	if winner.ThreadID != claudeResumeThreadID || winner.Status != "ready" {
		t.Fatalf("summary = %#v, want the resumed thread claimed at spawn and ready on transport", winner)
	}
	second := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if second.Code != http.StatusConflict {
		t.Fatalf("second resume status = %d, body = %s", second.Code, second.Body.String())
	}
	want := `{"error":{"code":"thread_attached","message":"Thread is already attached","details":{"existingSessionId":"` + winner.ID + `"}}}`
	if got := strings.TrimSpace(second.Body.String()); got != want {
		t.Fatalf("409 body = %q, want %q", got, want)
	}
	if starts := shim.callsOf(shimMethodStart); len(starts) != 1 {
		t.Fatalf("session/start calls = %d, want 1 — the second resume spawned another child", len(starts))
	}
	// And the catalog overlay says the same thing, so the row offers a jump
	// rather than a Resume that would fork the conversation.
	rt, ok := s.RuntimeFor("claude", claudeResumeThreadID)
	if !ok || !rt.Attached || rt.SessionID != winner.ID {
		t.Fatalf("runtime overlay = %+v (ok = %v), want the thread attached to %s", rt, ok, winner.ID)
	}
}

// The guard above must hold for the living only. A resume that dies pre-init
// (purged transcript is the canonical case) leaves an exited session still
// holding the thread id it seeded at spawn; matching that corpse would turn
// every later Resume click into a jump to the dead session instead of a retry.
func TestClaudeFailedResumeDoesNotBlockTheRetry(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) {
		for {
			call, ok := c.next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:
				c.replyError(call, shimCodeSessionNotFound, "session_not_found")
			case shimMethodStart:
				c.reply(call, map[string]any{"shimSessionId": "shim-2"})
				c.line(1, "recv", claudeMissingTranscriptResult)
			default:
				c.reply(call, nil)
			}
		}
	})
	s, _ := claudeResumeServer(t, shim.wsURL())
	seedCanonicalResumeOwner(t, s, "claude", claudeResumeThreadID, s.defaults.cwd)

	body := `{"provider":"claude","mode":"resume","threadId":"` + claudeResumeThreadID + `","cwd":"` + t.TempDir() + `"}`

	first := spawnedSummary(t, postClaudeResume(t, s, body))
	waitForSessionStatus(t, s, first.ID, "exited")

	second := postClaudeResume(t, s, body)
	if second.Code == http.StatusConflict {
		t.Fatalf("retry after a failed resume = 409 (%s), want a fresh attempt", second.Body.String())
	}
	retry := spawnedSummary(t, second)
	if retry.ID == first.ID {
		t.Fatalf("retry reused the dead session %s, want a fresh one", first.ID)
	}
	waitForClaude(t, "the retry ran the ladder again", func() bool {
		return len(shim.callsOf(shimMethodStart)) == 2
	})
}

// The cwd check belongs to the respawn rung alone. Rung 1 re-attaches to a
// child the shim already owns: it reads no transcript off disk, needs no
// working directory, and is the only rung that can keep a mid-flight turn
// alive — so a project directory that has since moved must not be what takes it
// away. (Its companion, TestClaudeResumeRefusesMissingWorkingDirectory, holds
// the respawn rung to the readable refusal.)
func TestClaudeResumeAttachRungIgnoresMissingWorkingDirectory(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "lastSeq": 1,
		})
		c.line(1, "recv", claudeInitLine)
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	rec := rememberedClaudeSession(t, "/stored/claude")
	rec.Cwd = filepath.Join(t.TempDir(), "deleted-project")
	store.remember(rec)

	sum := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	if sum.ThreadID != claudeResumeThreadID || sum.Status != "ready" {
		t.Fatalf("summary = %#v, want the live child attached despite the missing directory", sum)
	}
	if starts := shim.callsOf(shimMethodStart); len(starts) != 0 {
		t.Fatalf("the attach rung spawned a child: %+v", starts)
	}
}

// Lineage outlives the session that recorded it. durableAttachment rebuilds the
// record from the live transport on every attach and cannot know what
// conversation the session continued, so a whole-record write would erase
// continuedFrom — silently, and only on the threads that had been resumed.
func TestClaudeReattachKeepsContinuedFrom(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.replyError(call, shimCodeSessionNotFound, "session_not_found")
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	store.remember(rememberedClaudeSession(t, "/stored/claude"))

	sum := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	waitForResumedThreadID(t, s, sum.ID, "native-2")
	waitForClaude(t, "successor remembered with its lineage", func() bool {
		rec, ok := store.get("claude", "native-2")
		return ok && rec.ContinuedFrom == claudeResumeThreadID
	})
	successor, _ := store.get("claude", "native-2")

	// What a later re-attach writes: the same record, rebuilt from the transport.
	tr := &claudeShimTransport{
		owner: &claudeSession{sess: &session{cwd: successor.Cwd,
			lifecycleState: testLifecycleSnapshot("ready", false)}},
		posture: claudeSpawnPosture{Bin: successor.Bin, Args: successor.Args},
	}
	rewrite, ok := tr.durableAttachment("native-2", successor.Model)
	if !ok {
		t.Fatal("the shim transport refused to build a durable record")
	}
	if rewrite.ContinuedFrom != "" {
		t.Fatalf("durableAttachment invented lineage: %+v", rewrite)
	}
	store.remember(rewrite)

	after, _ := store.get("claude", "native-2")
	if after.ContinuedFrom != claudeResumeThreadID {
		t.Fatalf("record after re-attach = %+v, want continuedFrom %q carried forward", after, claudeResumeThreadID)
	}
}

// The shim reporting the session attached elsewhere (another nano instance) is
// the one rung with no fallback: it surfaces as a plain error, never as a 409
// pointing at a session id this nano does not have.
func TestClaudeResumeAttachedElsewhereIsPlainError(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.replyError(call, shimCodeSessionAttached, "session_attached")
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	store.remember(rememberedClaudeSession(t, "/stored/claude"))

	rec := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want a plain spawn error", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "attached elsewhere") {
		t.Fatalf("error = %q, want it to name the conflict", body)
	}
	if starts := shim.callsOf(shimMethodStart); len(starts) != 0 {
		t.Fatalf("an attached-elsewhere session was respawned anyway: %+v", starts)
	}
}

// The provider transport builder requires an explicit cwd without an attachment.
// Public resume separately requires a canonical owner before reaching this seam.
func TestClaudeResumeTransportWithoutAttachment(t *testing.T) {
	s, _ := claudeResumeServer(t, "")
	_, err := s.resolveClaudeResume(spawnRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID})
	if err == nil || !strings.Contains(err.Error(), "no attachment record") {
		t.Fatalf("err = %v, want a refusal to guess the cwd", err)
	}
	cwd := t.TempDir()
	req, err := s.resolveClaudeResume(spawnRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	args := claudeResumeArgs(req.ClaudeArgs, claudeResumeThreadID)
	want := append(claudeArgs(spawnRequest{AllowedTools: "Read,Grep"}), claudeResumeFlag, claudeResumeThreadID)
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("record-less transport args = %#v, want %#v", args, want)
	}
	if req.Cwd != cwd || req.ClaudeBin != resolveClaudeBin("/default/claude") {
		t.Fatalf("bin = %q cwd = %q, want explicit cwd and configured binary", req.ClaudeBin, req.Cwd)
	}
}

// claudeResumeArgs is the anti-stacking rule on its own: any number of stored
// `--resume` pairs collapse into exactly the current target, appended last.
func TestClaudeResumeArgsStripsStaleResume(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"no stored resume", []string{"-p", "--verbose"}, []string{"-p", "--verbose", "--resume", "t9"}},
		{"strips the pair", storedPosture, []string{"-p", "--verbose", "--permission-mode", "bypassPermissions", "--resume", "t9"}},
		{"repeated pairs", []string{"--resume", "a", "-p", "--resume", "b"}, []string{"-p", "--resume", "t9"}},
		{"dangling flag", []string{"-p", "--resume"}, []string{"-p", "--resume", "t9"}},
		{"empty posture", nil, []string{"--resume", "t9"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeResumeArgs(tc.in, "t9"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("claudeResumeArgs(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestClaudeCanonicalResumeArgsPreservesTransportAllowlist(t *testing.T) {
	stored := []string{
		"-p", "--model", "old-model", "--effort", "low", "--permission-mode", "default",
		"--allowed-tools", "Read", "--allowed-tools", "Grep", claudeResumeFlag, "native-1",
	}
	got := claudeCanonicalResumeArgs(stored, spawnRequest{Model: "canonical-model", Effort: "high", PermissionMode: "bypassPermissions"})
	if containsArgPair(got, claudeModelFlag, "old-model") || containsArgPair(got, "--effort", "low") || containsArgPair(got, "--permission-mode", "default") {
		t.Fatalf("stale canonical fields survived: %#v", got)
	}
	if !containsArgPair(got, claudeModelFlag, "canonical-model") || !containsArgPair(got, "--effort", "high") || !containsArgPair(got, "--permission-mode", "bypassPermissions") {
		t.Fatalf("canonical fields were not applied: %#v", got)
	}
	if !containsArgPair(got, "--allowed-tools", "Read") || !containsArgPair(got, "--allowed-tools", "Grep") {
		t.Fatalf("transport allow-list was removed: %#v", got)
	}
}

// Canonical posture passes into the provider builder after ownership resolves.
func TestClaudeResumeTransportWithoutAttachmentCarriesPosture(t *testing.T) {
	s, _ := claudeResumeServer(t, "")
	req, err := s.resolveClaudeResume(spawnRequest{
		Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID, Cwd: t.TempDir(),
		Model: "claude-opus-4-1", PermissionMode: "bypassPermissions", Effort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := claudeResumeArgs(req.ClaudeArgs, claudeResumeThreadID)
	want := []string{
		"-p", "--verbose", "--input-format", "stream-json",
		"--output-format", "stream-json", "--include-partial-messages",
		"--permission-prompt-tool", "stdio",
		"--model", "claude-opus-4-1", "--effort", "high",
		"--permission-mode", "bypassPermissions",
		claudeResumeFlag, claudeResumeThreadID,
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("posture argv = %#v, want %#v", args, want)
	}
}

// A resume into a project directory that no longer exists is refused before any
// child is spawned, naming the path — the child would otherwise die looking for
// a transcript under a path derived from it.
func TestClaudeResumeRefusesMissingWorkingDirectory(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.replyError(call, shimCodeSessionNotFound, "session_not_found")
	}))
	s, _ := claudeResumeServer(t, shim.wsURL())
	gone := filepath.Join(t.TempDir(), "deleted-project")
	seedCanonicalResumeOwner(t, s, "claude", claudeResumeThreadID, gone)

	_, err := s.spawnSession(spawnRequest{
		Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID, Cwd: gone,
	})
	if err == nil {
		t.Fatal("resume into a missing directory succeeded")
	}
	if !strings.Contains(err.Error(), gone) || !strings.Contains(err.Error(), "moved or deleted") {
		t.Fatalf("err = %v, want a readable error naming %s", err, gone)
	}
	if starts := shim.callsOf(shimMethodStart); len(starts) != 0 {
		t.Fatalf("a child was spawned for a missing directory: %+v", starts)
	}
}

// claudeMissingTranscriptResult is the exact line `claude -p --resume` emits
// when the session's native JSONL is gone (probed 2026-07-29 under a scratch
// HOME; see the plan's outcome section). No system.init ever follows it.
const claudeMissingTranscriptResult = `{"type":"result","subtype":"error_during_execution","duration_ms":0,"is_error":true,"num_turns":0,"session_id":"native-1","total_cost_usd":0,"errors":["No conversation found with session ID: native-1"]}`

// The purged-transcript failure reaches the operator as a sentence. Without the
// translation the session would simply go "exited" with an empty transcript and
// nothing to read.
func TestClaudeResumeMissingTranscriptIsAReadableError(t *testing.T) {
	// The shim loses the session, and the `--resume` child it then spawns
	// answers with the failure line instead of an init — the whole ladder
	// running out, as it does when the native JSONL is gone.
	shim := newFakeShim(t, func(c *shimConn) {
		for {
			call, ok := c.next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:
				c.replyError(call, shimCodeSessionNotFound, "session_not_found")
			case shimMethodStart:
				c.reply(call, map[string]any{"shimSessionId": "shim-2"})
				c.line(1, "recv", claudeMissingTranscriptResult)
			default:
				c.reply(call, nil)
			}
		}
	})
	s, _ := claudeResumeServer(t, shim.wsURL())

	seedCanonicalResumeOwner(t, s, "claude", claudeResumeThreadID, s.defaults.cwd)

	sum := spawnedSummary(t, postClaudeResume(t, s, `{"provider":"claude","mode":"resume","threadId":"`+claudeResumeThreadID+`","cwd":"`+t.TempDir()+`"}`))
	failed := waitForSessionStatus(t, s, sum.ID, "exited")
	if !strings.Contains(failed.Error, "no longer on disk") ||
		!strings.Contains(failed.Error, claudeResumeThreadID) {
		t.Fatalf("error = %q, want a readable purged-transcript message naming the thread", failed.Error)
	}
	if strings.Contains(failed.Error, claudeMissingTranscriptMarker) {
		t.Fatalf("error = %q, want the CLI's raw sentence translated, not passed through", failed.Error)
	}
}

// Anything the translator does not recognize still reaches the operator, and a
// result carrying no message at all still says the handshake never happened.
func TestClaudeSpawnFailureFallsBackToTheChildsOwnWords(t *testing.T) {
	cases := []struct {
		name string
		errs []string
		want string
	}{
		{"unrecognized", []string{"Credit balance is too low"}, "claude could not start the session: Credit balance is too low"},
		{"several", []string{"a", "b"}, "claude could not start the session: a; b"},
		{"silent", nil, "claude exited before the session handshake"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeSpawnFailure("native-1", tc.errs); got != tc.want {
				t.Fatalf("claudeSpawnFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

// The pre-init guard keys on the handshake, not on the thread id. Identity at
// spawn hands a `--resume` session its predecessor's id before any wire traffic,
// so a guard that read that id would make this translation unreachable on
// exactly the rung most likely to need it.
func TestClaudeSpawnFailureTranslatedDespiteTheSeededThreadID(t *testing.T) {
	sess, _ := claudeTestSession(t)
	sess.threadID = claudeResumeThreadID // identity at spawn, no handshake yet
	c := &claudeSession{sess: sess, exited: make(chan struct{})}

	c.captureState(&claudeChildTransport{owner: c}, []byte(claudeMissingTranscriptResult))

	sum := sess.summary()
	if sum.Status != "exited" || !strings.Contains(sum.Error, "no longer on disk") {
		t.Fatalf("summary = %#v, want the pre-init failure translated", sum)
	}
}

// A failing result after the handshake is one turn's error, not the session's:
// a live session must not be marked as a failed spawn by it.
func TestClaudeResultErrorAfterInitLeavesTheSessionAlone(t *testing.T) {
	sess, _ := claudeTestSession(t)
	c := &claudeSession{sess: sess, exited: make(chan struct{})}
	tr := &claudeChildTransport{owner: c}

	c.markTransportReady()
	c.captureState(tr, []byte(claudeInitLine))
	c.captureState(tr, []byte(claudeMissingTranscriptResult))

	if sum := sess.summary(); sum.Status != "ready" || sum.Error != "" {
		t.Fatalf("summary = %#v, want the handshaken session untouched", sum)
	}

	// The same holds when the handshake's provisional pair cannot be
	// recorded: the init still happened, so a later error result is not a
	// failed spawn, and the session keeps its provisional identity.
	unrecorded, _ := claudeTestSession(t)
	unrecorded.hostedThreadID = "nht_unrecorded"
	unrecorded.metadata = newTestControlMetadata(t) // holds no such hosted thread, so the claim fails
	unrecorded.threadID = "cs_handle"
	child := &claudeSession{sess: unrecorded, exited: make(chan struct{}), supersedes: "cs_handle"}
	shim := &claudeShimTransport{owner: child, shimSessionID: "cs_handle"}
	child.markTransportReady()
	child.captureState(shim, []byte(claudeInitLine))
	child.captureState(shim, []byte(claudeMissingTranscriptResult))
	if sum := unrecorded.summary(); sum.Status == "exited" || strings.Contains(sum.Error, "no longer on disk") {
		t.Fatalf("summary = %#v; a handshake whose pair was not recorded became a failed spawn", sum)
	}
	if got := unrecorded.currentThreadID(); got != "cs_handle" {
		t.Fatalf("identity = %q; want it to stay provisional until the pair is recorded", got)
	}
}
