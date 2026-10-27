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
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
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
		{"codex", "projection", "new-answer"},
		{"claude", "wire", "new-message:0"}, {"claude", "projection", "new-message:0"}, {"grok", "wire", "new-agent-event"},
	} {
		t.Run(test.provider+"/"+test.path, func(t *testing.T) {
			h := newSettlementHarness(t, test.provider)
			op, store := h.savedResultOperation(t, "new", h.server.savedOperationResult)
			h.server.agentExecutions = store
			turn, item := h.fixture.turns["new"], test.item
			var err error
			if err := store.markTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
				t.Fatal(err)
			}
			if test.path != "projection" {
				if err := store.observeCandidate(test.provider, h.fixture.native, turn, item, "final_answer", "wire prefix"); err != nil {
					t.Fatal(err)
				}
			}
			switch test.path {
			case "projection":
				sess := h.attach(test.provider, nil)
				sess.agentExecutions = store
				at := time.Now()
				if test.provider == "codex" {
					for _, frame := range []string{
						`{"method":"item/completed","params":{"threadId":%[1]q,"turnId":%[2]q,"item":{"id":"new-answer","type":"agentMessage","phase":"final_answer","text":"wire prefix"}}}`,
						`{"method":"item/completed","params":{"threadId":%[1]q,"turnId":%[2]q,"item":{"id":"late-commentary","type":"agentMessage","phase":"commentary","text":"one more note"}}}`,
					} {
						sess.projection.applyWire(protocol.Recv, []byte(fmt.Sprintf(frame, h.fixture.native, turn)), at)
					}
					sess.projection.updateCompletedTurn(turn, agentTurnCompleted, 1000, at.UnixMilli())
					sess.reconcileAgentOperations()
					break
				}
				sess.projection.setClaudeProviderTurnID(turn)
				sess.projection.applyWire(protocol.Send, []byte(fmt.Sprintf(`{"type":"user","uuid":%q,"message":{"role":"user","content":[{"type":"text","text":"new prompt"}]}}`, submissionEventID("hosted", "new"))), at)
				for _, frame := range []string{
					`{"type":"assistant","session_id":%[1]q,"uuid":"new-event","message":{"id":"new-message","role":"assistant","content":[{"type":"text","text":"wire prefix"}],"stop_reason":"end_turn"}}`,
					// A subagent's answer, inside the parent's tool call.
					`{"type":"assistant","parent_tool_use_id":"toolu_child","session_id":%[1]q,"uuid":"child-event","message":{"id":"child-message","role":"assistant","content":[{"type":"text","text":"child answer"}],"stop_reason":"end_turn"}}`,
				} {
					sess.projection.applyWire(protocol.Recv, []byte(fmt.Sprintf(frame, h.fixture.native)), at)
				}
				result := []byte(fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"result":"wire result","session_id":%q,"duration_ms":1000}`, h.fixture.native))
				sess.projection.setClaudeResultTurnID(turn) // as the session loop names it
				sess.projection.applyWire(protocol.Recv, result, at)
				sess.observeClaudeAgentResult(result)
			case "wire":
				if test.provider == "codex" {
					sess := h.attach("codex", nil)
					sess.agentExecutions = store
					frame, _ := json.Marshal(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": h.fixture.native, "turn": map[string]any{"id": turn, "status": "completed"}}})
					sess.observeAgentExecutionWire(protocol.Recv, frame)
				} else {
					err = store.markTurnTerminal(test.provider, h.fixture.native, turn, agentTurnCompleted, nil, "", "")
				}
			case "joined":
				err = store.finishJoinedTurn(op.ID, turn, agentTurnCompleted, nil, "incomplete owner", item, agentResultMeta{})
			case "resume":
				marks := []agentTerminalTurn{{ProviderTurnID: turn, Status: agentTurnCompleted, Response: "resume prefix", ItemID: item}}
				matches := store.resumedTurnMatches(op.HostedThreadID, "codex", h.fixture.native, marks)
				_, err = store.markResumedTurnsTerminal(op.HostedThreadID, "codex", h.fixture.native, marks, matches)
			}
			if err != nil {
				t.Fatal(err)
			}
			waitForCatalogWithin(t, 3*time.Second, func() bool { rec, _ := store.turn(op.ID); return agentTurnTerminal(rec.Status) })
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
			recorded, _ := store.turn(op.ID)
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
	if err := store.markTurnDelivery(op.ID, "turn-late", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.markTurnTerminal("codex", h.fixture.native, "turn-late", agentTurnCompleted, nil, "unsaved wire", "late-answer"); err != nil {
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
	pending, _ := store.turn(op.ID)
	if pending.Status != agentTurnAwaitingSaved || pending.FinalResponse != "" || agentTurnTerminal(pending.Status) {
		t.Fatalf("premature result: %+v", pending)
	}
	h.appendSaved(t, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-late"}}
{"type":"event_msg","payload":{"type":"user_message","message":"late prompt"}}
{"type":"response_item","payload":{"type":"message","id":"late-answer","role":"assistant","content":[{"type":"output_text","text":"complete saved answer"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-late"}}
`)
	waitForCatalogWithin(t, 5*time.Second, func() bool { rec, _ := store.turn(op.ID); return agentTurnTerminal(rec.Status) })
	result, _ := store.turn(op.ID)
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
					rows, err := h.server.savedPromptRows(context.Background(), h.fixture.native, history.SavedUserKeys{NativeTurnIDs: []string{test.turn}})
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
			store.mu.Lock()
			rec := store.turns[op.ID]
			rec.Status, rec.ProviderTurnID, rec.FinalItemID = agentTurnAwaitingSaved, test.turn, test.item
			rec.UpdatedAt = time.Now().Add(-savedOperationResultWait - time.Second).UTC().Format(time.RFC3339Nano)
			rec.Revision++
			store.setTurnLocked(op.ID, &rec)
			err := store.persistAndNotifyLocked()
			store.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := newControlAgentExecutionStore(store.control, "next-run")
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.bindSession(op.HostedThreadID, "next-session", h.fixture.native, "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			restarted.useSavedResults(h.server.savedOperationResult, nil)
			waitForCatalogWithin(t, savedResultLastRead+time.Second, func() bool { rec, _ := restarted.turn(op.ID); return agentTurnTerminal(rec.Status) })
			result, _ := restarted.turn(op.ID)
			if result.Status != test.status || result.FinalResponse != test.response || result.FinalItemID != test.item ||
				test.status == agentTurnFailed && (result.Error == nil || result.Error.Code != "result_not_saved") {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}

// Boundary: a result_not_saved operation reloaded from the control store, then
// the server's catalog commits for its native thread, with no session
// attached. Its exact answer, saved or indexed after the wait, must still
// complete it: found at startup, on a later commit, after a commit that came
// during a read, after a read the catalog cut short, or once a refused write
// succeeds. Another answer never does.
func TestCatalogCommitCompletesAnOperationWhoseAnswerOutlastedItsWait(t *testing.T) {
	type reply struct {
		item string
		err  error
	}
	for _, test := range []struct {
		name    string
		replies []reply // one per read, the last repeating
		commits int     // catalog commits after the startup read
		during  bool    // the first commit arrives while the startup read runs
		refused bool    // the store refuses finishing writes until the commits end
		status  string
		reads   int32
	}{
		{name: "indexed while Nano was down", replies: []reply{{item: "final-answer"}}, status: agentTurnCompleted, reads: 1},
		{name: "committed while detached", replies: []reply{{err: history.ErrSavedTranscriptUnavailable}, {item: "final-answer"}}, commits: 1, status: agentTurnCompleted, reads: 2},
		{name: "commit during a read", replies: []reply{{err: history.ErrSavedTranscriptUnavailable}, {item: "final-answer"}}, commits: 1, during: true, status: agentTurnCompleted, reads: 2},
		// A busy catalog cut the read short; no further commit comes.
		{name: "read cut short", replies: []reply{{err: history.ErrSavedTranscriptUnavailable}, {err: context.DeadlineExceeded}, {item: "final-answer"}}, commits: 1, status: agentTurnCompleted, reads: 3},
		{name: "refused write of a found answer", replies: []reply{{err: history.ErrSavedTranscriptUnavailable}, {item: "final-answer"}, {err: history.ErrSavedTranscriptUnavailable}}, commits: 1, refused: true, status: agentTurnCompleted, reads: 2},
		{name: "another answer saved", replies: []reply{{item: "other-answer"}}, commits: 2, status: agentTurnFailed, reads: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := newTestAgentExecutionStore(t, "result-run")
			hosted, err := first.ensureSession("hosted", "codex", presentationShared)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.bindSession(hosted.ID, "session", "native-thread", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			op, _, err := first.beginTurn(hosted.ID, "action", "prompt-hash")
			if err != nil {
				t.Fatal(err)
			}
			first.mu.Lock()
			rec := first.turns[op.ID]
			rec.Status, rec.ProviderTurnID, rec.FinalItemID = agentTurnFailed, "turn-1", "final-answer"
			rec.Error = &agentExecutionError{Source: "saved", Code: "result_not_saved", Message: "not saved"}
			rec.Revision++
			first.setTurnLocked(op.ID, &rec)
			err = first.persistAndNotifyLocked()
			first.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if test.refused {
				if _, err := first.control.db.Exec(`CREATE TRIGGER refuse_finish BEFORE UPDATE ON hosted_turn_operations
WHEN NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'disk unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			store, err := newControlAgentExecutionStore(first.control, "next-run")
			if err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int32
			inRead, release := make(chan struct{}), make(chan struct{})
			store.useSavedResults(func(context.Context, agentTurnRecord) (string, string, error) {
				n := reads.Add(1)
				if test.during && n == 1 {
					close(inRead)
					<-release
				}
				r := test.replies[min(int(n), len(test.replies))-1]
				if r.err != nil {
					return "", "", r.err
				}
				return "saved answer", r.item, nil
			}, nil)
			srv := &server{agentExecutions: store}
			commit := func() {
				srv.confirmDurableThread(context.Background(), transcriptChange{ThreadID: "catalog-thread", Provider: "codex", NativeThreadID: "native-thread"})
			}
			if test.during {
				<-inRead
				commit()
				close(release)
			}
			for i := 0; i < test.commits; i++ {
				if test.during && i == 0 {
					continue
				}
				// Each commit after the previous read has ended, as the worker serializes them.
				waitForCatalogWithin(t, 3*time.Second, func() bool {
					store.mu.Lock()
					defer store.mu.Unlock()
					job := store.savedResultJobs[op.ID]
					return int(reads.Load()) > i && (job == nil || !job.reading)
				})
				commit()
			}
			if test.refused {
				// The found answer's write keeps retrying; no read replaces it.
				waitForCatalogWithin(t, 3*time.Second, func() bool { return reads.Load() == test.reads })
				time.Sleep(1500 * time.Millisecond)
				if _, err := first.control.db.Exec(`DROP TRIGGER refuse_finish`); err != nil {
					t.Fatal(err)
				}
			}
			waitForCatalogWithin(t, 4*time.Second, func() bool {
				rec, _ := store.turn(op.ID)
				store.mu.Lock()
				defer store.mu.Unlock()
				job := store.savedResultJobs[op.ID]
				return reads.Load() == test.reads && rec.Status == test.status && (job == nil || !job.reading)
			})
			result, _ := store.turn(op.ID)
			if test.status == agentTurnCompleted && (result.FinalResponse != "saved answer" || result.FinalItemID != "final-answer" || result.Error != nil) ||
				test.status == agentTurnFailed && !answerNotSaved(result) {
				t.Fatalf("late result: %+v", result)
			}
		})
	}
}

// Boundary: one pending result's job, between its catalog reader and the
// control store, a second before its deadline. A slow catalog cannot extend
// the wait, and a refused commit retries the answer already found: it never
// reads again and loses that answer when a later read fails after expiry.
func TestOperationResultJobKeepsItsDeadlineAndItsAnswer(t *testing.T) {
	for _, test := range []struct {
		name                    string
		slow, recovers, refused bool
		status, answer          string
	}{
		{name: "slow catalog", slow: true, status: agentTurnFailed},
		// Cut short past the deadline, the failure is provisional: the job reads
		// again until the catalog answers.
		{name: "catalog answers after the wait", slow: true, recovers: true, status: agentTurnCompleted, answer: "saved answer"},
		{name: "refused commit", refused: true, status: agentTurnCompleted, answer: "saved answer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestAgentExecutionStore(t, "result-run")
			hosted, err := store.ensureSession("hosted", "codex", presentationShared)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.bindSession(hosted.ID, "session", "native-thread", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			op, _, err := store.beginTurn(hosted.ID, "action", "prompt-hash")
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			store.mu.Lock()
			rec := store.turns[op.ID]
			rec.Status, rec.ProviderTurnID, rec.FinalItemID = agentTurnAwaitingSaved, "turn-1", "final-answer"
			rec.UpdatedAt = deadline.Add(-savedOperationResultWait).UTC().Format(time.RFC3339Nano)
			rec.Revision++
			store.setTurnLocked(op.ID, &rec)
			err = store.persistAndNotifyLocked()
			store.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if test.refused {
				if _, err := store.control.db.Exec(`CREATE TRIGGER refuse_finish BEFORE UPDATE ON hosted_turn_operations
WHEN NEW.status IN ('completed', 'failed') BEGIN SELECT RAISE(ABORT, 'disk unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			var reads atomic.Int32
			store.useSavedResults(func(ctx context.Context, _ agentTurnRecord) (string, string, error) {
				recovered := test.recovers && time.Now().After(deadline.Add(savedResultLastRead))
				if !recovered && (reads.Add(1) > 1 || test.slow) {
					<-ctx.Done() // the catalog answers nothing in time
					return "", "", ctx.Err()
				}
				return "saved answer", "final-answer", nil
			}, nil)
			if test.refused {
				// Past the deadline, while every finishing write is refused.
				time.Sleep(time.Until(deadline) + savedResultLastRead)
				if rec, _ := store.turn(op.ID); rec.Status != agentTurnAwaitingSaved {
					t.Fatalf("refused commit changed the operation: %+v", rec)
				}
				if _, err := store.control.db.Exec(`DROP TRIGGER refuse_finish`); err != nil {
					t.Fatal(err)
				}
			}
			waitForCatalogWithin(t, time.Until(deadline)+savedResultLastRead+2*time.Second, func() bool { rec, _ := store.turn(op.ID); return rec.Status == test.status })
			result, _ := store.turn(op.ID)
			if result.Status != test.status || result.FinalResponse != test.answer || result.FinalItemID != "final-answer" {
				t.Fatalf("result: %+v", result)
			}
			if test.refused && reads.Load() != 1 {
				t.Fatalf("found answer was read again %d times", reads.Load()-1)
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
	store.useSavedResults(read, nil)
	hosted, err := store.ensureSession("hosted", string(h.fixture.root.Provider), presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindSession(hosted.ID, "session", h.fixture.native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	op, _, err := store.beginTurn(hosted.ID, action, "prompt-hash")
	if err != nil {
		t.Fatal(err)
	}
	// Bind the operation to the seeded prompt. Commits never rewrite an
	// operation's transport action, so set it where a restarted store reads it.
	if _, err := store.control.db.Exec(`UPDATE hosted_turn_operations SET transport_action_id=? WHERE operation_id=?`, action, op.ID); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	rec := store.turns[op.ID]
	rec.TransportActionID = action
	store.setTurnLocked(op.ID, &rec)
	store.mu.Unlock()
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
