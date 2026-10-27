package claude

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func journalRow(row map[string]json.RawMessage) bool {
	kind := rawString(row, "type")
	return (kind == "started" || kind == "result") && rawString(row, "key") != "" && rawString(row, "agentId") != "" && row["subtype"] == nil && row["is_error"] == nil
}

func (state *State) metadata(batch *model.Batch, obs model.Observation, row map[string]json.RawMessage) model.Disposition {
	obs.Detail = mustJSON(row)
	if journalRow(row) {
		// Journal task identity is not a Claude conversation session or turn.
		obs.Scope = model.Scope{Kind: model.ScopeConnection, Provider: obs.Scope.Provider, Source: obs.Scope.Source, Incarnation: obs.Scope.Incarnation, ConnectionID: obs.Scope.ConnectionID}
		obs.Times = append([]model.TimeObservation(nil), obs.Times...)
		for i := range obs.Times {
			obs.Times[i].Scope = obs.Scope
		}
		batch.Observations = append(batch.Observations, obs)
		return model.RecordNonContent
	}
	if rawString(row, "type") == "cost-state" {
		if raw, exists := row["startTime"]; exists {
			stamp := model.TimeObservation{Scope: obs.Scope, Class: model.TimeNative, Raw: raw, SourceField: "startTime", Precision: "millisecond", Meaning: model.TimeStart, ObservationRef: obs.ID}
			var millis int64
			null := bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
			if !null && json.Unmarshal(raw, &millis) == nil {
				value := time.UnixMilli(millis).UTC()
				if value.Year() >= 1 && value.Year() <= 9999 {
					stamp.Value = &value
				}
			}
			if !null && stamp.Value == nil {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_native_timestamp", obs.Source))
			}
			obs.Times = append(obs.Times, stamp)
		}
	}
	if knownMetadataRow(rawString(row, "type")) {
		batch.Observations = append(batch.Observations, obs)
		return model.RecordNonContent
	}
	obs.Kind = model.ObservationUnknown
	batch.Observations = append(batch.Observations, obs)
	batch.Diagnostics = append(batch.Diagnostics, diagnostic("unknown_row", obs.Source))
	return model.RecordUnknown
}

func (state *State) compaction(batch *model.Batch, obs model.Observation, row map[string]json.RawMessage) {
	state.NextItem++
	turn := state.Turns[state.ActiveTurns[threadKey(obs.Scope)]]
	scope := obs.Scope
	if turn.Key != "" {
		scope = turn.Scope
	}
	item := model.Item{ID: sourceKey(obs.Source, "compaction"), SourceSeq: state.NextItem, Scope: scope, TurnKey: turn.Key, TurnOrdinal: turn.Ordinal, Type: model.ItemCompaction, Category: model.CategorySystem, Role: model.RoleSystem,
		SourceLine: int(obs.Source.Line), SourceLineSHA256: obs.Source.SHA256, SourceRowType: rawString(row, "type"), SourcePayloadType: rawString(row, "subtype"), Detail: mustJSON(row)}
	item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
	obs.Kind, obs.Text, obs.Detail = model.ObservationSnapshot, rawText(row, "summary"), mustJSON(row)
	complete := true
	obs.Complete = &complete
	item.Parts = []model.Part{{Type: "compaction", Text: obs.Text, Source: obs.Source, Availability: model.AvailableInline, Detail: obs.Detail}}
	state.observeItem(batch, &item, &obs)
	if uuid := rawString(row, "uuid"); uuid != "" {
		state.Rows[scopedKey(obs.Scope, "row", uuid)] = []string{item.ID}
	}
	if turn.Key != "" {
		if turn.StartSourceSeq == 0 {
			turn.StartSourceSeq = item.SourceSeq
		}
		turn.EndSourceSeq = item.SourceSeq
		state.putTurn(batch, turn)
	}
}
