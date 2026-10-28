package app

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/threads"
)

// seedResumeTurns merges the provider's normalized prefix with already-live turns.
func (p *liveProjection) seedResumeTurns(id string, seedTurns []*liveProjectionTurn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id != "" {
		p.nativeThread = threads.TruncateText(id, 256)
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
