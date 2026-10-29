package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Boundary: operation completion and its HTTP read, backed by a parsed saved
// file. Wire, joined-turn and resume completion must all record saved text,
// even when the wire candidate is different. The prompt stays unconfirmed:
// settlement may lag the file the answer comes from. The projection rows take
// the final id from the live frames: a subagent's answer and commentary after
// the final answer must not stand for the parent's, or the saved lookup
// refuses the parent's own saved answer.
func TestOperationResultRecordsSavedFinalInsteadOfWireOrOwnerText(t *testing.T) {
	for _, test := range []struct{ provider, path, item string }{
		{"codex", "wire", "new-answer"}, {"codex", "joined", "new-answer"}, {"codex", "resume", "new-answer"},
		{"claude", "wire", "new-message:0"}, {"grok", "wire", "new-agent-event"},
	} {
		t.Run(test.provider+"/"+test.path, func(t *testing.T) {
			h := newSettlementHarness(t, test.provider)
			op, store := h.savedResultOperation(t, "new", h.server.savedOperationResult)
			h.server.agentExecutions = store
			turn, item := h.fixture.turns["new"], test.item
			var err error
			if err := store.MarkTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
				t.Fatal(err)
			}
			{
				if err := store.ObserveCandidate(test.provider, h.fixture.native, turn, item, "final_answer", "wire prefix"); err != nil {
					t.Fatal(err)
				}
			}
			switch test.path {
			case "wire":
				if test.provider == "codex" {
					sess := h.attach("codex", nil, func(cfg *sessionConfig) { cfg.AgentExecutions = store })
					frame, _ := json.Marshal(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": h.fixture.native, "turn": map[string]any{"id": turn, "status": "completed"}}})
					h.runtimes[sess].emitAndWait(t, frame)
				} else {
					err = store.MarkTurnTerminal(test.provider, h.fixture.native, turn, agentTurnCompleted, nil, "", "")
				}
			case "joined":
				err = store.FinishJoinedTurn(op.ID, turn, agentTurnCompleted, nil, "incomplete owner", item, agentResultMeta{})
			case "resume":
				marks := []agentTerminalTurn{{ProviderTurnID: turn, Status: agentTurnCompleted, Response: "resume prefix", ItemID: item}}
				matches := store.ResumedTurnMatches(op.HostedThreadID, "codex", h.fixture.native, marks)
				_, err = store.MarkResumedTurnsTerminal(op.HostedThreadID, "codex", h.fixture.native, marks, matches)
			}
			if err != nil {
				t.Fatal(err)
			}
			waitForCatalogWithin(t, 3*time.Second, func() bool { rec, _ := store.Turn(op.ID); return threadvalues.TurnTerminal(rec.Status) })
			request := httptest.NewRequest("GET", "/api/v1/turn-operations/"+op.ID, nil)
			request.SetPathValue("operationId", op.ID)
			response := httptest.NewRecorder()
			h.server.handleTurnOperationRead(response, request)
			var result agentTurnView
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Status != agentTurnCompleted || result.FinalResponse != "new answer" || (test.provider != "grok" && result.FinalItemID != item || test.provider == "grok" && result.FinalItemID == "") {
				t.Fatalf("operation used unsaved output: %+v", result)
			}
			recorded, _ := store.Turn(op.ID)
			if recorded.FinalResponse != result.FinalResponse {
				t.Fatal("HTTP answer differs from recorded saved answer")
			}
		})
	}
}

// Boundary: a polled operation while its native final row is not indexed yet.
// Completion starts a bounded read, exposes no wire answer, then records the
// exact saved answer when a subsequent file commit arrives.
func TestOperationResultWaitsForTheSavedFinalMessage(t *testing.T) {
	h := newSettlementHarness(t, "codex")
	seedAcceptedTransactionalTurn(t, h.prompts, "hosted", model.ProviderCodex, h.fixture.native, "late", "turn-late", "late prompt")
	read := make(chan error, 1)
	var first atomic.Bool
	op, store := h.savedResultOperation(t, "late", func(ctx context.Context, rec agentTurnRecord) (string, string, error) {
		text, id, err := h.server.savedOperationResult(ctx, rec)
		if first.CompareAndSwap(false, true) {
			read <- err
		}
		return text, id, err
	})
	if err := store.MarkTurnDelivery(op.ID, "turn-late", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTurnTerminal("codex", h.fixture.native, "turn-late", agentTurnCompleted, nil, "unsaved wire", "late-answer"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("the saved answer must initially be absent")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("saved read did not start")
	}
	pending, _ := store.Turn(op.ID)
	if pending.Status != agentTurnAwaitingSaved || pending.FinalResponse != "" || threadvalues.TurnTerminal(pending.Status) {
		t.Fatalf("premature result: %+v", pending)
	}
	h.appendSaved(t, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-late"}}
{"type":"event_msg","payload":{"type":"user_message","message":"late prompt"}}
{"type":"response_item","payload":{"type":"message","id":"late-answer","role":"assistant","content":[{"type":"output_text","text":"complete saved answer"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-late"}}
`)
	waitForCatalogWithin(t, 5*time.Second, func() bool { rec, _ := store.Turn(op.ID); return threadvalues.TurnTerminal(rec.Status) })
	result, _ := store.Turn(op.ID)
	if result.Status != agentTurnCompleted || result.FinalResponse != "complete saved answer" || result.FinalItemID != "late-answer" {
		t.Fatalf("late saved result: %+v", result)
	}
}

// Boundary: an awaiting-saved operation reloaded from the durable control
// store after its wait ended, read against a real catalog. Reattachment must
// not reset the deadline, so it ends at once instead of hanging, and its one
// last lookup records only the exact saved answer of the prompt's own turn.
func TestOperationResultAfterItsWaitTakesOnlyTheExactSavedAnswer(t *testing.T) {
	for _, test := range []struct {
		name, prompt, turn, item string
		expired, shares          bool
		status, response         string
	}{
		// A delivered prompt settlement never confirmed still names its answer.
		{"expired prompt whose answer is saved", "new", "turn-new", "new-answer", true, false, agentTurnCompleted, "new answer"},
		// Settlement reads every user row of a crowded turn; so does the answer.
		{"steered prompt in a crowded turn", "steered", "turn-crowded", "crowded-answer", false, true, agentTurnCompleted, "crowded answer"},
		// Bound only by turn-shared and never saved there: the turn's one user
		// row is the steered prompt's, claimed by its own event.
		{"turn-only prompt beside a steered prompt's saved row", "rejected", "turn-shared", "shared-answer", true, true, agentTurnFailed, ""},
		{"wrong final item", "new", "turn-new", "old-answer", false, false, agentTurnFailed, ""},
		// The prompt's binding names turn-new; turn-old's saved answer is not its own.
		{"operation bound to another turn", "new", "turn-old", "old-answer", false, false, agentTurnFailed, ""},
		{"unflushed completion", "running", "turn-running", "running-answer", false, false, agentTurnFailed, ""},
		{"binding matches no saved row", "unproven", "turn-absent", "unproven-answer", false, false, agentTurnFailed, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newSettlementHarness(t, "codex")
			if test.shares {
				// One user row per steered prompt; a crowded turn has 33.
				prompts, answer := 1, "shared"
				if test.turn == "turn-crowded" {
					prompts, answer = 33, "crowded"
				}
				var file strings.Builder
				fmt.Fprintf(&file, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%q}}\n", test.turn)
				for i := 0; i < prompts; i++ {
					fmt.Fprintf(&file, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"steered prompt %d\",\"client_id\":\"steered-event-%d\"}}\n", i, i)
				}
				fmt.Fprintf(&file, "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":%q,\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}}\n", answer+"-answer", answer+" answer")
				fmt.Fprintf(&file, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":%q}}\n", test.turn)
				h.appendSaved(t, file.String())
				if test.prompt != "steered" {
					seedAcceptedTransactionalTurn(t, h.prompts, "hosted", model.ProviderCodex, h.fixture.native, test.prompt, test.turn, "rejected prompt")
				}
				h.seedSteeredPrompt(t, "steered", test.turn, "steered-event-0")
				waitForCatalogWithin(t, 5*time.Second, func() bool {
					rows, err := h.server.hostedCatalog().SavedPromptRows(context.Background(), h.fixture.native, history.SavedUserKeys{NativeTurnIDs: []string{test.turn}})
					return err == nil && len(rows) == prompts && rows[0].TurnStatus == history.TurnCompleted
				})
			}
			op, store := h.savedResultOperation(t, test.prompt, h.server.savedOperationResult)
			if test.expired {
				if _, expired, err := h.prompts.Expire(context.Background(), test.prompt, time.Now().Add(time.Hour)); err != nil || !expired {
					t.Fatalf("expire prompt: expired=%v err=%v", expired, err)
				}
			}
			// Written as the stopped server left it: no transition, so no job.
			rec, _ := store.Turn(op.ID)
			rec.Status, rec.ProviderTurnID, rec.FinalItemID = agentTurnAwaitingSaved, test.turn, test.item
			rec.UpdatedAt = time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano)
			rec.Revision++
			err := fixtureWriteTurn(store, rec)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := newControlAgentExecutionStore(store.Control(), "next-run")
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.BindSession(op.HostedThreadID, "next-session", h.fixture.native, "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			restarted.UseSavedResults(h.server.savedOperationResult, nil)
			waitForCatalogWithin(t, 3*time.Second, func() bool { rec, _ := restarted.Turn(op.ID); return threadvalues.TurnTerminal(rec.Status) })
			result, _ := restarted.Turn(op.ID)
			if result.Status != test.status || result.FinalResponse != test.response || result.FinalItemID != test.item ||
				test.status == agentTurnFailed && (result.Error == nil || result.Error.Code != "result_not_saved") {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}

// savedResultOperation starts an operation in the harness thread whose prompt
// is the seeded action, with operation results read through read.
func (h *settlementHarness) savedResultOperation(t *testing.T, action string, read func(context.Context, agentTurnRecord) (string, string, error)) (agentTurnRecord, *agentExecutionStore) {
	t.Helper()
	h.server.turns = h.prompts
	store := newTestAgentExecutionStore(t, "result-run")
	store.UseSavedResults(read, nil)
	hosted, err := fixtureEnsureSession(store, "hosted", string(h.fixture.root.Provider), presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(hosted.ID, "session", h.fixture.native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	op, _, err := fixtureBeginTurn(store, hosted.ID, action, "prompt-hash")
	if err != nil {
		t.Fatal(err)
	}
	// Bind the operation to the seeded prompt. Commits never rewrite an
	// operation's transport action, so set it where a restarted store reads it.
	if _, err := store.Control().CanonicalDatabase().Exec(`UPDATE hosted_turn_operations SET transport_action_id=? WHERE operation_id=?`, action, op.ID); err != nil {
		t.Fatal(err)
	}
	store, err = newControlAgentExecutionStore(store.Control(), "result-run")
	if err != nil {
		t.Fatal(err)
	}
	store.UseSavedResults(read, nil)
	rec, _ := store.Turn(op.ID)
	return rec, store
}

// seedSteeredPrompt accepts a prompt bound by its own client event into a
// provider turn, as Codex binds one steered into a running turn.
func (h *settlementHarness) seedSteeredPrompt(t *testing.T, action, turn, event string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := h.prompts.Submit(ctx, turns.Submission{
		HostedThreadID: "hosted", Provider: model.ProviderCodex, NativeThreadID: h.fixture.native, ActionID: action,
		ProviderText: action, PresentationText: action, TextHash: sendTextHash(action), RequestHash: sendTextHash(action), Origin: turns.HumanOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	token := "fixture-claim-" + action
	if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim %s: claimed=%v err=%v", action, claimed, err)
	}
	native := h.fixture.native
	binding := turns.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}, EventID: event}
	if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: action, ClaimToken: token, AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}
}
