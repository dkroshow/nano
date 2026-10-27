package grok

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func updateParts(update decodedUpdate, source model.SourceRef) ([]model.Part, bool) {
	var body map[string]json.RawMessage
	_ = json.Unmarshal(update.Payload, &body)
	raw, present := body["content"]
	var entries []json.RawMessage
	if present {
		if json.Unmarshal(raw, &entries) != nil {
			entries = []json.RawMessage{raw}
		}
	}
	partType := ""
	if update.Kind == kindPlan {
		raw, present = body["entries"]
		if present {
			_ = json.Unmarshal(raw, &entries)
			partType = "plan_entry"
		}
	}
	var parts []model.Part
	for i, entry := range entries {
		var content map[string]json.RawMessage
		if json.Unmarshal(entry, &content) != nil || content == nil {
			index := i
			parts = append(parts, model.Part{Type: "unknown", Index: &index, Availability: model.AvailableInline, Source: source, Detail: mustJSON(entry)})
			continue
		}
		if documentString(content, "type") == "content" {
			var nested map[string]json.RawMessage
			if json.Unmarshal(content["content"], &nested) == nil && nested != nil {
				content = nested
			}
		}
		kind := documentString(content, "type")
		if partType != "" {
			kind = partType
		}
		if kind == "" {
			kind = "unknown"
		}
		index := i
		part := model.Part{Type: kind, Index: &index, Text: documentString(content, "text"), Availability: model.AvailableInline, Source: source, Detail: mustJSON(entry)}
		if partType == "plan_entry" {
			part.Text = documentString(content, "content")
		}
		if kind == "resource_link" {
			part.Availability = model.AvailableReference
		}
		parts = append(parts, part)
	}
	return parts, present
}
func partsText(parts []model.Part) string {
	var texts []string
	for _, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func (state *State) applyTask(batch *model.Batch, source model.SourceRef, obs model.Observation, update decodedUpdate) {
	var body map[string]json.RawMessage
	_ = json.Unmarshal(update.Payload, &body)
	task := body
	var snapshot map[string]json.RawMessage
	if json.Unmarshal(body["task_snapshot"], &snapshot) == nil && snapshot != nil {
		task = snapshot
	}
	id := documentString(task, "task_id")
	key := scopedKey(obs.Scope, "task", id)
	item := state.Items[state.Tasks[key]]
	if item.ID == "" {
		state.NextItem++
		item = model.Item{ID: sourceKey(source, "task"), SourceSeq: state.NextItem, Type: model.ItemCommand, Category: model.CategorySystem, Role: model.RoleSystem, Scope: obs.Scope, SourceLine: int(source.Line), SourceLineSHA256: source.SHA256, SourceRowType: "session/update", SourcePayloadType: "task"}
		item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
		if id != "" {
			item.Aliases = []model.Alias{{Namespace: "grok.taskId", Value: mustJSON(id), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}
		}
	}
	obs.Scope, obs.Kind = item.Scope, model.ObservationSnapshot
	obs.Text = documentString(task, "command")
	for i := range obs.Times {
		obs.Times[i].Scope = item.Scope
	}
	complete := false
	_ = json.Unmarshal(task["completed"], &complete)
	obs.Complete = &complete
	obs.Outcome = model.OutcomeUnknown
	if complete {
		var exit *int64
		if json.Unmarshal(task["exit_code"], &exit) == nil && exit != nil {
			if *exit == 0 {
				obs.Outcome = model.OutcomeCompleted
			} else {
				obs.Outcome = model.OutcomeFailed
			}
		}
		var killed bool
		_ = json.Unmarshal(task["explicitly_killed"], &killed)
		if killed {
			obs.Outcome = model.OutcomeCancelled
		}
	}
	var diagnostics []model.Diagnostic
	prior := len(item.Relations)
	item, diagnostics = assemble.Observe(item, obs)
	batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	if len(diagnostics) == 0 && len(item.Relations) == prior {
		item.Detail = mustJSON(body)
		if output, ok := task["output"]; ok {
			part := model.Part{Type: "tool_output", Availability: model.AvailableInline, Source: source, Text: documentString(task, "output"), Detail: mustJSON(output)}
			var truncated bool
			_ = json.Unmarshal(task["truncated"], &truncated)
			if truncated {
				part.Availability = model.AvailableTruncated
				var total *int64
				if json.Unmarshal(task["output_total_bytes"], &total) == nil {
					part.OriginalBytes = total
				}
			}
			item.Parts = []model.Part{part}
		}
	}
	if id != "" {
		state.Tasks[key] = item.ID
	}
	state.Items[item.ID] = item
	batch.Observations = append(batch.Observations, obs)
	batch.Changes = append(batch.Changes, model.Change{Item: &item})
}

// Each clock is compared only with its own session, source and native field.
// A regression is evidence, never permission to reorder observations.
func (state *State) checkTimes(batch *model.Batch, obs model.Observation) {
	for _, stamp := range obs.Times {
		if stamp.Class != model.TimeNative || stamp.Value == nil {
			continue
		}
		scope := stamp.Scope
		scope.NativeTurnID = nil
		key := scopedKey(scope, "clock", stamp.SourceField)
		if previous, ok := state.Clocks[key]; ok && stamp.Value.Before(previous) {
			d := diagnostic("native_time_regression", obs.Source)
			d.Scope = obs.Scope
			d.Message = fmt.Sprintf("native %s clock regressed; source order retained", stamp.SourceField)
			batch.Diagnostics = append(batch.Diagnostics, d)
		}
		state.Clocks[key] = *stamp.Value
	}
}

// Only the text block Nano sends owns this key. Request and update metadata
// are not submission identity.
func contentSubmissionID(parts []model.Part) string {
	for _, part := range parts {
		if part.Type != "text" {
			continue
		}
		var block struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		}
		var key string
		if json.Unmarshal(part.Detail, &block) == nil && json.Unmarshal(block.Meta["x.nano/submissionId"], &key) == nil && key != "" {
			return key
		}
	}
	return ""
}
