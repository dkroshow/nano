package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	testfixture "github.com/dkroshow/nano/internal/testsupport"
	grokfixture "github.com/dkroshow/nano/internal/testsupport/grokserver"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

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
			sess := newSession(runtimeFixtureConfig(t, sessionConfig{ID: "grok-boot-" + tc.name, Provider: "grok", HostedThreadID: hostedID, Projection: newLiveProjection("grok"), AgentExecutions: executions, Metadata: control}))
			if err := sess.InitializeBinding(generation); err != nil {
				t.Fatal(err)
			}
			sess.ReserveNativeID("native-" + tc.name)
			if err := executions.BindSession(hostedID, sess.ID(), sess.ThreadID(), "starting", presentationShared); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sess.Close)

			if err := sess.BeginProviderSpawn(providerRequest{Provider: "grok", Mode: "resume", ThreadID: sess.ThreadID(), Cwd: stateDir, GrokBin: bin, BootReattach: newBootProviderControl(0)}, nil, func() {}); err != nil {
				t.Fatal(err)
			}

			waitForGrokAttempts(t, stateDir, 1)
			waitForGrokStatus(t, sess, "reconnecting")
			waitForGrokStatus(t, sess, tc.wantStatus)
			if got := readGrokAttempts(t, stateDir); got != tc.wantAttempts {
				t.Fatalf("session/load attempts = %d, want %d", got, tc.wantAttempts)
			}
			// The stored status is written just after the in-memory one (the
			// session publishes and notifies first), so wait for it within the
			// same deadline rather than reading it once.
			for deadline := time.Now().Add(110 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if recorded, _ := executions.session(hostedID); recorded.Status == tc.wantStatus {
					break
				}
			}
			recorded, ok := executions.session(hostedID)
			if !ok || recorded.Status != tc.wantStatus {
				t.Fatalf("durable hosted state = %+v, found=%v; want %s", recorded, ok, tc.wantStatus)
			}
			if tc.wantStatus == "exited" {
				summary := sess.Summary()
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
	deadline := time.Now().Add(110 * time.Second)
	for time.Now().Before(deadline) {
		if sess.Summary().Status == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Grok session status = %q, want %q", sess.Summary().Status, want)
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
			promptStore := testPromptStore(t, attempts.Database())
			queue, err := newQueuedTurnStore(attempts.Database(), attempts.Path(), promptStore)
			if err != nil {
				t.Fatal(err)
			}
			attempts.BindTransactionStore(promptStore)
			queue.BindTransactionStore(promptStore)
			metadata, _ := newTestHostedControl(t)
			sess := newSession(sessionConfig{ID: "nano-session", Provider: "grok", HostedThreadID: "hosted-thread", CWD: t.TempDir(), Projection: newLiveProjection("grok", "hosted-thread"), Turns: promptStore, QueuedTurns: queue, SendAttempts: attempts, Metadata: metadata})
			if err := sess.InitializeBinding(1); err != nil {
				t.Fatal(err)
			}
			sess.ReserveNativeID("native-thread")
			t.Cleanup(sess.Close)

			root := t.TempDir()
			dir := filepath.Join(root, url.PathEscape(sess.CWD()), sess.ThreadID())
			if tc.files {
				writeSettlementFile(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"`+sess.ThreadID()+`","cwd":"`+sess.CWD()+`"}}`)
				writeSettlementFile(t, filepath.Join(dir, "updates.jsonl"), `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"`+sess.ThreadID()+`","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"earlier prompt"}},"_meta":{"eventId":"earlier-user-event"}}}`+"\n")
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
			if err := metadata.ensureHostedThread(sess.HostedThreadID(), "grok", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			if tc.created {
				if _, err := metadata.AppendEvent(metadataEvent{HostedThreadID: sess.HostedThreadID(), Field: postureFieldModel, Value: "grok-4.6",
					Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Evidence: "grok_session_new_response",
					NativeThreadID: sess.ThreadID(), Freshness: metadataFreshnessLive, SourceEventKey: "test:session-new"}); err != nil {
					t.Fatal(err)
				}
			}
			sess.ConfigureRegistration(nil, nil, false, catalog)
			sess.ReserveNativeID(sess.ThreadID())
			if tc.prompted {
				seedAcceptedTransactionalTurn(t, sess.PromptStore(), sess.HostedThreadID(), model.ProviderGrok, sess.ThreadID(), "earlier", "prompt-earlier", "earlier prompt")
			}
			if tc.parked {
				text := "parked prompt"
				if _, _, err := sess.PromptStore().Submit(context.Background(), turns.Submission{HostedThreadID: sess.HostedThreadID(), Provider: model.ProviderGrok, NativeThreadID: sess.ThreadID(),
					ActionID: "parked", ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
					t.Fatal(err)
				}
				if _, err := attempts.Database().Exec(`UPDATE queued_turns SET state='parked' WHERE client_action_id='parked'`); err != nil {
					t.Fatal(err)
				}
			}
			if rec := postTurn(t, sess, context.Background(), "after-restart", "queued while starting"); rec.Code != http.StatusAccepted {
				t.Fatalf("queue status %d: %s", rec.Code, rec.Body.String())
			}

			raw, err := testfixture.GrokLifecycleFixture("session-load-1.0.5.json")
			if err != nil {
				t.Fatal(err)
			}
			peerEnded := make(chan struct{})
			provider := grokfixture.NewServer(t, func(c *grokfixture.Conn) {
				defer close(peerEnded)
				for {
					call, ok := c.Next()
					if !ok {
						return
					}
					switch call.Method {
					case "initialize":
						c.Reply(call, map[string]any{"protocolVersion": 1})
					case "session/load":
						c.Reply(call, json.RawMessage(raw))
						if tc.exits {
							return
						}
					case "session/prompt":
						sends.Add(1)
						c.Reply(call, map[string]any{"_meta": map[string]any{"promptId": "queued-provider-turn"}})
					default:
						c.Reply(call, map[string]any{})
					}
				}
			})
			if err := sess.BeginProviderSpawn(providerRequest{Provider: "grok", Mode: "resume", ThreadID: sess.ThreadID(), Cwd: sess.CWD(), GrokBin: provider.Bin()}, nil, func() {}); err != nil {
				t.Fatal(err)
			}

			if tc.ready {
				waitForClaude(t, "the never-prompted session to become ready", func() bool { return sess.Summary().Status == "ready" })
				waitTurnSendDone(t, sess, "after-restart")
				if got := sess.Summary().Status; got != "ready" || sends.Load() != 1 {
					t.Fatalf("never-prompted session: status %q, sends %d; want ready and the first prompt sent once", got, sends.Load())
				}
				return
			}
			if tc.exits {
				select {
				case <-peerEnded:
				case <-time.After(10 * time.Second):
					t.Fatal("the proxy did not exit")
				}
				waitForClaude(t, "the session to observe its lost proxy", func() bool { return sess.Summary().Status == "reconnecting" })
			}
			if !tc.exits {
				// Close in the phase under test, once initialize has answered and the
				// handshake waits for the saved file. Under load the fake app-server can
				// still be answering initialize a second in, and a close then ends it.
				waitForClaude(t, "the fake app-server to answer initialize", func() bool {
					rec := httptest.NewRecorder()
					sess.HandleState(rec, httptest.NewRequest(http.MethodGet, "/state", nil))
					var state map[string]json.RawMessage
					if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
						t.Fatal(err)
					}
					_, initialized := state["grokInitialize"]
					return initialized
				})
			}
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if got := sess.Summary().Status; got == "ready" || sends.Load() != 0 {
					t.Fatalf("status %q, sends %d; want not ready and nothing sent", got, sends.Load())
				}
			}
			if !tc.exits {
				sess.Close() // ends the wait for a transcript that is not coming
				select {
				case <-peerEnded:
				case <-time.After(10 * time.Second):
					t.Fatal("close did not stop the provider while waiting for its saved transcript")
				}
			}
		})
	}
}
