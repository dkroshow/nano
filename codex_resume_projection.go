package main

import (
	"encoding/json"

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
func (p *liveProjection) seedCodexResume(resp *codexResumeResponse) {
	if p == nil || p.provider != "codex" {
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
			fallbackFinal = firstNonEmpty(parsed.NativeItemID, parsed.ID)
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

	p.mu.Lock()
	defer p.mu.Unlock()
	if id := codexObjectString(resp.thread, "id"); id != "" {
		p.nativeThread = truncateUTF8Live(id, 256)
	}
	seedTurnOrder := make([]string, 0, len(seedTurns))
	for _, turn := range seedTurns {
		if turn == nil || turn.ID == "" {
			continue
		}
		seedTurnOrder = append(seedTurnOrder, turn.ID)
		if current := p.turns[turn.ID]; current != nil {
			p.turns[turn.ID] = mergeResumeTurn(turn, current)
		} else {
			p.turns[turn.ID] = cloneTurn(turn)
		}
	}
	p.turnOrder = orderedIDUnion(seedTurnOrder, p.turnOrder)
	p.trimTurnsLocked()
	p.appendLocked(liveProjectionUpdate{Op: projectionSnapshot, Snapshot: &liveProjectionSnapshot{Revision: p.revision + 1}})
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

func mergeResumeTurn(baseline, live *liveProjectionTurn) *liveProjectionTurn {
	if baseline == nil {
		return cloneTurn(live)
	}
	if live == nil {
		return cloneTurn(baseline)
	}
	out := cloneTurn(baseline)
	if live.Status != "" {
		out.Status = live.Status
	}
	if live.StartedAtMs > 0 {
		out.StartedAtMs = live.StartedAtMs
	}
	if live.CompletedAtMs > 0 {
		out.CompletedAtMs = live.CompletedAtMs
	}
	if live.DurationMS > 0 {
		out.DurationMS = live.DurationMS
	}
	if live.FinalItemID != "" {
		out.FinalItemID = live.FinalItemID
	}
	if len(live.Error) > 0 {
		out.Error = append(json.RawMessage(nil), live.Error...)
	}
	return out
}

func orderedIDUnion(left, right []string) []string {
	left = nonEmptyUnique(left)
	right = nonEmptyUnique(right)
	lcs := make([][]int, len(left)+1)
	for index := range lcs {
		lcs[index] = make([]int, len(right)+1)
	}
	for i := len(left) - 1; i >= 0; i-- {
		for j := len(right) - 1; j >= 0; j-- {
			if left[i] == right[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	out := make([]string, 0, len(left)+len(right))
	for i, j := 0, 0; i < len(left) || j < len(right); {
		switch {
		case i == len(left):
			return nonEmptyUnique(append(out, right[j:]...))
		case j == len(right):
			return nonEmptyUnique(append(out, left[i:]...))
		case left[i] == right[j]:
			out = append(out, left[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, left[i])
			i++
		default:
			out = append(out, right[j])
			j++
		}
	}
	return nonEmptyUnique(out)
}

func nonEmptyUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
