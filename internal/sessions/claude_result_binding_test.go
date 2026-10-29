package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	prompts "github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	fixture "github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

type resultPrompt struct{ name, event, turn string }
type resultFrame struct {
	Seq    int64           `json:"seq"`
	Dir    string          `json:"dir"`
	TurnID string          `json:"turnId"`
	Line   json.RawMessage `json:"line"`
}
type resultTerminal struct{ turn, status, item string }
type resultRecorder struct {
	Executions
	mu        sync.Mutex
	terminals []resultTerminal
}

func (r *resultRecorder) ObserveCandidate(string, string, string, string, string, string) error {
	return nil
}
func (r *resultRecorder) MarkTurnTerminal(_, _, turn, status string, _ *agentExecutionError, _, item string, _ ...agentResultMeta) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.terminals = append(r.terminals, resultTerminal{turn, status, item})
	return nil
}
func (r *resultRecorder) snapshot() []resultTerminal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]resultTerminal(nil), r.terminals...)
}
func (*resultRecorder) ReadsSavedResults() bool                                           { return true }
func (*resultRecorder) BindSession(string, string, string, string, string) error          { return nil }
func (*resultRecorder) UpdateSession(string, string, string, string, string) error        { return nil }
func (*resultRecorder) MarkSessionTurns(string, string, *agentExecutionError) error       { return nil }
func (*resultRecorder) MarkWaiting(string, string, bool) error                            { return nil }
func (*resultRecorder) TurnsForHosted(string) []agentTurnRecord                           { return nil }
func (*resultRecorder) OpenTurnsForProviderTurn(string, string, string) []agentTurnRecord { return nil }
func (*resultRecorder) TurnForTransportAction(string) (agentTurnRecord, bool) {
	return agentTurnRecord{}, false
}
func (*resultRecorder) PersistedTurnForTransportAction(context.Context, string, string) (agentTurnRecord, bool, error) {
	return agentTurnRecord{}, false, nil
}

func newResultProjection(provider string, finals map[string]string) *LiveProjection {
	p := newDeliveryTestProjection(provider)
	for turn, final := range finals {
		p.turnWorkLocked(turn).finalAnswer = final
	}
	return p
}

func resultBindingFixture(t *testing.T) (*session, *resultRecorder, *coreturns.Store, []resultPrompt, []resultFrame) {
	t.Helper()
	sess, store := readyTransactionalSendTestSession(t, nil)
	sess.provider, sess.hostedThreadID, sess.threadID = "claude", "hosted", "claude-settle"
	sess.state = map[string]json.RawMessage{}
	sess.subs = map[chan wireEvent]struct{}{}
	sess.interactions = map[string]pendingInteraction{}
	recorder := &resultRecorder{}
	sess.agentExecutions = recorder
	identities := fixture.ClaudeResultPromptIdentities()
	sess.projection = newResultProjection("claude", map[string]string{identities[0].Turn: fixture.ClaudeResultAnswerA, identities[3].Turn: fixture.ClaudeResultAnswerMerged})
	var order []resultPrompt
	replacements := []string{"{{NATIVE}}", sess.ThreadID()}
	for _, identity := range identities {
		action := "prompt-" + identity.Name
		event := SubmissionEventID("hosted", action)
		order = append(order, resultPrompt{identity.Name, event, identity.Turn})
		replacements = append(replacements, "{{"+identity.Name+"}}", event)
		if _, _, err := store.Submit(context.Background(), prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: sess.ThreadID(), ActionID: action, ProviderText: action, PresentationText: action, TextHash: action, RequestHash: action}); err != nil {
			t.Fatal(err)
		}
		token := "claim-" + identity.Name
		if _, claimed, err := store.Claim(context.Background(), prompts.ClaimRequest{HostedThreadID: "hosted", Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", identity.Name, claimed, err)
		}
		native := sess.ThreadID()
		if _, err := store.Accepted(context.Background(), prompts.Acceptance{ActionID: action, ClaimToken: token, AttachmentEpoch: 1, Binding: prompts.Binding{EventID: event, Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &identity.Turn}}}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := fixture.ClaudeResultFixture("wire.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.NewReplacer(replacements...).Replace(string(raw)))
	var frames []resultFrame
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var frame resultFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.coreSession().Close)
	turns, err := coreturns.OpenStore(context.Background(), sess.queuedTurns.Database())
	if err != nil {
		t.Fatal(err)
	}
	return sess, recorder, turns, order, frames
}

// The app sibling verifies saved member proofs and durable operation results.
// This boundary preserves the captured live-input race and checks which
// prompt each result names before any saved transcript can correct it.
func TestClaudeResultBindsToThePromptClaudeAnswered(t *testing.T) {
	sess, recorder, turns, order, frames := resultBindingFixture(t)
	until := map[string]int64{"A": 36191, "B": 36196, "C": 36199, "D": 36227, "E": 51041}
	written := make(chan struct{})
	shim := newFakeShim(t, func(c *shimConn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
		next := 0
		for _, prompt := range order {
			input, ok := c.Next()
			if !ok {
				return
			}
			if prompt.name != "D" {
				c.Reply(input, map[string]any{"turnId": prompt.turn})
				<-written
			}
			for ; next < len(frames) && frames[next].Seq <= until[prompt.name]; next++ {
				frame := frames[next]
				c.LineForTurn(frame.Seq, frame.Dir, frame.TurnID, string(frame.Line))
			}
			if prompt.name == "D" {
				c.Reply(input, map[string]any{"turnId": prompt.turn})
				<-written
			}
		}
		c.AnswerRest()
	})
	owner := newClaudeSession(sess, sess.ThreadID())
	tr, err := startClaudeShimSession(owner, shim.WSURL(), claudeSpawnPosture{})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	owner.tr = tr
	sess.claude = owner
	for _, prompt := range order {
		if _, err := owner.sendTurnWithEventID("prompt "+prompt.name, prompt.event); err != nil {
			t.Fatal(err)
		}
		written <- struct{}{}
		waitForClaude(t, "captured frames", func() bool { return tr.lastSeq() >= until[prompt.name] })
	}
	if err := sess.coreSession().Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []resultTerminal{{order[0].turn, agentTurnCompleted, fixture.ClaudeResultAnswerA}, {order[3].turn, agentTurnCompleted, fixture.ClaudeResultAnswerMerged}} {
		record, err := turns.Get(context.Background(), "hosted", want.turn)
		if err != nil || record.Outcome != want.status || record.FinalMessageID != want.item || record.Source == "saved" {
			t.Fatalf("turn %s = %+v %v", want.turn, record, err)
		}
		found := false
		for _, call := range recorder.snapshot() {
			if call == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("result %s was not delivered to its own operation: %+v", want.turn, recorder.snapshot())
		}
	}
	for _, call := range recorder.snapshot() {
		if call.turn == order[4].turn {
			t.Fatalf("absorbed E took D's result: %+v", call)
		}
	}
	if sess.Summary().TurnActive {
		t.Fatal("the harness-started turn did not end")
	}
}

type directResultTransport struct{}

func (directResultTransport) sendLine([]byte) error { return nil }
func (directResultTransport) isReplaying() bool     { return false }
func (directResultTransport) lastSeq() int64        { return 0 }
func (directResultTransport) close() error          { return nil }
func (directResultTransport) durableAttachment(string, string) (attachmentRecord, bool) {
	return attachmentRecord{}, false
}

func TestClaudeDirectChildResultBindsToThePromptClaudeAnswered(t *testing.T) {
	sess, recorder, _, order, frames := resultBindingFixture(t)
	owner := newClaudeSession(sess, sess.ThreadID())
	tr := directResultTransport{}
	owner.tr = tr
	var logs resultLogBuffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	sess.projection.SetClaudeProviderTurnID(order[3].turn)
	if _, err := owner.sendTurnWithEventID("prompt D", order[3].event); err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if frame.Seq == 36217 || frame.Seq == 36221 || frame.Seq >= 50758 {
			owner.deliverLine(tr, protocol.Recv, frame.Line, 0, time.Now())
		}
		if frame.Seq == 51041 {
			owner.deliverLine(tr, protocol.Recv, frame.Line, 0, time.Now())
		}
	}
	if err := sess.coreSession().Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range recorder.snapshot() {
		if call.turn == order[0].turn && call.item == fixture.ClaudeResultAnswerA {
			found = true
		}
		if call.turn == order[3].turn {
			t.Fatalf("D took A's result: %+v", call)
		}
	}
	if !found {
		t.Fatalf("A's result missing: %+v", recorder.snapshot())
	}
	if n := strings.Count(logs.String(), "names no prompt while one Nano wrote is unanswered"); n != 1 {
		t.Fatalf("unnamed-result log lines = %d; want 1", n)
	}
	if sess.Summary().TurnActive {
		t.Fatal("the harness-started turn did not end")
	}
}

type resultLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *resultLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *resultLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func (*resultRecorder) SessionNeedsAttention(string) bool { return false }
