package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type rowEvent struct {
	Digest         string `json:"digest"`
	ObservationRef string `json:"observationRef"`
	TurnKey        string `json:"turnKey,omitempty"`
	ItemID         string `json:"itemId,omitempty"`
}

// Associate the receipt only after decoding establishes its turn. A replay
// keeps its first association, even if another prompt is now active.
func (state *State) rememberRowTurn(base model.Observation, observations []model.Observation) {
	key := scopedKey(base.Scope, "event", base.NativeEventID)
	receipt, exists := state.RowEvents[key]
	if !exists || receipt.ObservationRef != base.ID+"/receipt" {
		return
	}
	receipt.TurnKey = state.ActiveTurns[threadKey(base.Scope)]
	for _, obs := range observations {
		if obs.NativeEventID == base.NativeEventID && obs.Scope.ItemID != "" {
			receipt.ItemID = obs.Scope.ItemID
		}
		if obs.NativeEventID == base.NativeEventID && obs.Scope.TurnKey != "" {
			receipt.TurnKey = obs.Scope.TurnKey
			break
		}
	}
	if receipt.TurnKey != "" {
		receipt.ItemID = ""
	}
	state.RowEvents[key] = receipt
}

func rowDigest(row map[string]json.RawMessage) string {
	fields := copyMap(row)
	delete(fields, "timestamp")
	if kind := rawString(row, "type"); kind == "user" || kind == "assistant" {
		// A resumed row can carry a new prompt ID or secondary file snapshot
		// while preserving its UUID and actual message. Hash the fields that
		// define the message facet; retain the changing envelope separately.
		fields = make(map[string]json.RawMessage)
		for _, key := range []string{"type", "message", "apiBlockIndex", "isSidechain", "isMeta", "isCompactSummary", "isApiErrorMessage", "origin"} {
			if value, present := row[key]; present {
				fields[key] = value
			}
		}
		result := rawObject(row, "toolUseResult")
		for _, key := range []string{"answers", "questions"} {
			if value, present := result[key]; present {
				fields["toolUseResult."+key] = value
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(mustJSON(fields)))
	decoder.UseNumber()
	var value any
	_ = decoder.Decode(&value)
	digest := sha256.Sum256(mustJSON(value))
	return fmt.Sprintf("%x", digest)
}

func replayableRow(id, kind string) bool {
	return id != "" && (kind == "user" || kind == "assistant" || kind == "result" || kind == "system")
}

func (state *State) rowReplay(batch *model.Batch, base model.Observation, row map[string]json.RawMessage) (bool, model.Disposition) {
	kind := rawString(row, "type")
	if !replayableRow(base.NativeEventID, kind) {
		return false, ""
	}
	key := scopedKey(base.Scope, "event", base.NativeEventID)
	receipt := base
	receipt.ID += "/receipt"
	receipt.Detail = mustJSON(map[string]string{"type": kind, "subtype": rawString(row, "subtype")})
	receipt.Times = append([]model.TimeObservation(nil), base.Times...)
	for i := range receipt.Times {
		receipt.Times[i].ObservationRef = receipt.ID
	}
	batch.Observations = append(batch.Observations, receipt)
	digest := base.NativeEventDigest
	earlier, seen := state.RowEvents[key]
	if !seen {
		state.RowEvents[key] = rowEvent{Digest: digest, ObservationRef: receipt.ID}
		return false, ""
	}
	if digest != earlier.Digest {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("native_event_conflict", base.Source))
		return true, model.RecordUnknown
	}
	relation := model.Relation{Kind: model.RelationReplayOf, Scope: base.Scope, FromObservation: receipt.ID, ToObservation: earlier.ObservationRef, ObservationRefs: []string{earlier.ObservationRef, receipt.ID}}
	batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
	envelopeRef := ""
	if kind == "user" || kind == "assistant" {
		envelope := state.messageEnvelope(batch, base, model.Turn{}, row, rawObject(row, "message"))
		envelopeRef = envelope.ID
	}
	rowKey := scopedKey(base.Scope, "row", base.NativeEventID)
	for index, id := range state.Rows[rowKey] {
		item := state.Items[id]
		for _, prior := range item.Observations {
			if prior.NativeEventID != base.NativeEventID {
				continue
			}
			observation := prior
			observation.ID = sourceKey(base.Source, fmt.Sprintf("block:%d", index))
			observation.Source = base.Source
			observation.Source.BlockPath = prior.Source.BlockPath
			observation.Scope = item.Scope
			observation.Times = append([]model.TimeObservation(nil), base.Times...)
			for i := range observation.Times {
				observation.Times[i].Scope = item.Scope
				observation.Times[i].ObservationRef = observation.ID
			}
			next, diagnostics := assemble.Observe(item, observation)
			if envelopeRef != "" {
				next = envelopeAliases(next, row, envelopeRef)
			}
			state.Items[id] = next
			batch.Observations = append(batch.Observations, observation)
			batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
			batch.Changes = append(batch.Changes, model.Change{Item: &next})
			break
		}
	}
	if kind == "system" {
		return true, model.RecordNonContent
	}
	return true, model.RecordSemantic
}
