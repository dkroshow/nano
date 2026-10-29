package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	testfixture "github.com/dkroshow/nano/internal/testsupport"
	grokfixture "github.com/dkroshow/nano/internal/testsupport/grokserver"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

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
			sess := newSession(sessions.Config{ID: "nano-session", Provider: "grok", HostedThreadID: "hosted-thread", CWD: t.TempDir(), Projection: newLiveProjection("grok", "hosted-thread"), Turns: promptStore, QueuedTurns: queue, SendAttempts: attempts, Metadata: metadata})
			if err := sess.InitializeBinding(1); err != nil {
				t.Fatal(err)
			}
			sess.ReserveNativeID("native-thread")
			t.Cleanup(sess.Lifecycle().Close)

			root := t.TempDir()
			dir := filepath.Join(root, url.PathEscape(sess.CWD()), sess.State().ThreadID())
			if tc.files {
				writeSettlementFile(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"`+sess.State().ThreadID()+`","cwd":"`+sess.CWD()+`"}}`)
				writeSettlementFile(t, filepath.Join(dir, "updates.jsonl"), `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"`+sess.State().ThreadID()+`","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"earlier prompt"}},"_meta":{"eventId":"earlier-user-event"}}}`+"\n")
			}
			store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			catalog, err := newThreadCatalogService(context.Background(), store, threadindex.Options{
				Roots: []history.SourceRoot{{Provider: history.ProviderGrok, Kind: history.RootGrokSessions, Path: root}},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(catalog.Close)
			catalog.SetTranscriptChanged(func(context.Context, threadindex.TranscriptChange) {})
			attached := &pausedAttachedCatalog{CatalogSource: catalog}
			if tc.exits {
				release := attached.pause()
				t.Cleanup(release)
				defer release()
			}
			if err := metadata.EnsureHostedThread(sess.HostedThreadID(), "grok", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			if tc.created {
				if _, err := metadata.AppendEvent(hostedstore.MetadataEvent{HostedThreadID: sess.HostedThreadID(), Field: postureFieldModel, Value: "grok-4.6",
					Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Evidence: "grok_session_new_response",
					NativeThreadID: sess.State().ThreadID(), Freshness: metadataFreshnessLive, SourceEventKey: "test:session-new"}); err != nil {
					t.Fatal(err)
				}
			}
			sess.ConfigureRegistration(nil, nil, false, attached)
			sess.ReserveNativeID(sess.State().ThreadID())
			if tc.prompted {
				seedAcceptedTransactionalTurn(t, sess.Delivery().PromptStore(), sess.HostedThreadID(), model.ProviderGrok, sess.State().ThreadID(), "earlier", "prompt-earlier", "earlier prompt")
			}
			if tc.parked {
				text := "parked prompt"
				if _, _, err := sess.Delivery().PromptStore().Submit(context.Background(), prompts.Submission{HostedThreadID: sess.HostedThreadID(), Provider: model.ProviderGrok, NativeThreadID: sess.State().ThreadID(),
					ActionID: "parked", ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
					t.Fatal(err)
				}
				if _, err := attempts.Database().Exec(`UPDATE queued_turns SET state='parked' WHERE client_action_id='parked'`); err != nil {
					t.Fatal(err)
				}
			}
			if rec := postTurn(t, sess.Delivery(), context.Background(), "after-restart", "queued while starting"); rec.Code != http.StatusAccepted {
				t.Fatalf("queue status %d: %s", rec.Code, rec.Body.String())
			}

			raw, err := testfixture.GrokLifecycleFixture("session-load-1.0.5.json")
			if err != nil {
				t.Fatal(err)
			}
			peerEnded := make(chan struct{})
			initializeReplied := make(chan struct{}, 1)
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
						select {
						case initializeReplied <- struct{}{}:
						default:
						}
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
			if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "grok", Mode: "resume", ThreadID: sess.State().ThreadID(), Cwd: sess.CWD(), GrokBin: provider.Bin()}, nil, func() {}); err != nil {
				t.Fatal(err)
			}

			if tc.ready {
				waitForClaude(t, "the never-prompted session to become ready", func() bool { return sess.View().Summary().Status == "ready" })
				waitTurnSendDone(t, sess, "after-restart")
				if got := sess.View().Summary().Status; got != "ready" || sends.Load() != 1 {
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
				waitForClaude(t, "the session to observe its lost proxy", func() bool { return sess.View().Summary().Status == "reconnecting" })
			}
			if !tc.exits {
				// Close in the phase under test, once initialize has answered and the
				// handshake waits for the saved file. Under load the fake app-server can
				// still be answering initialize a second in, and a close then ends it.
				// Process startup is fixture setup. Start the existing state-check
				// window only after the peer has actually answered initialize.
				select {
				case <-initializeReplied:
				case <-peerEnded:
					t.Fatal("fake app-server exited before initialize completed")
				}
				waitForClaude(t, "the fake app-server to answer initialize", func() bool {
					rec := httptest.NewRecorder()
					sess.View().HandleState(rec, httptest.NewRequest(http.MethodGet, "/state", nil))
					var state map[string]json.RawMessage
					if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
						t.Fatal(err)
					}
					_, initialized := state["grokInitialize"]
					return initialized
				})
			}
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if got := sess.View().Summary().Status; got == "ready" || sends.Load() != 0 {
					t.Fatalf("status %q, sends %d; want not ready and nothing sent", got, sends.Load())
				}
			}
			if !tc.exits {
				sess.Lifecycle().Close() // ends the wait for a transcript that is not coming
				select {
				case <-peerEnded:
				case <-time.After(10 * time.Second):
					t.Fatal("close did not stop the provider while waiting for its saved transcript")
				}
			}
		})
	}
}
