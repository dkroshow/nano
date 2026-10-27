package app

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Grok's saved segment carries only its newest chunk's eventId. The live
// entry keeps its first eventId so appends never replace the browser's entry.
type grokLiveUpdate struct {
	eventID, turn, kind, text, stopReason string
	chunkID, response                     int64
}

type grokLiveResponse struct {
	turn              string
	response, chunkID int64
	segmentID         string
	segmentOpen       bool // also remembers a segment suppressed after a restart
}

// The parser preserves the exact envelope metadata separately from the
// content observation. Pair them by eventId; replay never starts live text.
func (l *liveTextLoop) observeGrokBatch(batch model.Batch) {
	var events []liveTextEvent
	for _, envelope := range batch.Observations {
		var meta struct {
			Meta struct {
				EventID  string `json:"eventId"`
				PromptID string `json:"promptId"`
				ChunkID  int64  `json:"chunkId"`
				Response int64  `json:"streamStartMs"`
				Replay   bool   `json:"isReplay"`
			} `json:"_meta"`
		}
		if json.Unmarshal(envelope.Detail, &meta) != nil || meta.Meta.EventID == "" || meta.Meta.Replay {
			continue
		}
		for _, obs := range batch.Observations {
			if obs.NativeEventID != meta.Meta.EventID || obs.Scope.ParentNativeID != nil || obs.Scope.NativeParentCallID != nil {
				continue
			}
			var update struct {
				Kind       string `json:"sessionUpdate"`
				StopReason string `json:"stop_reason"`
				Content    struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(obs.Detail, &update) != nil {
				continue
			}
			switch update.Kind {
			case "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update", "turn_completed":
			default:
				continue
			}
			turn := meta.Meta.PromptID
			if obs.Scope.NativeTurnID != nil {
				turn = *obs.Scope.NativeTurnID
			}
			if turn == "" {
				continue
			}
			u := grokLiveUpdate{eventID: meta.Meta.EventID, turn: turn, kind: update.Kind, text: update.Content.Text, stopReason: update.StopReason, chunkID: meta.Meta.ChunkID, response: meta.Meta.Response}
			events = append(events, liveTextEvent{grok: &u})
			break
		}
	}
	if len(events) > 0 {
		l.post(func(in *liveTextInbox) { in.events = append(in.events, events...) })
	}
}

func (l *liveTextLoop) endGrokSegment(finished bool) bool {
	entry := l.byID[l.grok.segmentID]
	l.grok.segmentID, l.grok.segmentOpen = "", false
	if entry == nil || entry.endOrder != 0 {
		return false
	}
	l.end(entry)
	if finished {
		entry.finished = true
		l.awaitSave(entry.key, entry.turn)
	}
	return true // check the final key even if the file applied before it arrived
}

func (l *liveTextLoop) applyGrokUpdate(u grokLiveUpdate) bool {
	if u.kind == "turn_completed" {
		if l.grok.turn != u.turn {
			return false
		}
		return l.endGrokSegment(u.stopReason == "end_turn")
	}
	if u.kind == "tool_call" || u.kind == "tool_call_update" {
		if l.grok.turn != u.turn {
			return false
		}
		return l.endGrokSegment(true)
	}
	if u.chunkID <= 0 {
		return false
	}
	changed := false
	if l.grok.turn != u.turn || l.grok.response != u.response {
		changed = l.endGrokSegment(true)
		l.grok = grokLiveResponse{turn: u.turn, response: u.response}
	}
	// A repeated or older chunk is transport replay, not another append.
	if u.chunkID <= l.grok.chunkID {
		return changed
	}
	previous := l.grok.chunkID
	l.grok.chunkID = u.chunkID
	if u.kind == "agent_thought_chunk" {
		return l.endGrokSegment(true) || changed
	}
	if !l.grok.segmentOpen {
		l.grok.segmentOpen = true
		if u.chunkID != 1 && previous != u.chunkID-1 {
			return changed
		}
		id := "reply:" + u.eventID
		l.grok.segmentID = id
		if !l.retired[id] {
			entry := &liveTextEntry{ID: id, Kind: "reply", key: u.eventID, turn: u.turn, order: l.next(), delivered: true}
			l.entries = append(l.entries, entry)
			l.byID[id] = entry
			changed = true
		}
	}
	if entry := l.byID[l.grok.segmentID]; entry != nil {
		entry.Text += u.text
		entry.key = u.eventID
	}
	return changed
}
