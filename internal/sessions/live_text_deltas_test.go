package sessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// liveTextDeltaAgentTexts is the agent text the live reader holds: what live
// text and the turn's answer are built from.
func liveTextDeltaAgentTexts(t *testing.T, p *LiveProjection) []string {
	t.Helper()
	p.adapterMu.Lock()
	defer p.adapterMu.Unlock()
	var texts []string
	for _, item := range parsedAdapter(p).(*codexLiveAdapter).reader.Items {
		if item.Type == model.ItemAgentMessage {
			texts = append(texts, item.Text)
		}
	}
	return texts
}

// runLiveTextDeltaTurn applies one Codex turn. Deltas must publish nothing when
// dropped, and the empty agent start must not leave a blank row.
func runLiveTextDeltaTurn(t *testing.T, drop bool, deltas [][]byte, tail ...[]byte) *LiveProjection {
	t.Helper()
	p := newLiveProjection("codex", "hosted", "nano")
	p.SetNativeThreadID("native-thread")
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
	p.SetNativeThreadID("native-thread")
	p.dropLiveTextDeltas = true
	p.applyWire(protocol.Recv, []byte(liveTextDeltaTurnStarted), time.Now())
	p.applyWire(protocol.Recv, []byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"agent-1","type":"agentMessage","text":"already here"}}}`), time.Now())
	if texts := liveTextDeltaAgentTexts(t, p); len(texts) != 1 || texts[0] != "already here" {
		t.Fatalf("nonempty agent start rows = %q", texts)
	}

	claude := newLiveProjection("claude", "hosted", "nano")
	claude.dropLiveTextDeltas = true
	claude.SetClaudeProviderTurnID("claude-turn")
	claude.applyWire(protocol.Recv, []byte(`{"type":"assistant","session_id":"native","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}}`), time.Now())
	if claude.FinalAgentItemID("claude-turn") == "" {
		t.Fatal("claude projection affected by the Codex-only switch")
	}
}

// The switch is projection-only: every frame still reaches the event buffer,
// raw subscribers, the spool, and the session's runtime observer.
func TestDropLiveTextDeltasKeepsRawWireAndObservers(t *testing.T) {
	projection := newLiveProjection("codex", "hosted", "nano")
	projection.dropLiveTextDeltas = true
	f := newProviderRuntimeFixture(t, Config{ID: "raw-deltas", Provider: "codex", HostedThreadID: "hosted", Projection: projection}, "native-thread", "")
	sess := f.session
	before := liveWirePageEvents(sess)
	prior := before[len(before)-1].Seq
	spoolDir := t.TempDir()
	if err := sess.OpenWireSpool(spoolDir); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(sess.HandleEvents))
	defer httpServer.Close()
	response, err := http.Get(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	sub := make(chan wireEvent, 64)
	go func() {
		defer close(sub)
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event wireEvent
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Seq > prior {
				sub <- event
			}
		}
	}()

	frames := append([][]byte{[]byte(liveTextDeltaTurnStarted), []byte(liveTextDeltaItemStarted)}, liveTextDeltaFrames("Hello", " world")...)
	for i, line := range frames {
		var value any
		_ = json.Unmarshal(line, &value)
		frames[i], _ = json.Marshal(value)
		f.emitAndWait(t, frames[i])
	}
	if !sess.Summary().TurnActive {
		t.Fatal("runtime observer missed turn/started")
	}
	tail := [][]byte{liveTextDeltaItemCompleted("Hello world"), liveTextDeltaTurnCompleted("completed")}
	for i, line := range tail {
		var value any
		_ = json.Unmarshal(line, &value)
		tail[i], _ = json.Marshal(value)
		f.emitAndWait(t, tail[i])
	}
	frames = append(frames, tail...)
	if sess.Summary().TurnActive {
		t.Fatal("runtime observer missed turn/completed")
	}
	if texts := liveTextDeltaAgentTexts(t, projection); len(texts) != 1 || texts[0] != "Hello world" {
		t.Fatalf("projection agent rows = %q", texts)
	}
	if got := projection.FinalAgentItemID("stream-turn"); got != "agent-1" {
		t.Fatalf("turn completion named answer %q, want agent-1", got)
	}

	var events []wireEvent
	for _, event := range liveWirePageEvents(sess) {
		if event.Seq > prior {
			events = append(events, event)
		}
	}
	if len(events) != len(frames) {
		t.Fatalf("raw events buffered %d, want %d", len(events), len(frames))
	}
	for index, event := range events {
		if !bytes.Equal(event.Msg, frames[index]) {
			t.Fatalf("event %d = %s, want %s", index, event.Msg, frames[index])
		}
		select {
		case live, ok := <-sub:
			if !ok || !bytes.Equal(live.Msg, frames[index]) {
				t.Fatalf("subscriber event %d = %s, want %s", index, live.Msg, frames[index])
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber missed event %d", index)
		}
	}
	path := filepath.Join(spoolDir, "nano-wire-"+sess.ID()+".jsonl")
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
	want.SetNativeThreadID("native-thread")
	base := time.Now()
	for index, frame := range frames {
		want.applyWire(protocol.Recv, frame, base.Add(time.Duration(index)*time.Millisecond))
	}

	p := newLiveProjection("codex", "hosted-thread")
	f := newProviderRuntimeFixture(t, Config{ID: "batch-deltas", Provider: "codex", HostedThreadID: "hosted-thread", Projection: p}, "native-thread", "")
	sess := f.session
	release := holdFirstProviderFrame(t, f, p, frames[0])
	defer release()
	for _, frame := range frames[1:] {
		f.emit(t, frame)
	}
	awaitProviderIngress(t, f)
	release()
	awaitProviderFrame(t, f, frames[len(frames)-1])

	if got, unbatched := liveTextDeltaAgentTexts(t, p), liveTextDeltaAgentTexts(t, want); !reflect.DeepEqual(got, unbatched) ||
		p.FinalAgentItemID("stream-turn") != want.FinalAgentItemID("stream-turn") {
		t.Fatalf("batched text %q differs from unbatched %q", got, unbatched)
	}
	if len(receivedProviderFrames(sess)) != len(frames) {
		t.Fatalf("raw event count = %d, want %d", len(receivedProviderFrames(sess)), len(frames))
	}
	for index, event := range receivedProviderFrames(sess) {
		if !bytes.Equal(event.Msg, frames[index]) {
			t.Fatalf("raw event %d = %s, want %s", index, event.Msg, frames[index])
		}
	}
}
