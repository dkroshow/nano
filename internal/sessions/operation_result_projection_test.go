package sessions

import (
	"encoding/json"
	"testing"
)

// The app sibling checks the real saved-result store. These producer cases
// retain the receiver's projection fallback and Claude result-to-turn binding.
func TestOperationResultRecordsSavedFinalInsteadOfWireOrOwnerText(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider+"/projection", func(t *testing.T) {
			recorder := &projectionTerminalRecorder{}
			projection := &completedResultProjection{deliveryTestProjection: newDeliveryTestProjection(provider)}
			projection.resultTurn = "provider-turn"
			sess := New(Config{ID: "projection-result", Provider: provider, HostedThreadID: "hosted", Projection: projection, AgentExecutions: recorder})
			sess.ReserveNativeID("native-thread")
			if provider == "codex" {
				sess.reconcileAgentOperations()
			} else {
				sess.observeClaudeAgentResult(json.RawMessage(`{"type":"result","subtype":"success","is_error":false,"result":"wire result"}`))
			}
			if len(recorder.calls) != 1 {
				t.Fatalf("terminal calls=%+v", recorder.calls)
			}
			call := recorder.calls[0]
			wantText := ""
			if provider == "claude" {
				wantText = "wire result"
			}
			if call.provider != provider || call.native != "native-thread" || call.turn != "provider-turn" || call.status != agentTurnCompleted || call.item != "final-item" || call.response != wantText {
				t.Fatalf("terminal producer lost its exact final identity: %+v", call)
			}
		})
	}
}

type completedResultProjection struct{ *deliveryTestProjection }

func (*completedResultProjection) TurnList() []liveProjectionTurn {
	return []liveProjectionTurn{{ID: "provider-turn", Status: agentTurnCompleted, FinalItemID: "final-item"}}
}
func (*completedResultProjection) FinalAgentItemID(string) string { return "final-item" }

type projectionTerminal struct{ provider, native, turn, status, response, item string }
type projectionTerminalRecorder struct {
	Executions
	calls []projectionTerminal
}

func (*projectionTerminalRecorder) ReadsSavedResults() bool { return true }
func (r *projectionTerminalRecorder) MarkTurnTerminal(provider, native, turn, status string, _ *agentExecutionError, response, item string, _ ...agentResultMeta) error {
	r.calls = append(r.calls, projectionTerminal{provider, native, turn, status, response, item})
	return nil
}
