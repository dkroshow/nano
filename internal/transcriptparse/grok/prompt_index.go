package grok

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func nativePromptIndex(update decodedUpdate) (int64, bool) {
	if update.Kind != kindUserMessage {
		return 0, false
	}
	var body struct {
		Meta struct {
			Index json.RawMessage `json:"promptIndex"`
		} `json:"_meta"`
	}
	if json.Unmarshal(update.Payload, &body) != nil || len(body.Meta.Index) == 0 {
		return 0, false
	}
	var index *int64
	if json.Unmarshal(body.Meta.Index, &index) != nil || index == nil || *index < 0 {
		return 0, false
	}
	return *index, true
}

// Different native prompt indices prove a boundary even before promptId arrives.
// Archive the earlier unknown turn without declaring it complete or inventing
// a native ID. Only the newest unbound prompt can take the later promptId.
func (state *State) separateIndexedPrompt(scope model.Scope, index int64, update decodedUpdate) {
	if scope.NativeTurnID != nil || update.IsReplay {
		return
	}
	if update.EventID != "" && state.Events[scopedKey(scope, "event", update.EventID)] != "" {
		return
	}
	group := scopedKey(scope, "turn", "")
	previous, known := state.UnboundPromptIndices[group]
	turn, exists := state.Turns[group]
	if known && previous != index && exists {
		archived := scopedKey(scope, "unbound-turn", turn.Key)
		state.Turns[archived] = turn
		delete(state.Turns, group)
		if item := state.Segments[group]; item != "" {
			state.Segments[archived] = item
			delete(state.Segments, group)
		}
		if item := state.LastAssistant[group]; item != "" {
			state.LastAssistant[archived] = item
			delete(state.LastAssistant, group)
		}
	}
	state.UnboundPromptIndices[group] = index
}

// Exact event/call evidence can point into an archived unknown turn. The source
// key is parser identity; it never becomes a provider-native prompt ID.
func (state *State) turnGroup(scope model.Scope) string {
	if scope.TurnKey != "" {
		for group, turn := range state.Turns {
			if turn.Key == scope.TurnKey && sameSession(scope, turn.Scope) {
				return group
			}
		}
	}
	return scopedKey(scope, "turn", "")
}
