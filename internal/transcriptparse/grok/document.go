package grok

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"strings"
	"time"
	"unicode/utf8"
)

// Summary field names are extracted from history/grok_adapter.go. A summary
// is a session snapshot: its latest model never restamps past turns.
type summaryInfo struct {
	ID  string `json:"id"`
	CWD string `json:"cwd"`
}
type summaryDocument struct {
	Info            summaryInfo `json:"info"`
	CreatedAt       string      `json:"created_at"`
	UpdatedAt       string      `json:"updated_at"`
	LastActiveAt    string      `json:"last_active_at"`
	CurrentModelID  string      `json:"current_model_id"`
	ReasoningEffort string      `json:"reasoning_effort"`
	SandboxProfile  string      `json:"sandbox_profile"`
	GeneratedTitle  string      `json:"generated_title"`
	SessionSummary  string      `json:"session_summary"`
}

func (state State) parseDocument(batch model.Batch, input model.Input) (State, model.Batch, error) {
	if state.DocumentClosed {
		return state, batch, readerError("closed_document", input.Source)
	}
	state.DocumentBytes += int64(len(input.Bytes))
	count := state.Limit - len(state.Document)
	if count > len(input.Bytes) {
		count = len(input.Bytes)
	}
	state.Document = append(append([]byte(nil), state.Document...), input.Bytes[:count]...)
	if !input.Closed {
		return state, batch, nil
	}
	state.DocumentClosed = true
	source := state.Source
	source.Line, source.End = 1, state.DocumentBytes
	record := model.Record{Source: source, Disposition: model.RecordNonContent, Capture: model.CaptureComplete}
	raw := state.Document
	if state.DocumentBytes > int64(state.Limit) {
		record.Disposition, record.Capture = model.RecordMalformed, model.CaptureTruncated
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("document_too_large", source))
	} else if !utf8.Valid(raw) || !json.Valid(raw) || len(bytes.TrimSpace(raw)) == 0 {
		record.Disposition, record.Capture = model.RecordMalformed, model.CapturePartial
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_document", source))
	} else {
		hash := sha256.Sum256(raw)
		source.SHA256 = fmt.Sprintf("%x", hash[:])
		record.Source = source
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		scope := scopeFrom(input, decodedUpdate{}, model.ScopeThread)
		scope.NativeTurnID = nil
		var metadata model.Metadata
		summaryClocks := false
		if info := fields["info"]; len(info) > 0 {
			var summary summaryDocument
			if json.Unmarshal(raw, &summary) != nil {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_summary", source))
			} else {
				metadata = summaryMetadata(summary)
				summaryClocks = true
				if summary.Info.ID != "" {
					scope.NativeThreadID = &summary.Info.ID
				}
			}
		}
		if child := documentString(fields, "child_session_id"); child != "" {
			scope.NativeThreadID = &child
			if metadata == nil {
				metadata = model.Metadata{}
			}
			metadata[model.MetadataNativeThreadID] = model.MetadataValue{State: model.MetadataKnown, Value: child}
			metadata[model.MetadataSourceKind] = model.MetadataValue{State: model.MetadataKnown, Value: "subagent"}
		}
		if parent := documentString(fields, "parent_session_id"); parent != "" {
			scope.ParentNativeID = &parent
			if metadata == nil {
				metadata = model.Metadata{}
			}
			metadata[model.MetadataParentNativeID] = model.MetadataValue{State: model.MetadataKnown, Value: parent}
		}
		obs := model.Observation{ID: sourceKey(source, "document"), Kind: model.ObservationMetadata, Scope: scope, Source: source, Capture: model.CaptureComplete, Metadata: metadata, Detail: mustJSON(json.RawMessage(raw))}
		for _, field := range []string{"created_at", "updated_at", "last_active_at", "started_at", "completed_at"} {
			if native, ok := fields[field]; ok {
				meaning := model.TimeEvent
				switch field {
				case "created_at", "started_at":
					meaning = model.TimeStart
				case "updated_at":
					meaning = model.TimeWrite
				case "completed_at":
					meaning = model.TimeEnd
				}
				stamp := model.TimeObservation{Scope: scope, Class: model.TimeNative, Raw: mustJSON(native), SourceField: field, Meaning: meaning, Precision: "native_string", ObservationRef: obs.ID}
				if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(documentString(fields, field))); err == nil {
					parsed = parsed.UTC()
					stamp.Value = &parsed
				} else if string(native) != "null" {
					batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_timestamp", source))
				}
				obs.Times = append(obs.Times, stamp)
				if summaryClocks && (field == "created_at" || field == "updated_at" || field == "last_active_at") {
					stamp.Meaning = model.TimeThreadActivity
					obs.Times = append(obs.Times, stamp)
				}
			}
		}
		for _, clock := range []struct {
			value *time.Time
			class model.TimeClass
			field string
		}{{input.CaptureAt, model.TimeCapture, "caller.captureAt"}, {input.ObservedAt, model.TimeObserved, "caller.observedAt"}} {
			if clock.value != nil {
				value := clock.value.UTC()
				obs.Times = append(obs.Times, model.TimeObservation{Scope: scope, Class: clock.class, Value: &value, Meaning: model.TimeEvent, SourceField: clock.field, Precision: "caller_supplied", ObservationRef: obs.ID})
			}
		}
		if state.compactionDocument(&batch, obs, fields) {
			record.Disposition = model.RecordSemantic
		} else {
			batch.Observations = append(batch.Observations, obs)
		}
	}
	state.Document = nil
	batch.Records = append(batch.Records, record)
	return state.closedCheckpoint(), batch, nil
}

func (state *State) compactionDocument(batch *model.Batch, obs model.Observation, fields map[string]json.RawMessage) bool {
	phase, idField, alias, historyField, version := "", "", "", "", 0
	if fields["checkpoint_id"] != nil && fields["compacted_history"] != nil {
		phase, idField, alias, historyField, version = "checkpoint_saved", "checkpoint_id", "grok.compactionCheckpointId", "compacted_history", 1
	} else if fields["request_id"] != nil && fields["chat_history"] != nil && fields["trigger"] != nil {
		phase, idField, alias, historyField, version = "requested", "request_id", "grok.compactionRequestId", "chat_history", 2
	} else {
		return false
	}
	var schema int
	history := bytes.TrimSpace(fields[historyField])
	id := documentString(fields, idField)
	if json.Unmarshal(fields["schema_version"], &schema) != nil || schema != version || id == "" || len(history) == 0 || history[0] != '[' {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unverified_compaction_schema", obs.Source))
		return false
	}
	item := model.Item{Type: model.ItemCompaction, Category: model.CategorySystem, Role: model.RoleSystem, Phase: phase, Detail: obs.Detail, SourceRowType: "compaction_snapshot", SourcePayloadType: phase}
	item.Aliases = []model.Alias{{Namespace: alias, Value: mustJSON(id), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}
	if text := documentString(fields, "summary"); text != "" {
		item.Parts = []model.Part{{Type: "compaction_summary", Text: text, Availability: model.AvailableInline, Source: nativeChild(obs.Source, ".summary")}}
	}
	state.emitAuxiliaryItem(batch, obs, item)
	return true
}
func documentString(row map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(row[key], &value)
	return value
}

// CheckLineage reports conflicting native parent snapshots across sources.
// It retains both observations; a temporary absent or malformed sidecar is not
// evidence that a previously observed native edge disappeared.
func CheckLineage(observations []model.Observation) []model.Diagnostic {
	parents := map[string]string{}
	var diagnostics []model.Diagnostic
	for _, obs := range observations {
		if obs.Scope.Provider != model.ProviderGrok || obs.Scope.NativeThreadID == nil || obs.Scope.ParentNativeID == nil {
			continue
		}
		child, parent := *obs.Scope.NativeThreadID, *obs.Scope.ParentNativeID
		if old, exists := parents[child]; exists && old != parent {
			d := diagnostic("lineage_conflict", obs.Source)
			d.Scope = obs.Scope
			diagnostics = append(diagnostics, d)
		} else {
			parents[child] = parent
		}
	}
	return diagnostics
}
