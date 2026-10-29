package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/sessions"
	grokfixture "github.com/dkroshow/nano/internal/testsupport/grokserver"
)

// providerRuntimeFixture drives attachment state through the captured provider protocols.
type providerRuntimeFixture struct {
	session      *session
	projection   *sessions.LiveProjection
	codex        *fakeAppServer
	daemon       *codexDaemon
	claude       *shimConn
	grok         *grokfixture.Conn
	seq          int64
	send         func(context.Context, string) (json.RawMessage, error)
	onInterrupt  func()
	resumeResult map[string]any
}

func newProviderRuntimeFixture(t *testing.T, cfg sessionConfig, native, model string, prepare ...func(*providerRuntimeFixture)) *providerRuntimeFixture {
	t.Helper()
	if cfg.HostedThreadID == "" {
		cfg.HostedThreadID = "fixture-" + cfg.ID
	}
	cfg = runtimeFixtureConfig(t, cfg)
	if cfg.Projection == nil {
		cfg.Projection = newLiveProjection(cfg.Provider, cfg.HostedThreadID)
	}
	sess := newSession(cfg)
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	return connectProviderRuntimeFixture(t, sess, cfg.Projection, native, model, prepare...)
}

func connectProviderRuntimeFixture(t *testing.T, sess *session, view *sessions.LiveProjection, native, model string, prepare ...func(*providerRuntimeFixture)) *providerRuntimeFixture {
	t.Helper()
	f := &providerRuntimeFixture{session: sess}
	f.projection = view
	originalNative := native
	if native == "" {
		native = sess.ID() + "-native"
	}
	sess.ReserveNativeID(native)
	for _, fn := range prepare {
		fn(f)
	}
	switch sess.Provider() {
	case "codex":
		f.codex = newFakeAppServer(t)
		f.daemon = f.codex.daemon()
		f.codex.OnTurnInterrupt = f.onInterrupt
		if f.send != nil {
			f.codex.TurnStartReply = func(text string) (json.RawMessage, error) { return f.send(context.Background(), text) }
		}
		f.codex.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "provider-turn-1"}})
		if f.resumeResult != nil {
			f.codex.SetResumeResult(f.resumeResult)
		} else {
			f.codex.SetResumeResult(map[string]any{"thread": map[string]any{"id": native}, "model": model})
		}
		if err := sess.BeginProviderSpawn(providerRequest{Provider: "codex", Mode: "resume", ThreadID: native}, f.daemon, func() {}); err != nil {
			t.Fatal(err)
		}
	case "claude":
		connected := make(chan *shimConn, 1)
		shim := newFakeShim(t, func(c *shimConn) {
			start, ok := c.Next()
			if !ok {
				return
			}
			c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
			connected <- c
			for {
				call, ok := c.Next()
				if !ok {
					return
				}
				if call.Method == shimMethodInput {
					if f.send != nil {
						var params struct {
							Line string `json:"line"`
						}
						_ = json.Unmarshal(call.Params, &params)
						var input struct {
							Type    string `json:"type"`
							Message struct {
								Content json.RawMessage `json:"content"`
							} `json:"message"`
						}
						_ = json.Unmarshal([]byte(params.Line), &input)
						if input.Type != "user" {
							c.Reply(call, map[string]any{})
							continue
						}
						var text string
						if json.Unmarshal(input.Message.Content, &text) != nil {
							var parts []struct {
								Text string `json:"text"`
							}
							_ = json.Unmarshal(input.Message.Content, &parts)
							for _, part := range parts {
								text += part.Text
							}
						}
						result, err := f.send(context.Background(), text)
						if err != nil {
							c.ReplyError(call, -32000, err.Error())
							continue
						}
						var reply struct {
							Turn struct {
								ID string `json:"id"`
							} `json:"turn"`
						}
						_ = json.Unmarshal(result, &reply)
						c.Reply(call, map[string]any{"turnId": reply.Turn.ID})
					} else {
						c.Reply(call, map[string]any{"turnId": "provider-turn-1"})
					}
				} else {
					c.Reply(call, map[string]any{})
				}
			}
		})
		if err := sess.BeginProviderSpawn(providerRequest{Provider: "claude", ClaudeShimWS: shim.WSURL(), ClaudeBin: "/fake/claude", Cwd: sess.CWD()}, nil, func() {}); err != nil {
			t.Fatal(err)
		}
		select {
		case f.claude = <-connected:
		case <-time.After(time.Second):
			t.Fatal("shim did not connect")
		}
	case "grok":
		connected := make(chan *grokfixture.Conn, 1)
		provider := grokfixture.NewServer(t, func(c *grokfixture.Conn) {
			for {
				call, ok := c.Next()
				if !ok {
					return
				}
				switch call.Method {
				case "initialize":
					c.Reply(call, map[string]any{"protocolVersion": 1})
				case "session/new", "session/load":
					c.Reply(call, map[string]any{"sessionId": native, "_meta": map[string]any{"modelState": map[string]any{"currentModelId": model}, "x.ai/sessionConfig": map[string]any{"options": []any{map[string]any{"category": "mode", "id": "high", "selected": true}}}}})
					connected <- c
				case "session/prompt":
					id := "provider-turn-1"
					if f.send != nil {
						var params struct {
							Prompt []struct {
								Text string `json:"text"`
							} `json:"prompt"`
						}
						_ = json.Unmarshal(call.Params, &params)
						text := ""
						for _, part := range params.Prompt {
							text += part.Text
						}
						result, err := f.send(context.Background(), text)
						if err != nil {
							c.Send(map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32000, "message": err.Error()}})
							continue
						}
						var reply struct {
							Turn struct {
								ID string `json:"id"`
							} `json:"turn"`
						}
						_ = json.Unmarshal(result, &reply)
						id = reply.Turn.ID
					}
					c.Reply(call, map[string]any{"_meta": map[string]any{"promptId": id}})
				default:
					c.Reply(call, map[string]any{})
				}
			}
		})
		if err := sess.BeginProviderSpawn(providerRequest{Provider: "grok", Mode: "resume", ThreadID: native, Cwd: t.TempDir(), GrokBin: provider.Bin(), GrokLeaderSocket: "fixture-only"}, nil, func() {}); err != nil {
			t.Fatal(err)
		}
		// This is a child-process setup wait, not a product timing assertion.
		select {
		case f.grok = <-connected:
		case <-time.After(30 * time.Second):
			t.Fatalf("Grok fixture did not connect: %+v", sess.Summary())
		}
	default:
		t.Fatalf("unsupported runtime fixture provider %q", sess.Provider())
	}
	t.Cleanup(sess.Close)
	waitReconnectCondition(t, func() bool { return sess.Summary().Status == "ready" })
	if sess.Provider() == "claude" {
		emitClaudeInit(t, sess, f.claude, 1, native, model)
		f.seq = 2
	}
	if originalNative == "" {
		sess.ReserveNativeID("")
	}
	return f
}

func (f *providerRuntimeFixture) emit(t *testing.T, line []byte) { f.emitAt(t, line, time.Time{}, "") }
func (f *providerRuntimeFixture) emitAt(t *testing.T, line []byte, at time.Time, turn string) {
	f.emitDirectionAt(t, "recv", line, at, turn)
}
func (f *providerRuntimeFixture) emitDirectionAt(t *testing.T, dir string, line []byte, at time.Time, turn string) {
	t.Helper()
	if f.codex != nil {
		f.codex.EmitRawThreadNotification(f.session.ThreadID(), line)
	} else if f.grok != nil {
		var frame map[string]any
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatal(err)
		}
		f.grok.Send(frame)
	} else {
		f.seq++
		params := map[string]any{"shimSessionId": "shim-1", "seq": f.seq, "dir": dir, "line": string(line), "turnId": turn}
		if !at.IsZero() {
			params["ts"] = at.Format(time.RFC3339Nano)
		}
		f.claude.Notify(shimMethodLine, params)
	}
}

func runtimeFixtureConfig(t *testing.T, cfg sessionConfig) sessionConfig {
	t.Helper()
	if cfg.Turns == nil {
		queue := mustQueuedTurnStore(t)
		cfg.Turns = testPromptStore(t, queue.Database())
		cfg.QueuedTurns = queue
		journal, err := newSendAttemptStore(queue.Database(), queue.Path(), "runtime-fixture", cfg.Turns)
		if err != nil {
			t.Fatal(err)
		}
		cfg.SendAttempts = journal
		queue.BindTransactionStore(cfg.Turns)
		journal.BindTransactionStore(cfg.Turns)
	}

	return cfg
}

func configuredFixtureSession(t *testing.T, cfg sessionConfig, native, status, model string, active ...bool) *session {
	t.Helper()
	if status == "ready" {
		f := newProviderRuntimeFixture(t, cfg, native, model)
		if len(active) > 0 && active[0] {
			switch cfg.Provider {
			case "codex":
				f.emit(t, []byte(`{"method":"turn/started","params":{"threadId":"`+f.session.ThreadID()+`","turn":{}}}`))
			case "claude":
				f.emit(t, []byte(`{"type":"assistant","message":{"id":"fixture-message","role":"assistant","content":[{"type":"text","text":"working"}]}}`))
			case "grok":
				f.emit(t, []byte(`{"method":"session/update","params":{"sessionId":"`+f.session.ThreadID()+`","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"working"}},"_meta":{"promptId":"fixture-turn","eventId":"fixture-event"}}}`))
			}
			waitReconnectCondition(t, func() bool { return f.session.Summary().TurnActive })
		}
		return f.session
	}
	sess := newFixtureSession(cfg, native)
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	if status == sessionStatusExited {
		sess.Close()
	}
	return sess
}

// emitAndWait observes the public wire log after the frame's semantic application.
func (f *providerRuntimeFixture) emitAndWait(t *testing.T, line []byte) {
	t.Helper()
	f.emitAtAndWait(t, line, time.Time{}, "")
}

func (f *providerRuntimeFixture) emitAtAndWait(t *testing.T, line []byte, at time.Time, turn string) {
	f.emitDirectionAtAndWait(t, "recv", line, at, turn)
}
func (f *providerRuntimeFixture) emitDirectionAtAndWait(t *testing.T, dir string, line []byte, at time.Time, turn string) {
	t.Helper()
	var expected any
	if err := json.Unmarshal(line, &expected); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(expected)
	count := func() int {
		n := 0
		for _, event := range claudeWireEvents(f.session) {
			var value any
			if json.Unmarshal(event.Msg, &value) == nil {
				encoded, _ := json.Marshal(value)
				if string(encoded) == string(canonical) {
					n++
				}
			}
		}
		return n
	}
	before := count()
	f.emitDirectionAt(t, dir, line, at, turn)
	waitForClaude(t, "wire frame application", func() bool { return count() > before })
}

func (f *providerRuntimeFixture) activate(t *testing.T) {
	switch f.session.Provider() {
	case "codex":
		f.emitAndWait(t, []byte(`{"method":"turn/started","params":{"threadId":"`+f.session.ThreadID()+`","turn":{}}}`))
	case "claude":
		f.emitAndWait(t, []byte(`{"type":"assistant","message":{"id":"fixture-message","role":"assistant","content":[{"type":"text","text":"working"}]}}`))
	}
	waitForClaude(t, "active provider turn", func() bool { return f.session.Summary().TurnActive })
}
func (f *providerRuntimeFixture) finishTurn(t *testing.T) {
	if f.session.Provider() == "codex" {
		turn := liveStatusForTest(t, f.session).Runtime.TurnID
		f.emitAndWait(t, []byte(`{"method":"turn/completed","params":{"threadId":"`+f.session.ThreadID()+`","turn":{"id":"`+turn+`","status":"completed"}}}`))
	} else {
		f.emitAndWait(t, []byte(`{"type":"result","subtype":"success","is_error":false}`))
	}
}

// completeTurn reports a provider terminal for the named accepted prompt.
func (f *providerRuntimeFixture) completeTurn(t *testing.T, turn, action string) {
	t.Helper()
	var line []byte
	switch f.session.Provider() {
	case "codex":
		line = []byte(`{"method":"turn/completed","params":{"threadId":"` + f.session.ThreadID() + `","turn":{"id":"` + turn + `","status":"completed"}}}`)
	case "claude":
		line = []byte(`{"type":"result","subtype":"success","is_error":false,"user_message_uuid":"` + submissionEventID(f.session.HostedThreadID(), action) + `"}`)
	case "grok":
		line = []byte(`{"method":"_x.ai/session_notification","params":{"sessionId":"` + f.session.ThreadID() + `","update":{"sessionUpdate":"turn_completed","prompt_id":"` + turn + `","stop_reason":"end_turn"}}}`)
	}
	f.emitAndWait(t, line)
}

// emitAndDrain waits past provider-side state capture as well as raw publication.
// A subsequent frame on the same reader cannot arrive until the prior callback
// returns; its raw event also passes the session's ordered consumer.
func (f *providerRuntimeFixture) emitAndDrain(t *testing.T, line []byte) {
	t.Helper()
	f.emitAndWait(t, line)
	if f.codex != nil {
		f.emitAndWait(t, []byte(`{"method":"fixture/provider-state-drained","params":{"threadId":"`+f.session.ThreadID()+`"}}`))
		return
	}
	f.emitAndWait(t, []byte(`{"type":"fixture_provider_state_drained"}`))
}
