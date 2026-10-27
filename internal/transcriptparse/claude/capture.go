package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type captureReceipt struct {
	Digest         string            `json:"digest"`
	ObservationRef string            `json:"observationRef"`
	Facets         []captureFacet    `json:"facets,omitempty"`
	Disposition    model.Disposition `json:"disposition"`
}
type captureFacet struct {
	ItemID         string `json:"itemId,omitempty"`
	TurnKey        string `json:"turnKey,omitempty"`
	ObservationRef string `json:"observationRef"`
}
type captureFrame struct {
	row         map[string]json.RawMessage
	source      model.SourceRef
	times       []model.TimeObservation
	key         string
	consumed    bool
	disposition model.Disposition
}

func nativePath(prefix, path string) string {
	if prefix == "" {
		return path
	}
	return prefix + strings.TrimPrefix(path, "$")
}

func (state *State) capture(batch *model.Batch, input model.Input, source model.SourceRef, row map[string]json.RawMessage) captureFrame {
	frame := captureFrame{row: row, source: source}
	method := rawString(row, "method")
	fields := row
	path := "$.line::json"
	wrapped := row["type"] == nil && row["line"] != nil && (row["seq"] != nil || row["dir"] != nil)
	if method == "session/line" {
		wrapped = true
		fields = rawObject(row, "params")
		path = "$.params.line::json"
	} else if strings.HasPrefix(method, "session/") {
		fields = rawObject(row, "params")
		obs := captureObservation(input, source, fields)
		var detail map[string]json.RawMessage
		_ = json.Unmarshal(obs.Detail, &detail)
		detail["method"] = mustJSON(method)
		for _, key := range []string{"lastSeq", "state", "turnActive", "exitCode"} {
			if raw, ok := fields[key]; ok {
				detail[key] = raw
			}
		}
		obs.Detail = mustJSON(detail)
		batch.Observations = append(batch.Observations, obs)
		frame.consumed, frame.disposition = true, model.RecordNonContent
		switch method {
		case "session/replayDone", "session/exited", "session/start", "session/attach", "session/input", "session/close", "session/list":
		default:
			frame.disposition = model.RecordUnknown
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unknown_shim_method", source))
		}
		return frame
	}
	if !wrapped {
		return frame
	}
	observation := captureObservation(input, source, fields)
	batch.Observations = append(batch.Observations, observation)
	for _, stamp := range observation.Times {
		if stamp.Class == model.TimeCapture && stamp.SourceField == "shim.ts" {
			frame.times = append(frame.times, stamp)
			if stamp.Value == nil {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_capture_timestamp", source))
			}
		}
	}
	var line string
	var native map[string]json.RawMessage
	if json.Unmarshal(fields["line"], &line) != nil || !utf8.ValidString(line) || json.Unmarshal([]byte(line), &native) != nil || native == nil {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_capture_line", source))
		frame.consumed, frame.disposition = true, model.RecordMalformed
		return frame
	}
	direction := rawString(fields, "dir")
	if direction != "send" && direction != "recv" {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_capture_direction", source))
	} else {
		frame.source.Direction = direction
	}
	frame.source.BlockPath = path
	frame.row = native
	var seq int64
	if json.Unmarshal(fields["seq"], &seq) != nil || seq <= 0 {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_capture_sequence", source))
		return frame
	}
	frame.key = string(mustJSON([]any{input.Scope.ConnectionID, rawString(fields, "shimSessionId"), seq}))
	digest := rowDigest(map[string]json.RawMessage{"native_line": mustJSON(native), "ts": fields["ts"], "dir": mustJSON(direction)})
	if earlier, exists := state.Captures[frame.key]; exists {
		frame.consumed, frame.disposition = true, earlier.Disposition
		if earlier.Digest != digest {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("capture_event_conflict", source))
			frame.disposition = model.RecordUnknown
			return frame
		}
		relation := model.Relation{Kind: model.RelationReplayOf, Scope: observation.Scope, FromObservation: observation.ID, ToObservation: earlier.ObservationRef, ObservationRefs: []string{earlier.ObservationRef, observation.ID}}
		batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
		state.replayCapture(batch, frame.source, observation.Times, earlier)
		return frame
	}
	state.Captures[frame.key] = captureReceipt{Digest: digest, ObservationRef: observation.ID}
	return frame
}

func captureObservation(input model.Input, source model.SourceRef, fields map[string]json.RawMessage) model.Observation {
	if direction := rawString(fields, "dir"); direction == "send" || direction == "recv" {
		source.Direction = direction
	}
	scope := model.Scope{Kind: model.ScopeConnection, Provider: model.ProviderClaude, Source: source.Label, Incarnation: source.Incarnation, ConnectionID: input.Scope.ConnectionID}
	obs := model.Observation{ID: sourceKey(source, "shim-capture"), Kind: model.ObservationMetadata, Scope: scope, Source: source, Capture: model.CaptureComplete}
	detail := map[string]json.RawMessage{"layer": mustJSON("claude_shim")}
	for _, key := range []string{"seq", "dir", "shimSessionId"} {
		if raw, ok := fields[key]; ok {
			detail[key] = raw
		}
	}
	obs.Detail = mustJSON(detail)
	if raw, ok := fields["ts"]; ok {
		stamp := model.TimeObservation{Scope: scope, Class: model.TimeCapture, Raw: raw, SourceField: "shim.ts", Precision: "native_string", Meaning: model.TimeEvent, ObservationRef: obs.ID}
		if value, err := time.Parse(time.RFC3339Nano, rawString(fields, "ts")); err == nil {
			value = value.UTC()
			stamp.Value = &value
		}
		obs.Times = append(obs.Times, stamp)
	}
	for _, clock := range []struct {
		value *time.Time
		class model.TimeClass
		field string
	}{{input.CaptureAt, model.TimeCapture, "caller.captureAt"}, {input.ObservedAt, model.TimeObserved, "caller.observedAt"}} {
		if clock.value != nil {
			value := clock.value.UTC()
			obs.Times = append(obs.Times, model.TimeObservation{Scope: scope, Value: &value, Class: clock.class, SourceField: clock.field, Precision: "caller_supplied", Meaning: model.TimeEvent, ObservationRef: obs.ID})
		}
	}
	return obs
}

func (state *State) captureFacets(key string, observations []model.Observation, disposition model.Disposition) {
	if key == "" {
		return
	}
	receipt := state.Captures[key]
	receipt.Disposition = disposition
	seen := map[string]bool{}
	for _, obs := range observations {
		if seen[obs.ID] {
			continue
		}
		if obs.Scope.ItemID != "" || obs.Scope.Kind == model.ScopeTurn && obs.Scope.TurnKey != "" {
			receipt.Facets = append(receipt.Facets, captureFacet{ItemID: obs.Scope.ItemID, TurnKey: obs.Scope.TurnKey, ObservationRef: obs.ID})
			seen[obs.ID] = true
		}
	}
	state.Captures[key] = receipt
}

func (state *State) replayCapture(batch *model.Batch, source model.SourceRef, clocks []model.TimeObservation, receipt captureReceipt) {
	for i, facet := range receipt.Facets {
		item := state.Items[facet.ItemID]
		turn := state.Turns[facet.TurnKey]
		scope, observations := item.Scope, item.Observations
		if facet.ItemID == "" {
			scope, observations = turn.Scope, turn.Observations
		}
		found := false
		for _, prior := range observations {
			if prior.ID != facet.ObservationRef {
				continue
			}
			found = true
			obs := prior
			obs.ID = sourceKey(source, fmt.Sprintf("capture-replay:%d", i))
			obs.Scope, obs.Source = scope, source
			obs.Source.BlockPath = prior.Source.BlockPath
			obs.Times = nil
			for _, stamp := range prior.Times {
				if stamp.Class == model.TimeNative {
					obs.Times = append(obs.Times, stamp)
				}
			}
			obs.Times = append(obs.Times, clocks...)
			for j := range obs.Times {
				obs.Times[j].Scope = scope
				obs.Times[j].ObservationRef = obs.ID
			}
			relation := model.Relation{Kind: model.RelationReplayOf, Scope: scope, FromObservation: obs.ID, ToObservation: prior.ID, ObservationRefs: []string{prior.ID, obs.ID}}
			if facet.ItemID != "" {
				item.Observations = append(append([]model.Observation(nil), item.Observations...), obs)
				item.Times = append(append([]model.TimeObservation(nil), item.Times...), obs.Times...)
				item.Relations = append(append([]model.Relation(nil), item.Relations...), relation)
				state.Items[item.ID] = item
				batch.Changes = append(batch.Changes, model.Change{Item: &item})
			} else {
				turn.Observations = append(append([]model.Observation(nil), turn.Observations...), obs)
				state.putTurn(batch, turn)
			}
			batch.Observations = append(batch.Observations, obs)
			batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
			break
		}
		if !found {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("missing_capture_evidence", source))
		}
	}
}
