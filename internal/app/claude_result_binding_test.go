package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	fixture "github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// claudeResultPrompt is one prompt of the 2026-10-18 capture.
type claudeResultPrompt struct{ name, op, transport, event, turn string }

// claudeResultFixture is that thread before its results: five accepted
// prompts with running operations, under the turn ids the shim gave them.
// fill puts their submission ids into the captured frames and saved rows.
type claudeResultFixture struct {
	h          *settlementHarness
	executions *hostedstore.ExecutionStore
	completed  *hostedmodel.CompletedTurnStore
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
	if _, err := fixtureEnsureSessionWithIdentity(executions, "hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
	core, err := completed.CoreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var order []*claudeResultPrompt
	for _, identity := range fixture.ClaudeResultPromptIdentities() {
		order = append(order, &claudeResultPrompt{name: identity.Name, turn: identity.Turn})
	}
	for _, p := range order {
		op, _, err := fixtureBeginTurn(executions, "hosted", "prompt-"+p.name, "hash-"+p.name)
		if err != nil {
			t.Fatal(err)
		}
		p.op, p.transport = op.ID, op.TransportActionID
		p.event = submissionEventID("hosted", p.transport)
		text := "prompt " + p.name
		if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: p.transport,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
		token := "claim-" + p.name
		if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", p.name, claimed, err)
		}
		binding := prompts.Binding{EventID: p.event, Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &p.turn}}
		if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: p.transport, ClaimToken: token, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.MarkTurnDelivery(op.ID, p.turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
	}
	fill := strings.NewReplacer("{{NATIVE}}", native, "{{A}}", order[0].event, "{{B}}", order[1].event,
		"{{C}}", order[2].event, "{{D}}", order[3].event, "{{E}}", order[4].event)
	// Operations take their answers through the saved-result reader, as Nano
	// serves them.
	h.server.turns = h.prompts
	executions.UseSavedResults(fixtureOwners(h.server).attachmentCoordinator().SavedOperationResult, fixtureOwners(h.server).attachmentCoordinator().SavedOperationResultFinished)
	return claudeResultFixture{h: h, executions: executions, completed: completed, core: core, order: order, fill: fill}
}

// Failure caught (Instant thread open, 2026-10-18 15:25Z): Claude was running
// prompt A when Nano wrote B, C and D. A's result came after those writes and
// was recorded on D's turn, the prompt last written, instead of A's; the
// merged turn's result then landed on E, a prompt Claude absorbed mid-turn.
// The saved turn had two answering executions and A, B and C stayed running.
// Each result names the prompt it answered in user_message_uuid. A later
// harness-started result names none: it ends the turn and binds nothing.
// Boundary: the saved-commit member step on a real catalog and operation store.
// The session sibling preserves the live shim transport and result-routing checks.
// The shared claude-result-binding fixture is a scrubbed capture of that sequence.
func TestClaudeResultSavedRowsBindToAnsweredPrompt(t *testing.T) {
	ctx := context.Background()
	f := newClaudeResultFixture(t)
	h, executions, core, order, fill := f.h, f.executions, f.core, f.order, f.fill
	sess := newClaudeResultSession(t, f)
	seedTestSessionRegistry(&h.server.Registry, map[string]*sessions.Session{sess.ID(): sess})
	// The session sibling proves these captured result identities at ingress.
	// This boundary owns their saved-row joins and durable operation results.
	for _, result := range []struct{ turn, final string }{{order[0].turn, fixture.ClaudeResultAnswerA}, {order[3].turn, fixture.ClaudeResultAnswerMerged}} {
		if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: h.fixture.native, TurnID: result.turn, FinalMessageID: result.final, Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := fixture.ClaudeResultFixture("saved.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	h.appendSaved(t, fill.Replace(string(saved)))

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := sess.Delivery().SettlePrompts(ctx); err == nil {
			done := true
			for _, p := range order {
				if op, _ := executions.Turn(p.op); !threads.TurnTerminal(op.Status) {
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

	answerA, merged := fixture.ClaudeResultAnswerA, fixture.ClaudeResultAnswerMerged
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
		op, _ := executions.Turn(p.op)
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
}
func newClaudeResultSession(t *testing.T, f claudeResultFixture) *sessions.Session {
	t.Helper()
	h := f.h
	p := newLiveProjection("claude", "hosted")
	sess := newSession(sessions.Config{ID: "nano-session", Provider: "claude", HostedThreadID: "hosted", Projection: p, AgentExecutions: f.executions, Metadata: h.control, CompletedTurns: f.completed.CoreStore, Turns: h.prompts, QueuedTurns: h.queue, SendAttempts: h.attempts})
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID(h.fixture.native)
	sess.ConfigureSavedReaders(nil, h.server.savedPromptsFor(sess), h.server.hostedCatalog().SavedPromptRows, h.server.hostedCatalog().SavedTurnFinal, nil)
	if h.runtimes == nil {
		h.runtimes = map[*sessions.Session]*providerRuntimeFixture{}
	}
	h.runtimes[sess] = &providerRuntimeFixture{session: sess, projection: p}
	t.Cleanup(sess.Close)
	return sess
}
