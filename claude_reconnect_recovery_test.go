package main

import (
	"context"
	"encoding/json"
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

	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// spoolBuilder writes a Claude conversation the way the shim spools it: one
// frame per stdio line, prompted frames carrying their provider turn id.
type spoolBuilder struct {
	key    string
	seq    int64
	frames []claudeTestFrame
	// events maps a prompted turn to the submission id its user line carries.
	events map[string]string
}

func (b *spoolBuilder) add(dir, turnID, line string) int64 {
	b.seq++
	b.frames = append(b.frames, claudeTestFrame{Seq: b.seq, Dir: dir, Line: line, TurnID: turnID})
	return b.seq
}

func (b *spoolBuilder) turnID(promptSeq int64) string {
	return fmt.Sprintf("claude:%s:%d", b.key, promptSeq)
}

// prompt spools a Nano send of action's prompt, stamped with its submission
// id as Nano stamps it, and returns its provider turn id.
func (b *spoolBuilder) prompt(action string) string {
	id := b.turnID(b.seq + 1)
	event := submissionEventID(b.key, action)
	if b.events == nil {
		b.events = map[string]string{}
	}
	b.events[id] = event
	b.add("send", id, `{"type":"user","uuid":"`+event+`","message":{"role":"user","content":[{"type":"text","text":"prompt"}]}}`)
	return id
}

func (b *spoolBuilder) blockStart(turnID string, index int) {
	raw, _ := json.Marshal(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}}})
	b.add("recv", turnID, string(raw))
}

// openMessage starts a message whose blocks are complete except the last,
// which is still streaming. It returns the message_start sequence.
func (b *spoolBuilder) openMessage(turnID, id string, blocks ...string) int64 {
	start := b.add("recv", turnID, messageStart(id))
	for index, text := range blocks {
		b.blockStart(turnID, index)
		b.add("recv", turnID, textDelta(index, text))
		if index == len(blocks)-1 {
			break
		}
		b.add("recv", turnID, fmt.Sprintf(`{"type":"stream_event","event":{"type":"content_block_stop","index":%d}}`, index))
		row, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": fmt.Sprintf("%s-%d", id, index),
			"message": map[string]any{"id": "msg_" + id, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
		b.add("recv", turnID, string(row))
	}
	return start
}

// message spools a complete one-block text message.
func (b *spoolBuilder) message(turnID, id, text string) int64 {
	start := b.openMessage(turnID, id, text)
	b.add("recv", turnID, `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`)
	row, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": id + "-0",
		"message": map[string]any{"id": "msg_" + id, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
	b.add("recv", turnID, string(row))
	b.add("recv", turnID, `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"}}}`)
	b.add("recv", turnID, testMessageStop)
	return start
}

const testResultWithUsage = `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"session_id":"native-1","usage":{"input_tokens":11,"cache_read_input_tokens":5,"cache_creation_input_tokens":2,"output_tokens":7}}`

// result spools turnID's result. A prompted turn's names its prompt, as
// Claude's does; a background turn's names none.
func (b *spoolBuilder) result(turnID string) int64 {
	line := testResultWithUsage
	if event := b.events[turnID]; event != "" {
		line = strings.Replace(line, `{"type":"result",`, `{"type":"result","user_message_uuid":"`+event+`",`, 1)
	}
	return b.add("recv", turnID, line)
}

// background spools finished turns no Nano prompt started until at least n
// more frames exist.
func (b *spoolBuilder) background(n int) {
	target := b.seq + int64(n)
	for turn := 0; b.seq < target; turn++ {
		b.message("", fmt.Sprintf("bg%d", turn), fmt.Sprintf("background %d", turn))
		b.result("")
	}
}

// spoolShim is a fake shim backed by frames it actually holds: attach replays
// exactly the frames after fromSeq, like cmd/claude-shim's readAfter.
type spoolShim struct {
	*fakeShim
	mu           sync.Mutex
	frames       []claudeTestFrame
	state        string
	turnActive   bool
	lastSeq      int64 // overrides the frames' own end when set
	beforeAttach func()
	fromSeqs     []int64
	replayed     int
}

func newSpoolShim(t *testing.T, frames []claudeTestFrame) *spoolShim {
	shim := &spoolShim{frames: frames, state: "running"}
	shim.fakeShim = newFakeShim(t, func(c *shimConn) {
		for {
			call, ok := c.next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:
				shim.attach(c, call)
			case shimMethodStart:
				c.reply(call, map[string]any{"shimSessionId": "replacement"})
			default:
				c.reply(call, nil)
			}
		}
	})
	return shim
}

func (s *spoolShim) attach(c *shimConn, call shimCall) {
	var params struct {
		FromSeq int64 `json:"fromSeq"`
	}
	_ = json.Unmarshal(call.Params, &params)
	s.mu.Lock()
	hook := s.beforeAttach
	frames := append([]claudeTestFrame(nil), s.frames...)
	last := s.lastSeq
	if last == 0 && len(frames) > 0 {
		last = frames[len(frames)-1].Seq
	}
	state, active := s.state, s.turnActive
	s.fromSeqs = append(s.fromSeqs, params.FromSeq)
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	c.reply(call, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID, "state": state, "turnActive": active, "lastSeq": last})
	sent := 0
	for _, frame := range frames {
		if frame.Seq <= params.FromSeq {
			continue
		}
		c.notify(shimMethodLine, map[string]any{"shimSessionId": "shim-1", "seq": frame.Seq, "ts": "2026-09-18T12:00:00Z", "dir": frame.Dir, "turnId": frame.TurnID, "line": frame.Line})
		sent++
	}
	s.mu.Lock()
	s.replayed = sent
	s.mu.Unlock()
	c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": last})
}

func (s *spoolShim) lastFromSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.fromSeqs) == 0 {
		return -1
	}
	return s.fromSeqs[len(s.fromSeqs)-1]
}

func (s *spoolShim) replayedFrames() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replayed
}

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
	store.remember(record)
	seedHostedAttachmentFixture(t, s, "claude", claudeResumeThreadID)
	stored, _ := s.attachmentFor("claude", claudeResumeThreadID)
	return s, stored.HostedThreadID
}

func openCompletedTurns(t *testing.T, s *server) *coreturns.Store {
	t.Helper()
	store, err := coreturns.OpenStore(context.Background(), s.queuedTurns.db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func completedTurnRows(t *testing.T, s *server, hosted, turnID string) int {
	t.Helper()
	var n int
	if err := s.queuedTurns.db.QueryRow(`SELECT count(*) FROM completed_turns WHERE target_hosted_thread_id=? AND turn_id=? AND outcome<>''`, hosted, turnID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitForCompletedTurn(t *testing.T, s *server, hosted, turnID string) coreturns.Record {
	t.Helper()
	store := openCompletedTurns(t, s)
	var record coreturns.Record
	waitForClaude(t, "completion of "+turnID, func() bool {
		var err error
		record, err = store.Get(context.Background(), hosted, turnID)
		return err == nil && record.Outcome != ""
	})
	return record
}

func sessionForNative(t *testing.T, s *server) *session {
	t.Helper()
	sess := s.sessionForThread("claude", claudeResumeThreadID)
	if sess == nil {
		t.Fatal("no attached Claude session")
	}
	return sess
}

// Incident shape: every accepted prompt finished durably long ago, a stale
// anchor names the old prompt, and thousands of background frames followed.
// Saved history already holds all of it, so nothing replays.
func TestClaudeReconnectIdleStaleAnchorReplaysNothing(t *testing.T) {
	var shim *spoolShim
	shim = newSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.wsURL(), true)
	b := &spoolBuilder{key: hosted}
	b.add("recv", "", claudeInitLine)
	turn := b.prompt("done-action")
	b.message(turn, "done", "answer")
	b.result(turn)
	b.background(3000)
	shim.frames = b.frames
	writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames...)
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
	if got := shim.lastFromSeq(); got != b.seq || shim.replayedFrames() != 0 {
		t.Fatalf("fromSeq = %d replayed %d; want the finished spool's end %d and nothing replayed", got, shim.replayedFrames(), b.seq)
	}
	if elapsed > 5*time.Second || len(shim.callsOf(shimMethodStart)) != 0 || len(shim.callsOf(shimMethodAttach)) != 1 {
		t.Fatalf("attach took %s with %d starts and %d attaches", elapsed, len(shim.callsOf(shimMethodStart)), len(shim.callsOf(shimMethodAttach)))
	}
	t.Logf("idle %d-frame spool attached in %s", b.seq, elapsed)
}

// A long thread's resume checks every accepted prompt for a recorded outcome
// while restore writes keep the one writer connection busy. One read for the
// thread waits for the writer once; a read per prompt waited once per prompt,
// so a thread with a thousand prompts held its boot handshake slot throughout.
func TestClaudeUnfinishedPromptsWaitForABusyWriterOncePerThread(t *testing.T) {
	s, hosted := reconnectServer(t, "", false)
	ctx := context.Background()
	completed := openCompletedTurns(t, s)
	prefix := "claude:" + hosted + ":"
	// The last two prompts are still owed: one turn started without an
	// outcome, and one has no turn row yet. Every other turn ended.
	const prompts = 300
	for i := 1; i <= prompts; i++ {
		turn := fmt.Sprintf("%s%d", prefix, i)
		seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, fmt.Sprintf("action-%d", i), turn, fmt.Sprintf("prompt %d", i))
		var err error
		switch i {
		case prompts - 1:
			_, _, err = completed.Started(ctx, coreturns.Start{HostedThreadID: hosted, Provider: "claude", NativeThreadID: claudeResumeThreadID, TurnID: turn, Source: "adapter"})
		case prompts:
		default:
			_, _, err = completed.Finished(ctx, coreturns.Finish{HostedThreadID: hosted, Provider: "claude", NativeThreadID: claudeResumeThreadID, TurnID: turn, Outcome: agentTurnCompleted, Source: "adapter"})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	sess := &session{hostedThreadID: hosted, turns: s.turns, completedTurns: newCompletedTurnSource(&completedTurnStore{db: s.queuedTurns.db})}

	// Restore writes, back to back: each holds the writer for 5 ms.
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			tx, err := s.queuedTurns.db.BeginTx(ctx, nil)
			if err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
			_ = tx.Rollback()
		}
	}()
	started := time.Now()
	wanted, _, err := sess.unfinishedClaudePrompts(ctx, prefix, claudeResumeThreadID)
	took := time.Since(started)
	close(stop)
	<-done
	if err != nil || !slices.Equal(wanted, []int64{prompts - 1, prompts}) {
		t.Fatalf("unfinished = %v, %v; want exactly the two owed prompts %d and %d", wanted, err, prompts-1, prompts)
	}
	// A read per prompt waits at least 300 x 5 ms = 1.5 s here.
	if took > 500*time.Millisecond {
		t.Fatalf("checking %d prompts took %s behind a busy writer; want one outcome read for the thread", prompts, took)
	}
	t.Logf("checked %d prompts behind a busy writer in %s", prompts, took)
}

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
func TestClaudeReconnectRecordsOfflineCompletionBehindBackgroundBacklog(t *testing.T) {
	shim := newSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.wsURL(), false)
	b := &spoolBuilder{key: hosted}
	b.add("recv", "", claudeInitLine)
	turn := b.prompt("offline-action")
	b.message(turn, "tool", "working")
	finalStart := b.message(turn, "final", "the final answer")
	b.result(turn)
	b.background(claudeBacklogFrames(t))
	shim.frames = b.frames
	writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames...)
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "offline-action", turn, "prompt")
	// Progress-aware: the backlog may outlast this window as long as the
	// session loop keeps applying frames.
	previous := claudeShimReplayTimeout
	claudeShimReplayTimeout = time.Second
	t.Cleanup(func() { claudeShimReplayTimeout = previous })

	started := time.Now()
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	elapsed := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.lastFromSeq(); got != finalStart-1 {
		t.Fatalf("fromSeq = %d, want the prompt's last message start %d", got, finalStart-1)
	}
	// Background answers after the result are not the prompt's: its final
	// message is its own last answer.
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
	t.Logf("%d-frame backlog (%d replayed) attached in %s", b.seq, shim.replayedFrames(), elapsed)
}

// A failed attach must stop its queued replay: nothing it queued may publish
// or record a completion afterwards, and the retry computes the same floor
// and records the completion exactly once. Another thread is unaffected.
func TestClaudeReconnectStalledReplayStopsAndRetrySucceeds(t *testing.T) {
	shim := newSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.wsURL(), false)
	b := &spoolBuilder{key: hosted}
	b.add("recv", "", claudeInitLine)
	turn := b.prompt("stall-action")
	finalStart := b.message(turn, "final", "answer")
	b.result(turn)
	b.background(200)
	shim.frames = b.frames
	writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames...)
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "stall-action", turn, "prompt")
	previous := claudeShimReplayTimeout
	claudeShimReplayTimeout = 300 * time.Millisecond
	t.Cleanup(func() { claudeShimReplayTimeout = previous })

	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var stalled *session
	shim.beforeAttach = func() {
		stalled = s.sessionForThread("claude", claudeResumeThreadID)
		blocked := make(chan struct{})
		stalled.coreSession().ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error {
			close(blocked)
			<-release
			return nil
		}})
		<-blocked
	}
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), errClaudeReplayUnsettled.Error()) {
		t.Fatalf("stalled resume = %d %s, want an unsettled replay failure", response.Code, response.Body.String())
	}
	published := func() int64 {
		stalled.mu.Lock()
		defer stalled.mu.Unlock()
		return stalled.seq
	}
	before := published()
	unblock()
	// The loop now reaches every frame the abandoned attach queued.
	if err := stalled.coreSession().Observe(context.Background(), coresession.AdapterEvent{}); err != nil && !strings.Contains(err.Error(), "closed") {
		t.Fatal(err)
	}
	if after := published(); after != before {
		t.Fatalf("abandoned replay published %d more events after failure", after-before)
	}
	if n := completedTurnRows(t, s, hosted, turn); n != 0 {
		t.Fatalf("abandoned replay recorded %d completions", n)
	}
	if starts := len(shim.callsOf(shimMethodStart)); starts != 0 {
		t.Fatalf("stalled replay started %d replacements", starts)
	}

	shim.beforeAttach = nil
	claudeShimReplayTimeout = previous
	response = postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if response.Code != http.StatusOK {
		t.Fatalf("retry = %d %s", response.Code, response.Body.String())
	}
	if shim.lastFromSeq() != finalStart-1 {
		t.Fatalf("retry fromSeq = %d, want the same floor %d", shim.lastFromSeq(), finalStart-1)
	}
	waitForCompletedTurn(t, s, hosted, turn)
	if n := completedTurnRows(t, s, hosted, turn); n != 1 {
		t.Fatalf("completion rows = %d, want exactly one", n)
	}
}

// Reconnecting while a message streams starts at that message's start, so
// its blocks keep the native identities saved rows use (msg_<id>:<index>),
// for prompted and unprompted work alike.
func TestClaudeReconnectMidMessageKeepsNativeBlockIdentity(t *testing.T) {
	for _, prompted := range []bool{true, false} {
		t.Run(fmt.Sprintf("prompted=%v", prompted), func(t *testing.T) {
			shim := newSpoolShim(t, nil)
			s, hosted := reconnectServer(t, shim.wsURL(), false)
			b := &spoolBuilder{key: hosted}
			b.add("recv", "", claudeInitLine)
			turn := ""
			if prompted {
				turn = b.prompt("open-action")
				seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "open-action", turn, "prompt")
			}
			b.message(turn, "earlier", "earlier text")
			start := b.openMessage(turn, "open", "first block", "second block so far")
			shim.frames, shim.turnActive = b.frames, true
			writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames...)

			response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
			if response.Code != http.StatusOK {
				t.Fatalf("resume = %d %s", response.Code, response.Body.String())
			}
			if got := shim.lastFromSeq(); got != start-1 {
				t.Fatalf("fromSeq = %d, want the open message's start %d", got, start-1)
			}
			// Live text shows only text with an exact native identity, so each
			// block shows with its own text only if the replay kept
			// msg_open:0 and msg_open:1.
			sess := sessionForNative(t, s)
			texts := sess.liveText.currentTexts(context.Background())
			if !slices.Contains(texts, "first block") || !slices.Contains(texts, "second block so far") {
				t.Fatalf("live text after the replay = %q, want each block of the open message", texts)
			}
		})
	}
}

// A message cut off without message_stop is closed by its result: it must
// not hold the floor at its old start.
func TestClaudeReconnectInterruptedMessageDoesNotPinFloor(t *testing.T) {
	shim := newSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.wsURL(), false)
	b := &spoolBuilder{key: hosted}
	b.add("recv", "", claudeInitLine)
	b.openMessage("", "cut", "partial")
	b.result("")
	b.background(20)
	shim.frames = b.frames
	writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames...)
	if response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID)); response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.lastFromSeq(); got != b.seq {
		t.Fatalf("fromSeq = %d, want the spool's end %d", got, b.seq)
	}
}

// An unanswered permission request raised while Nano was away is recovered
// once and can be answered through the shim.
func TestClaudeReconnectRecoversPendingPermissionOnce(t *testing.T) {
	shim := newSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.wsURL(), false)
	b := &spoolBuilder{key: hosted}
	b.add("recv", "", claudeInitLine)
	turn := b.prompt("ask-action")
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "ask-action", turn, "prompt")
	b.message(turn, "earlier", "checking")
	// An older request answered by the previous Nano stays settled.
	b.add("recv", turn, `{"type":"control_request","request_id":"old","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	b.add("send", "", `{"type":"control_response","response":{"subtype":"success","request_id":"old","response":{"behavior":"allow"}}}`)
	start := b.openMessage(turn, "ask", "about to run")
	b.add("recv", turn, `{"type":"control_request","request_id":"perm-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"pwd"}}}`)
	shim.frames, shim.turnActive = b.frames, true
	writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames...)

	if response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID)); response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.lastFromSeq(); got != start-1 {
		t.Fatalf("fromSeq = %d, want the request's enclosing message start %d", got, start-1)
	}
	sess := sessionForNative(t, s)
	waitForClaude(t, "the recovered permission", func() bool { return len(sess.pendingInteractionList()) > 0 })
	pending := sess.pendingInteractionList()
	if len(pending) != 1 || pending[0].ID != "perm-1" {
		t.Fatalf("pending = %+v, want perm-1 exactly once", pending)
	}
	res := httptest.NewRecorder()
	sess.handleInteraction(res, httptest.NewRequest(http.MethodPost, "/api/sessions/x/interactions/perm-1", strings.NewReader(`{"decision":"accept"}`)), "perm-1")
	if res.Code != http.StatusOK {
		t.Fatalf("answer = %d %s", res.Code, res.Body.String())
	}
	answered := false
	for _, line := range shim.inputLines() {
		answered = answered || (strings.Contains(line, `"control_response"`) && strings.Contains(line, `"perm-1"`))
	}
	if !answered {
		t.Fatalf("shim input = %v, want the perm-1 answer", shim.inputLines())
	}
}

// Recovery evidence that cannot be proven fails visibly: no replacement
// child, no close, and the record stays for a retry. A child the shim reports
// as never having recorded anything is the one positively empty case.
func TestClaudeReconnectUnprovableEvidenceFailsWithoutReplacement(t *testing.T) {
	cases := []struct {
		name     string
		spool    func(t *testing.T, s *server, hosted string, b *spoolBuilder)
		accepted bool
		lastSeq  int64
		ok       bool
	}{
		{name: "missing spool with an unfinished accepted prompt", accepted: true, lastSeq: 3},
		{name: "missing spool and a shim holding lines", lastSeq: 5},
		{name: "malformed spool", accepted: true, lastSeq: 3, spool: func(t *testing.T, s *server, hosted string, b *spoolBuilder) {
			path := writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames[:1]...)
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = file.WriteString("{torn\n")
			file.Close()
		}},
		{name: "spool of another conversation", lastSeq: 3, spool: func(t *testing.T, s *server, hosted string, b *spoolBuilder) {
			frames := append([]claudeTestFrame(nil), b.frames...)
			frames[0].Line = claudeSuccessorInitLine
			writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, frames...)
		}},
		{name: "never used child", lastSeq: 0, ok: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			shim := newSpoolShim(t, nil)
			s, hosted := reconnectServer(t, shim.wsURL(), false)
			if err := os.Remove(filepath.Join(s.defaults.claudeShimSpools, hosted+".jsonl")); err != nil {
				t.Fatal(err)
			}
			b := &spoolBuilder{key: hosted}
			b.add("recv", "", claudeInitLine)
			turn := b.prompt("unresolved-action")
			b.openMessage(turn, "live", "streaming")
			if test.accepted {
				seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "unresolved-action", turn, "prompt")
			}
			if test.spool != nil {
				test.spool(t, s, hosted, b)
			}
			if test.lastSeq > 0 {
				shim.frames = b.frames[:test.lastSeq]
			}
			// Not postClaudeResume: its fixture seeding would recreate the spool.
			response := httptest.NewRecorder()
			s.handleSessionCreate(response, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(resumeBody(claudeResumeThreadID))))
			if test.ok {
				if response.Code != http.StatusOK {
					t.Fatalf("resume = %d %s", response.Code, response.Body.String())
				}
				return
			}
			if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), errClaudeRecoveryUnproven.Error()) {
				t.Fatalf("resume = %d %s, want a recovery-evidence failure", response.Code, response.Body.String())
			}
			if starts, closes := len(shim.callsOf(shimMethodStart)), len(shim.callsOf(shimMethodClose)); starts != 0 || closes != 0 {
				t.Fatalf("unprovable recovery made %d starts and %d closes", starts, closes)
			}
			if _, ok := s.attachmentFor("claude", claudeResumeThreadID); !ok {
				t.Fatal("unprovable recovery removed the retryable record")
			}
		})
	}
}

// Lines the shim spooled after Nano's scan (including a torn tail the scan
// skipped) replay exactly once: no gap, no duplicate.
func TestClaudeReconnectReplaysLinesAppendedAfterScan(t *testing.T) {
	shim := newSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.wsURL(), false)
	b := &spoolBuilder{key: hosted}
	b.add("recv", "", claudeInitLine)
	b.message("", "old", "old work")
	b.result("")
	scanned := b.seq
	raw := claudeTestSpoolBytes(t, b.frames...)
	b.message("", "late", "appended after the scan")
	b.result("")
	raw = append(raw, []byte(`{"seq":`+fmt.Sprint(scanned+1)+`,"dir":"recv","line":"{\"type\":\"stream_`)...)
	if err := os.WriteFile(filepath.Join(s.defaults.claudeShimSpools, hosted+".jsonl"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The shim resends one appended frame; the sequence keeps it single.
	shim.frames = append(append([]claudeTestFrame(nil), b.frames[:scanned+1]...), b.frames[scanned:]...)

	if response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID)); response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.lastFromSeq(); got != scanned {
		t.Fatalf("fromSeq = %d, want the scanned end %d", got, scanned)
	}
	sess := sessionForNative(t, s)
	count := 0
	for _, text := range sess.liveText.currentTexts(context.Background()) {
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
	shim := newSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.wsURL(), false)
	b := &spoolBuilder{key: hosted}
	b.add("recv", "", claudeInitLine)
	turn := b.prompt("retired-action")
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "retired-action", turn, "prompt")
	start := b.message(turn, "final", "answer")
	b.result(turn)
	b.add("recv", "", `{"type":"control_request","request_id":"stale","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	b.result("")
	shim.frames = b.frames
	writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames...)

	if response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID)); response.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", response.Code, response.Body.String())
	}
	if got := shim.lastFromSeq(); got != start-1 {
		t.Fatalf("fromSeq = %d, want %d", got, start-1)
	}
	if pending := sessionForNative(t, s).pendingInteractionList(); len(pending) != 0 {
		t.Fatalf("pending = %+v, want the retired request left settled", pending)
	}
}

// Once the shim has answered for the child, every later failure is local to
// this attachment. A replay that never finishes ingesting must not reach the
// `--resume` rung, which would start a second writer.
func TestClaudeResumeIngestionTimeoutNeverStartsReplacement(t *testing.T) {
	previous := claudeShimReplayTimeout
	claudeShimReplayTimeout = 200 * time.Millisecond
	t.Cleanup(func() { claudeShimReplayTimeout = previous })
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 1})
		c.line(1, "recv", claudeInitLine) // replayDone never arrives
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	store.remember(rememberedClaudeSession(t, "/fake/claude"))
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), errClaudeReplayUnsettled.Error()) {
		t.Fatalf("resume = %d %s, want an unsettled replay refusal", response.Code, response.Body.String())
	}
	if starts := len(shim.callsOf(shimMethodStart)); starts != 0 {
		t.Fatalf("ingestion timeout started %d replacement children", starts)
	}
	if _, ok := s.attachmentFor("claude", claudeResumeThreadID); !ok {
		t.Fatal("ingestion timeout removed the retryable record")
	}
}

// A replay with a gap after the floor is incomplete evidence: the attach
// fails without a replacement rather than applying around the hole.
func TestClaudeReconnectRejectsReplayGap(t *testing.T) {
	shim := newSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.wsURL(), false)
	b := &spoolBuilder{key: hosted}
	b.add("recv", "", claudeInitLine)
	b.message("", "a", "text")
	b.result("")
	writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames[:2]...)
	shim.frames = append(append([]claudeTestFrame(nil), b.frames[:2]...), b.frames[3:]...) // seq 3 missing
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), errClaudeRecoveryUnproven.Error()) {
		t.Fatalf("resume = %d %s, want a recovery-evidence failure", response.Code, response.Body.String())
	}
	if starts := len(shim.callsOf(shimMethodStart)); starts != 0 {
		t.Fatalf("replay gap started %d replacements", starts)
	}
}

// A frame the session loop already admitted cannot be interrupted mid-apply.
// Hold one (the prompt's result, blocked inside its own apply) past the
// abandonment wait: the failure says the drain is unacknowledged, a retry in
// that window is refused without attaching, and once released the frame
// neither publishes nor records a completion. The retry after the drain
// acknowledges attaches and records the completion exactly once.
func TestClaudeReconnectAdmittedFrameStallBlocksOverlappingRetry(t *testing.T) {
	shim := newSpoolShim(t, nil)
	s, hosted := reconnectServer(t, shim.wsURL(), false)
	b := &spoolBuilder{key: hosted}
	b.add("recv", "", claudeInitLine)
	turn := b.prompt("admitted-action")
	b.message(turn, "final", "answer")
	b.result(turn)
	b.background(40)
	shim.frames = b.frames
	writeClaudeTestSpool(t, s.defaults.claudeShimSpools, hosted, b.frames...)
	seedAcceptedTransactionalTurn(t, s.turns, hosted, model.ProviderClaude, claudeResumeThreadID, "admitted-action", turn, "prompt")
	// A real hosted agent operation bound to the same provider turn: its
	// terminal status is what an admitted result frame could otherwise write.
	op, _, err := s.agentExecutions.beginTurn(hosted, "admitted-op", "prompt-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.markTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	operation := func() agentTurnRecord {
		rec, ok := s.agentExecutions.turn(op.ID)
		if !ok {
			t.Fatal("bound operation disappeared")
		}
		return rec
	}
	previous := claudeShimReplayTimeout
	claudeShimReplayTimeout = 300 * time.Millisecond
	t.Cleanup(func() { claudeShimReplayTimeout = previous })

	// The result frame nudges the catalog from inside applyWireSemantic:
	// after the gate admitted it, before publication and completion.
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
	var stalled *session
	shim.beforeAttach = func() {
		if stalled == nil {
			stalled = s.sessionForThread("claude", claudeResumeThreadID)
		}
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
	published := func() int64 {
		stalled.mu.Lock()
		defer stalled.mu.Unlock()
		return stalled.seq
	}
	before := published()

	// Retry while the admitted frame is still running: refused, no attach.
	shim.beforeAttach = nil
	attaches := len(shim.callsOf(shimMethodAttach))
	retry := httptest.NewRecorder()
	s.handleSessionCreate(retry, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(resumeBody(claudeResumeThreadID))))
	if retry.Code == http.StatusOK || !strings.Contains(retry.Body.String(), "still draining") {
		t.Fatalf("overlapping retry = %d %s, want a draining refusal", retry.Code, retry.Body.String())
	}
	if got := len(shim.callsOf(shimMethodAttach)); got != attaches || len(shim.callsOf(shimMethodStart)) != 0 {
		t.Fatalf("overlapping retry attached (%d→%d) or started %d children", attaches, got, len(shim.callsOf(shimMethodStart)))
	}

	unblock()
	waitForClaude(t, "the abandoned drain to acknowledge", func() bool { return claudeDrainSettled(stalled) == nil })
	if after := published(); after != before {
		t.Fatalf("abandoned generation published %d events after failure", after-before)
	}
	if n := completedTurnRows(t, s, hosted, turn); n != 0 {
		t.Fatalf("abandoned generation recorded %d completions", n)
	}
	if rec := operation(); rec.Status != agentTurnRunning || rec.FinalResponse != "" || rec.FinalItemID != "" {
		t.Fatalf("abandoned generation advanced the bound operation: %+v", rec)
	}
	var durableStatus string
	if err := s.controlMetadata.db.QueryRow(`SELECT status FROM hosted_turn_operations WHERE operation_id=?`, op.ID).Scan(&durableStatus); err != nil || durableStatus != agentTurnRunning {
		t.Fatalf("durable operation status = %q, %v; want still running", durableStatus, err)
	}

	claudeShimReplayTimeout = previous
	if response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID)); response.Code != http.StatusOK {
		t.Fatalf("retry after drain = %d %s", response.Code, response.Body.String())
	}
	waitForCompletedTurn(t, s, hosted, turn)
	if n := completedTurnRows(t, s, hosted, turn); n != 1 {
		t.Fatalf("completion rows = %d, want exactly one", n)
	}
	// The answer text is the saved file's (this server has no saved-result
	// reader); the operation must name the prompt's own final message.
	waitForClaude(t, "the bound operation to complete", func() bool { return operation().Status == agentTurnCompleted })
	if rec := operation(); rec.FinalItemID != "msg_final:0" {
		t.Fatalf("bound operation = %+v, want the prompt's final message msg_final:0", rec)
	}
}
