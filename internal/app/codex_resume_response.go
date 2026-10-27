package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// codexResumeResponse owns one decoded reply for the lifetime of a handshake.
// Keep unknown fields and JSON numbers intact for the compact state/wire view.
// Only selected turns pass through the canonical item parser.
type codexResumeResponse struct {
	fields          map[string]any
	thread          map[string]any
	turns           []any
	parsedItems     map[[2]string]model.Item
	parsedFrom      int
	projection      []codexResumeProjectionTurn
	projectionRead  bool
	projectionValid bool
}

func decodeCodexResumeResponse(raw []byte) (*codexResumeResponse, error) {
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values in Codex response")
		}
		return nil, err
	}
	return newCodexResumeResponse(fields), nil
}

func newCodexResumeResponse(fields map[string]any) *codexResumeResponse {
	thread, _ := fields["thread"].(map[string]any)
	turns, _ := thread["turns"].([]any)
	return &codexResumeResponse{fields: fields, thread: thread, turns: turns, parsedFrom: -1}
}

func copyCodexFields(fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	return out
}

func (r *codexResumeResponse) compactFields() map[string]any {
	fields := copyCodexFields(r.fields)
	if _, present := r.thread["turns"]; present {
		thread := copyCodexFields(r.thread)
		thread["turns"] = []any{}
		fields["thread"] = thread
	}
	return fields
}

func (r *codexResumeResponse) compact() json.RawMessage {
	raw, _ := json.Marshal(r.compactFields())
	return raw
}

// inFlightTurn reports the turn a thread/resume response says is still
// running. It is the codex half of surviving a nano restart mid-turn, and it
// reads `thread.turns` from the full response before compacting stored state.
//
// Probed live 2026-07-31 against the supervised app-server, resuming into a
// running turn from a second connection: the last entry carried
// {"id":…,"status":"inProgress","startedAt":1785510514,"completedAt":null}.
// Only the LAST entry can be in flight — turns are ordered and a thread runs
// one at a time — and only `inProgress` counts, so a thread whose newest turn
// finished while nano was down comes back idle rather than falsely working.
//
// A response with no turns, or an unparseable one, yields nothing: unknown
// working state renders as no dot and no timer, never as an invented start
// (D2/D8).

func (r *codexResumeResponse) inFlightTurn() (codexTurn, bool) {
	// Decode only the three small fields used by this consumer, preserving its
	// strict type checks without traversing the turn's item history again.
	var last codexTurn
	for _, value := range r.turns {
		last = codexTurn{}
		turn, object := value.(map[string]any)
		if !object && value != nil {
			return codexTurn{}, false
		}
		fields := map[string]any{}
		for _, key := range []string{"id", "status", "startedAt"} {
			if value, ok := turn[key]; ok {
				fields[key] = value
			}
		}
		raw, _ := json.Marshal(fields)
		if json.Unmarshal(raw, &last) != nil {
			return codexTurn{}, false
		}
	}
	return last, last.ID != "" && last.Status == codexTurnInProgressStatus
}

func (r *codexResumeResponse) projectionTurns() ([]codexResumeProjectionTurn, bool) {
	if r.projectionRead {
		return r.projection, r.projectionValid
	}
	r.projectionRead = true
	turns := make([]codexResumeProjectionTurn, 0, len(r.turns))
	for _, value := range r.turns {
		fields, ok := value.(map[string]any)
		if !ok && value != nil {
			return nil, false
		}
		header := copyCodexFields(fields)
		delete(header, "items")
		raw, _ := json.Marshal(header)
		var turn codexResumeProjectionTurn
		if json.Unmarshal(raw, &turn) != nil {
			return nil, false
		}
		if items, exists := fields["items"]; exists && items != nil {
			var ok bool
			turn.Items, ok = items.([]any)
			if !ok {
				return nil, false
			}
		}
		turns = append(turns, turn)
	}
	r.projection, r.projectionValid = turns, true
	return turns, true
}

func codexObjectString(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}
