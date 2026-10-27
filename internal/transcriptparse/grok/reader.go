package grok

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/jsonl"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// State is a versioned checkpoint. Treat its fields as immutable between calls;
// encoding/json can save and restore it. A new source incarnation starts at zero.
type State struct {
	UnboundPromptIndices map[string]int64         `json:"unboundPromptIndices"`
	Requests             map[string]rpcRequest    `json:"requests"`
	RunningPrompts       map[string]runningPrompt `json:"runningPrompts"`
	Format               string                   `json:"format"`
	Document             []byte                   `json:"document,omitempty"`
	DocumentBytes        int64                    `json:"documentBytes"`
	DocumentClosed       bool                     `json:"documentClosed"`
	Tasks                map[string]string        `json:"tasks"`
	Clocks               map[string]time.Time     `json:"clocks"`
	Version              int                      `json:"version"`
	Source               model.SourceRef          `json:"source"`
	Framing              jsonl.State              `json:"framing"`
	Frame                int64                    `json:"frame"`
	Limit                int                      `json:"limit"`
	Items                map[string]model.Item    `json:"items"`
	Turns                map[string]model.Turn    `json:"turns"`
	Segments             map[string]string        `json:"segments"`
	Events               map[string]string        `json:"events"`
	Calls                map[string]string        `json:"calls"`
	LastAssistant        map[string]string        `json:"lastAssistant"`
	NextItem             int64                    `json:"nextItem"`
	NextTurn             int                      `json:"nextTurn"`

	// Continuation size accounting, deliberately unpersisted: measured is the
	// last whole-state encoding, pending the input consumed since, and
	// measurements the number of encodings taken. A restored checkpoint simply
	// measures again once enough new input has arrived.
	measured     int
	pending      int
	measurements int
}

// ParseFile consumes chunks of updates.jsonl. An open file's unterminated tail
// stays pending until Closed is explicit; closing bytes does not complete turns.
func ParseFile(state State, input model.Input) (State, model.Batch, error) {
	next, batch, err := prepare(state, input, "file")
	if err != nil {
		return state, batch, err
	}
	if next.Format == "json" {
		return next.parseDocument(batch, input)
	}
	framing, records, err := jsonl.Feed(next.Framing, input.Bytes, next.Limit, input.Closed)
	if err != nil {
		if framingError, ok := err.(*jsonl.Error); ok {
			return state, batch, readerError(framingError.Code, input.Source)
		}
		return state, batch, readerError("invalid_checkpoint", input.Source)
	}
	next.Framing = framing
	for _, record := range records {
		source := next.Source
		source.Line, source.Start, source.End = record.Line, record.Start, record.End
		if record.Problem != "" {
			batch.Records = append(batch.Records, model.Record{Source: source, Disposition: model.RecordMalformed, Capture: model.CaptureTruncated})
			batch.Diagnostics = append(batch.Diagnostics, diagnostic(record.Problem, source))
			continue
		}
		next.consume(&batch, input, source, record.Bytes, record.Terminated)
		next.evictSettled(false)
	}
	if len(records) == 0 {
		// A read with no new records still applies retention, so a stored
		// checkpoint is compacted by reading nothing.
		next.evictSettled(false)
	}
	if next.Framing.Closed {
		next = next.closedCheckpoint()
	}
	next.checkpointDiagnostic(&batch)
	return next, batch, nil
}

// ParseWire consumes one complete ACP frame. The caller supplies any capture and
// receipt clocks. File and wire inputs use the same decoder and assembly.
func ParseWire(state State, frame model.Input) (State, model.Batch, error) {
	next, batch, err := prepare(state, frame, "wire")
	if err != nil {
		return state, batch, err
	}
	next.Frame++
	source := next.Source
	source.Frame, source.End, source.Direction = next.Frame, int64(len(frame.Bytes)), frame.Source.Direction
	if len(frame.Bytes) > next.Limit {
		batch.Records = append(batch.Records, model.Record{Source: source, Disposition: model.RecordMalformed, Capture: model.CaptureTruncated})
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("row_too_large", source))
	} else {
		next.consume(&batch, frame, source, frame.Bytes, true)
	}
	next.evictSettled(true)
	next.checkpointDiagnostic(&batch)
	return next, batch, nil
}

func prepare(state State, input model.Input, kind string) (State, model.Batch, error) {
	batch := model.Batch{Versions: model.Versions{
		Schema: "transcript-v3", Reader: "grok-reader-v24", Assembly: "observations-v3",
		Taxonomy: "history-v1", Usage: "native-grain-v1", Checkpoint: 14,
	}}
	format := input.Format
	if format == "" {
		format = state.Format
	}
	if format == "" {
		format = "jsonl"
	}
	if (format != "jsonl" && format != "json") || (kind == "wire" && format != "jsonl") {
		return state, batch, readerError("unsupported_format", input.Source)
	}
	source := model.SourceRef{Kind: kind, Label: input.Source.Label, Incarnation: input.Source.Incarnation}
	if source.Label == "" || source.Incarnation == "" || (input.Scope.Provider != "" && input.Scope.Provider != model.ProviderGrok) {
		return state, batch, readerError("invalid_source", source)
	}
	limit := input.MaxRowBytes
	if limit == 0 {
		limit = state.Limit
		if limit == 0 {
			limit = MaxUpdateBytes
		}
	}
	if limit <= 0 {
		return state, batch, readerError("invalid_limit", source)
	}
	if state.Version == 0 {
		if !reflect.DeepEqual(state, State{}) {
			return state, batch, readerError("invalid_checkpoint", source)
		}
		state.Version, state.Source, state.Limit, state.Format = 14, source, limit, format
	} else if state.Version != 14 || state.Source != source || state.Limit != limit || state.Format != format {
		return state, batch, readerError("incompatible_checkpoint", source)
	}
	if state.Format == "json" && (len(state.Document) > state.Limit || state.DocumentBytes < int64(len(state.Document)) || (state.DocumentClosed && len(state.Document) != 0)) {
		return state, batch, readerError("invalid_checkpoint", source)
	}
	state.pending += len(input.Bytes)
	state.UnboundPromptIndices = copyMap(state.UnboundPromptIndices)
	state.Requests = copyMap(state.Requests)
	state.RunningPrompts = copyMap(state.RunningPrompts)
	state.Tasks = copyMap(state.Tasks)
	state.Clocks = copyMap(state.Clocks)
	state.Items = copyMap(state.Items)
	state.Turns = copyMap(state.Turns)
	state.Segments = copyMap(state.Segments)
	state.Events = copyMap(state.Events)
	state.Calls = copyMap(state.Calls)
	state.LastAssistant = copyMap(state.LastAssistant)
	return state, batch, nil
}

func copyMap[V any](source map[string]V) map[string]V {
	out := make(map[string]V, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func readerError(code string, source model.SourceRef) error {
	d := diagnostic(code, source)
	d.Severity = "error"
	return d
}

func diagnostic(code string, source model.SourceRef) model.Diagnostic {
	return model.Diagnostic{
		Code: code, Severity: "warning", Source: source,
		Scope:   model.Scope{Kind: model.ScopeThread, Provider: model.ProviderGrok, Source: source.Label, Incarnation: source.Incarnation},
		Message: "Grok source record or checkpoint could not be interpreted",
	}
}

func (state *State) consume(batch *model.Batch, input model.Input, source model.SourceRef, raw []byte, terminated bool) {
	digest := sha256.Sum256(raw)
	source.SHA256 = fmt.Sprintf("%x", digest)
	record := model.Record{Source: source, Disposition: model.RecordSemantic, Capture: model.CaptureComplete}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		record.Disposition = model.RecordNonContent
		batch.Records = append(batch.Records, record)
		return
	}
	if !utf8.Valid(raw) {
		record.Disposition = model.RecordMalformed
		batch.Records = append(batch.Records, record)
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_record", source))
		return
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if source.Kind == "wire" && fieldsOf(fields["msg"]) != nil {
		source.Direction = documentString(fields, "dir")
		source.BlockPath = "$.msg"
		capture := pickFields(fields, "seq", "ts", "dir")
		batch.Observations = append(batch.Observations, captureClock(mustJSON(capture), scopeFrom(input, decodedUpdate{}, model.ScopeConnection), source))
		raw = fields["msg"]
		fields = fieldsOf(raw)
		record.Source = source
	}
	updates, err := decodeUpdateLimit(raw, state.Limit)
	if err != nil || !utf8.Valid(raw) || trimmed[0] != '{' {
		record.Disposition = model.RecordMalformed
		if !terminated {
			record.Capture = model.CapturePartial
		}
		batch.Records = append(batch.Records, record)
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_record", source))
		return
	}
	if len(updates) == 0 {
		if source.Kind == "file" && fields["method"] == nil && fields["result"] == nil && fields["error"] == nil && fields["jsonrpc"] == nil {
			record.Disposition = state.auxiliary(batch, input, source, fields)
		} else {
			record.Disposition = state.control(batch, input, source, fields)
		}
	} else {
		if source.Kind == "wire" && source.Direction == "send" {
			scope := scopeFrom(input, decodedUpdate{}, model.ScopeConnection)
			batch.Observations = append(batch.Observations, model.Observation{ID: sourceKey(source, "outgoing-update"), Scope: scope, Source: source, Kind: model.ObservationMetadata, Capture: model.CaptureComplete, Detail: mustJSON(fields)})
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unexpected_update_direction", source))
			record.Disposition = model.RecordNonContent
			batch.Records = append(batch.Records, record)
			return
		}
		for _, update := range updates {
			if meta := fieldsOf(fields["params"])["_meta"]; meta != nil {
				scope := scopeFrom(input, update, model.ScopeThread)
				if scope.NativeTurnID != nil {
					scope.Kind = model.ScopeTurn
				}
				metaSource := nativeChild(source, ".params._meta")
				batch.Observations = append(batch.Observations, model.Observation{ID: sourceKey(metaSource, "envelope-meta"), Scope: scope, Source: metaSource, Kind: model.ObservationMetadata, Capture: model.CaptureComplete, Detail: mustJSON(map[string]json.RawMessage{"_meta": meta})})
			}
			if update.Kind == kindUnclassified {
				record.Disposition = model.RecordUnknown
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("unknown_update", source))
			}
			if update.Kind == kindMetadata || update.Kind == kindHook || update.Kind == kindHiddenUser || update.Kind == kindSubagentProgress || update.Kind == kindSubagentDiagnostic {
				record.Disposition = model.RecordNonContent
			}
			state.apply(batch, input, source, update)
		}
	}
	batch.Records = append(batch.Records, record)
}

func mustJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func sourceKey(source model.SourceRef, facet string) string {
	// Identity depends on first source position, never clocks or native text.
	source.SHA256 = ""
	digest := sha256.Sum256(mustJSON(struct {
		Source model.SourceRef
		Facet  string
	}{source, facet}))
	return fmt.Sprintf("local:%x", digest[:])
}

func scopeFrom(input model.Input, update decodedUpdate, kind model.ScopeKind) model.Scope {
	scope := input.Scope
	scope.TurnKey, scope.ItemID = "", ""
	scope.Provider, scope.Source, scope.Incarnation, scope.Kind = model.ProviderGrok, input.Source.Label, input.Source.Incarnation, kind
	if update.SessionID != "" {
		if scope.NativeThreadID == nil || *scope.NativeThreadID != update.SessionID {
			scope.NativeTurnID, scope.ParentNativeID = nil, nil
		}
		value := update.SessionID
		scope.NativeThreadID = &value
	}
	if update.PromptID != "" {
		value := update.PromptID
		scope.NativeTurnID = &value
	}
	return scope
}

func scopedKey(scope model.Scope, facet, value string) string {
	return string(mustJSON([]any{scope.Provider, scope.Source, scope.Incarnation, scope.ConnectionID, scope.NativeThreadID, scope.NativeTurnID, facet, value}))
}
