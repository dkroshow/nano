package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/internal/jsonl"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const defaultMaxRowBytes = 32 << 20

// State is a versioned checkpoint. Treat its fields as immutable between calls;
// encoding/json can save and restore it. A new source incarnation starts at zero.
type State struct {
	Catalog        map[string]assemble.Catalog `json:"catalog,omitempty"`
	Version        int                         `json:"version"`
	Source         model.SourceRef             `json:"source"`
	Framing        jsonl.State                 `json:"framing"`
	Frame          int64                       `json:"frame"`
	Limit          int                         `json:"limit"`
	NativeThreads  map[string]string           `json:"nativeThreads,omitempty"`
	Items          map[string]model.Item       `json:"items"`
	Turns          map[string]model.Turn       `json:"turns"`
	ActiveTurns    map[string]string           `json:"activeTurns"`
	TurnPrompts    map[string]string           `json:"turnPrompts,omitempty"` // a saved turn's Claude promptId
	ActiveMessages map[string]string           `json:"activeMessages"`
	Messages       map[string]messageState     `json:"messages"`
	Rows           map[string][]string         `json:"rows"`
	RowEvents      map[string]rowEvent         `json:"rowEvents"`
	Captures       map[string]captureReceipt   `json:"captures"`
	Controls       map[string]controlRequest   `json:"controls"`
	Calls          map[string]string           `json:"calls"`
	NextItem       int64                       `json:"nextItem"`
	NextTurn       int                         `json:"nextTurn"`

	// Continuation size accounting, deliberately unpersisted: measured is the
	// last whole-state encoding, pending the input consumed since, and
	// measurements the number of encodings taken. A restored checkpoint simply
	// measures again once enough new input has arrived.
	measured     int
	pending      int
	measurements int
}

type messageState struct {
	ID           string                     `json:"id"`
	TurnKey      string                     `json:"turnKey"`
	NextBlock    int                        `json:"nextBlock"`
	SnapshotNext int                        `json:"snapshotNext"`
	Stream       bool                       `json:"stream"`
	Blocks       map[int]string             `json:"blocks"`
	Usage        map[string]json.RawMessage `json:"usage,omitempty"`
}

// ParseFile consumes chunks of Claude native JSONL. An open file's unterminated tail
// stays pending until Closed is explicit; closing bytes does not complete turns.
func ParseFile(state State, input model.Input) (State, model.Batch, error) {
	next, batch, err := prepare(state, input, "file")
	if err != nil {
		return state, batch, err
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

// ParseWire consumes one complete Claude stream-json frame. The caller supplies any capture and
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
	if !streamDeltaOnly(batch) {
		next.checkpointDiagnostic(&batch)
	}
	return next, batch, nil
}

// streamDeltaOnly reports a frame that only extended an open content block.
// Such deltas are most wire frames, and measuring the whole state on each one
// JSON-encoded it per frame (about an eighth of a replay's CPU). The block's
// content_block_stop, its assistant row, or the result follows every delta
// and measures the same growth.
func streamDeltaOnly(batch model.Batch) bool {
	if len(batch.Observations) == 0 {
		return false
	}
	for _, observation := range batch.Observations {
		if observation.Kind != model.ObservationDelta {
			return false
		}
	}
	return true
}

func prepare(state State, input model.Input, kind string) (State, model.Batch, error) {
	batch := model.Batch{Versions: model.Versions{
		Schema: "transcript-v3", Reader: "claude-reader-v36", Assembly: "observations-v3",
		Taxonomy: "history-v1", Usage: "native-grain-v1", Checkpoint: 21,
	}}
	if input.Format != "" && input.Format != "jsonl" {
		return state, batch, readerError("unsupported_format", input.Source)
	}
	source := model.SourceRef{Kind: kind, Label: input.Source.Label, Incarnation: input.Source.Incarnation}
	if source.Label == "" || source.Incarnation == "" || (input.Scope.Provider != "" && input.Scope.Provider != model.ProviderClaude) {
		return state, batch, readerError("invalid_source", source)
	}
	limit := input.MaxRowBytes
	if limit == 0 {
		limit = state.Limit
		if limit == 0 {
			limit = defaultMaxRowBytes
		}
	}
	if limit <= 0 {
		return state, batch, readerError("invalid_limit", source)
	}
	if state.Version == 0 {
		if !reflect.DeepEqual(state, State{}) {
			return state, batch, readerError("invalid_checkpoint", source)
		}
		state.Version, state.Source, state.Limit = 21, source, limit
	} else if state.Version != 21 || state.Source != source || state.Limit != limit {
		return state, batch, readerError("incompatible_checkpoint", source)
	}
	state.pending += len(input.Bytes)
	state.Catalog = copyMap(state.Catalog)
	state.Items = copyMap(state.Items)
	state.NativeThreads = copyMap(state.NativeThreads)
	state.Turns = copyMap(state.Turns)
	state.ActiveTurns = copyMap(state.ActiveTurns)
	state.TurnPrompts = copyMap(state.TurnPrompts)
	state.ActiveMessages = copyMap(state.ActiveMessages)
	state.Messages = copyMap(state.Messages)
	state.Rows = copyMap(state.Rows)
	state.RowEvents = copyMap(state.RowEvents)
	state.Captures = copyMap(state.Captures)
	state.Controls = copyMap(state.Controls)
	state.Calls = copyMap(state.Calls)
	for key, message := range state.Messages {
		message.Blocks = copyMap(message.Blocks)
		if message.Usage != nil {
			message.Usage = copyMap(message.Usage)
		}
		state.Messages[key] = message
	}
	return state, batch, nil
}

func copyMap[K comparable, V any](source map[K]V) map[K]V {
	out := make(map[K]V, len(source))
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
		Scope:   model.Scope{Kind: model.ScopeThread, Provider: model.ProviderClaude, Source: source.Label, Incarnation: source.Incarnation},
		Message: "Claude source record or checkpoint could not be interpreted",
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
	var row map[string]json.RawMessage
	if !utf8.Valid(raw) || json.Unmarshal(raw, &row) != nil || row == nil {
		record.Disposition = model.RecordMalformed
		if !terminated {
			record.Capture = model.CapturePartial
		}
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_record", source))
	} else {
		capture := state.capture(batch, input, source, row)
		record.Source.Direction = capture.source.Direction
		if capture.consumed {
			record.Disposition = capture.disposition
		} else {
			start := len(batch.Observations)
			record.Disposition = state.apply(batch, input, capture.source, capture.row, capture.times)
			state.captureFacets(capture.key, batch.Observations[start:], record.Disposition)
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

func scopedKey(scope model.Scope, facet, value string) string {
	return string(mustJSON([]any{scope.Provider, scope.Source, scope.Incarnation, scope.ConnectionID, scope.NativeThreadID, scope.NativeTurnID, scope.NativeParentCallID, facet, value}))
}
