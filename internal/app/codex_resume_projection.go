package app

import (
	"encoding/json"

	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type codexResumeProjectionTurn struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	StartedAt   *int64          `json:"startedAt"`
	CompletedAt *int64          `json:"completedAt"`
	DurationMS  *int64          `json:"durationMs"`
	Error       json.RawMessage `json:"error"`
	Items       []any           `json:"items"`
}

// seedCodexResume reconstructs the bounded turn tail from the one response
// Nano must already receive to repair its app-server subscription. The reply
// and live notifications can cross on the wire, so this is an ordered merge:
// resume history supplies the prefix while already-live fields win. Rows come
// from the saved file, never from this response.
func seedCodexResume(p resumeProjection, resp *codexResumeResponse) {
	if p == nil || p.providerName() != "codex" {
		return
	}
	turns, valid := resp.projectionTurns()
	if !valid || len(turns) == 0 {
		return
	}
	start := len(turns)
	terminal := 0
	for index := len(turns) - 1; index >= 0; index-- {
		turn := turns[index]
		if codexResumeTurnTerminal(turn.Status) {
			if terminal == liveProjectionTurnTail {
				break
			}
			terminal++
		}
		start = index
	}
	selected := turns[start:]
	parsedItems := resp.items(start)
	seedTurns := make([]*liveProjectionTurn, 0, len(selected))
	for _, turn := range selected {
		projectedTurn := &liveProjectionTurn{
			ID:            turn.ID,
			Status:        normalizeCodexResumeTurnStatus(turn.Status),
			StartedAtMs:   codexTimestampMS(turn.StartedAt),
			CompletedAtMs: codexTimestampMS(turn.CompletedAt),
		}
		if turn.DurationMS != nil && *turn.DurationMS > 0 {
			projectedTurn.DurationMS = *turn.DurationMS
		}
		if len(turn.Error) > 0 && string(turn.Error) != "null" {
			projectedTurn.Error = append(json.RawMessage(nil), turn.Error...)
		}
		fallbackFinal := ""
		for _, rawItem := range turn.Items {
			raw, _ := rawItem.(map[string]any)
			if len(raw) == 0 {
				continue
			}
			parsed, found := parsedItems[[2]string{turn.ID, codexObjectString(raw, "id")}]
			if !found || parsed.Type != model.ItemAgentMessage {
				continue
			}
			fallbackFinal = threadvalues.FirstNonEmpty(parsed.NativeItemID, parsed.ID)
			if parsed.Phase == "final_answer" {
				projectedTurn.FinalItemID = fallbackFinal
			}
		}
		if projectedTurn.Status == "completed" && projectedTurn.FinalItemID == "" {
			projectedTurn.FinalItemID = fallbackFinal
		} else if projectedTurn.Status != "completed" {
			projectedTurn.FinalItemID = ""
		}
		seedTurns = append(seedTurns, projectedTurn)
	}

	p.seedResumeTurns(codexObjectString(resp.thread, "id"), seedTurns)
}

// The RPC client supplies a result body after removing the transport envelope.
// Rewrap only the selected native tail with a caller-local correlation label.
// No request is fabricated: ParseWire diagnoses an uncorrelated snapshot, and
// receives no item/completed notifications or invented native clocks.
func (r *codexResumeResponse) items(start int) map[[2]string]model.Item {
	if start < 0 || start > len(r.turns) {
		return nil
	}
	end := len(r.turns)
	if r.parsedFrom >= 0 {
		if start >= r.parsedFrom {
			return r.parsedItems
		}
		end = r.parsedFrom
	}
	result := copyCodexFields(r.fields)
	thread := copyCodexFields(r.thread)
	thread["turns"] = r.turns[start:end]
	result["thread"] = thread
	frame, err := json.Marshal(map[string]any{"id": "consumer-resume", "result": result})
	if err != nil {
		return nil
	}
	_, batch, err := codex.ParseWire(codex.State{}, model.Input{Bytes: frame,
		Source: model.SourceRef{Label: "resume-result", Incarnation: "selected-tail", Direction: "recv"}})
	if err != nil {
		return nil
	}
	items := make(map[[2]string]model.Item)
	for _, change := range batch.Changes {
		item := change.Item
		if item != nil && item.NativeItemID != "" && item.Scope.NativeTurnID != nil {
			items[[2]string{*item.Scope.NativeTurnID, item.NativeItemID}] = *item
		}
	}
	if r.parsedItems == nil {
		r.parsedItems = items
	} else {
		for key, item := range items {
			r.parsedItems[key] = item
		}
	}
	r.parsedFrom = start
	return r.parsedItems
}

func codexResumeTurnTerminal(status string) bool {
	return status != "" && status != codexTurnInProgressStatus && status != "in_progress"
}

func normalizeCodexResumeTurnStatus(status string) string {
	if status == codexTurnInProgressStatus {
		return "in_progress"
	}
	return status
}

func codexTimestampMS(value *int64) int64 {
	if value == nil || *value <= 0 {
		return 0
	}
	if *value > residualMsThreshold {
		return *value
	}
	return *value * msPerSecond
}
