package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const (
	commandOutputIncidentChunks = 4096
	commandOutputChunkBytes     = 9 << 10
)

func TestCommandOutputIncidentDrainsMailboxBoundsPresentationAndReleasesNextPrompt(t *testing.T) {
	delivered := make(chan string, 1)
	sess, _ := readyTransactionalSendTestSession(t, func(_ context.Context, text string) (json.RawMessage, error) {
		delivered <- text
		return json.RawMessage(`{"turn":{"id":"next-provider-turn"}}`), nil
	})
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	core := sess.coreSession()
	t.Cleanup(core.Close)

	// The spool is the record of every raw frame; the in-memory ring keeps a tail.
	sess.spool = newTestWireSpool(t)
	base := time.Now()
	sess.recordWireAt(protocol.Recv, []byte(liveTextDeltaTurnStarted), true, false, base)
	if err := core.Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	if !sess.summary().TurnActive {
		t.Fatal("turn did not become active before the incident burst")
	}
	queuedText := "next prompt after large command output"
	// Held for the next turn: this test needs the prompt released by the turn's end.
	queued := httptest.NewRecorder()
	sess.handleTurn(queued, httptest.NewRequest(http.MethodPost, "/api/sessions/nano-session/turn",
		strings.NewReader(`{"text":`+string(mustJSON(t, queuedText))+`,"clientActionId":"command-output-next","nextTurn":true}`)))
	if queued.Code != http.StatusAccepted || !strings.Contains(queued.Body.String(), `"status":"queued"`) {
		t.Fatalf("next prompt was not queued: status=%d body=%s", queued.Code, queued.Body.String())
	}

	frames := makeCommandOutputIncidentFrames(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	core.ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error {
		close(entered)
		<-release
		return nil
	}})
	<-entered

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for index, frame := range frames {
		sess.recordWireAt(protocol.Recv, frame, true, false, base.Add(time.Duration(index+1)*time.Microsecond))
	}
	started := time.Now()
	close(release)
	if err := core.Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	t.Logf("35MiB command-output mailbox drain: elapsed=%s allocated=%d bytes frames=%d", elapsed, after.TotalAlloc-before.TotalAlloc, len(frames))

	if sess.summary().TurnActive {
		t.Fatal("turn remained active after final answer and turn completion")
	}
	select {
	case got := <-delivered:
		if got != queuedText {
			t.Fatalf("next prompt text=%q, want exact queued text", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("next queued prompt did not progress after completion")
	}

	command, output, found := liveReaderItemForTest(sess.projection, "incident-command")
	if !found {
		t.Fatal("bounded command is missing")
	}
	if len(output) > liveProjectionOutputCap || !strings.Contains(output, "live command output truncated") ||
		!command.ContentTruncated || command.OriginalBytes <= liveProjectionOutputCap {
		t.Fatalf("command output bytes=%d marker=%t truncated=%t original=%d", len(output), strings.Contains(output, "live command output truncated"), command.ContentTruncated, command.OriginalBytes)
	}
	if final, _, found := liveReaderItemForTest(sess.projection, "agent-1"); !found || final.Text != "EXACT FINAL MESSAGE" || !final.Complete ||
		sess.projection.FinalAgentItemID("stream-turn") != "agent-1" {
		t.Fatalf("final message mismatch: present=%t text=%q done=%t", found, final.Text, final.Complete)
	}
	sess.projection.(*liveProjection).adapterMu.Lock()
	semanticFrames := sess.projection.(*liveProjection).adapter.(*codexLiveAdapter).reader.Frame
	sess.projection.(*liveProjection).adapterMu.Unlock()
	if semanticFrames >= int64(commandOutputIncidentChunks/2) {
		t.Fatalf("command deltas were not coalesced before semantic application: frames=%d rawChunks=%d", semanticFrames, commandOutputIncidentChunks)
	}

	if err := sess.spool.close(false); err != nil {
		t.Fatal(err)
	}
	events, more, err := sess.spool.readBefore(math.MaxInt64, len(frames)+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(frames)+1 || more {
		t.Fatalf("raw event count=%d (more=%t), want=%d", len(events), more, len(frames)+1)
	}
	for index, frame := range frames {
		if sha256.Sum256(events[index+1].Msg) != sha256.Sum256(frame) {
			t.Fatalf("raw event order/content mismatch at index %d", index)
		}
	}
}

func TestQueuedCodexCommandOutputBatchesWithoutCrossingBoundaries(t *testing.T) {
	sess, _ := readyTransactionalSendTestSession(t, nil)
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	core := sess.coreSession()
	t.Cleanup(core.Close)

	frames := [][]byte{
		[]byte(liveTextDeltaTurnStarted),
		[]byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"command-a","type":"commandExecution","command":"a","status":"inProgress"}}}`),
		commandOutputDeltaFrame(t, "command-a", "one"),
		commandOutputDeltaFrame(t, "command-a", "two"),
		[]byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"native-thread","turnId":"stream-turn","tokenUsage":{"total":{"totalTokens":2}}}}`),
		commandOutputDeltaFrame(t, "command-a", "three"),
		commandOutputDeltaFrame(t, "command-a", "four"),
		[]byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"command-b","type":"commandExecution","command":"b","status":"inProgress"}}}`),
		commandOutputDeltaFrame(t, "command-b", "boundary"),
		commandOutputDeltaFrame(t, "command-a", "five"),
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	core.ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error {
		close(entered)
		<-release
		return nil
	}})
	<-entered
	for index, frame := range frames {
		sess.recordWireAt(protocol.Recv, frame, true, false, time.Unix(100, int64(index)))
	}
	close(release)
	if err := core.Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{}
	for _, id := range []string{"command-a", "command-b"} {
		_, outputs[id], _ = liveReaderItemForTest(sess.projection, id)
	}
	if outputs["command-a"] != "onetwothreefourfive" || outputs["command-b"] != "boundary" {
		t.Fatalf("command outputs crossed a boundary: %#v", outputs)
	}
	sess.projection.(*liveProjection).adapterMu.Lock()
	semanticFrames := sess.projection.(*liveProjection).adapter.(*codexLiveAdapter).reader.Frame
	sess.projection.(*liveProjection).adapterMu.Unlock()
	if semanticFrames != int64(len(frames)-2) {
		t.Fatalf("semantic frames=%d, want %d after two adjacent pair merges", semanticFrames, len(frames)-2)
	}
	if len(sess.events) != len(frames) {
		t.Fatalf("raw frames=%d, want=%d", len(sess.events), len(frames))
	}
	for index, frame := range frames {
		if sha256.Sum256(sess.events[index].Msg) != sha256.Sum256(frame) {
			t.Fatalf("raw boundary order/content mismatch at %d", index)
		}
	}
}

func TestCommandOutputBoundedBaselineComparison(t *testing.T) {
	sess, _ := readyTransactionalSendTestSession(t, nil)
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	core := sess.coreSession()
	t.Cleanup(core.Close)

	entered := make(chan struct{})
	release := make(chan struct{})
	core.ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error {
		close(entered)
		<-release
		return nil
	}})
	<-entered
	sess.recordWireAt(protocol.Recv, []byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"turn","item":{"id":"command","type":"commandExecution","command":"baseline","status":"inProgress"}}}`), true, false, time.Now())
	for index := 0; index < 128; index++ {
		sess.recordWireAt(protocol.Recv, commandOutputDeltaFrame(t, "command", strings.Repeat(string('a'+rune(index%26)), 8<<10)), true, false, time.Now())
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	started := time.Now()
	close(release)
	if err := core.Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)

	_, output, _ := liveReaderItemForTest(sess.projection, "command")
	t.Logf("bounded comparison: elapsed=%s allocated=%d bytes output=%d bytes frames=%d", elapsed, after.TotalAlloc-before.TotalAlloc, len(output), 129)
	if len(output) > liveProjectionOutputCap || !strings.Contains(output, "live command output truncated") {
		t.Fatalf("bounded comparison output=%d marker=%t", len(output), strings.Contains(output, "live command output truncated"))
	}
}

// liveReaderItemForTest is the Codex live reader's item with this native id,
// and its tool output. Output deltas read before their item/started carry no
// turn, so the reader can hold two items for one command; the one holding the
// longest output is the one under test.
func liveReaderItemForTest(view sessionProjection, nativeID string) (model.Item, string, bool) {
	p := view.(*liveProjection)
	p.adapterMu.Lock()
	defer p.adapterMu.Unlock()
	var found model.Item
	output, ok := "", false
	for _, item := range p.adapter.(*codexLiveAdapter).reader.Items {
		if item.NativeItemID != nativeID {
			continue
		}
		text := ""
		for _, part := range item.Parts {
			if part.Type == "tool_output" {
				text = part.Text
			}
		}
		if !ok || len(text) > len(output) {
			found, output, ok = item, text, true
		}
	}
	return found, output, ok
}

func makeCommandOutputIncidentFrames(t *testing.T) [][]byte {
	t.Helper()
	frames := make([][]byte, 0, commandOutputIncidentChunks+8)
	frames = append(frames, []byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"incident-command","type":"commandExecution","command":"generate incident output","status":"inProgress"}}}`))
	for index := 0; index < commandOutputIncidentChunks; index++ {
		if index == commandOutputIncidentChunks/3 {
			frames = append(frames, []byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"native-thread","turnId":"stream-turn","tokenUsage":{"total":{"totalTokens":123}}}}`))
		}
		if index == 2*commandOutputIncidentChunks/3 {
			frames = append(frames,
				[]byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"boundary-command","type":"commandExecution","command":"boundary","status":"inProgress"}}}`),
				commandOutputDeltaFrame(t, "boundary-command", "boundary-output"),
				[]byte(`{"method":"item/completed","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"boundary-command","type":"commandExecution","command":"boundary","aggregatedOutput":"boundary-output","status":"completed","exitCode":0}}}`),
			)
		}
		prefix := fmt.Sprintf("%06d:", index)
		delta := prefix + strings.Repeat(string('a'+rune(index%26)), commandOutputChunkBytes-len(prefix))
		frames = append(frames, commandOutputDeltaFrame(t, "incident-command", delta))
	}
	completionOutput := strings.Repeat("completion-head-", 70000) + "completion-tail"
	frames = append(frames,
		commandOutputCompletedFrame(t, completionOutput),
		liveTextDeltaItemCompleted("EXACT FINAL MESSAGE"),
		liveTextDeltaTurnCompleted("completed"),
	)
	return frames
}

func commandOutputDeltaFrame(t *testing.T, itemID, delta string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"method": "item/commandExecution/outputDelta", "params": map[string]any{
		"threadId": "native-thread", "turnId": "stream-turn", "itemId": itemID, "delta": delta,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func commandOutputCompletedFrame(t *testing.T, output string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": "native-thread", "turnId": "stream-turn", "item": map[string]any{
			"id": "incident-command", "type": "commandExecution", "command": "generate incident output",
			"aggregatedOutput": output, "status": "completed", "exitCode": 0,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
