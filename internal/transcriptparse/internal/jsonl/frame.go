package jsonl

import (
	"bytes"
	"math"
)

const stateVersion = 1

// State holds a bounded unfinished row and its complete-record checkpoint.
// Offset includes pending bytes; Checkpoint advances only when a record emits.
type State struct {
	Version    int    `json:"version"`
	Limit      int    `json:"limit"`
	Offset     int64  `json:"offset"`
	Checkpoint int64  `json:"checkpoint"`
	Line       int64  `json:"line"`
	Pending    []byte `json:"pending,omitempty"`
	Overflow   bool   `json:"overflow,omitempty"`
	TrailingCR bool   `json:"trailingCR,omitempty"`
	Closed     bool   `json:"closed,omitempty"`
}

type Record struct {
	Line       int64  `json:"line"`
	Start      int64  `json:"start"`
	End        int64  `json:"end"`
	Bytes      []byte `json:"bytes"`
	Terminated bool   `json:"terminated"`
	Problem    string `json:"problem,omitempty"`
}

// Error reports an unusable checkpoint or limit. Record defects use Problem.
type Error struct{ Code string }

func (e *Error) Error() string { return "jsonl: " + e.Code }

// Feed returns owned records and state, leaving the input and prior state intact.
// Limits count content bytes, excluding LF and its optional preceding CR.
// A growing source keeps its final unterminated row pending. Only an explicit
// close emits that tail, marked Terminated=false for the decoder to interpret.
func Feed(state State, input []byte, limit int, closed bool) (State, []Record, error) {
	prior := state
	if limit <= 0 {
		return prior, nil, &Error{Code: "invalid_limit"}
	}
	if state.Version == 0 {
		if state.Limit != 0 || state.Offset != 0 || state.Checkpoint != 0 || state.Line != 0 || len(state.Pending) != 0 || state.Overflow || state.TrailingCR || state.Closed {
			return prior, nil, &Error{Code: "invalid_state"}
		}
		state.Version, state.Limit, state.Line = stateVersion, limit, 1
	}
	if !validState(state) || state.Limit != limit {
		return prior, nil, &Error{Code: "incompatible_state"}
	}
	if state.Closed {
		return prior, nil, &Error{Code: "source_closed"}
	}
	if int64(len(input)) > math.MaxInt64-state.Offset {
		return prior, nil, &Error{Code: "offset_overflow"}
	}
	state.Pending = bytes.Clone(state.Pending)
	var records []Record
	for len(input) > 0 {
		end := bytes.IndexByte(input, '\n')
		terminated := end >= 0
		if !terminated {
			end = len(input)
		}
		segment := input[:end]
		retain := limit - len(state.Pending)
		if retain > len(segment) {
			retain = len(segment)
		}
		state.Pending = append(state.Pending, segment[:retain]...)
		state.Offset += int64(len(segment))
		length := state.Offset - state.Checkpoint
		if len(segment) > 0 && length > int64(limit) {
			// Keep at most limit bytes, while permitting a CR split from its LF.
			state.TrailingCR = !state.Overflow && length-1 == int64(limit) && segment[len(segment)-1] == '\r'
			state.Overflow = !state.TrailingCR
		}
		input = input[end:]
		if terminated {
			state.Offset++
			records = append(records, emit(&state, true))
			input = input[1:]
		}
	}
	if closed {
		if state.Offset > state.Checkpoint {
			if state.TrailingCR {
				state.Overflow = true
			}
			records = append(records, emit(&state, false))
		}
		state.Closed = true
	}
	return state, records, nil
}

func emit(state *State, terminated bool) Record {
	data := state.Pending
	if terminated && !state.Overflow && !state.TrailingCR {
		data = bytes.TrimSuffix(data, []byte{'\r'})
	}
	record := Record{Line: state.Line, Start: state.Checkpoint, End: state.Offset, Bytes: append([]byte{}, data...), Terminated: terminated}
	if state.Overflow {
		record.Problem = "row_too_large"
	}
	state.Checkpoint, state.Line = state.Offset, state.Line+1
	state.Pending, state.Overflow, state.TrailingCR = nil, false, false
	return record
}

func validState(state State) bool {
	if state.Version != stateVersion || state.Limit <= 0 || state.Checkpoint < 0 || state.Offset < state.Checkpoint || state.Line < 1 || state.Line > state.Checkpoint+1 {
		return false
	}
	length := state.Offset - state.Checkpoint
	if state.Closed && length != 0 {
		return false
	}
	if length <= int64(state.Limit) {
		return int64(len(state.Pending)) == length && !state.Overflow && !state.TrailingCR
	}
	if len(state.Pending) != state.Limit {
		return false
	}
	return (state.Overflow && !state.TrailingCR) || (!state.Overflow && state.TrailingCR && length-1 == int64(state.Limit))
}
