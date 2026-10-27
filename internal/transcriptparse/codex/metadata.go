package codex

import (
	"bytes"
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// The history and session-index files can interleave unrelated threads. Their
// entries are auxiliary facts, never conversation prompts or session defaults.
func (state *State) auxiliary(batch *model.Batch, input model.Input, source model.SourceRef, row map[string]json.RawMessage) bool {
	if _, present := row["type"]; present {
		return false
	}
	id, field, unit := "", "", ""
	switch {
	case row["thread_name"] != nil && row["updated_at"] != nil:
		id, field, unit = rawString(row, "id"), "updated_at", "native_string"
	case row["text"] != nil && row["ts"] != nil:
		id, field, unit = rawString(row, "session_id"), "ts", "second"
	}
	if id == "" {
		return false
	}
	scope := state.scope(input, nil)
	scope.NativeThreadID, scope.NativeTurnID = &id, nil
	obs := observation(source, scope, row)
	stamp := nativeStamp(row[field], field, unit, model.TimeEvent, scope, obs.ID)
	obs.Times = append([]model.TimeObservation{stamp}, callerTimes(input, scope, obs.ID)...)
	if stamp.Value == nil && !bytes.Equal(bytes.TrimSpace(row[field]), []byte("null")) {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_native_time", source))
	}
	batch.Observations = append(batch.Observations, obs)
	return true
}
