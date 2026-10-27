package grok

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func rpcScalarID(raw json.RawMessage) bool {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return false
	}
	switch value.(type) {
	case string, json.Number:
		return true
	}
	return false
}

func rpcDigest(row map[string]json.RawMessage) string {
	var native any
	decoder := json.NewDecoder(bytes.NewReader(mustJSON(row)))
	decoder.UseNumber()
	_ = decoder.Decode(&native)
	return fmt.Sprintf("%x", sha256.Sum256(mustJSON(native)))
}

// Native event equality covers the decoded update's content and classification.
// Transport clocks and the replay marker are separate observations of delivery.
func updateEventDigest(update decodedUpdate) string {
	return rpcDigest(map[string]json.RawMessage{
		"kind": mustJSON(update.Kind), "payload": update.Payload,
		"status": mustJSON(update.Status), "callId": mustJSON(update.CallID),
		"toolName": mustJSON(update.ToolName), "toolKind": mustJSON(update.ToolKind),
		"text": mustJSON(update.Text), "title": mustJSON(update.Title),
		"usage": update.Usage,
	})
}

func rpcReplay(batch *model.Batch, obs model.Observation, scope model.Scope, prior string) {
	relation := model.Relation{Kind: model.RelationReplayOf, Scope: scope, FromObservation: obs.ID, ToObservation: prior, ObservationRefs: []string{prior, obs.ID}}
	batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
}

func (state *State) emitControl(batch *model.Batch, obs model.Observation, detail map[string]any, disposition model.Disposition) model.Disposition {
	obs.Detail = mustJSON(detail)
	for i := range obs.Times {
		obs.Times[i].Scope = obs.Scope
	}
	if item, found := state.Items[obs.Scope.ItemID]; found {
		if len(item.Observations) == 0 {
			item.Detail = obs.Detail
		}
		var diagnostics []model.Diagnostic
		item, diagnostics = assemble.Observe(item, obs)
		batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
		state.Items[item.ID] = item
		batch.Changes = append(batch.Changes, model.Change{Item: &item})
		if item.TurnKey != "" {
			group := scopedKey(item.Scope, "turn", "")
			turn := state.Turns[group]
			if turn.StartSourceSeq == 0 {
				turn.StartSourceSeq = item.SourceSeq
			}
			if turn.EndSourceSeq < item.SourceSeq {
				turn.EndSourceSeq = item.SourceSeq
			}
			state.Turns[group] = turn
			batch.Changes = append(batch.Changes, model.Change{Turn: &turn})
		}
	}
	batch.Observations = append(batch.Observations, obs)
	return disposition
}
