package assemble

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Observe assembles one already-decoded observation. Provider readers choose
// the logical item and whether evidence is a delta, snapshot, stop or result.
// It never mutates prior state, sorts evidence, or completes a parent turn.
func Observe(item model.Item, observation model.Observation) (model.Item, []model.Diagnostic) {
	prior := item.Observations
	item.Observations = append(append([]model.Observation(nil), prior...), observation)
	item.Times = append(append([]model.TimeObservation(nil), item.Times...), observation.Times...)
	if item.Capture == "" {
		item.Capture = model.CaptureUnknown
	}
	if item.Outcome == "" {
		item.Outcome = model.OutcomeUnknown
	}
	if item.Finality.State == "" {
		item.Finality.State = model.FinalityUnknown
	}
	if item.Status == "" {
		item.Status = model.ItemStatusUnknown
	}
	if !sameItemOwner(item.Scope, observation.Scope) {
		return item, []model.Diagnostic{problem("observation_scope_mismatch", observation)}
	}
	for _, earlier := range prior {
		if observation.NativeEventID == "" || observation.NativeEventID != earlier.NativeEventID || !sameItemOwner(observation.Scope, earlier.Scope) {
			continue
		}
		if !SameEvidence(earlier, observation) {
			return item, []model.Diagnostic{problem("native_event_conflict", observation)}
		}
		item.Relations = append(append([]model.Relation(nil), item.Relations...), model.Relation{
			Kind: model.RelationReplayOf, Scope: observation.Scope,
			FromObservation: observation.ID, ToObservation: earlier.ID,
			ObservationRefs: []string{earlier.ID, observation.ID},
		})
		return item, nil
	}
	switch observation.Kind {
	case model.ObservationDelta:
		item.Text += observation.Text
	case model.ObservationSnapshot, model.ObservationResult:
		item.Text = observation.Text
	case model.ObservationProgress:
		if observation.Text != "" {
			item.Text = observation.Text
		}
	}
	if observation.Complete != nil {
		item.Complete = *observation.Complete
	}
	if observation.Outcome != "" {
		item.Outcome = observation.Outcome
		item.Status = itemStatus(observation.Outcome)
	}
	// A complete record cannot repair an earlier partial or truncated capture.
	if observation.Capture == model.CaptureTruncated || (item.Capture != model.CaptureTruncated && observation.Capture == model.CapturePartial) || (item.Capture == model.CaptureUnknown && observation.Capture != "") {
		item.Capture = observation.Capture
	}
	if item.CreatedAt == nil {
		for _, stamp := range observation.Times {
			if stamp.Class == model.TimeNative && stamp.Value != nil && (stamp.Meaning == model.TimeEvent || stamp.Meaning == model.TimeWrite || stamp.Meaning == model.TimeStart) {
				value := stamp.Value.UTC()
				item.CreatedAt = &value
				break
			}
		}
	}
	return item, nil
}

// LinkResult adds evidence only to a result facet. A scoped call mismatch leaves
// the result visible and diagnosed; it never changes invocation or turn outcome.
func LinkResult(call, result model.Item) (model.Item, []model.Diagnostic) {
	var refs []string
	for _, observation := range result.Observations {
		refs = append(refs, observation.ID)
	}
	if call.ID == "" || result.ID == "" || call.CallID == "" || call.CallID != result.CallID || !sameTurn(call.Scope, result.Scope) || !invocationType(call.Type) || result.Type != model.ItemToolResult {
		return result, []model.Diagnostic{{Code: "unresolved_tool_result", Severity: "warning", Scope: result.Scope, Message: "result has no matching scoped invocation"}}
	}
	result.Relations = append(append([]model.Relation(nil), result.Relations...), model.Relation{
		Kind: model.RelationResultOf, Scope: result.Scope, FromItem: result.ID,
		ToItem: call.ID, ObservationRefs: refs,
	})
	return result, nil
}

func sameTurn(a, b model.Scope) bool {
	if a.Provider != b.Provider || a.Source != b.Source || a.Incarnation != b.Incarnation || a.ConnectionID != b.ConnectionID || !compatibleID(a.NativeThreadID, b.NativeThreadID) || !compatibleID(a.NativeTurnID, b.NativeTurnID) || !reflect.DeepEqual(a.NativeParentCallID, b.NativeParentCallID) {
		return false
	}
	// An immutable source-local turn key survives explicit late native binding.
	// Earlier observations keep their original unknown scope. Conflicting known
	// identities above still reject a join, even when local keys are equal.
	if a.TurnKey != "" && a.TurnKey == b.TurnKey {
		return true
	}
	if !reflect.DeepEqual(a.NativeThreadID, b.NativeThreadID) {
		return false
	}
	if a.NativeTurnID != nil && b.NativeTurnID != nil {
		return *a.NativeTurnID == *b.NativeTurnID
	}
	return false
}

func compatibleID(a, b *string) bool { return a == nil || b == nil || *a == *b }

// SameEvidence compares native observation content without source coordinates or
// clock fields. JSON field order is irrelevant; large integers remain exact.
func SameEvidence(a, b model.Observation) bool {
	if a.Kind != b.Kind || a.Text != b.Text || !reflect.DeepEqual(a.Complete, b.Complete) || a.Outcome != b.Outcome || a.Capture != b.Capture {
		return false
	}
	var ad, bd any
	if len(a.Detail) > 0 && decodeDetail(a.Detail, &ad) != nil {
		return false
	}
	if len(b.Detail) > 0 && decodeDetail(b.Detail, &bd) != nil {
		return false
	}
	return reflect.DeepEqual(ad, bd)
}

func decodeDetail(raw []byte, value *any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(value)
}

func problem(code string, observation model.Observation) model.Diagnostic {
	return model.Diagnostic{Code: code, Severity: "warning", Scope: observation.Scope, Source: observation.Source, Message: "observation retained without changing assembled content"}
}

func itemStatus(outcome model.Outcome) model.ItemStatus {
	switch outcome {
	case model.OutcomeCompleted:
		return model.ItemStatusCompleted
	case model.OutcomeRunning:
		return model.ItemStatusInProgress
	case model.OutcomeFailed:
		return model.ItemStatusFailed
	case model.OutcomeInterrupted, model.OutcomeCancelled:
		return model.ItemStatusInterrupted
	default:
		return model.ItemStatusUnknown
	}
}

// Commands, file changes and native plan calls are specialized invocation
// facets. LinkResult also requires a scoped call ID before linking a result.
func invocationType(kind model.ItemType) bool {
	switch kind {
	case model.ItemToolCall, model.ItemCommand, model.ItemFileChange, model.ItemInteraction, model.ItemPlan:
		return true
	default:
		return false
	}
}

func sameItemOwner(a, b model.Scope) bool {
	if a.ItemID != "" && b.ItemID != "" && a.ItemID != b.ItemID {
		return false
	}
	if sameTurn(a, b) {
		return true
	}
	return a.ItemID != "" && a.ItemID == b.ItemID && a.TurnKey == "" && b.TurnKey == "" && a.NativeTurnID == nil && b.NativeTurnID == nil && a.Provider == b.Provider && a.Source == b.Source && a.Incarnation == b.Incarnation && a.ConnectionID == b.ConnectionID && compatibleID(a.NativeThreadID, b.NativeThreadID) && reflect.DeepEqual(a.NativeParentCallID, b.NativeParentCallID)
}
