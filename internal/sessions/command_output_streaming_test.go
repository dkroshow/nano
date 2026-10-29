package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"github.com/dkroshow/nano/internal/wire"
)

const (
	commandOutputIncidentChunks = 4096
	commandOutputChunkBytes     = 9 << 10
)

func TestCommandOutputIncidentDrainsMailboxBoundsPresentationAndReleasesNextPrompt(t *testing.T) {
	delivered := make(chan string, 1)
	p := newLiveProjection("codex", "hosted-thread")
	f := newProviderRuntimeFixture(t, Config{ID: "incident-output", Provider: "codex", HostedThreadID: "hosted-thread", Projection: p}, "native-thread", "", func(f *providerRuntimeFixture) {
		f.send = func(_ context.Context, text string) (json.RawMessage, error) {
			delivered <- text
			return json.RawMessage(`{"turn":{"id":"next-provider-turn"}}`), nil
		}
	})
	sess := f.session
	spoolDir := t.TempDir()
	if err := sess.wire.OpenWireSpool(spoolDir); err != nil {
		t.Fatal(err)
	}
	f.emitAndWait(t, []byte(liveTextDeltaTurnStarted))

	if !sess.Summary().TurnActive {
		t.Fatal("turn did not become active before the incident burst")
	}
	queuedText := "next prompt after large command output"
	// Held for the next turn: this test needs the prompt released by the turn's end.
	queued := httptest.NewRecorder()
	sess.HandleTurn(queued, httptest.NewRequest(http.MethodPost, "/api/sessions/nano-session/turn",
		strings.NewReader(`{"text":`+string(mustJSON(t, queuedText))+`,"clientActionId":"command-output-next","nextTurn":true}`)))
	if queued.Code != http.StatusAccepted || !strings.Contains(queued.Body.String(), `"status":"queued"`) {
		t.Fatalf("next prompt was not queued: status=%d body=%s", queued.Code, queued.Body.String())
	}

	frames := makeCommandOutputIncidentFrames(t)
	release := holdFirstProviderFrame(t, f, p, frames[0])
	defer release()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for _, frame := range frames[1:] {
		f.emit(t, frame)
	}
	awaitProviderIngress(t, f)
	started := time.Now()
	release()
	awaitProviderFrameWithin(t, f, frames[len(frames)-1], 30*time.Second)

	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	t.Logf("35MiB command-output mailbox drain: elapsed=%s allocated=%d bytes frames=%d", elapsed, after.TotalAlloc-before.TotalAlloc, len(frames))

	// The released follow-up can already be active on the real transport.
	// The incident turn itself must be terminal before that handoff.
	if turn, _, _, ok := p.retainedTurnEvidence("stream-turn"); !ok || turn.Status != "completed" {
		t.Fatalf("incident turn remained active after final answer and turn completion: %+v", turn)
	}
	select {
	case got := <-delivered:
		if got != queuedText {
			t.Fatalf("next prompt text=%q, want exact queued text", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("next queued prompt did not progress after completion")
	}

	waitTurnSendDone(t, sess, "command-output-next")
	command, output, found := liveReaderItemForTest(p, "incident-command")
	if !found {
		t.Fatal("bounded command is missing")
	}
	if len(output) > liveProjectionOutputCap || !strings.Contains(output, "live command output truncated") ||
		!command.ContentTruncated || command.OriginalBytes <= liveProjectionOutputCap {
		t.Fatalf("command output bytes=%d marker=%t truncated=%t original=%d", len(output), strings.Contains(output, "live command output truncated"), command.ContentTruncated, command.OriginalBytes)
	}
	if final, _, found := liveReaderItemForTest(p, "agent-1"); !found || final.Text != "EXACT FINAL MESSAGE" || !final.Complete ||
		p.finalAgentItemID("stream-turn") != "agent-1" {
		t.Fatalf("final message mismatch: present=%t text=%q done=%t", found, final.Text, final.Complete)
	}
	p.adapterMu.Lock()
	semanticFrames := parsedAdapter(p).(*codexLiveAdapter).reader.Frame
	p.adapterMu.Unlock()
	if semanticFrames >= int64(commandOutputIncidentChunks/2) {
		t.Fatalf("command deltas were not coalesced before semantic application: frames=%d rawChunks=%d", semanticFrames, commandOutputIncidentChunks)
	}

	// The public page flushes the spool before its file is inspected.
	page := httptest.NewRecorder()
	sess.wire.HandleEventsPage(page, httptest.NewRequest(http.MethodGet, "/events?beforeSeq=9223372036854775807&limit=2000", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("wire page = %d %s", page.Code, page.Body.String())
	}
	events := readReceivedSpoolFrames(t, spoolDir)
	if len(events) != len(frames)+1 {
		t.Fatalf("raw event count=%d, want=%d", len(events), len(frames)+1)
	}

	for index, frame := range frames {
		if sha256.Sum256(events[index+1].Msg) != sha256.Sum256(frame) {
			t.Fatalf("raw event order/content mismatch at index %d", index)
		}
	}
}

func TestQueuedCodexCommandOutputBatchesWithoutCrossingBoundaries(t *testing.T) {
	p := newLiveProjection("codex", "hosted-thread")
	f := newProviderRuntimeFixture(t, Config{ID: "batch-commands", Provider: "codex", HostedThreadID: "hosted-thread", Projection: p}, "native-thread", "")
	sess := f.session
	p.adapterMu.Lock()
	initialFrames := parsedAdapter(p).(*codexLiveAdapter).reader.Frame
	p.adapterMu.Unlock()

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
	release := holdFirstProviderFrame(t, f, p, frames[0])
	defer release()
	for _, frame := range frames[1:] {
		f.emit(t, frame)
	}
	awaitProviderIngress(t, f)
	release()
	awaitProviderFrame(t, f, frames[len(frames)-1])

	outputs := map[string]string{}
	for _, id := range []string{"command-a", "command-b"} {
		_, outputs[id], _ = liveReaderItemForTest(p, id)
	}
	if outputs["command-a"] != "onetwothreefourfive" || outputs["command-b"] != "boundary" {
		t.Fatalf("command outputs crossed a boundary: %#v", outputs)
	}
	p.adapterMu.Lock()
	semanticFrames := parsedAdapter(p).(*codexLiveAdapter).reader.Frame - initialFrames
	p.adapterMu.Unlock()
	if semanticFrames != int64(len(frames)-2) {
		t.Fatalf("semantic frames=%d, want %d after two adjacent pair merges", semanticFrames, len(frames)-2)
	}
	if len(receivedProviderFrames(sess)) != len(frames) {
		t.Fatalf("raw frames=%d, want=%d", len(receivedProviderFrames(sess)), len(frames))
	}
	for index, frame := range frames {
		if sha256.Sum256(receivedProviderFrames(sess)[index].Msg) != sha256.Sum256(frame) {
			t.Fatalf("raw boundary order/content mismatch at %d", index)
		}
	}
}

func TestCommandOutputBoundedBaselineComparison(t *testing.T) {
	p := newLiveProjection("codex", "hosted-thread")
	f := newProviderRuntimeFixture(t, Config{ID: "baseline-output", Provider: "codex", HostedThreadID: "hosted-thread", Projection: p}, "native-thread", "")
	first := []byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"turn","item":{"id":"command","type":"commandExecution","command":"baseline","status":"inProgress"}}}`)
	release := holdFirstProviderFrame(t, f, p, first)
	defer release()
	var last []byte
	for index := 0; index < 128; index++ {
		last = commandOutputDeltaFrame(t, "command", strings.Repeat(string('a'+rune(index%26)), 8<<10))
		f.emit(t, last)
	}
	awaitProviderIngress(t, f)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	started := time.Now()
	release()
	awaitProviderFrame(t, f, last)
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)

	_, output, _ := liveReaderItemForTest(p, "command")
	t.Logf("bounded comparison: elapsed=%s allocated=%d bytes output=%d bytes frames=%d", elapsed, after.TotalAlloc-before.TotalAlloc, len(output), 129)
	if len(output) > liveProjectionOutputCap || !strings.Contains(output, "live command output truncated") {
		t.Fatalf("bounded comparison output=%d marker=%t", len(output), strings.Contains(output, "live command output truncated"))
	}
}

// liveReaderItemForTest is the Codex live reader's item with this native id,
// and its tool output. Output deltas read before their item/started carry no
// turn, so the reader can hold two items for one command; the one holding the
// longest output is the one under test.
func liveReaderItemForTest(p *LiveProjection, nativeID string) (model.Item, string, bool) {
	p.adapterMu.Lock()
	defer p.adapterMu.Unlock()
	var found model.Item
	output, ok := "", false
	for _, item := range parsedAdapter(p).(*codexLiveAdapter).reader.Items {
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

func readReceivedSpoolFrames(t *testing.T, dir string) []wire.Event {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("wire spool files = %v, %v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var events []wire.Event
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event wire.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		var body struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(event.Msg, &body)
		if event.Dir == "recv" && body.Method != "" {
			events = append(events, event)
		}
	}
	return events
}
