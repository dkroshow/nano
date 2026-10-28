package codex

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

type pendingSettings struct {
	Tier        string            `json:"tier"`
	Observation model.Observation `json:"observation"`
}

// State is a versioned checkpoint. Treat its fields as immutable between calls;
// encoding/json can save and restore it. A new source incarnation starts at zero.
type State struct {
	PendingSettings map[string]pendingSettings  `json:"pendingSettings,omitempty"`
	Catalog         map[string]assemble.Catalog `json:"catalog,omitempty"`
	Version         int                         `json:"version"`
	Source          model.SourceRef             `json:"source"`
	Framing         jsonl.State                 `json:"framing"`
	Frame           int64                       `json:"frame"`
	Limit           int                         `json:"limit"`
	NativeThreads   map[string]string           `json:"nativeThreads"`
	UsageTotals     map[string]model.TokenUsage `json:"usageTotals"`
	Requests        map[string]rpcRequest       `json:"requests"`
	UpdateItems     map[string]string           `json:"updateItems"`
	Processes       map[string]processOutput    `json:"processes"`
	Items           map[string]model.Item       `json:"items"`
	Turns           map[string]model.Turn       `json:"turns"`
	NativeTurns     map[string]string           `json:"nativeTurns"`
	NativeItems     map[string]string           `json:"nativeItems"`
	ActiveTurns     map[string]string           `json:"activeTurns"`
	Twins           map[string][]string         `json:"twins"`
	NewestItems     map[string]string           `json:"newestItems"`
	Calls           map[string]string           `json:"calls"`
	// A turn's usage observations, one entry each under usageKey, so a token
	// count adds an entry instead of rewriting the turn; Turns holds no list.
	// UsageCounts holds each turn's number of observations.
	Usage       map[string]model.UsageObservation `json:"usage"`
	UsageCounts map[string]int                    `json:"usageCounts"`
	NextItem    int64                             `json:"nextItem"`
	NextTurn    int                               `json:"nextTurn"`

	// Continuation size accounting, deliberately unpersisted: measured is the
	// last whole-state encoding, pending the input consumed since, and
	// measurements the number of encodings taken. A restored checkpoint simply
	// measures again once enough new input has arrived.
	measured     int
	pending      int
	measurements int

	// The turn changes this call emitted, each with its usage count then;
	// fillUsage gives them their lists once, before the call returns.
	turnChanges []turnChange
	// changed names the items, turns and usage entries set or deleted since
	// ForgetChanged, by checkpoint field; nil means unknown.
	changed map[string]map[string]bool
}

// checkpointVersion is State.Version: 23 keeps usage samples in Usage.
const checkpointVersion = 23

// ParseFile consumes chunks of Codex native JSONL. An open file's unterminated tail
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
	next.fillUsage("")
	if next.Framing.Closed {
		next = next.closedCheckpoint()
	}
	next.checkpointDiagnostic(&batch)
	return next, batch, nil
}

// ParseWire consumes one complete Codex stream-json frame. The caller supplies any capture and
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
	next.fillUsage("")
	return next, batch, nil
}

func prepare(state State, input model.Input, kind string) (State, model.Batch, error) {
	batch := model.Batch{Versions: model.Versions{
		Schema: "transcript-v3", Reader: "codex-reader-v37", Assembly: "observations-v3",
		Taxonomy: "history-v1", Usage: "native-grain-v1", Checkpoint: 23,
	}}
	if input.Format != "" && input.Format != "jsonl" {
		return state, batch, readerError("unsupported_format", input.Source)
	}
	source := model.SourceRef{Kind: kind, Label: input.Source.Label, Incarnation: input.Source.Incarnation}
	if source.Label == "" || source.Incarnation == "" || (input.Scope.Provider != "" && input.Scope.Provider != model.ProviderCodex) {
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
		state.Version, state.Source, state.Limit = checkpointVersion, source, limit
	} else if state.Version != checkpointVersion && state.Version != 22 || state.Source != source || state.Limit != limit {
		return state, batch, readerError("incompatible_checkpoint", source)
	}
	state.pending += len(input.Bytes)
	state.PendingSettings = copyMap(state.PendingSettings)
	state.Catalog = copyMap(state.Catalog)
	state.Items = copyMap(state.Items)
	state.NativeThreads = copyMap(state.NativeThreads)
	state.UsageTotals = copyMap(state.UsageTotals)
	state.Requests = copyMap(state.Requests)
	state.UpdateItems = copyMap(state.UpdateItems)
	state.Processes = copyMap(state.Processes)
	state.Turns = copyMap(state.Turns)
	state.NativeTurns = copyMap(state.NativeTurns)
	state.NativeItems = copyMap(state.NativeItems)
	state.ActiveTurns = copyMap(state.ActiveTurns)
	state.Twins = copyMap(state.Twins)
	state.NewestItems = copyMap(state.NewestItems)
	state.Calls = copyMap(state.Calls)
	state.Usage = copyMap(state.Usage)
	state.UsageCounts = copyMap(state.UsageCounts)
	if state.changed != nil {
		changed := make(map[string]map[string]bool, len(state.changed))
		for field, keys := range state.changed {
			changed[field] = copyMap(keys)
		}
		state.changed = changed
	}
	// Version 22 kept usage samples in Turns. A reader that knows only 22
	// rejects 23, so after a rollback it rebuilds instead of reading a turn
	// without its samples.
	state.Version = checkpointVersion
	for key, turn := range state.Turns {
		// A checkpoint saved before usage moved out of Turns.
		if len(turn.UsageObservations) > 0 {
			for _, usage := range turn.UsageObservations {
				state.appendUsage(key, usage)
			}
			turn.UsageObservations = nil
			state.setTurn(turn)
		}
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
		Scope:   model.Scope{Kind: model.ScopeThread, Provider: model.ProviderCodex, Source: source.Label, Incarnation: source.Incarnation},
		Message: "Codex source record or checkpoint could not be interpreted",
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
		record.Disposition = state.apply(batch, input, source, row)
	}
	batch.Records = append(batch.Records, record)
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
	return string(mustJSON([]any{scope.Provider, scope.Source, scope.Incarnation, scope.ConnectionID, scope.NativeThreadID, scope.NativeTurnID, facet, value}))
}
