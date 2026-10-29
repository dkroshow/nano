package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/testsupport/claudeshim"
	"github.com/dkroshow/nano/internal/wire"
)

// Failure caught: the shim decodes a session's whole spool before it answers
// session/attach, so a fixed 10 s timeout abandoned attaches of large spools
// at boot and left the thread detached. The attach timeout grows with the
// spool and stays bounded. Boundary: the attach timeout.
func TestClaudeShimAttachTimeoutFollowsTheSpoolAndStaysBounded(t *testing.T) {
	if got := claudeShimAttachTimeout(0); got != claudeShimCallTimeout {
		t.Fatalf("empty spool timeout = %s, want %s", got, claudeShimCallTimeout)
	}
	if got := claudeShimAttachTimeout(357 << 20); got < 40*time.Second {
		t.Fatalf("357 MB spool timeout = %s, want at least 40s", got)
	}
	if got := claudeShimAttachTimeout(1 << 40); got != claudeShimAttachMaxTimeout {
		t.Fatalf("huge spool timeout = %s, want the %s bound", got, claudeShimAttachMaxTimeout)
	}
}

func claudeWireEvents(sess *Session) []wire.Event {
	sess.wire.mu.Lock()
	defer sess.wire.mu.Unlock()
	return append([]wire.Event(nil), sess.wire.events...)
}

// waitForClaudeThreadID waits for the native id the child announces on its
// system.init. A claude session now goes "ready" as soon as its transport is up
// (docs/decisions/claude-ready-on-transport.md), so status is no longer a proxy
// for "the handshake landed" — the identity is what these assertions are about.
func waitForClaudeThreadID(t *testing.T, sess *Session, want string) {
	t.Helper()
	waitForClaude(t, "native thread id "+want, func() bool { return sess.Summary().ThreadID == want })
}

func waitForClaudeStatus(t *testing.T, sess *Session, want string) {
	t.Helper()
	waitForClaude(t, "session status "+want, func() bool { return sess.Summary().Status == want })
}

func claudeShimSpawnRequest(sess *Session, shimURL string) ProviderRequest {
	return ProviderRequest{Provider: "claude", Cwd: sess.CWD(), Model: "sonnet", ClaudeBin: "/fake/claude", ClaudeShimWS: shimURL}
}

// Ready on transport (docs/decisions/claude-ready-on-transport.md): claude CLI
// 2.1.220 emits no system.init until it has been *sent* a turn, while the
// composer stays locked for as long as a session reads "starting" — so a
// session that waited for the handshake could never be sent the turn that
// produces it. A child that is up and streaming is a session the operator can
// type into, and that is what "ready" has to mean.
func TestClaudeSessionIsReadyOnceItsTransportIsUp(t *testing.T) {
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
		c.AnswerRest() // no init line, ever — the child is waiting for input
	})

	sess, attachments := claudeTestSession(t)
	sess.pendingName = "overnight run"
	sess.displayName = "overnight run"
	req := claudeShimSpawnRequest(sess, shim.WSURL())
	claude, err := startClaudeSession(sess, req)
	if err != nil {
		t.Fatalf("startClaudeSession: %v", err)
	}
	defer claude.Close()

	sum := sess.Summary()
	if sum.Status != "ready" {
		t.Fatalf("summary = %+v, want ready as soon as the transport is up", sum)
	}
	// Durability does NOT wait for the real init: the shim session id stands in
	// as the provisional identity (adoptDurableIdentity), so a restart before
	// the first prompt re-attaches to the waiting child instead of dropping it.
	if sum.ThreadID != "shim-1" {
		t.Fatalf("summary = %+v, want the shim handle as provisional identity", sum)
	}
	recs := attachments.All()
	want := hostedstore.Attachment{
		Provider: "claude", NativeThreadID: "shim-1", HostedThreadID: sess.HostedThreadID(),
		Bin: "/fake/claude", Args: claudeArgs(req), ShimSessionID: "shim-1",
		PendingName: "overnight run",
	}
	if len(recs) != 1 || !reflect.DeepEqual(recs[0], want) {
		t.Fatalf("attachments = %+v, want the one provisional record %+v", recs, want)
	}
	if !recs[0].Provisional() {
		t.Fatalf("record %+v should read as provisional", recs[0])
	}
}

// The attach rung gets the same readiness from the same rule. A spool that
// holds no init line — the shim started the child, nothing has been typed yet —
// must not leave a perfectly live session stuck at "starting".
func TestClaudeAttachIsReadyWithoutAReplayedInit(t *testing.T) {
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		attach, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(attach, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": "native-1",
			"state": "running", "lastSeq": 0,
		})
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 0})
		c.AnswerRest()
	})

	sess, _ := claudeTestSession(t)
	claude, err := attachClaudeSession(sess, shim.WSURL(), "native-1", claudeReplayPlan{}, claudeSpawnPosture{}, hostedstore.Attachment{})
	if err != nil {
		t.Fatalf("attachClaudeSession: %v", err)
	}
	defer claude.Close()

	if sum := sess.Summary(); sum.Status != "ready" {
		t.Fatalf("summary = %+v, want the attached session ready", sum)
	}
}

// A shim-hosted session records the child's wire exactly as a direct child
// does — recv lines in the buffer, system.init captured into identity — and,
// because this transport's child outlives nano, remembers the attachment with
// the posture a `--resume` fallback would need.
func TestClaudeShimStartRecordsWireAndRemembersAttachment(t *testing.T) {
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		// A real shim id (cs_ prefix) is a provisional Claude handle: the init
		// may then land before or after Nano adopts it. "shim-1" was not, so an
		// init after adoption could never resolve it.
		c.Reply(start, map[string]any{"shimSessionId": "cs_shim1"})
		c.Line(1, "recv", claudeInitLine)
		c.AnswerRest()
	})

	sess, attachments := claudeTestSession(t)
	req := claudeShimSpawnRequest(sess, shim.WSURL())
	claude, err := startClaudeSession(sess, req)
	if err != nil {
		t.Fatalf("startClaudeSession: %v", err)
	}
	defer claude.Close()

	starts := shim.CallsOf(shimMethodStart)
	if len(starts) != 1 {
		t.Fatalf("session/start calls = %d, want 1", len(starts))
	}
	var startParams struct {
		Bin  string   `json:"bin"`
		Args []string `json:"args"`
		Cwd  string   `json:"cwd"`
	}
	if err := json.Unmarshal(starts[0].Params, &startParams); err != nil {
		t.Fatal(err)
	}
	if startParams.Bin != "/fake/claude" || startParams.Cwd != sess.CWD() {
		t.Fatalf("session/start params = %+v", startParams)
	}
	if !reflect.DeepEqual(startParams.Args, claudeArgs(req)) {
		t.Fatalf("session/start args = %#v, want %#v", startParams.Args, claudeArgs(req))
	}

	waitForClaudeThreadID(t, sess, "native-1")
	// Data lines only: the control channel's own `initialize` frame also rides
	// the wire (claude_control.go) and races this read.
	events := claudeDataWireEvents(sess)
	if len(events) != 1 || events[0].Dir != "recv" || string(events[0].Msg) != claudeInitLine {
		t.Fatalf("wire events = %+v", events)
	}
	if sum := sess.Summary(); sum.ThreadID != "native-1" || sum.Model != "sonnet" {
		t.Fatalf("summary = %+v", sum)
	}

	// The init retires the provisional record (the shim handle that stood in
	// for the native id), leaving exactly the real one.
	want := hostedstore.Attachment{
		Provider: "claude", NativeThreadID: "native-1", HostedThreadID: sess.HostedThreadID(),
		Bin: "/fake/claude", Args: claudeArgs(req),
		ShimSessionID: "cs_shim1",
	}
	waitForClaude(t, "attachment record", func() bool {
		recs := attachments.All()
		return len(recs) == 1 && recs[0].NativeThreadID == "native-1"
	})
	if rec := attachments.All()[0]; !reflect.DeepEqual(rec, want) {
		t.Fatalf("attachment = %+v, want %+v", rec, want)
	}
}

// SendTurn goes out as session/input and is recorded exactly once: the shim
// spools the send line but never echoes it live, so there is no duplicate.
func TestClaudeShimSendTurnRecordsSendOnce(t *testing.T) {
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
		c.AnswerRest()
	})

	sess, _ := claudeTestSession(t)
	claude, err := startClaudeSession(sess, claudeShimSpawnRequest(sess, shim.WSURL()))
	if err != nil {
		t.Fatalf("startClaudeSession: %v", err)
	}
	defer claude.Close()

	if err := claude.SendTurn("hello"); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	// The control channel's own `initialize` frame also rides session/input
	// (claude_control.go), so the assertion is about the DATA lines: exactly one
	// send record for exactly one turn, no shim echo doubling it.
	lines := dataLines(shim.InputLines())
	if len(lines) != 1 || !strings.Contains(lines[0], `"hello"`) {
		t.Fatalf("session/input data lines = %#v", lines)
	}
	events := claudeDataWireEvents(sess)
	if len(events) != 1 || events[0].Dir != "send" || string(events[0].Msg) != lines[0] {
		t.Fatalf("wire events = %+v, want one send matching the input line", events)
	}
	if sess.Summary().TurnActive {
		t.Fatal("local send line invented provider work")
	}
}

func TestClaudeShimSendTurnPreservesSubmissionEventUUID(t *testing.T) {
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
		c.AnswerRest()
	})

	sess, _ := claudeTestSession(t)
	claude, err := startClaudeSession(sess, claudeShimSpawnRequest(sess, shim.WSURL()))
	if err != nil {
		t.Fatalf("startClaudeSession: %v", err)
	}
	defer claude.Close()
	eventID := submissionEventID("hosted", "action")
	if _, err := claude.sendTurnWithEventID("hello", eventID); err != nil {
		t.Fatalf("sendTurnWithEventID: %v", err)
	}
	lines := dataLines(shim.InputLines())
	if len(lines) != 1 {
		t.Fatalf("session/input data lines = %#v", lines)
	}
	var sent struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &sent); err != nil || sent.UUID != eventID {
		t.Fatalf("sent event UUID = %q, %v; want %q", sent.UUID, err, eventID)
	}
	events := claudeDataWireEvents(sess)
	if len(events) != 1 || string(events[0].Msg) != lines[0] {
		t.Fatalf("wire did not preserve the exact submitted frame: %+v", events)
	}
}

// A provider exit ends the attachment; a socket drop proves only connection loss.
func TestClaudeShimChildDeathMarksSessionExited(t *testing.T) {
	cases := []struct {
		name string
		end  func(c *claudeshim.Conn)
	}{
		{"exited notification", func(c *claudeshim.Conn) {
			c.Notify(shimMethodExited, map[string]any{"shimSessionId": "shim-1", "exitCode": 0})
			c.AnswerRest()
		}},
		{"connection drop", func(c *claudeshim.Conn) {}}, // returning closes the conn
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shim := newFakeShim(t, func(c *claudeshim.Conn) {
				start, ok := c.Next()
				if !ok {
					return
				}
				c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
				tc.end(c)
			})
			sess, _ := claudeTestSession(t)
			claude, err := startClaudeSession(sess, claudeShimSpawnRequest(sess, shim.WSURL()))
			if err != nil {
				t.Fatalf("startClaudeSession: %v", err)
			}
			want := "exited"
			if tc.name == "connection drop" {
				want = "reconnecting"
			}
			waitForClaudeStatus(t, sess, want)
			select {
			case <-claude.exited:
			default:
				t.Fatal("exited channel still open")
			}
		})
	}
}

// An unreachable shim is not a spawn failure: nano warns loudly, spawns the
// child itself, and — because that child dies with nano — remembers nothing.
func TestClaudeShimUnreachableFallsBackToDirectChild(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := "ws" + strings.TrimPrefix(dead.URL, "http")
	dead.Close()

	logs := &testsupport.LogBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	sess, attachments := claudeTestSession(t)
	req := claudeShimSpawnRequest(sess, deadURL)
	req.ClaudeBin = fakeClaudeBinary(t)
	claude, err := startClaudeSession(sess, req)
	if err != nil {
		t.Fatalf("startClaudeSession: %v", err)
	}
	defer claude.Close()

	waitForClaudeThreadID(t, sess, "native-direct")
	if sum := sess.Summary(); sum.Status != "ready" {
		t.Fatalf("summary = %+v, want the direct child ready", sum)
	}
	if got := logs.String(); !strings.Contains(got, "WARNING: claude shim unreachable — direct child spawn (no durability)") {
		t.Fatalf("missing fallback warning in log: %q", got)
	}
	if recs := attachments.All(); len(recs) != 0 {
		t.Fatalf("direct child remembered an attachment: %+v", recs)
	}
}

// Nano signs the children it spawns. A direct child inherits
// CLAUDE_CODE_ENTRYPOINT=nano, so the transcript it writes reads
// `entrypoint: nano` and the thread classifies interactive
// (internal/history/interaction.go) instead of hiding behind the Explorer's
// default filter as if it were an `sdk-cli` batch run.
func TestDirectClaudeChildCarriesTheNanoEntrypoint(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n" +
		`printf '{"type":"entrypoint","value":"%s"}\n' "$CLAUDE_CODE_ENTRYPOINT"` + "\n" +
		"cat >/dev/null\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	sess, _ := claudeTestSession(t)
	req := claudeShimSpawnRequest(sess, "") // no shim: the direct child path
	req.ClaudeBin = bin
	claude, err := startClaudeSession(sess, req)
	if err != nil {
		t.Fatalf("startClaudeSession: %v", err)
	}
	defer claude.Close()

	waitForClaude(t, "the child's entrypoint line", func() bool {
		return len(claudeDataWireEvents(sess)) > 0
	})
	got := string(claudeDataWireEvents(sess)[0].Msg)
	if want := `{"type":"entrypoint","value":"nano"}`; got != want {
		t.Fatalf("child reported %s, want %s", got, want)
	}
}

// Attach replays the spool through the same paths live lines take — send lines
// included, so a re-attached feed shows past user turns — re-derives identity
// from the replayed init line, and then follows live traffic.
func TestAttachClaudeSessionReplaysThenFollowsLive(t *testing.T) {
	const userLine = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`
	const resultLine = `{"type":"result","subtype":"success"}`
	const liveLine = `{"type":"assistant","message":{"role":"assistant"}}`

	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		attach, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(attach, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": "native-1",
			"state": "running", "lastSeq": 3,
		})
		c.Line(1, "recv", claudeInitLine)
		c.Line(2, "send", userLine)
		c.Line(2, "send", userLine) // replay duplicate: seq, not content, is the key
		c.Line(3, "recv", resultLine)
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 3})
		c.Line(4, "recv", liveLine)
		c.AnswerRest()
	})

	sess, _ := claudeTestSession(t)
	posture := claudeSpawnPosture{Bin: "/fake/claude", Args: []string{"-p"}}
	claude, err := attachClaudeSession(sess, shim.WSURL(), "native-1", claudeReplayPlan{}, posture, hostedstore.Attachment{})
	if err != nil {
		t.Fatalf("attachClaudeSession: %v", err)
	}
	defer claude.Close()

	var attachParams struct {
		NativeSessionID string `json:"nativeSessionId"`
		FromSeq         int64  `json:"fromSeq"`
	}
	calls := shim.CallsOf(shimMethodAttach)
	if len(calls) != 1 {
		t.Fatalf("session/attach calls = %d, want 1", len(calls))
	}
	if err := json.Unmarshal(calls[0].Params, &attachParams); err != nil {
		t.Fatal(err)
	}
	if attachParams.NativeSessionID != "native-1" || attachParams.FromSeq != 0 {
		t.Fatalf("session/attach params = %+v", attachParams)
	}

	// Replay has landed by the time attach returns: identity is re-derived.
	if sum := sess.Summary(); sum.Status != "ready" || sum.ThreadID != "native-1" || sum.Model != "sonnet" {
		t.Fatalf("summary after attach = %+v", sum)
	}
	// The control channel starts as soon as replay completes, so its initialize
	// request is a legitimate fifth wire event. Count provider data lines only;
	// the assertion is about replay/live-follow, not control-channel timing.
	waitForClaude(t, "live line", func() bool { return len(claudeDataWireEvents(sess)) == 4 })
	events := claudeDataWireEvents(sess)
	var got []string
	for _, evt := range events {
		got = append(got, evt.Dir+" "+string(evt.Msg))
	}
	want := []string{
		"recv " + claudeInitLine, "send " + userLine,
		"recv " + resultLine, "recv " + liveLine,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire events = %#v, want %#v", got, want)
	}
}

func TestClaudeShimTransportCarriesAcceptedItemAndResultTurnID(t *testing.T) {
	const turnID = "claude:nht_claude_test:7"
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		var startParams map[string]any
		if err := json.Unmarshal(start.Params, &startParams); err != nil {
			t.Error(err)
		}
		if got := startParams["turnIdSession"]; got != "nht_claude_test" {
			t.Errorf("turnIdSession = %v, want the stable hosted session id", got)
		}
		c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
		input, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(input, map[string]any{"turnId": turnID})
		c.Notify(shimMethodLine, map[string]any{
			"shimSessionId": "shim-1", "seq": 8, "dir": "recv", "turnId": turnID,
			"line": `{"type":"assistant","message":{"role":"assistant","content":"answer"}}`,
		})
		c.Notify(shimMethodLine, map[string]any{
			"shimSessionId": "shim-1", "seq": 9, "dir": "recv", "turnId": turnID,
			"line": `{"type":"result","subtype":"success"}`,
		})
		c.AnswerRest()
	})

	sess, _ := claudeTestSession(t)
	owner := newClaudeSession(sess, "native-1")
	tr, err := startClaudeShimSession(owner, shim.WSURL(), claudeSpawnPosture{})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()

	var mu sync.Mutex
	type observedShimEvent struct {
		Kind           string
		ProviderTurnID string
	}
	var events []observedShimEvent
	setSink := reflect.ValueOf(tr).MethodByName("SetTurnEventSink")
	if !setSink.IsValid() {
		t.Fatal("Claude shim transport does not surface provider turn events")
	}
	sinkType := setSink.Type().In(0)
	sink := reflect.MakeFunc(sinkType, func(args []reflect.Value) []reflect.Value {
		event := args[0]
		mu.Lock()
		defer mu.Unlock()
		events = append(events, observedShimEvent{
			Kind:           event.FieldByName("Kind").String(),
			ProviderTurnID: event.FieldByName("ProviderTurnID").String(),
		})
		return nil
	})
	setSink.Call([]reflect.Value{sink})
	if err := tr.sendLine([]byte(`{"type":"user","message":{"role":"user","content":"question"}}`)); err != nil {
		t.Fatal(err)
	}
	waitForClaude(t, "accepted, item, and result shim events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) == 3
	})

	mu.Lock()
	defer mu.Unlock()
	wantKinds := map[string]bool{
		"accepted": false, "item": false, "result": false,
	}
	for i, event := range events {
		if _, ok := wantKinds[event.Kind]; !ok || wantKinds[event.Kind] || event.ProviderTurnID != turnID {
			t.Fatalf("event %d = %+v, want one accepted, item, and result with provider turn id %q", i, event, turnID)
		}
		wantKinds[event.Kind] = true
	}
}

// Attaching to a session whose child already died replays first, then ends
// exited — the shim's exited notification follows replayDone.
func TestAttachClaudeSessionExitedStateEndsExited(t *testing.T) {
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		attach, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(attach, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": "native-1",
			"state": "exited", "lastSeq": 1,
		})
		c.Line(1, "recv", claudeInitLine)
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
		c.Notify(shimMethodExited, map[string]any{"shimSessionId": "shim-1", "exitCode": 0})
		c.AnswerRest()
	})

	sess, _ := claudeTestSession(t)
	claude, err := attachClaudeSession(sess, shim.WSURL(), "native-1", claudeReplayPlan{}, claudeSpawnPosture{}, hostedstore.Attachment{})
	if err != nil {
		t.Fatalf("attachClaudeSession: %v", err)
	}
	defer claude.Close()

	if events := claudeWireEvents(sess); len(events) != 1 {
		t.Fatalf("replay did not complete before attach returned: %+v", events)
	}
	waitForClaudeStatus(t, sess, "exited")
}

// Receiving replayDone is not sufficient: the ordered session loop must have
// applied the replay before attach returns, even when the provider already exited.
func TestClaudeAttachWaitsForReplayProcessing(t *testing.T) {
	for _, state := range []string{"running", "exited"} {
		t.Run(state, func(t *testing.T) {
			sess, _ := claudeTestSession(t)
			if err := sess.ensureCoreSession(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sess.core.Close)
			blocked, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			replaySent := make(chan struct{})
			shim := newFakeShim(t, func(c *claudeshim.Conn) {
				attach, ok := c.Next()
				if !ok {
					return
				}
				// Attach has completed outbox restoration. Hold the processing
				// loop now, while leaving the socket reader free to receive replay.
				sess.core.ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error {
					close(blocked)
					<-release
					return nil
				}})
				<-blocked
				c.Reply(attach, map[string]any{
					"shimSessionId": "shim-1", "nativeSessionId": "native-1",
					"state": state, "lastSeq": 1,
				})
				c.Line(1, "recv", claudeInitLine)
				c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
				if state == "exited" {
					c.Notify(shimMethodExited, map[string]any{"shimSessionId": "shim-1", "exitCode": 0})
				}
				close(replaySent)
				c.AnswerRest()
			})
			type result struct {
				claude *claudeSession
				err    error
			}
			attached := make(chan result, 1)
			go func() {
				c, err := attachClaudeSession(sess, shim.WSURL(), "native-1", claudeReplayPlan{}, claudeSpawnPosture{}, hostedstore.Attachment{})
				attached <- result{c, err}
			}()
			select {
			case <-replaySent:
			case <-time.After(3 * time.Second):
				t.Fatal("shim did not send replay")
			}
			select {
			case got := <-attached:
				unblock()
				if got.claude != nil {
					got.claude.Close()
				}
				t.Fatalf("attach returned before replay processing was released: %v", got.err)
			case <-time.After(150 * time.Millisecond):
			}
			if state == "running" && sess.Summary().Status == "ready" {
				t.Fatal("attachment became ready before replay processing finished")
			}
			unblock()
			select {
			case got := <-attached:
				if got.err != nil {
					t.Fatal(got.err)
				}
				defer got.claude.Close()
				// The live control handshake may already have added send frames.
				// Check the exact replayed receive row, not the whole wire length.
				replayed := 0
				for _, event := range claudeWireEvents(sess) {
					if event.Dir == "recv" && string(event.Msg) == claudeInitLine {
						replayed++
					}
				}
				if replayed != 1 {
					t.Fatalf("attach returned with %d copies of the replayed row, want 1", replayed)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("attach did not finish after replay processing was released")
			}
		})
	}
}

func TestClaudeAttachRejectsReplayWhenProcessingLoopClosed(t *testing.T) {
	sess, _ := claudeTestSession(t)
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		attach, ok := c.Next()
		if !ok {
			return
		}
		// Outbox restoration created the core before opening this socket.
		// Received replay cannot be settled if its processor is gone.
		sess.core.Close()
		c.Reply(attach, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": "native-1",
			"state": "running", "lastSeq": 1,
		})
		c.Line(1, "recv", claudeInitLine)
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
		c.AnswerRest()
	})
	c, err := attachClaudeSession(sess, shim.WSURL(), "native-1", claudeReplayPlan{}, claudeSpawnPosture{}, hostedstore.Attachment{})
	if c != nil {
		defer c.Close()
	}
	if !errors.Is(err, errClaudeReplayUnsettled) {
		t.Fatalf("attach = %v, %v; want replay-processing refusal", c, err)
	}
}

// The shim's attach failures are typed so the resume ladder can branch: another
// client holds the session, or the shim never knew it.
func TestAttachClaudeSessionTypedErrors(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		message string
		want    error
	}{
		{"already attached", shimCodeSessionAttached, "session_attached", errShimSessionAttached},
		{"unknown session", shimCodeSessionNotFound, "session_not_found", errShimSessionNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shim := newFakeShim(t, func(c *claudeshim.Conn) {
				attach, ok := c.Next()
				if !ok {
					return
				}
				c.ReplyError(attach, tc.code, tc.message)
				c.AnswerRest()
			})
			sess, _ := claudeTestSession(t)
			claude, err := attachClaudeSession(sess, shim.WSURL(), "native-1", claudeReplayPlan{}, claudeSpawnPosture{}, hostedstore.Attachment{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("attachClaudeSession err = %v, want %v", err, tc.want)
			}
			if claude != nil {
				t.Fatal("failed attach returned a session")
			}
		})
	}
}

// A boot re-attach can replay thousands of lines. The WebSocket reader must
// keep draining while the slower transcript projection consumes them, or the
// shim's bounded outbound queue fills and severs an otherwise healthy child.
// This is the exact production shape that stranded a 1,670-line session.
func TestAttachClaudeSessionDrainsLargeReplayBeforeFollowingLive(t *testing.T) {
	const replayCount = 1100 // deliberately beyond the real shim's 1,024-frame queue
	const largeReplayTimeout = 30 * time.Second
	writeResult := make(chan int, 1)
	padding := strings.Repeat("x", 8*1024)
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		attach, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(attach, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": "native-1",
			"state": "running", "lastSeq": replayCount,
		})
		for seq := 1; seq <= replayCount; seq++ {
			msg := map[string]any{
				"jsonrpc": "2.0", "method": shimMethodLine,
				"params": map[string]any{
					"shimSessionId": "shim-1", "seq": seq, "dir": "recv",
					"line": `{"type":"system","subtype":"status","pad":"` + padding + `"}`,
				},
			}
			if err := c.WriteWithin(msg, 2*time.Second); err != nil {
				writeResult <- seq - 1
				return
			}
		}
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": replayCount})
		c.Line(replayCount+1, "recv", `{"type":"assistant","message":{"role":"assistant","content":"live"}}`)
		writeResult <- replayCount
		c.AnswerRest()
	})

	sess, _ := claudeTestSession(t)
	sess.projection = newBlockedProjection("claude")
	// Make projection consumption slower than socket ingestion. A correct
	// transport keeps reading and queues ordered notifications until this lock
	// is released; the old transport blocked its sole reader here.
	sess.projection.mu.Lock()
	locked := true
	defer func() {
		if locked {
			sess.projection.mu.Unlock()
		}
	}()
	attached := make(chan struct {
		claude *claudeSession
		err    error
	}, 1)
	go func() {
		claude, err := attachClaudeSession(sess, shim.WSURL(), "native-1", claudeReplayPlan{}, claudeSpawnPosture{}, hostedstore.Attachment{})
		attached <- struct {
			claude *claudeSession
			err    error
		}{claude: claude, err: err}
	}()

	var wrote int
	select {
	case wrote = <-writeResult:
	case <-time.After(largeReplayTimeout):
		t.Fatal("shim did not finish the bounded large replay write")
	}
	if wrote != replayCount {
		t.Fatalf("shim socket stalled after %d replay lines, want all %d drained", wrote, replayCount)
	}
	sess.projection.mu.Unlock()
	locked = false

	var result struct {
		claude *claudeSession
		err    error
	}
	select {
	case result = <-attached:
	case <-time.After(largeReplayTimeout):
		t.Fatal("attach did not finish after projection resumed")
	}
	if result.err != nil {
		t.Fatalf("attachClaudeSession: %v", result.err)
	}
	defer result.claude.Close()
	waitForClaudeWithin(t, largeReplayTimeout, "live line after large replay", func() bool {
		return result.claude.tr.lastSeq() == replayCount+1
	})
}

func TestClaudeReattachWithTwoHundredSpooledLinesDoesNotBlockCoreMailbox(t *testing.T) {
	sess, _ := claudeTestSession(t)
	sess.projection = newBlockedProjection("claude")
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	owner := newClaudeSession(sess, "native-1")
	transport := &claudeShimTransport{owner: owner, replayDone: make(chan struct{})}
	for seq := int64(1); seq <= 200; seq++ {
		params, err := json.Marshal(map[string]any{
			"shimSessionId": "shim-1", "seq": seq, "dir": "recv",
			"line": `{"type":"system","subtype":"status"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		transport.onNotify(shimMethodLine, params)
	}

	resumed := make(chan error, 1)
	go func() {
		resumed <- sess.core.ResumeLegacy(context.Background(), func(context.Context) error {
			transport.markSessionReady()
			return nil
		})
	}()
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reattach blocked while replaying 200 spooled lines")
	}
	closed := make(chan struct{})
	go func() { sess.core.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not drain the replayed lines")
	}
}

// Notification ingestion and transcript projection are separate concerns:
// processing one slow line must not stop the socket reader from accepting the
// next line, and the eventual projection order must remain exact.
func TestClaudeShimNotificationIngestionDoesNotBlockOnProjection(t *testing.T) {
	sess, _ := claudeTestSession(t)
	sess.projection = newBlockedProjection("claude")
	owner := newClaudeSession(sess, "native-1")
	tr := &claudeShimTransport{owner: owner, replayDone: make(chan struct{})}

	line := func(seq int64, text string) json.RawMessage {
		params, err := json.Marshal(map[string]any{
			"shimSessionId": "shim-1", "seq": seq, "dir": "recv",
			"line": `{"type":"assistant","message":{"role":"assistant","content":"` + text + `"}}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		return params
	}

	tr.onNotify(shimMethodLine, line(1, "first"))
	sess.projection.mu.Lock()
	locked := true
	defer func() {
		if locked {
			sess.projection.mu.Unlock()
		}
	}()
	ready := make(chan struct{})
	go func() {
		tr.markSessionReady()
		close(ready)
	}()
	// Give markSessionReady time to enter projection while its lock is held.
	time.Sleep(20 * time.Millisecond)
	accepted := make(chan struct{})
	go func() {
		tr.onNotify(shimMethodLine, line(2, "second"))
		close(accepted)
	}()
	select {
	case <-accepted:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("onNotify blocked behind transcript projection")
	}
	sess.projection.mu.Unlock()
	locked = false
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("notification drain did not finish")
	}
	waitForClaude(t, "both ordered notifications", func() bool { return tr.lastSeq() == 2 })
	events := claudeDataWireEvents(sess)
	if len(events) != 2 || !strings.Contains(string(events[0].Msg), `"first"`) || !strings.Contains(string(events[1].Msg), `"second"`) {
		t.Fatalf("notification order = %+v, want first then second", events)
	}
}

// While boot replay is still being projected the session is starting, not
// broken. A submitted prompt must enter the durable FIFO and be delivered as
// soon as the attach becomes ready.
func TestClaudePromptDuringReplayDrainQueuesThenDelivers(t *testing.T) {
	replaySent := make(chan struct{})
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		attach, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(attach, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": "native-1",
			"state": "running", "lastSeq": 1,
		})
		c.Line(1, "recv", `{"type":"system","subtype":"status"}`)
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
		close(replaySent)
		c.AnswerRest()
	})

	sess, _ := claudeTestSession(t)
	sess.projection = newBlockedProjection("claude")

	sess.projection.mu.Lock()
	locked := true
	defer func() {
		if locked {
			sess.projection.mu.Unlock()
		}
	}()
	attached := make(chan struct {
		claude *claudeSession
		err    error
	}, 1)
	go func() {
		claude, err := attachClaudeSession(sess, shim.WSURL(), "native-1", claudeReplayPlan{}, claudeSpawnPosture{}, hostedstore.Attachment{})
		attached <- struct {
			claude *claudeSession
			err    error
		}{claude: claude, err: err}
	}()
	select {
	case <-replaySent:
	case <-time.After(time.Second):
		t.Fatal("shim did not send replay")
	}

	submitted := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/nano-session/turn", strings.NewReader(`{"text":"deliver after replay","clientActionId":"during-replay"}`))
		sess.delivery.HandleTurn(rec, req)
		submitted <- rec
	}()
	// Admission publishes the owner snapshot before acknowledging the prompt,
	// so it may wait briefly for replay's projection critical section.
	time.Sleep(20 * time.Millisecond)
	sess.projection.mu.Unlock()
	locked = false
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-submitted:
	case <-time.After(time.Second):
		t.Fatal("submission did not resume after replay projection unlocked")
	}
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"queued"`) {
		t.Fatalf("status = %d, body = %s, want durable queued operation", rec.Code, rec.Body.String())
	}
	queued, ok := sess.delivery.queuedTurns.Get("during-replay")
	if !ok || queued.Text != "deliver after replay" {
		t.Fatalf("queued prompt = %+v, found = %v", queued, ok)
	}

	var result struct {
		claude *claudeSession
		err    error
	}
	select {
	case result = <-attached:
	case <-time.After(3 * time.Second):
		t.Fatal("attach did not finish after replay projection resumed")
	}
	if result.err != nil {
		t.Fatalf("attachClaudeSession: %v", result.err)
	}
	defer result.claude.Close()
	waitTurnSendDone(t, sess, "during-replay")
	journal, ok := sess.delivery.sendAttempts.Get("during-replay")
	if !ok || journal.Status != sendAttemptAccepted {
		t.Fatalf("send journal = %+v, found = %v, want accepted", journal, ok)
	}
	lines := shim.InputLines()
	found := false
	for _, line := range lines {
		if strings.Contains(line, "deliver after replay") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("shim input lines = %q, queued prompt was not delivered", lines)
	}
}
