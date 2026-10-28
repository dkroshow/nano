package codex

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type processOutput struct {
	ItemID       string          `json:"itemId"`
	Stdout       []byte          `json:"stdout"`
	Stderr       []byte          `json:"stderr"`
	StdoutSeen   bool            `json:"stdoutSeen"`
	StderrSeen   bool            `json:"stderrSeen"`
	StdoutCap    bool            `json:"stdoutCap"`
	StderrCap    bool            `json:"stderrCap"`
	StdoutSource model.SourceRef `json:"stdoutSource"`
	StderrSource model.SourceRef `json:"stderrSource"`
}

func (state *State) processUpdate(batch *model.Batch, obs model.Observation, method string, params map[string]json.RawMessage) model.Disposition {
	obs.Detail = mustJSON(map[string]any{"method": method, "params": params})
	scope := obs.Scope
	scope.Kind, scope.NativeThreadID, scope.NativeTurnID, scope.TurnKey, scope.ItemID = model.ScopeConnection, nil, nil, "", ""
	obs = bindObservation(obs, scope)
	family, field := "process", "processHandle"
	if method == "command/exec/outputDelta" {
		family, field = "command/exec", "processId"
	}
	id := rawString(params, field)
	if id == "" {
		return state.unknown(batch, obs, "missing_process_id")
	}
	key := scopedKey(scope, "process_output", family+"/"+id)
	output := state.Processes[key]
	exited := method == "process/exited"
	if !exited {
		stream := rawString(params, "stream")
		if stream != "stdout" && stream != "stderr" {
			return state.unknown(batch, obs, "invalid_process_stream")
		}
		var encoded string
		if json.Unmarshal(params["deltaBase64"], &encoded) != nil || string(params["deltaBase64"]) == "null" {
			return state.unknown(batch, obs, "invalid_process_base64")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return state.unknown(batch, obs, "invalid_process_base64")
		}
		capReached, valid := nativeBool(params["capReached"])
		if !valid {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_process_cap", obs.Source))
		}
		if stream == "stdout" {
			output.Stdout = append(append([]byte(nil), output.Stdout...), data...)
			output.StdoutSeen, output.StdoutSource, output.StdoutCap = true, obs.Source, output.StdoutCap || capReached
		} else {
			output.Stderr = append(append([]byte(nil), output.Stderr...), data...)
			output.StderrSeen, output.StderrSource, output.StderrCap = true, obs.Source, output.StderrCap || capReached
		}
	} else {
		for _, stream := range []string{"stdout", "stderr"} {
			var text *string
			if json.Unmarshal(params[stream], &text) != nil || text == nil {
				obs.Capture = model.CapturePartial
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_process_buffer", obs.Source))
				continue
			}
			data := []byte(*text)
			prior, seen, source, capReached := &output.Stdout, &output.StdoutSeen, &output.StdoutSource, &output.StdoutCap
			if stream == "stderr" {
				prior, seen, source, capReached = &output.Stderr, &output.StderrSeen, &output.StderrSource, &output.StderrCap
			}
			// A streamed exit contains empty buffers. A nonempty conflicting
			// buffer gives no offset proving overlap with previously sent bytes.
			if !*seen {
				*prior, *seen, *source = data, true, obs.Source
			} else if len(data) > 0 && !bytes.Equal(*prior, data) {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("process_output_overlap_unproven", obs.Source))
			}
			if capped, valid := nativeBool(params[stream+"CapReached"]); valid {
				*capReached = *capReached || capped
			} else {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_process_cap", obs.Source))
			}
		}
		var exit *int32
		obs.Outcome = model.OutcomeUnknown
		if json.Unmarshal(params["exitCode"], &exit) != nil || exit == nil {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_process_exit", obs.Source))
		} else if *exit == 0 {
			obs.Outcome = model.OutcomeCompleted
		} else {
			obs.Outcome = model.OutcomeFailed
		}
		complete := true
		obs.Complete = &complete
	}
	item := state.Items[output.ItemID]
	if item.ID == "" {
		item = model.Item{Type: model.ItemToolResult, Category: model.CategoryToolResult, Role: model.RoleTool, SourceRowType: "wire", SourcePayloadType: method, Aliases: []model.Alias{{Namespace: "codex." + field, Value: mustJSON(id), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}}
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unresolved_process_invocation", obs.Source))
	}
	item.Detail = obs.Detail
	item.Parts = nil
	var texts []string
	for _, stream := range []struct {
		name      string
		data      []byte
		seen, cap bool
		source    model.SourceRef
	}{{"stdout", output.Stdout, output.StdoutSeen, output.StdoutCap, output.StdoutSource}, {"stderr", output.Stderr, output.StderrSeen, output.StderrCap, output.StderrSource}} {
		if !stream.seen {
			continue
		}
		part := model.Part{Type: stream.name, Availability: model.AvailableInline, Source: stream.source, Detail: mustJSON(map[string]any{"encoding": "base64", "data": base64.StdEncoding.EncodeToString(stream.data), "capturedBytes": len(stream.data)})}
		if utf8.Valid(stream.data) {
			part.Text = string(stream.data)
		} else if exited {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("non_utf8_process_output", obs.Source))
		}
		if stream.cap {
			part.Availability = model.AvailableTruncated
		}
		item.Parts = append(item.Parts, part)
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	item.ContentTruncated = output.StdoutCap || output.StderrCap
	if item.ContentTruncated {
		obs.Capture = model.CaptureTruncated
	}
	obs.Kind, obs.Text = model.ObservationSnapshot, strings.Join(texts, "\n")
	if item.ID == "" {
		item = state.emitScopedItem(batch, obs, item)
		output.ItemID = item.ID
	} else {
		state.observeRequestItem(batch, item, obs)
	}
	if exited {
		// Exit ends byte assembly even when the captured exit code is unknown.
		// The emitted result remains with the consumer and keeps its native alias.
		delete(state.Processes, key)
		state.dropItem(output.ItemID)
	} else {
		state.Processes[key] = output
	}
	return model.RecordSemantic
}
