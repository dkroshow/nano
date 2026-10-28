package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGrokCurrentUsesLoadResponseIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           lifecycle.Event
		valid          bool
	}{
		{"idle", `{"_meta":{"sessionId":"native"}}`, lifecycle.EventReadyIdle, true},
		{"working", `{"_meta":{"sessionId":"native","x.ai/runningPromptId":"running"}}`, lifecycle.EventReadyWorking, true},
		{"synthetic", `{"_meta":{"sessionId":"native","x.ai/runningPromptId":"subagent-completed-child"}}`, lifecycle.EventReadyIdle, true},
		{"wrong-native", `{"_meta":{"sessionId":"other"}}`, "", false},
		{"requested-native-idle", `{}`, lifecycle.EventReadyIdle, true},
		{"requested-native-working", `{"_meta":{"x.ai/runningPromptId":"running"}}`, lifecycle.EventReadyWorking, true},
		{"detail-native", `{"_meta":{"x.ai/sessionDetail":{"sessionId":"native"}}}`, lifecycle.EventReadyIdle, true},
		{"wrong-detail-native", `{"_meta":{"x.ai/sessionDetail":{"sessionId":"other"}}}`, "", false},
		{"malformed-response", `{"_meta":`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := grokLifecycleAdapter{epoch: 7, sequence: 11, settled: true, nativeID: "native", response: json.RawMessage(tc.response)}
			observation, ok := adapter.Current()
			if ok != tc.valid || (ok && observation != (lifecycle.Observation{Event: tc.want, Epoch: 7, Sequence: 11})) {
				t.Fatalf("Current() = %+v, %v; want event %s, valid %v", observation, ok, tc.want, tc.valid)
			}
		})
	}
}

func TestGrokBootReattachRetriesSessionLoadTimeout(t *testing.T) {
	for _, tc := range []struct {
		name         string
		timeouts     int
		neverAnswers bool
		wantStatus   string
		wantAttempts int
	}{
		{name: "once", timeouts: 1, wantStatus: "ready", wantAttempts: 2},
		{name: "twice", timeouts: 2, wantStatus: "ready", wantAttempts: 3},
		{name: "three_times", timeouts: 3, wantStatus: "exited", wantAttempts: 3},
		{name: "never_answers", neverAnswers: true, wantStatus: "exited", wantAttempts: 3},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stateDir := t.TempDir()
			bin := writeRetryingGrokACP(t, stateDir, tc.timeouts, tc.neverAnswers)
			control := newTestControlMetadata(t)
			executions, err := newControlAgentExecutionStore(control, "grok-boot-retry")
			if err != nil {
				t.Fatal(err)
			}
			hostedID := "nht_grok_boot_" + tc.name
			if _, err := executions.ensureSessionWithIdentity(hostedID, "grok", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			generation, err := control.beginTransportBinding(hostedID)
			if err != nil {
				t.Fatal(err)
			}
			state, err := lifecycle.New(uint64(generation))
			if err != nil {
				t.Fatal(err)
			}
			sess := &session{
				id:                   "grok-boot-" + tc.name,
				provider:             "grok",
				hostedThreadID:       hostedID,
				threadID:             "native-" + tc.name,
				state:                map[string]json.RawMessage{},
				projection:           newLiveProjection("grok"),
				lifecycleState:       state,
				attachmentGeneration: generation,
				sendOps:              map[string]*turnSendOperation{},
				agentExecutions:      executions,
				metadata:             control,
			}
			if err := executions.bindSession(hostedID, sess.id, sess.threadID, "starting", presentationShared); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sess.close)

			startGrokSession(sess, providerRequest{Provider: "grok", Mode: "resume", ThreadID: sess.threadID,
				Cwd: stateDir, GrokBin: bin,
				bootReattach: &bootProviderControl{
					loadTimeout:      50 * time.Millisecond,
					retryBackoffBase: 5 * time.Millisecond,
				}})

			waitForGrokAttempts(t, stateDir, 1)
			waitForGrokStatus(t, sess, "reconnecting")
			waitForGrokStatus(t, sess, tc.wantStatus)
			if got := readGrokAttempts(t, stateDir); got != tc.wantAttempts {
				t.Fatalf("session/load attempts = %d, want %d", got, tc.wantAttempts)
			}
			// The stored status is written just after the in-memory one (the
			// session publishes and notifies first), so wait for it within the
			// same deadline rather than reading it once.
			for deadline := time.Now().Add(12 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if recorded, _ := executions.session(hostedID); recorded.Status == tc.wantStatus {
					break
				}
			}
			recorded, ok := executions.session(hostedID)
			if !ok || recorded.Status != tc.wantStatus {
				t.Fatalf("durable hosted state = %+v, found=%v; want %s", recorded, ok, tc.wantStatus)
			}
			if tc.wantStatus == "exited" {
				summary := sess.summary()
				if !strings.Contains(summary.Error, "session/load") || !strings.Contains(summary.Error, context.DeadlineExceeded.Error()) {
					t.Fatalf("terminal reason = %q, want session/load deadline", summary.Error)
				}
				if recorded.Error != summary.Error {
					t.Fatalf("durable terminal reason = %q, want %q", recorded.Error, summary.Error)
				}
			}
		})
	}
}

func writeRetryingGrokACP(t *testing.T, stateDir string, timeouts int, never bool) string {
	t.Helper()
	bin := filepath.Join(stateDir, "fake-grok-acp")
	neverValue := 0
	if never {
		neverValue = 1
	}
	script := fmt.Sprintf(`#!/bin/sh
state_dir=%s
lock="$state_dir/attempt.lock"
while ! mkdir "$lock" 2>/dev/null; do sleep 0.01; done
attempt=0
if [ -f "$state_dir/attempts" ]; then read -r attempt < "$state_dir/attempts"; fi
attempt=$((attempt + 1))
printf '%%s\n' "$attempt" > "$state_dir/attempts.$$"
mv "$state_dir/attempts.$$" "$state_dir/attempts"
rmdir "$lock"
IFS= read -r request || exit 1
printf '%%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
IFS= read -r request || exit 1
if [ %d -eq 1 ] || [ "$attempt" -le %d ]; then
  while :; do sleep 1; done
fi
printf '%%s\n' '{"jsonrpc":"2.0","id":2,"result":{"_meta":{"x.ai/sessionConfig":{"options":[{"category":"mode","id":"high","selected":true}]}}}}'
while IFS= read -r request; do :; done
`, strconv.Quote(stateDir), neverValue, timeouts)
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func readGrokAttempts(t *testing.T, stateDir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "attempts"))
	if err != nil {
		return 0
	}
	attempts, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("decode Grok attempt count: %v", err)
	}
	return attempts
}

func waitForGrokAttempts(t *testing.T, stateDir string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if readGrokAttempts(t, stateDir) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Grok ACP attempts never reached %d", want)
}

func waitForGrokStatus(t *testing.T, sess *session, want string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if sess.summary().Status == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Grok session status = %q, want %q", sess.summary().Status, want)
}

func TestGrokLoadHandshakeSettlesLifecycleAndQueue(t *testing.T) {
	for _, tc := range []struct {
		name, mode, identity string
		active               bool
	}{
		{"new", "start", "captured", false},
		{"load-idle", "resume", "captured", false},
		{"load-working", "resume", "captured", true},
		{"load-no-id-idle", "resume", "absent", false},
		{"load-no-id-working", "resume", "absent", true},
		{"load-detail-id", "resume", "detail", false},
		{"load-wrong-id", "resume", "wrong", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "grok-restart")
			if err != nil {
				t.Fatal(err)
			}
			sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
				sends.Add(1)
				return json.RawMessage(`{"turn":{"id":"queued-provider-turn"}}`), nil
			})
			sess.provider = "grok"
			sess.state = map[string]json.RawMessage{}
			sess.lifecycleState = testLifecycleSnapshot("starting", false)
			sess.turnID, sess.turnStartedAtMs = "old-anchor", 123
			t.Cleanup(sess.close)
			if rec := postTurn(t, sess, context.Background(), "after-restart", "queued while starting"); rec.Code != http.StatusAccepted {
				t.Fatalf("queue status %d: %s", rec.Code, rec.Body.String())
			}

			fixture := "session-load-1.0.5.json"
			if tc.mode == "start" {
				fixture = "session-new-1.0.5.json"
			}
			raw, err := os.ReadFile(filepath.Join("testdata", "grok-lifecycle", fixture))
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			meta := result["_meta"].(map[string]any)
			switch tc.identity {
			case "absent", "detail":
				delete(result, "sessionId")
				delete(meta, "sessionId")
				if tc.identity == "absent" {
					delete(meta, "x.ai/sessionDetail")
				}
			case "wrong":
				meta["sessionId"] = "wrong-native"
			}
			if tc.active {
				meta["x.ai/runningPromptId"] = "surviving-turn"
			}
			response, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": result})
			if err != nil {
				t.Fatal(err)
			}
			// A disposable ACP peer uses the observed session/load shape. Run the
			// real handshake so missing ordered-ingress coordinates also fail here.
			bin := filepath.Join(t.TempDir(), "grok-fixture")
			script := fmt.Sprintf(`#!/bin/sh
IFS= read -r request || exit 1
printf '%%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
IFS= read -r request || exit 1
printf '%%s\n' '%s'
while IFS= read -r request; do :; done
`, response)
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			requestedID := sess.threadID
			if tc.mode == "start" {
				requestedID = ""
			}
			err = runGrokHandshake(sess, providerRequest{Mode: tc.mode, ThreadID: requestedID, Cwd: t.TempDir(), GrokBin: bin})
			if tc.identity == "wrong" {
				if err == nil || !strings.Contains(err.Error(), "sessionId") || sends.Load() != 0 || sess.summary().Status == "ready" {
					t.Fatalf("mismatched native identity accepted: err=%v, sends=%d", err, sends.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := sess.summary().Status; got != "ready" {
				t.Fatalf("settled session/load left connection %q", got)
			}
			if tc.active {
				if sum := sess.summary(); !sum.TurnActive || sum.TurnStartedAtMs != 0 || sess.turnID != "surviving-turn" {
					t.Fatalf("provider work was lost or inherited an old anchor: %+v, turn %q", sum, sess.turnID)
				}
				sess.drainQueuedTurn()
				if row, ok := attempts.get("after-restart"); !ok || row.Status != sendAttemptQueued || sends.Load() != 0 {
					t.Fatalf("running provider drained queued work: row=%+v, sends=%d", row, sends.Load())
				}
				return
			}
			waitTurnSendDone(t, sess, "after-restart")
			if row, ok := attempts.get("after-restart"); !ok || row.Status != sendAttemptAccepted || sends.Load() != 1 {
				t.Fatalf("idle provider did not drain exactly once: row=%+v, sends=%d", row, sends.Load())
			}
		})
	}
}

// A resumed Grok session is ready only once its saved transcript is proven,
// on a proxy that is still connected. A proxy that exits while the saved file
// catches up is a lost link, never a ready thread. A session Nano created and
// never sent a prompt, whose transcript Grok has not written, is ready at
// once: its first prompt creates the file. Without that whole proof (a session
// Nano did not create, a prompt parked or delivered before) it waits for its
// transcript.
// Boundary: the real Grok handshake against a disposable ACP peer, with a real
// catalog over a Grok sessions root, to the queued prompt's delivery.
func TestGrokResumeReadinessNeedsItsSavedTranscriptAndALiveProxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    bool // the session's summary and transcript exist
		exits    bool // the ACP peer exits right after answering session/load
		created  bool // Nano's session/new answer named this session
		parked   bool // a prompt to this session was parked before
		prompted bool // Nano delivered a prompt to this session before
		ready    bool
	}{
		{name: "proxy exits while the saved file catches up", files: true, exits: true, created: true},
		{name: "never prompted, no transcript yet", created: true, ready: true},
		{name: "not created by Nano, no transcript", ready: false},
		{name: "a prompt parked before, transcript missing", created: true, parked: true},
		{name: "prompted before, transcript missing", created: true, prompted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "grok-readiness")
			if err != nil {
				t.Fatal(err)
			}
			sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
				sends.Add(1)
				return json.RawMessage(`{"turn":{"id":"queued-provider-turn"}}`), nil
			})
			sess.provider = "grok"
			sess.state = map[string]json.RawMessage{}
			sess.lifecycleState = testLifecycleSnapshot("starting", false)
			sess.cwd = t.TempDir()
			t.Cleanup(sess.close)

			root := t.TempDir()
			dir := filepath.Join(root, url.PathEscape(sess.cwd), sess.threadID)
			if tc.files {
				writeSettlementFile(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"`+sess.threadID+`","cwd":"`+sess.cwd+`"}}`)
				writeSettlementFile(t, filepath.Join(dir, "updates.jsonl"), `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"`+sess.threadID+`","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"earlier prompt"}},"_meta":{"eventId":"earlier-user-event"}}}`+"\n")
			}
			store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			catalog, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
				Roots: []history.SourceRoot{{Provider: history.ProviderGrok, Kind: history.RootGrokSessions, Path: root}},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(catalog.Close)
			catalog.setTranscriptChanged(func(context.Context, transcriptChange) {})
			if tc.exits {
				catalog.projectionMu.Lock() // the saved file cannot catch up
				var once sync.Once
				t.Cleanup(func() { once.Do(catalog.projectionMu.Unlock) })
				defer once.Do(catalog.projectionMu.Unlock)
			}
			metadata, _ := newTestHostedControl(t)
			sess.metadata = metadata
			if err := metadata.ensureHostedThread(sess.hostedThreadID, "grok", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			if tc.created {
				if _, err := sess.metadata.appendEvent(metadataEvent{HostedThreadID: sess.hostedThreadID, Field: postureFieldModel, Value: "grok-4.6",
					Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Evidence: "grok_session_new_response",
					NativeThreadID: sess.threadID, Freshness: metadataFreshnessLive, SourceEventKey: "test:session-new"}); err != nil {
					t.Fatal(err)
				}
			}
			sess.catalogSources = catalog
			sess.setProjectionThreadID(sess.threadID)
			if tc.prompted {
				seedAcceptedTransactionalTurn(t, sess.turns, sess.hostedThreadID, model.ProviderGrok, sess.threadID, "earlier", "prompt-earlier", "earlier prompt")
			}
			if tc.parked {
				text := "parked prompt"
				if _, _, err := sess.turns.Submit(context.Background(), turns.Submission{HostedThreadID: sess.hostedThreadID, Provider: model.ProviderGrok, NativeThreadID: sess.threadID,
					ActionID: "parked", ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
					t.Fatal(err)
				}
				if _, err := attempts.db.Exec(`UPDATE queued_turns SET state='parked' WHERE client_action_id='parked'`); err != nil {
					t.Fatal(err)
				}
			}
			if rec := postTurn(t, sess, context.Background(), "after-restart", "queued while starting"); rec.Code != http.StatusAccepted {
				t.Fatalf("queue status %d: %s", rec.Code, rec.Body.String())
			}

			raw, err := os.ReadFile(filepath.Join("testdata", "grok-lifecycle", "session-load-1.0.5.json"))
			if err != nil {
				t.Fatal(err)
			}
			var load bytes.Buffer
			if err := json.Compact(&load, raw); err != nil {
				t.Fatal(err)
			}
			tail := "while IFS= read -r request; do :; done"
			if tc.exits {
				tail = "exit 0"
			}
			bin := filepath.Join(t.TempDir(), "grok-fixture")
			script := fmt.Sprintf(`#!/bin/sh
IFS= read -r request || exit 1
printf '%%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
IFS= read -r request || exit 1
printf '%%s\n' '{"jsonrpc":"2.0","id":2,"result":%s}'
%s
`, load.Bytes(), tail)
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			handshake := make(chan error, 1)
			go func() {
				handshake <- runGrokHandshake(sess, providerRequest{Mode: "resume", ThreadID: sess.threadID, Cwd: sess.cwd, GrokBin: bin})
			}()

			if tc.ready {
				select {
				case err := <-handshake:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the never-prompted session still waits for a transcript only its first prompt creates")
				}
				waitTurnSendDone(t, sess, "after-restart")
				if got := sess.summary().Status; got != "ready" || sends.Load() != 1 {
					t.Fatalf("never-prompted session: status %q, sends %d; want ready and the first prompt sent once", got, sends.Load())
				}
				return
			}
			if tc.exits {
				select {
				case err := <-handshake:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the handshake still waits for the saved file after its proxy exited")
				}
			}
			if !tc.exits {
				// Close in the phase under test, once initialize has answered and the
				// handshake waits for the saved file. Under load the fake app-server can
				// still be answering initialize a second in, and a close then ends it.
				waitForClaude(t, "the fake app-server to answer initialize", func() bool {
					sess.stateMu.Lock()
					defer sess.stateMu.Unlock()
					_, initialized := sess.state["grokInitialize"]
					return initialized
				})
			}
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if got := sess.summary().Status; got == "ready" || sends.Load() != 0 {
					t.Fatalf("status %q, sends %d; want not ready and nothing sent", got, sends.Load())
				}
			}
			if !tc.exits {
				sess.close() // ends the wait for a transcript that is not coming
				if err := <-handshake; !errors.Is(err, errSpawnSessionClosed) {
					t.Fatalf("handshake after close = %v", err)
				}
			}
		})
	}
}
