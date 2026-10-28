package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const liveTextDeltaTurnStarted = `{"method":"turn/started","params":{"threadId":"native-thread","turn":{"id":"stream-turn","status":"inProgress"}}}`
const liveTextDeltaItemStarted = `{"method":"item/started","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"agent-1","type":"agentMessage","text":""}}}`

func liveTextDeltaFrames(parts ...string) [][]byte {
	var frames [][]byte
	for _, part := range parts {
		frames = append(frames, []byte(fmt.Sprintf(`{"method":"item/agentMessage/delta","params":{"threadId":"native-thread","turnId":"stream-turn","itemId":"agent-1","delta":%q}}`, part)))
	}
	return frames
}

func liveTextDeltaItemCompleted(text string) []byte {
	return []byte(fmt.Sprintf(`{"method":"item/completed","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"agent-1","type":"agentMessage","text":%q}}}`, text))
}

func liveTextDeltaTurnCompleted(status string) []byte {
	return []byte(fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":"native-thread","turn":{"id":"stream-turn","status":%q}}}`, status))
}

// liveTextDeltaAgentTexts is the agent text the live reader holds: what live
// text and the turn's answer are built from.
func liveTextDeltaAgentTexts(t *testing.T, view sessionProjection) []string {
	p := view.(*liveProjection)
	t.Helper()
	p.adapterMu.Lock()
	defer p.adapterMu.Unlock()
	var texts []string
	for _, item := range p.adapter.(*codexLiveAdapter).reader.Items {
		if item.Type == model.ItemAgentMessage {
			texts = append(texts, item.Text)
		}
	}
	return texts
}

// runLiveTextDeltaTurn applies one Codex turn. Deltas must publish nothing when
// dropped, and the empty agent start must not leave a blank row.
func runLiveTextDeltaTurn(t *testing.T, drop bool, deltas [][]byte, tail ...[]byte) *liveProjection {
	t.Helper()
	p := newLiveProjection("codex", "hosted", "nano")
	p.setNativeThreadID("native-thread")
	p.dropLiveTextDeltas = drop
	p.applyWire(protocol.Recv, []byte(liveTextDeltaTurnStarted), time.Now())
	p.applyWire(protocol.Recv, []byte(liveTextDeltaItemStarted), time.Now())
	for _, line := range deltas {
		p.applyWire(protocol.Recv, line, time.Now())
	}
	if texts := liveTextDeltaAgentTexts(t, p); drop && len(texts) != 0 {
		t.Fatalf("agent text read before completion: %q", texts)
	} else if !drop && len(texts) == 0 {
		t.Fatal("streaming deltas were not read")
	}
	for _, line := range tail {
		p.applyWire(protocol.Recv, line, time.Now())
	}
	return p
}

func TestDropLiveTextDeltasCompletedMessageMatchesStreaming(t *testing.T) {
	deltas := liveTextDeltaFrames("Hello", ", ", "world")
	tail := [][]byte{liveTextDeltaItemCompleted("Hello, world"), liveTextDeltaTurnCompleted("completed")}
	streamed := liveTextDeltaAgentTexts(t, runLiveTextDeltaTurn(t, false, deltas, tail...))
	dropped := liveTextDeltaAgentTexts(t, runLiveTextDeltaTurn(t, true, deltas, tail...))
	if len(dropped) != 1 || dropped[0] != "Hello, world" || fmt.Sprint(streamed) != fmt.Sprint(dropped) {
		t.Fatalf("final agent rows: streamed %q, dropped %q", streamed, dropped)
	}
}

func TestDropLiveTextDeltasInterruptedMessageUsesCompletedText(t *testing.T) {
	deltas := liveTextDeltaFrames("Partial", " answ")
	tail := [][]byte{liveTextDeltaItemCompleted("Partial answ"), liveTextDeltaTurnCompleted("interrupted")}
	streamed := liveTextDeltaAgentTexts(t, runLiveTextDeltaTurn(t, false, deltas, tail...))
	dropped := liveTextDeltaAgentTexts(t, runLiveTextDeltaTurn(t, true, deltas, tail...))
	if len(dropped) != 1 || dropped[0] != "Partial answ" || fmt.Sprint(streamed) != fmt.Sprint(dropped) {
		t.Fatalf("interrupted agent rows: streamed %q, dropped %q", streamed, dropped)
	}
}

// Documented cost of the switch: a message whose item/completed never arrives
// has no live partial text. The saved transcript remains the durable source.
func TestDropLiveTextDeltasMissingCompletionShowsNoPartialText(t *testing.T) {
	deltas := liveTextDeltaFrames("Never", " finished")
	tail := liveTextDeltaTurnCompleted("interrupted")
	streamed := liveTextDeltaAgentTexts(t, runLiveTextDeltaTurn(t, false, deltas, tail))
	dropped := liveTextDeltaAgentTexts(t, runLiveTextDeltaTurn(t, true, deltas, tail))
	if len(streamed) != 1 || streamed[0] != "Never finished" {
		t.Fatalf("streamed partial rows = %q", streamed)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped partial rows = %q, want none", dropped)
	}
}

func TestCodexAgentTextDeltaClassifierScope(t *testing.T) {
	for _, line := range []string{
		`{"method":"item/agentMessage/delta","params":{"itemId":"a","delta":"x"}}`,
		`{"method":"item/started","params":{"item":{"id":"a","type":"agentMessage","text":""}}}`,
		`{"method":"item/started","params":{"item":{"id":"a","type":"agentMessage"}}}`,
	} {
		if _, ok := codexAgentTextDelta([]byte(line)); !ok {
			t.Fatalf("agent text delta not recognized: %s", line)
		}
	}
	for _, line := range []string{
		`{"method":"item/started","params":{"item":{"id":"a","type":"agentMessage","text":"already here"}}}`,
		// Parser fixture shape: a reasoning start can carry summary text.
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn-1","item":{"id":"reason-1","type":"reasoning","summary":["consider"],"content":[]}}}`,
		`{"method":"item/started","params":{"item":{"id":"c","type":"commandExecution"}}}`,
		`{"method":"item/reasoning/summaryTextDelta","params":{"itemId":"r","summaryIndex":0,"delta":"x"}}`,
		`{"method":"item/commandExecution/outputDelta","params":{"itemId":"c","delta":"x"}}`,
		`{"method":"item/completed","params":{"item":{"id":"a","type":"agentMessage","text":"done"}}}`,
		`not json agentMessage`,
	} {
		if _, ok := codexAgentTextDelta([]byte(line)); ok {
			t.Fatalf("frame outside agent-text scope dropped: %s", line)
		}
	}

	p := newLiveProjection("codex", "hosted", "nano")
	p.setNativeThreadID("native-thread")
	p.dropLiveTextDeltas = true
	p.applyWire(protocol.Recv, []byte(liveTextDeltaTurnStarted), time.Now())
	p.applyWire(protocol.Recv, []byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"agent-1","type":"agentMessage","text":"already here"}}}`), time.Now())
	if texts := liveTextDeltaAgentTexts(t, p); len(texts) != 1 || texts[0] != "already here" {
		t.Fatalf("nonempty agent start rows = %q", texts)
	}

	claude := newLiveProjection("claude", "hosted", "nano")
	claude.dropLiveTextDeltas = true
	claude.setClaudeProviderTurnID("claude-turn")
	claude.applyWire(protocol.Recv, []byte(`{"type":"assistant","session_id":"native","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}}`), time.Now())
	if claude.finalAgentItemID("claude-turn") == "" {
		t.Fatal("claude projection affected by the Codex-only switch")
	}
}

// The switch is projection-only: every frame still reaches the event buffer,
// raw subscribers, the spool, and the session's runtime observer.
func TestDropLiveTextDeltasKeepsRawWireAndObservers(t *testing.T) {
	spool, err := newWireSpool(t.TempDir(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	projection := newLiveProjection("codex", "hosted", "nano")
	projection.setNativeThreadID("native-thread")
	projection.dropLiveTextDeltas = true
	sub := make(chan wireEvent, 64)
	sess := &session{provider: "codex",
		state:          map[string]json.RawMessage{},
		projection:     projection,
		subs:           map[chan wireEvent]struct{}{sub: {}},
		spool:          spool,
		lifecycleState: testLifecycleSnapshot("ready", false)}

	frames := append([][]byte{[]byte(liveTextDeltaTurnStarted), []byte(liveTextDeltaItemStarted)}, liveTextDeltaFrames("Hello", " world")...)
	for _, line := range frames {
		sess.recordWireLive(protocol.Recv, line, true)
	}
	if !sess.summary().TurnActive {
		t.Fatal("runtime observer missed turn/started")
	}
	tail := [][]byte{liveTextDeltaItemCompleted("Hello world"), liveTextDeltaTurnCompleted("completed")}
	for _, line := range tail {
		sess.recordWireLive(protocol.Recv, line, true)
	}
	frames = append(frames, tail...)
	if sess.summary().TurnActive {
		t.Fatal("runtime observer missed turn/completed")
	}
	if texts := liveTextDeltaAgentTexts(t, projection); len(texts) != 1 || texts[0] != "Hello world" {
		t.Fatalf("projection agent rows = %q", texts)
	}
	if got := projection.finalAgentItemID("stream-turn"); got != "agent-1" {
		t.Fatalf("turn completion named answer %q, want agent-1", got)
	}

	if len(sess.events) != len(frames) || len(sub) != len(frames) {
		t.Fatalf("raw events buffered %d, subscriber got %d, want %d", len(sess.events), len(sub), len(frames))
	}
	for index, evt := range sess.events {
		if !bytes.Equal(evt.Msg, frames[index]) {
			t.Fatalf("event %d = %s, want %s", index, evt.Msg, frames[index])
		}
	}
	path := spool.path
	if err := spool.close(false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(raw, []byte("\n")); lines != len(frames) {
		t.Fatalf("spool lines = %d, want %d", lines, len(frames))
	}
}

func TestQueuedCodexTextDeltasBatchAcrossMailboxWithoutCrossingControl(t *testing.T) {
	frames := [][]byte{
		[]byte(liveTextDeltaTurnStarted),
		[]byte(liveTextDeltaItemStarted),
		liveTextDeltaFrames("one ", "two ")[0],
		liveTextDeltaFrames("one ", "two ")[1],
		[]byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"native-thread","turnId":"stream-turn","tokenUsage":{"total":{"totalTokens":2}}}}`),
		liveTextDeltaFrames("three ", "four")[0],
		liveTextDeltaFrames("three ", "four")[1],
		liveTextDeltaItemCompleted("one two three four"),
		liveTextDeltaTurnCompleted("completed"),
	}

	want := newLiveProjection("codex", "hosted-thread")
	want.setNativeThreadID("native-thread")
	base := time.Now()
	for index, frame := range frames {
		want.applyWire(protocol.Recv, frame, base.Add(time.Duration(index)*time.Millisecond))
	}

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
	for index, frame := range frames {
		sess.recordWireAt(protocol.Recv, frame, true, false, base.Add(time.Duration(index)*time.Millisecond))
	}
	close(release)
	if err := core.Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}

	if got, unbatched := liveTextDeltaAgentTexts(t, sess.projection), liveTextDeltaAgentTexts(t, want); !reflect.DeepEqual(got, unbatched) ||
		sess.projection.finalAgentItemID("stream-turn") != want.finalAgentItemID("stream-turn") {
		t.Fatalf("batched text %q differs from unbatched %q", got, unbatched)
	}
	if len(sess.events) != len(frames) {
		t.Fatalf("raw event count = %d, want %d", len(sess.events), len(frames))
	}
	for index, event := range sess.events {
		if !bytes.Equal(event.Msg, frames[index]) {
			t.Fatalf("raw event %d = %s, want %s", index, event.Msg, frames[index])
		}
	}
}

func TestCodexTextDeltaIngressDoesNotWaitForOutstandingProviderCall(t *testing.T) {
	t.Setenv("NANO_LIVE_TEXT_DELTAS", "on")
	providerEntered := make(chan struct{})
	providerRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseProvider := func() { releaseOnce.Do(func() { close(providerRelease) }) }
	defer releaseProvider()

	sess, _ := readyTransactionalSendTestSession(t, func(context.Context, string) (json.RawMessage, error) {
		close(providerEntered)
		<-providerRelease
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	core := sess.coreSession()
	t.Cleanup(core.Close)

	response := make(chan int, 1)
	go func() {
		response <- postTurn(t, sess, context.Background(), "callback-interlock", "exercise callback interlock").Code
	}()
	select {
	case <-providerEntered:
	case <-time.After(time.Second):
		t.Fatal("provider call did not start")
	}

	callbackReturned := make(chan struct{})
	go func() {
		sess.recordWireAt(protocol.Recv, liveTextDeltaFrames("streaming")[0], true, false, time.Now())
		close(callbackReturned)
	}()
	select {
	case <-callbackReturned:
	case <-time.After(250 * time.Millisecond):
		releaseProvider()
		<-response
		t.Fatal("root text-delta callback waited for the outstanding provider call")
	}

	releaseProvider()
	select {
	case code := <-response:
		if code != http.StatusOK {
			t.Fatalf("provider response status = %d, want %d", code, http.StatusOK)
		}
	case <-time.After(time.Second):
		t.Fatal("provider response did not progress after callback returned")
	}
}

func TestAsyncTurnStoreFailureReachesTheThreadDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	sess, _ := readyTransactionalSendTestSession(t, nil)
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	core := sess.coreSession()
	t.Cleanup(core.Close)
	if err := sess.queuedTurns.(*queuedTurnStore).db.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		sess.recordWireLive(protocol.Recv, []byte(liveTextDeltaTurnCompleted("completed")), true)
	}
	if err := core.Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	diagnostics := sess.projection.threadDiagnosticsSnapshot()
	if len(diagnostics) != 1 || diagnostics[0].Code != "async_session_event_failed" ||
		diagnostics[0].Message != "An asynchronous session event could not be applied." || diagnostics[0].Occurrences != 3 {
		t.Fatalf("async failure diagnostics = %#v", diagnostics)
	}
	if got := strings.Count(logs.String(), "thread diagnostic session"); got != 1 {
		t.Fatalf("bounded diagnostic log count = %d, want 1; logs=%q", got, logs.String())
	}
	if strings.Contains(logs.String(), "core session async event failed") {
		t.Fatalf("async failures bypassed bounded diagnostic logging: %q", logs.String())
	}
}

func TestLiveTextDeltasEnvironmentSwitch(t *testing.T) {
	t.Setenv("NANO_LIVE_TEXT_DELTAS", "")
	if liveTextDeltasDisabled() {
		t.Fatal("unset switch disabled live text deltas")
	}
	t.Setenv("NANO_LIVE_TEXT_DELTAS", " OFF ")
	if !liveTextDeltasDisabled() {
		t.Fatal("NANO_LIVE_TEXT_DELTAS=off not honored")
	}
	s := &server{dropLiveTextDeltas: liveTextDeltasDisabled()}
	if !s.newSessionProjection("codex", "hosted").dropLiveTextDeltas {
		t.Fatal("server setting did not reach the session projection")
	}
}
