package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnResponsesBypassReplayNotifications(t *testing.T) {
	c := &conn{
		out:         make(chan []byte, 1),
		priorityOut: make(chan []byte, 1),
		done:        make(chan struct{}),
	}
	c.notify(methodLine, lineParams{Seq: 1, Line: "replayed"})
	id := json.RawMessage("7")
	c.respond(&id, struct{}{}, nil)

	select {
	case frame := <-c.priorityOut:
		var response rpcResponse
		if err := json.Unmarshal(frame, &response); err != nil {
			t.Fatal(err)
		}
		if response.ID == nil || string(*response.ID) != "7" {
			t.Fatalf("priority response = %+v", response)
		}
	default:
		t.Fatal("response was not placed on the priority queue")
	}
	if len(c.out) != 1 {
		t.Fatalf("replay notification queue length = %d, want 1", len(c.out))
	}
}

func TestAttachStopsAfterFirstReplayQueueFailure(t *testing.T) {
	sp, err := newSpool(t.TempDir(), "replay-drop")
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close(true)
	for seq := int64(1); seq <= 3; seq++ {
		if err := sp.append(spoolEvent{Seq: seq, Dir: dirRecv, Line: "line"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	c := &conn{
		out: make(chan []byte), priorityOut: make(chan []byte, 1), done: done,
		sessions: map[string]*shimSession{},
	}
	s := &shimSession{id: "replay-drop", state: stateRunning, spool: sp}
	err = s.attach(c, 0, func(sessionInfo) {})
	if !errors.Is(err, errReplayClientDropped) {
		t.Fatalf("attach error = %v, want replay client dropped", err)
	}
	if got := len(c.out); got != 0 {
		t.Fatalf("queued notifications = %d, want replay to stop at the first failed send", got)
	}
	if info := s.info(); info.Attached {
		t.Fatalf("failed replay left session attached: %+v", info)
	}
}

type failingInput struct{}

func (failingInput) Write([]byte) (int, error) { return 0, errors.New("stdin closed") }
func (failingInput) Close() error              { return nil }

func TestFailedInputIsNotSpooledAsDelivered(t *testing.T) {
	sp, err := newSpool(t.TempDir(), "failed-input")
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close(true)
	s := &shimSession{state: stateRunning, stdin: io.WriteCloser(failingInput{}), spool: sp}

	if _, err := s.writeInputWithTurnID("not delivered"); err == nil {
		t.Fatal("writeInputWithTurnID succeeded with a failing stdin")
	}
	lines, err := sp.readAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 || s.seq != 0 {
		t.Fatalf("failed input left delivery evidence: seq=%d lines=%+v", s.seq, lines)
	}
}

func TestShimSessionTracksProviderTurnActivity(t *testing.T) {
	sp, err := newSpool(t.TempDir(), "turn-activity")
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close(true)
	s := &shimSession{id: "turn-activity", state: stateRunning, spool: sp}

	if info := s.info(); info.TurnActive {
		t.Fatalf("new idle child reports an active turn: %+v", info)
	}
	s.record(dirRecv, `{"type":"assistant","message":{"role":"assistant"}}`, false)
	if info := s.info(); !info.TurnActive {
		t.Fatalf("assistant execution evidence did not assert activity: %+v", info)
	}
	s.record(dirRecv, `{"type":"result","subtype":"success"}`, false)
	if info := s.info(); info.TurnActive {
		t.Fatalf("terminal result did not clear activity: %+v", info)
	}
	s.record(dirRecv, `{"type":"user","origin":{"kind":"task-notification"}}`, false)
	if info := s.info(); !info.TurnActive {
		t.Fatalf("task notification did not assert activity: %+v", info)
	}
}

func TestAttachReportsTrackedTurnActivity(t *testing.T) {
	sh := newTestShim(t)
	first, id := sh.start("nat-active", "raw")
	first.waitLines(1)
	first.input(id, `{"type":"user","message":{"role":"user","content":"work"}}`)
	first.close()
	sh.waitDetached(id)

	second := sh.dial()
	var res attachResult
	second.mustCall(methodAttach, attachParams{ShimSessionID: id}, &res)
	if res.State != stateRunning || !res.TurnActive {
		t.Fatalf("attach result = %+v, want a live child with an active turn", res)
	}
}

func TestStartSpoolsBothDirectionsAndNotifiesOnlyRecv(t *testing.T) {
	sh := newTestShim(t)
	client, id := sh.start("nat-echo")
	client.waitLines(1) // the child's init line

	client.input(id, "hello")
	lines := client.waitLines(2)

	// The sender already has its own line, so only recv lines are notified —
	// and the send line's sequence number is simply absent from the stream.
	for _, line := range lines {
		if line.Dir != dirRecv {
			t.Fatalf("notified a %q line; only recv lines should reach the sender: %+v", line.Dir, line)
		}
		if line.ShimSessionID != id {
			t.Fatalf("line notification carried session %q, want %q", line.ShimSessionID, id)
		}
	}
	if lines[0].Seq != 1 || lines[1].Seq != 3 {
		t.Fatalf("notified seqs %d,%d; want 1,3 (2 is the un-notified send line)", lines[0].Seq, lines[1].Seq)
	}
	if !strings.Contains(lines[1].Line, `"hello"`) {
		t.Fatalf("echo line %q does not contain the input", lines[1].Line)
	}

	events := sh.spoolEvents(id)
	if len(events) != 3 {
		t.Fatalf("spooled %d events, want 3 (init, send, echo): %+v", len(events), events)
	}
	for i, evt := range events {
		if evt.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, evt.Seq, i+1)
		}
		if evt.TS == "" {
			t.Fatalf("event %d has no timestamp: %+v", i, evt)
		}
	}
	if events[0].Dir != dirRecv || !strings.Contains(events[0].Line, `"subtype":"init"`) {
		t.Fatalf("first spooled event is not the init line: %+v", events[0])
	}
	if events[1].Dir != dirSend || events[1].Line != "hello" {
		t.Fatalf("second spooled event is not the verbatim send line: %+v", events[1])
	}
	if events[2].Dir != dirRecv {
		t.Fatalf("third spooled event should be the echo: %+v", events[2])
	}
	if got := sh.session(id).native(); got != "nat-echo" {
		t.Fatalf("init probe recorded native id %q, want %q", got, "nat-echo")
	}
}

func TestChildRunsInRequestedCwd(t *testing.T) {
	sh := newTestShim(t)
	client, id := sh.start("nat-cwd")
	client.waitLines(1)

	client.input(id, "pwd")
	lines := client.waitLines(2)

	var reported struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(lines[1].Line), &reported); err != nil {
		t.Fatalf("decoding the child's pwd line %q: %v", lines[1].Line, err)
	}
	// /var vs /private/var on darwin: compare the resolved paths.
	if got, want := resolve(t, reported.Cwd), resolve(t, sh.cwd); got != want {
		t.Fatalf("child ran in %s, want %s", got, want)
	}
}

// Every child the shim owns is one of nano's, so the shim signs them all: they
// inherit CLAUDE_CODE_ENTRYPOINT=nano and their transcripts read
// `entrypoint: nano`, which classifies the thread interactive instead of
// hiding it as an `sdk-cli` batch run (nano's internal/history/interaction.go).
// The override matters, not just the value: the shim's own environment can
// carry an inherited entrypoint.
func TestChildIsSignedWithTheNanoEntrypoint(t *testing.T) {
	sh := newTestShim(t)
	client, id := sh.start("nat-entrypoint")
	client.waitLines(1)

	client.input(id, "entrypoint")
	lines := client.waitLines(2)

	want := fmt.Sprintf("{\"type\":\"entrypoint\",\"value\":%q}", nanoEntrypoint)
	if got := lines[1].Line; got != want {
		t.Fatalf("child reported %s, want %s", got, want)
	}
}

func resolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolving %s: %v", path, err)
	}
	return resolved
}

func TestAttachReplaysFromSeqThenGoesLive(t *testing.T) {
	sh := newTestShim(t)
	first, id := sh.start("nat-replay")
	first.waitLines(1)
	first.input(id, "one")
	first.waitLines(2)
	first.input(id, "two")
	first.waitLines(3)

	first.close()
	sh.waitDetached(id)

	// Sequences so far: 1 init(recv) 2 "one"(send) 3 echo(recv)
	//                   4 "two"(send) 5 echo(recv)
	second := sh.dial()
	var res attachResult
	second.mustCall(methodAttach, attachParams{ShimSessionID: id, FromSeq: 2}, &res)
	if res.ShimSessionID != id || res.NativeSessionID != "nat-replay" {
		t.Fatalf("attach result identifies the wrong session: %+v", res)
	}
	if res.State != stateRunning || res.LastSeq != 5 {
		t.Fatalf("attach result = %+v, want state %q lastSeq 5", res, stateRunning)
	}

	replayed := second.waitLines(3)
	wantDirs := []string{dirRecv, dirSend, dirRecv}
	for i, line := range replayed {
		if line.Seq != int64(i+3) {
			t.Fatalf("replayed event %d has seq %d, want %d", i, line.Seq, i+3)
		}
		if line.Dir != wantDirs[i] {
			t.Fatalf("replayed event %d has dir %q, want %q", i, line.Dir, wantDirs[i])
		}
	}
	if replayed[1].Line != "two" {
		t.Fatalf("replay dropped the send line's payload: %+v", replayed[1])
	}

	var done replayDoneParams
	second.decode(second.waitMethod(methodReplayDone), &done)
	if done.ShimSessionID != id || done.LastSeq != 5 {
		t.Fatalf("replayDone = %+v, want session %s lastSeq 5", done, id)
	}

	// Live traffic continues on the new connection, from where replay stopped.
	second.input(id, "three")
	live := second.waitLines(4)
	if live[3].Seq != 7 || live[3].Dir != dirRecv || !strings.Contains(live[3].Line, `"three"`) {
		t.Fatalf("live line after replay = %+v, want seq 7 recv echoing \"three\"", live[3])
	}
}

// TestReplayLiveBoundaryHasNoGapsOrDuplicates is the ordering guarantee: a
// client attaches while the child is mid-stream, and the union of replayed and
// live events must be exactly the sequence 1..N — nothing lost between the
// spool snapshot and the attachment, nothing delivered twice.
func TestReplayLiveBoundaryHasNoGapsOrDuplicates(t *testing.T) {
	const (
		burstLines   = 200
		burstDelayMs = 3
	)
	sh := newTestShim(t)
	first, id := sh.start("nat-boundary")
	first.waitLines(1)

	first.input(id, "burst 200 3") // ~600ms of streaming
	first.waitLines(3)             // some of it lands while the first client is attached
	first.close()
	sh.waitDetached(id)

	second := sh.dial()
	var res attachResult
	second.mustCall(methodAttach, attachParams{ShimSessionID: id, FromSeq: 0}, &res)

	total := burstLines + 2 // init + the burst command (send) + the burst lines
	if res.LastSeq >= int64(total) {
		t.Fatalf("child finished before the attach (lastSeq %d of %d); the boundary was never exercised", res.LastSeq, total)
	}

	lines := second.waitLines(total)
	if len(lines) != total {
		t.Fatalf("received %d line notifications, want exactly %d", len(lines), total)
	}
	for i, line := range lines {
		if line.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d — gap or duplicate across the replay/live boundary", i, line.Seq, i+1)
		}
	}
}

// TestResultsPrecedeTheirNotifications pins the ordering nano's client relies
// on: session/start and session/attach answer before any notification about
// the session they name — an attach by native id learns the shim session id
// from that result, and every session/line quotes it.
func TestResultsPrecedeTheirNotifications(t *testing.T) {
	sh := newTestShim(t)
	first, id := sh.start("nat-order")
	first.waitLines(1)
	if kinds := first.frameKinds(); kinds[0] != "result" {
		t.Fatalf("frames on the starting connection = %v, want the start result first", kinds)
	}

	first.close()
	sh.waitDetached(id)

	second := sh.dial()
	var res attachResult
	second.mustCall(methodAttach, attachParams{NativeSessionID: "nat-order", FromSeq: 0}, &res)
	second.waitMethod(methodReplayDone)

	want := []string{"result", methodLine, methodReplayDone}
	kinds := second.frameKinds()
	if len(kinds) != len(want) {
		t.Fatalf("frames on the attaching connection = %v, want %v", kinds, want)
	}
	for i, kind := range want {
		if kinds[i] != kind {
			t.Fatalf("frame %d was %s, want %s (full order %v)", i, kinds[i], kind, kinds)
		}
	}
}

func TestSecondConnectionCannotAttach(t *testing.T) {
	sh := newTestShim(t)
	first, id := sh.start("nat-guard")
	first.waitLines(1)

	second := sh.dial()
	rpcErr := second.callErr(methodAttach, attachParams{ShimSessionID: id, FromSeq: 0})
	if rpcErr.Code != codeSessionAttached || rpcErr.Message != "session_attached" {
		t.Fatalf("second attach returned %+v, want code %d session_attached", rpcErr, codeSessionAttached)
	}

	// The holder may re-attach to itself (re-syncing from an earlier seq).
	var res attachResult
	first.mustCall(methodAttach, attachParams{ShimSessionID: id, FromSeq: 0}, &res)

	first.close()
	sh.waitDetached(id)
	second.mustCall(methodAttach, attachParams{ShimSessionID: id, FromSeq: 0}, &res)
	if !sh.session(id).info().Attached {
		t.Fatal("session is not attached after the second connection took it")
	}
}

func TestNativeSessionIDAddressing(t *testing.T) {
	sh := newTestShim(t)
	first, id := sh.start("nat-lazy", fakeChildLazy)

	// Before the init probe has seen it, the native id addresses nothing.
	second := sh.dial()
	rpcErr := second.callErr(methodAttach, attachParams{NativeSessionID: "nat-lazy", FromSeq: 0})
	if rpcErr.Code != codeSessionNotFound {
		t.Fatalf("attach before init returned %+v, want code %d", rpcErr, codeSessionNotFound)
	}

	first.input(id, "init")
	first.waitLines(1)
	if got := sh.session(id).native(); got != "nat-lazy" {
		t.Fatalf("native id after the init line is %q, want %q", got, "nat-lazy")
	}
	first.close()
	sh.waitDetached(id)

	var res attachResult
	second.mustCall(methodAttach, attachParams{NativeSessionID: "nat-lazy", FromSeq: 0}, &res)
	if res.ShimSessionID != id || res.NativeSessionID != "nat-lazy" || res.State != stateRunning {
		t.Fatalf("attach by native id = %+v, want session %s running", res, id)
	}
}

func TestExitedSessionIsRetainedAndReplayed(t *testing.T) {
	sh := newTestShim(t)
	first, id := sh.start("nat-exit")
	first.waitLines(1)

	first.input(id, "quit") // the child prints one line, then exits 7
	first.waitLines(2)

	var exited exitedParams
	first.decode(first.waitMethod(methodExited), &exited)
	if exited.ShimSessionID != id || exited.ExitCode != fakeChildExitCode {
		t.Fatalf("exited notification = %+v, want session %s code %d", exited, id, fakeChildExitCode)
	}

	rpcErr := first.callErr(methodInput, inputParams{ShimSessionID: id, Line: "anyone there"})
	if rpcErr.Code != codeSessionExited || rpcErr.Message != "session_exited" {
		t.Fatalf("input to an exited session returned %+v, want code %d session_exited", rpcErr, codeSessionExited)
	}

	var list listResult
	first.mustCall(methodList, struct{}{}, &list)
	if len(list.Sessions) != 1 {
		t.Fatalf("session/list returned %d rows, want the exited session retained: %+v", len(list.Sessions), list.Sessions)
	}
	row := list.Sessions[0]
	if row.State != stateExited || row.LastSeq != 3 || !row.Attached {
		t.Fatalf("list row = %+v, want exited, lastSeq 3, attached", row)
	}
	if row.Cwd != sh.cwd || row.NativeSessionID != "nat-exit" || row.StartedAt == "" {
		t.Fatalf("list row = %+v, want cwd %s and the native id filled in", row, sh.cwd)
	}

	first.close()
	sh.waitDetached(id)

	// A client returning after the death replays the gap, then learns of it.
	second := sh.dial()
	var res attachResult
	second.mustCall(methodAttach, attachParams{ShimSessionID: id, FromSeq: 0}, &res)
	if res.State != stateExited || res.LastSeq != 3 {
		t.Fatalf("attach to an exited session = %+v, want state %q lastSeq 3", res, stateExited)
	}
	second.waitLines(3)
	second.waitMethod(methodExited)

	order := second.methodOrder()
	wantOrder := []string{methodLine, methodLine, methodLine, methodReplayDone, methodExited}
	if len(order) != len(wantOrder) {
		t.Fatalf("notification methods = %v, want %v", order, wantOrder)
	}
	for i, method := range wantOrder {
		if order[i] != method {
			t.Fatalf("notification %d was %s, want %s (full order %v)", i, order[i], method, order)
		}
	}
	if _, err := os.Stat(sh.spoolPath(id)); err != nil {
		t.Fatalf("spool of an exited session should be retained until close: %v", err)
	}
}

func TestCloseDeletesRecordAndRetainsTurnIDSpool(t *testing.T) {
	sh := newTestShim(t)
	client, id := sh.start("nat-close")
	client.waitLines(1)
	if _, err := os.Stat(sh.spoolPath(id)); err != nil {
		t.Fatalf("spool file missing while the session is live: %v", err)
	}

	client.mustCall(methodClose, closeParams{ShimSessionID: id}, nil)

	if _, err := os.Stat(sh.spoolPath(id)); err != nil {
		t.Fatalf("turn-id spool did not survive session/close: %v", err)
	}
	var list listResult
	client.mustCall(methodList, struct{}{}, &list)
	if len(list.Sessions) != 0 {
		t.Fatalf("session/list still returns %+v after close", list.Sessions)
	}
	for _, call := range []struct {
		method string
		params any
	}{
		{methodAttach, attachParams{ShimSessionID: id, FromSeq: 0}},
		{methodInput, inputParams{ShimSessionID: id, Line: "hello"}},
		{methodClose, closeParams{ShimSessionID: id}},
	} {
		if rpcErr := client.callErr(call.method, call.params); rpcErr.Code != codeSessionNotFound || rpcErr.Message != "session_not_found" {
			t.Fatalf("%s after close returned %+v, want code %d session_not_found", call.method, rpcErr, codeSessionNotFound)
		}
	}
}

func TestUnknownMethodAndMissingParams(t *testing.T) {
	sh := newTestShim(t)
	client := sh.dial()

	if rpcErr := client.callErr("session/teleport", struct{}{}); rpcErr.Code != codeMethodNotFound {
		t.Fatalf("unknown method returned %+v, want code %d", rpcErr, codeMethodNotFound)
	}
	if rpcErr := client.callErr(methodStart, startParams{Cwd: sh.cwd}); rpcErr.Code != codeInvalidParams {
		t.Fatalf("start without a bin returned %+v, want code %d", rpcErr, codeInvalidParams)
	}
	if rpcErr := client.callErr(methodAttach, attachParams{FromSeq: 0}); rpcErr.Code != codeInvalidParams {
		t.Fatalf("attach without an id returned %+v, want code %d", rpcErr, codeInvalidParams)
	}
}
