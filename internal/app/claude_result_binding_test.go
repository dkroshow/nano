package app

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// claudeResultPrompt is one prompt of the 2026-10-18 capture.
type claudeResultPrompt struct{ name, op, transport, event, turn string }

// claudeResultFixture is that thread before its results: five accepted
// prompts with running operations, under the turn ids the shim gave them.
// fill puts their submission ids into the captured frames and saved rows.
type claudeResultFixture struct {
	h          *settlementHarness
	executions *agentExecutionStore
	completed  *completedTurnStore
	core       *coreturns.Store
	order      []*claudeResultPrompt
	fill       *strings.Replacer
}

func newClaudeResultFixture(t *testing.T) claudeResultFixture {
	t.Helper()
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	executions := newTestAgentExecutionStore(t, "run-1")
	if _, err := executions.ensureSessionWithIdentity("hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.bindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := &completedTurnStore{db: h.queue.db}
	core, err := completed.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}

	order := []*claudeResultPrompt{{name: "A", turn: "claude:hosted:36187"}, {name: "B", turn: "claude:hosted:36195"},
		{name: "C", turn: "claude:hosted:36198"}, {name: "D", turn: "claude:hosted:36201"}, {name: "E", turn: "claude:hosted:36228"}}
	for _, p := range order {
		op, _, err := executions.beginTurn("hosted", "prompt-"+p.name, "hash-"+p.name)
		if err != nil {
			t.Fatal(err)
		}
		p.op, p.transport = op.ID, op.TransportActionID
		p.event = submissionEventID("hosted", p.transport)
		text := "prompt " + p.name
		if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: p.transport,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
		token := "claim-" + p.name
		if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", p.name, claimed, err)
		}
		binding := turns.Binding{EventID: p.event, Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &p.turn}}
		if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: p.transport, ClaimToken: token, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.markTurnDelivery(op.ID, p.turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
	}
	fill := strings.NewReplacer("{{NATIVE}}", native, "{{A}}", order[0].event, "{{B}}", order[1].event,
		"{{C}}", order[2].event, "{{D}}", order[3].event, "{{E}}", order[4].event)
	// Operations take their answers through the saved-result reader, as Nano
	// serves them.
	h.server.turns = h.prompts
	executions.useSavedResults(h.server.savedOperationResult, h.server.savedOperationResultFinished)
	return claudeResultFixture{h: h, executions: executions, completed: completed, core: core, order: order, fill: fill}
}

// Failure caught (Instant thread open, 2026-10-18 15:25Z): Claude was running
// prompt A when Nano wrote B, C and D. A's result came after those writes and
// was recorded on D's turn, the prompt last written, instead of A's; the
// merged turn's result then landed on E, a prompt Claude absorbed mid-turn.
// The saved turn had two answering executions and A, B and C stayed running.
// Each result names the prompt it answered in user_message_uuid. A later
// harness-started result names none: it ends the turn and binds nothing.
// Boundary: the live shim transport through the session loop to durable
// turns and operations, then the saved-commit member step on a real catalog.
// testdata/claude-result-binding is a scrubbed capture of that sequence.
func TestClaudeResultBindsToThePromptClaudeAnswered(t *testing.T) {
	ctx := context.Background()
	f := newClaudeResultFixture(t)
	h, native, executions, completed, core, order, fill := f.h, f.h.fixture.native, f.executions, f.completed, f.core, f.order, f.fill
	type frame struct {
		Seq    int64           `json:"seq"`
		Dir    string          `json:"dir"`
		TurnID string          `json:"turnId"`
		Line   json.RawMessage `json:"line"`
	}
	var frames []frame
	file, err := os.Open("testdata/claude-result-binding/wire.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var f frame
		if err := json.Unmarshal([]byte(fill.Replace(scanner.Text())), &f); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f)
	}
	file.Close()
	// Each prompt's write is followed, once it returns, by the frames the shim
	// spooled before the next write, with the turn ids it tagged them with.
	// A's result reaches Nano while D's write is still in flight, so Nano
	// holds it behind that send.
	until := map[string]int64{"A": 36191, "B": 36196, "C": 36199, "D": 36227, "E": 51041}
	written := make(chan struct{})
	shim := newFakeShim(t, func(c *shimConn) {
		start, ok := c.next()
		if !ok {
			return
		}
		c.reply(start, map[string]any{"shimSessionId": "shim-1"})
		next := 0
		for _, p := range order {
			input, ok := c.next()
			if !ok {
				return
			}
			if p.name != "D" {
				c.reply(input, map[string]any{"turnId": p.turn})
				<-written
			}
			for ; next < len(frames) && frames[next].Seq <= until[p.name]; next++ {
				fr := frames[next]
				c.lineForTurn(fr.Seq, fr.Dir, fr.TurnID, string(fr.Line))
			}
			if p.name == "D" {
				c.reply(input, map[string]any{"turnId": p.turn})
				<-written
			}
		}
		c.answerRest()
	})

	sess := h.attach("claude", nil)
	sess.agentExecutions, sess.completedTurns = executions, newCompletedTurnSource(completed)
	h.server.sessions = map[string]*session{sess.id: sess}
	sess.state, sess.subs, sess.interactions = map[string]json.RawMessage{}, map[chan wireEvent]struct{}{}, map[string]pendingInteraction{}
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	owner := newClaudeSession(sess, native)
	tr, err := startClaudeShimSession(owner, shim.wsURL(), claudeSpawnPosture{})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	owner.tr = tr
	sess.stateMu.Lock()
	sess.claude = owner
	sess.stateMu.Unlock()
	for _, p := range order {
		if _, err := owner.sendTurnWithEventID("prompt "+p.name, p.event); err != nil {
			t.Fatal(err)
		}
		written <- struct{}{}
		waitForClaude(t, "the frames before the next write", func() bool { return tr.lastSeq() >= until[p.name] })
	}
	saved, err := os.ReadFile("testdata/claude-result-binding/saved.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	h.appendSaved(t, fill.Replace(string(saved)))

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := sess.settlePrompts(ctx); err == nil {
			done := true
			for _, p := range order {
				if op, _ := executions.turn(p.op); !agentTurnTerminal(op.Status) {
					done = false
				}
			}
			if owing, err := h.prompts.Owing(ctx, "hosted", []string{order[0].transport, order[1].transport, order[2].transport, order[3].transport, order[4].transport}); err != nil || len(owing) > 0 {
				done = false
			}
			if done {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	answerA, merged := "msg_945108b79d7659eba44f02ca7d8bf0be:0", "msg_62a3a49eff7b5ff194d5d58d220ef8a4:0"
	for _, check := range []struct{ turn, final string }{{order[0].turn, answerA}, {order[3].turn, merged}} {
		if record, err := core.Get(ctx, "hosted", check.turn); err != nil || record.Outcome != agentTurnCompleted || record.FinalMessageID != check.final || record.Source == "saved" {
			t.Fatalf("turn %s = %+v, %v; want the live result naming its prompt, final %s", check.turn, record, err, check.final)
		}
	}
	for _, p := range order {
		want, final := "merged answer", merged
		if p.name == "A" {
			want, final = "answer to A", answerA
		}
		op, _ := executions.turn(p.op)
		if op.Status != agentTurnCompleted || op.FinalItemID != final || op.FinalResponse != want {
			t.Fatalf("operation %s = status %s final %q %q, error %+v; want %q", p.name, op.Status, op.FinalItemID, op.FinalResponse, op.Error, want)
		}
	}
	transports := make([]string, 0, len(order))
	for _, p := range order {
		transports = append(transports, p.transport)
	}
	if owing, err := h.prompts.Owing(ctx, "hosted", transports); err != nil || !reflect.DeepEqual(owing, map[string]bool{}) {
		t.Fatalf("owed = %v, %v; want none", owing, err)
	}
	// The harness-started turn ran last; its result ended it.
	waitForClaude(t, "the harness-started turn to end", func() bool { return !sess.summary().TurnActive })
}

// directClaudeTransport is a direct child's transport: frames carry no
// shim identity, and the test delivers them itself.
type directClaudeTransport struct{}

func (directClaudeTransport) sendLine([]byte) error { return nil }
func (directClaudeTransport) isReplaying() bool     { return false }
func (directClaudeTransport) lastSeq() int64        { return 0 }
func (directClaudeTransport) close() error          { return nil }
func (directClaudeTransport) durableAttachment(string, string) (attachmentRecord, bool) {
	return attachmentRecord{}, false
}

// Failure caught (review of the first fix): a direct child, Nano's fallback
// when the shim is unreachable, still gave a result to the projection's
// latest turn. A's result arriving after D's write went to D, and a result
// naming no prompt would too. Such a result while D is unanswered is logged
// once, however many arrive. Boundary: the session's frame application for
// a transport without frame identity, to the operations and the log.
func TestClaudeDirectChildResultBindsToThePromptClaudeAnswered(t *testing.T) {
	f := newClaudeResultFixture(t)
	a, d := f.order[0], f.order[3]
	sess := f.h.attach("claude", nil)
	sess.agentExecutions, sess.completedTurns = f.executions, newCompletedTurnSource(f.completed)
	sess.state, sess.subs, sess.interactions = map[string]json.RawMessage{}, map[chan wireEvent]struct{}{}, map[string]pendingInteraction{}
	owner := newClaudeSession(sess, f.h.fixture.native)
	tr := directClaudeTransport{}
	owner.tr = tr
	var logs syncBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })
	// D's turn is the projection's latest when A's late frames arrive.
	sess.projection.setClaudeProviderTurnID(d.turn)
	if _, err := owner.sendTurnWithEventID("prompt D", d.event); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open("testdata/claude-result-binding/wire.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var fr struct {
			Seq  int64           `json:"seq"`
			Line json.RawMessage `json:"line"`
		}
		if err := json.Unmarshal([]byte(f.fill.Replace(scanner.Text())), &fr); err != nil {
			t.Fatal(err)
		}
		// A's answer and result, then the turn the harness started, whose
		// result comes twice.
		if fr.Seq == 36217 || fr.Seq == 36221 || fr.Seq >= 50758 {
			owner.deliverLine(tr, protocol.Recv, fr.Line, 0, time.Now())
		}
		if fr.Seq == 51041 {
			owner.deliverLine(tr, protocol.Recv, fr.Line, 0, time.Now())
		}
	}
	// Frames apply on the session loop, after deliverLine returns.
	waitForClaude(t, "A's result", func() bool { op, _ := f.executions.turn(a.op); return op.Status != agentTurnRunning })
	waitForClaude(t, "the harness-started turn to end", func() bool { return !sess.summary().TurnActive })
	if op, _ := f.executions.turn(a.op); op.FinalItemID != "msg_945108b79d7659eba44f02ca7d8bf0be:0" {
		t.Fatalf("A = status %s final %q; want its own result", op.Status, op.FinalItemID)
	}
	if op, _ := f.executions.turn(d.op); op.Status != agentTurnRunning {
		t.Fatalf("D = status %s final %q; want it still running", op.Status, op.FinalItemID)
	}
	if n := strings.Count(logs.String(), "names no prompt while one Nano wrote is unanswered"); n != 1 {
		t.Fatalf("unnamed-result log lines = %d; want 1", n)
	}
}
