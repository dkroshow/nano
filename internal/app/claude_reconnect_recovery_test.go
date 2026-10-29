package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/sessions"
	replayfixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
	"github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

var NewSpoolShim = replayfixture.NewSpoolShim

// reconnectServer is a resume server with one remembered Claude thread whose
// hosted identity exists before the test writes its spool and prompt rows.
func reconnectServer(t *testing.T, shimURL string, stale bool) (*server, string) {
	t.Helper()
	s, store := claudeResumeServer(t, shimURL)
	record := rememberedClaudeSession(t, "/fake/claude")
	record.ShimSessionID = "shim-1"
	if stale {
		record.TurnStartedAtMs, record.TurnAnchorSeq = 1789766804441, 1
	}
	store.Remember(record)
	seedHostedAttachmentFixture(t, s, "claude", claudeResumeThreadID)
	stored, _ := fixtureAttachmentFor(s, "claude", claudeResumeThreadID)
	return s, stored.HostedThreadID
}

func openCompletedTurns(t *testing.T, s *server) *coreturns.Store {
	t.Helper()
	store, err := coreturns.OpenStore(context.Background(), s.queuedTurns.Database())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func completedTurnRows(t *testing.T, s *server, hosted, TurnID string) int {
	t.Helper()
	var n int
	if err := s.queuedTurns.Database().QueryRow(`SELECT count(*) FROM completed_turns WHERE target_hosted_thread_id=? AND turn_id=? AND outcome<>''`, hosted, TurnID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitForCompletedTurn(t *testing.T, s *server, hosted, TurnID string) coreturns.Record {
	t.Helper()
	store := openCompletedTurns(t, s)
	var record coreturns.Record
	waitForClaude(t, "completion of "+TurnID, func() bool {
		var err error
		record, err = store.Get(context.Background(), hosted, TurnID)
		return err == nil && record.Outcome != ""
	})
	return record
}

func sessionForNative(t *testing.T, s *server) *sessions.Session {
	t.Helper()
	sess := s.SessionForThread("claude", claudeResumeThreadID)
	if sess == nil {
		t.Fatal("no attached Claude session")
	}
	return sess
}

// Incident shape: every accepted prompt finished durably long ago, a stale
// anchor names the old prompt, and thousands of background frames followed.
// Saved history already holds all of it, so nothing replays.
func TestClaudeReconnectIdleStaleAnchorReplaysNothing(t *testing.T) {
	var shim *replayfixture.SpoolShim
	shim = NewSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.WSURL(), true)
	b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
	b.Add("recv", "", claudeInitLine)
	turn := b.Prompt("done-action")
	b.Message(turn, "done", "answer")
	b.Result(turn)
	b.Background(3000)
	shim.Frames = b.Frames
	writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, b.Frames...)
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "done-action", turn, "prompt")
	if _, _, err := openCompletedTurns(t, s).Finished(context.Background(), coreturns.Finish{HostedThreadID: hosted, Provider: "claude", NativeThreadID: claudeResumeThreadID, TurnID: turn, Outcome: agentTurnCompleted, Source: "adapter"}); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	elapsed := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.LastFromSeq(); got != b.Seq || shim.ReplayedFrames() != 0 {
		t.Fatalf("fromSeq = %d replayed %d; want the finished spool's end %d and nothing replayed", got, shim.ReplayedFrames(), b.Seq)
	}
	if elapsed > 5*time.Second || len(shim.CallsOf(shimMethodStart)) != 0 || len(shim.CallsOf(shimMethodAttach)) != 1 {
		t.Fatalf("attach took %s with %d starts and %d attaches", elapsed, len(shim.CallsOf(shimMethodStart)), len(shim.CallsOf(shimMethodAttach)))
	}
	t.Logf("idle %d-frame spool attached in %s", b.Seq, elapsed)
}

// A long thread's resume checks every accepted prompt for a recorded outcome
// while restore writes keep the one writer connection busy. One read for the
// thread waits for the writer once; a read per prompt waited once per prompt,
// so a thread with a thousand prompts held its boot handshake slot throughout.
// claudeBacklogFrames is the background backlog behind an offline completion.
// The acceptance size (about 3,000 frames) takes minutes at today's per-frame
// transcript cost, so the default run uses a smaller one that still outlasts
// the shortened progress window; WI2796_BACKLOG_FRAMES restores the full size.
func claudeBacklogFrames(t *testing.T) int {
	if raw := os.Getenv("WI2796_BACKLOG_FRAMES"); raw != "" {
		var n int
		if _, err := fmt.Sscan(raw, &n); err != nil || n <= 0 {
			t.Fatalf("WI2796_BACKLOG_FRAMES=%q", raw)
		}
		return n
	}
	return 400
}

// A prompt finished while Nano was away and background turns kept running
// after it. Replay starts at the prompt's last message, so its one completion
// lands with its final message and its own result's usage.
func TestClaudeReconnectSavedCompletionBehindBacklog(t *testing.T) {
	shim := NewSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.WSURL(), false)
	b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
	b.Add("recv", "", claudeInitLine)
	turn := b.Prompt("offline-action")
	b.Message(turn, "tool", "working")
	finalStart := b.Message(turn, "final", "the final answer")
	b.Result(turn)
	b.Background(claudeBacklogFrames(t))
	shim.Frames = b.Frames
	writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, b.Frames...)
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "offline-action", turn, "prompt")

	started := time.Now()
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	elapsed := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.LastFromSeq(); got != finalStart-1 {
		t.Fatalf("fromSeq = %d, want the prompt's last message start %d", got, finalStart-1)
	}

	record := waitForCompletedTurn(t, s, hosted, turn)
	if record.Outcome != agentTurnCompleted || record.FinalMessageID != "msg_final:0" {
		t.Fatalf("completion = %+v, want completed with final message msg_final:0", record)
	}
	if record.UsageInputTokens == nil || *record.UsageInputTokens != 11 || *record.UsageCachedInputTokens != 7 || *record.UsageOutputTokens != 7 {
		t.Fatalf("completion usage = %v/%v/%v, want the result line's 11/7/7", record.UsageInputTokens, record.UsageCachedInputTokens, record.UsageOutputTokens)
	}
	if n := completedTurnRows(t, s, hosted, turn); n != 1 {
		t.Fatalf("completion rows = %d, want exactly one", n)
	}
	t.Logf("%d-frame backlog (%d replayed) attached in %s", b.Seq, shim.ReplayedFrames(), elapsed)
}

// A failed attach must stop its queued replay: nothing it queued may publish
// or record a completion afterwards, and the retry computes the same floor
// and records the completion exactly once. Another thread is unaffected.

// Reconnecting while a message streams starts at that message's start, so
// its blocks keep the native identities saved rows use (msg_<id>:<index>),
// for prompted and unprompted work alike.
func TestClaudeReconnectMidMessageKeepsNativeBlockIdentity(t *testing.T) {
	for _, prompted := range []bool{true, false} {
		t.Run(fmt.Sprintf("prompted=%v", prompted), func(t *testing.T) {
			shim := NewSpoolShim(t, nil)
			s, hosted := reconnectServer(t, shim.WSURL(), false)
			b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
			b.Add("recv", "", claudeInitLine)
			turn := ""
			if prompted {
				turn = b.Prompt("open-action")
				seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "open-action", turn, "prompt")
			}
			b.Message(turn, "earlier", "earlier text")
			start := b.OpenMessage(turn, "open", "first block", "second block so far")
			shim.Frames, shim.TurnActive = b.Frames, true
			writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, b.Frames...)

			response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
			if response.Code != http.StatusOK {
				t.Fatalf("resume = %d %s", response.Code, response.Body.String())
			}
			if got := shim.LastFromSeq(); got != start-1 {
				t.Fatalf("fromSeq = %d, want the open message's start %d", got, start-1)
			}

			sess := sessionForNative(t, s)
			texts := sess.LiveTextService().CurrentTexts(context.Background())
			if !slices.Contains(texts, "first block") || !slices.Contains(texts, "second block so far") {
				t.Fatalf("live text after the replay = %q, want each block of the open message", texts)
			}
		})
	}
}

// A message cut off without message_stop is closed by its result: it must
// not hold the floor at its old start.
func TestClaudeReconnectInterruptedMessageDoesNotPinFloor(t *testing.T) {
	shim := NewSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.WSURL(), false)
	b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
	b.Add("recv", "", claudeInitLine)
	b.OpenMessage("", "cut", "partial")
	b.Result("")
	b.Background(20)
	shim.Frames = b.Frames
	writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, b.Frames...)
	if response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID)); response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.LastFromSeq(); got != b.Seq {
		t.Fatalf("fromSeq = %d, want the spool's end %d", got, b.Seq)
	}
}

// An unanswered permission request raised while Nano was away is recovered
// once and can be answered through the shim.
func TestClaudeReconnectRecoversPendingPermissionOnce(t *testing.T) {
	shim := NewSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.WSURL(), false)
	b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
	b.Add("recv", "", claudeInitLine)
	turn := b.Prompt("ask-action")
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "ask-action", turn, "prompt")
	b.Message(turn, "earlier", "checking")

	b.Add("recv", turn, `{"type":"control_request","request_id":"old","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	b.Add("send", "", `{"type":"control_response","response":{"subtype":"success","request_id":"old","response":{"behavior":"allow"}}}`)
	start := b.OpenMessage(turn, "ask", "about to run")
	b.Add("recv", turn, `{"type":"control_request","request_id":"perm-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"pwd"}}}`)
	shim.Frames, shim.TurnActive = b.Frames, true
	writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, b.Frames...)

	if response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID)); response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.LastFromSeq(); got != start-1 {
		t.Fatalf("fromSeq = %d, want the request's enclosing message start %d", got, start-1)
	}
	sess := sessionForNative(t, s)
	waitForClaude(t, "the recovered permission", func() bool { return len(sess.Delivery().PendingInteractionList()) > 0 })
	pending := sess.Delivery().PendingInteractionList()
	if len(pending) != 1 || pending[0].ID != "perm-1" {
		t.Fatalf("pending = %+v, want perm-1 exactly once", pending)
	}
	res := httptest.NewRecorder()
	sess.Delivery().HandleInteraction(res, httptest.NewRequest(http.MethodPost, "/api/sessions/x/interactions/perm-1", strings.NewReader(`{"decision":"accept"}`)), "perm-1")
	if res.Code != http.StatusOK {
		t.Fatalf("answer = %d %s", res.Code, res.Body.String())
	}
	answered := false
	for _, line := range shim.InputLines() {
		answered = answered || (strings.Contains(line, `"control_response"`) && strings.Contains(line, `"perm-1"`))
	}
	if !answered {
		t.Fatalf("shim input = %v, want the perm-1 answer", shim.InputLines())
	}
}

// Recovery evidence that cannot be proven fails visibly: no replacement
// child, no close, and the record stays for a retry. A child the shim reports
// as never having recorded anything is the one positively empty case.
func TestClaudeReconnectUnprovableEvidenceFailsWithoutReplacement(t *testing.T) {
	cases := []struct {
		name     string
		spool    func(t *testing.T, s *server, hosted string, b *replayfixture.SpoolBuilder)
		accepted bool
		LastSeq  int64
		ok       bool
	}{
		{name: "missing spool with an unfinished accepted prompt", accepted: true, LastSeq: 3},
		{name: "missing spool and a shim holding lines", LastSeq: 5},
		{name: "malformed spool", accepted: true, LastSeq: 3, spool: func(t *testing.T, s *server, hosted string, b *replayfixture.SpoolBuilder) {
			path := writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, b.Frames[:1]...)
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = file.WriteString("{torn\n")
			file.Close()
		}},
		{name: "spool of another conversation", LastSeq: 3, spool: func(t *testing.T, s *server, hosted string, b *replayfixture.SpoolBuilder) {
			Frames := append([]replayfixture.Frame(nil), b.Frames...)
			Frames[0].Line = claudeSuccessorInitLine
			writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, Frames...)
		}},
		{name: "never used child", LastSeq: 0, ok: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			shim := NewSpoolShim(t, nil)
			s, hosted := reconnectServer(t, shim.WSURL(), false)
			if err := os.Remove(filepath.Join(s.defaults.ClaudeShimSpools, hosted+".jsonl")); err != nil {
				t.Fatal(err)
			}
			b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
			b.Add("recv", "", claudeInitLine)
			turn := b.Prompt("unresolved-action")
			b.OpenMessage(turn, "live", "streaming")
			if test.accepted {
				seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "unresolved-action", turn, "prompt")
			}
			if test.spool != nil {
				test.spool(t, s, hosted, b)
			}
			if test.LastSeq > 0 {
				shim.Frames = b.Frames[:test.LastSeq]
			}

			response := httptest.NewRecorder()
			fixtureOwners(s).hostedController().HandleLegacyCreate(response, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(resumeBody(claudeResumeThreadID))))
			if test.ok {
				if response.Code != http.StatusOK {
					t.Fatalf("resume = %d %s", response.Code, response.Body.String())
				}
				return
			}
			if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "claude recovery evidence is missing or unprovable") {
				t.Fatalf("resume = %d %s, want a recovery-evidence failure", response.Code, response.Body.String())
			}
			if starts, closes := len(shim.CallsOf(shimMethodStart)), len(shim.CallsOf(shimMethodClose)); starts != 0 || closes != 0 {
				t.Fatalf("unprovable recovery made %d starts and %d closes", starts, closes)
			}
			if _, ok := fixtureAttachmentFor(s, "claude", claudeResumeThreadID); !ok {
				t.Fatal("unprovable recovery removed the retryable record")
			}
		})
	}
}

// Lines the shim spooled after Nano's scan (including a torn tail the scan
// skipped) replay exactly once: no gap, no duplicate.
func TestClaudeReconnectReplaysLinesAppendedAfterScan(t *testing.T) {
	shim := NewSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.WSURL(), false)
	b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
	b.Add("recv", "", claudeInitLine)
	b.Message("", "old", "old work")
	b.Result("")
	scanned := b.Seq
	raw := claudeTestSpoolBytes(t, b.Frames...)
	b.Message("", "late", "appended after the scan")
	b.Result("")
	raw = append(raw, []byte(`{"seq":`+fmt.Sprint(scanned+1)+`,"dir":"recv","line":"{\"type\":\"stream_`)...)
	if err := os.WriteFile(filepath.Join(s.defaults.ClaudeShimSpools, hosted+".jsonl"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	shim.Frames = append(append([]replayfixture.Frame(nil), b.Frames[:scanned+1]...), b.Frames[scanned:]...)

	if response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID)); response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.LastFromSeq(); got != scanned {
		t.Fatalf("fromSeq = %d, want the scanned end %d", got, scanned)
	}
	sess := sessionForNative(t, s)
	count := 0
	for _, text := range sess.LiveTextService().CurrentTexts(context.Background()) {
		if text == "appended after the scan" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("appended message shown %d times, want exactly once", count)
	}
}

// A request whose own turn ended without an answer is settled. When the floor
// replays it for another reason, it must not come back as a dead dialog.
func TestClaudeReconnectDoesNotResurrectRequestRetiredByItsResult(t *testing.T) {
	shim := NewSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.WSURL(), false)
	b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
	b.Add("recv", "", claudeInitLine)
	turn := b.Prompt("retired-action")
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "retired-action", turn, "prompt")
	start := b.Message(turn, "final", "answer")
	b.Result(turn)
	b.Add("recv", "", `{"type":"control_request","request_id":"stale","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	b.Result("")
	shim.Frames = b.Frames
	writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, b.Frames...)

	if response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID)); response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.LastFromSeq(); got != start-1 {
		t.Fatalf("fromSeq = %d, want %d", got, start-1)
	}
	if pending := sessionForNative(t, s).Delivery().PendingInteractionList(); len(pending) != 0 {
		t.Fatalf("pending = %+v, want the retired request left settled", pending)
	}
}

// Once the shim has answered for the child, every later failure is local to
// this attachment. A replay that never finishes ingesting must not reach the
// `--resume` rung, which would start a second writer.
func TestClaudeResumeIngestionTimeoutNeverStartsReplacement(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *replayfixture.Conn, call replayfixture.Call) {
		c.Reply(call, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 1})
		c.Line(1, "recv", claudeInitLine)
	}))
	s, store := claudeResumeServer(t, shim.WSURL())
	store.Remember(rememberedClaudeSession(t, "/fake/claude"))
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "claude replay did not settle") {
		t.Fatalf("resume = %d %s, want an unsettled replay refusal", response.Code, response.Body.String())
	}
	if starts := len(shim.CallsOf(shimMethodStart)); starts != 0 {
		t.Fatalf("ingestion timeout started %d replacement children", starts)
	}
	if _, ok := fixtureAttachmentFor(s, "claude", claudeResumeThreadID); !ok {
		t.Fatal("ingestion timeout removed the retryable record")
	}
}

// A replay with a gap after the floor is incomplete evidence: the attach
// fails without a replacement rather than applying around the hole.
func TestClaudeReconnectRejectsReplayGap(t *testing.T) {
	shim := NewSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.WSURL(), false)
	b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
	b.Add("recv", "", claudeInitLine)
	b.Message("", "a", "text")
	b.Result("")
	writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, b.Frames[:2]...)
	shim.Frames = append(append([]replayfixture.Frame(nil), b.Frames[:2]...), b.Frames[3:]...)
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "claude recovery evidence is missing or unprovable") {
		t.Fatalf("resume = %d %s, want a recovery-evidence failure", response.Code, response.Body.String())
	}
	if starts := len(shim.CallsOf(shimMethodStart)); starts != 0 {
		t.Fatalf("replay gap started %d replacements", starts)
	}
}

// A frame the session loop already admitted cannot be interrupted mid-apply.
// Hold one (the prompt's result, blocked inside its own apply) past the
// abandonment wait: the failure says the drain is unacknowledged, a retry in
// that window is refused without attaching, and once released the frame
// neither publishes nor records a completion. The retry after the drain
// acknowledges attaches and records the completion exactly once.

func TestClaudeReconnectHTTPRetryWaitsForAbandonedDrain(t *testing.T) {
	shim := NewSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.WSURL(), false)
	b := &replayfixture.SpoolBuilder{Key: hosted, EventID: submissionEventID}
	b.Add("recv", "", claudeInitLine)
	turn := b.Prompt("admitted-action")
	b.Message(turn, "final", "answer")
	b.Result(turn)
	b.Background(40)
	shim.Frames = b.Frames
	writeClaudeTestSpool(t, s.defaults.ClaudeShimSpools, hosted, b.Frames...)
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "admitted-action", turn, "prompt")

	op, _, err := fixtureBeginTurn(s.agentExecutions, hosted, "admitted-op", "prompt-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.MarkTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	operation := func() threads.TurnRecord {
		rec, ok := s.agentExecutions.Turn(op.ID)
		if !ok {
			t.Fatal("bound operation disappeared")
		}
		return rec
	}

	entered, release := make(chan struct{}), make(chan struct{})
	var armed sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.catalogRefresh = func(string) {
		armed.Do(func() {
			close(entered)
			<-release
		})
	}
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	select {
	case <-entered:
	default:
		t.Fatal("the result frame was never admitted")
	}
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "still draining") {
		t.Fatalf("stalled resume = %d %s, want an unacknowledged-drain failure", response.Code, response.Body.String())
	}

	shim.BeforeAttach = nil
	attaches := len(shim.CallsOf(shimMethodAttach))
	retry := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleLegacyCreate(retry, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(resumeBody(claudeResumeThreadID))))
	if retry.Code == http.StatusOK || !strings.Contains(retry.Body.String(), "still draining") {
		t.Fatalf("overlapping retry = %d %s, want a draining refusal", retry.Code, retry.Body.String())
	}
	if got := len(shim.CallsOf(shimMethodAttach)); got != attaches || len(shim.CallsOf(shimMethodStart)) != 0 {
		t.Fatalf("overlapping retry attached (%d→%d) or started %d children", attaches, got, len(shim.CallsOf(shimMethodStart)))
	}

	if n := completedTurnRows(t, s, hosted, turn); n != 0 {
		t.Fatalf("abandoned generation recorded %d completions", n)
	}
	if rec := operation(); rec.Status != agentTurnRunning || rec.FinalResponse != "" || rec.FinalItemID != "" {
		t.Fatalf("abandoned generation advanced the bound operation: %+v", rec)
	}
	var durableStatus string
	if err := s.controlMetadata.CanonicalDatabase().QueryRow(`SELECT status FROM hosted_turn_operations WHERE operation_id=?`, op.ID).Scan(&durableStatus); err != nil || durableStatus != agentTurnRunning {
		t.Fatalf("durable operation status = %q, %v; want still running", durableStatus, err)
	}

	unblock()
	waitForCatalogWithin(t, 5*time.Second, func() bool {
		response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
		if response.Code == http.StatusOK {
			return true
		}
		if !strings.Contains(response.Body.String(), "still draining") {
			t.Fatalf("retry after drain = %d %s", response.Code, response.Body.String())
		}
		return false
	})
	waitForCompletedTurn(t, s, hosted, turn)
	if n := completedTurnRows(t, s, hosted, turn); n != 1 {
		t.Fatalf("completion rows = %d, want exactly one", n)
	}

	waitForClaude(t, "the bound operation to complete", func() bool { return operation().Status == agentTurnCompleted })
	if rec := operation(); rec.FinalItemID != "msg_final:0" {
		t.Fatalf("bound operation = %+v, want the prompt's final message msg_final:0", rec)
	}
}
