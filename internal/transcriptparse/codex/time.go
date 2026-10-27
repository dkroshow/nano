package codex

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"time"
)

func nativeStamp(raw json.RawMessage, field, unit string, meaning model.TimeMeaning, scope model.Scope, ref string) model.TimeObservation {
	stamp := model.TimeObservation{Scope: scope, Class: model.TimeNative, Raw: raw, SourceField: field, Precision: unit, Meaning: meaning, ObservationRef: ref}
	if len(raw) == 0 || string(raw) == "null" {
		return stamp
	}
	var value time.Time
	if unit == "native_string" {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return stamp
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return stamp
		}
		value = parsed.UTC()
	} else {
		var number int64
		if json.Unmarshal(raw, &number) != nil {
			return stamp
		}
		if unit == "millisecond" {
			value = time.UnixMilli(number).UTC()
		} else {
			value = time.Unix(number, 0).UTC()
		}
	}
	if value.Year() >= 0 && value.Year() <= 9999 {
		stamp.Value = &value
	}
	return stamp
}
func callerTimes(input model.Input, scope model.Scope, ref string) []model.TimeObservation {
	var times []model.TimeObservation
	for _, clock := range []struct {
		value *time.Time
		class model.TimeClass
		field string
	}{{input.CaptureAt, model.TimeCapture, "caller.captureAt"}, {input.ObservedAt, model.TimeObserved, "caller.observedAt"}} {
		if clock.value != nil {
			value := clock.value.UTC()
			times = append(times, model.TimeObservation{Scope: scope, Value: &value, Class: clock.class, SourceField: clock.field, Meaning: model.TimeEvent, Precision: "caller_supplied", ObservationRef: ref})
		}
	}
	return times
}
func fileTimes(input model.Input, row, payload map[string]json.RawMessage, scope model.Scope, ref string) []model.TimeObservation {
	field := "timestamp"
	raw := row[field]
	if len(raw) == 0 {
		field = "ts"
		raw = row[field]
	}
	times := []model.TimeObservation{nativeStamp(raw, field, "native_string", model.TimeWrite, scope, ref)}
	for _, name := range []string{"timestamp", "created_at", "createdAt"} {
		if raw, ok := payload[name]; ok {
			times = append(times, nativeStamp(raw, "payload."+name, "native_string", model.TimeEvent, scope, ref))
		}
	}
	for _, entry := range []struct {
		field, unit string
		meaning     model.TimeMeaning
	}{{"occurred_at_ms", "millisecond", model.TimeEvent}, {"started_at", "second", model.TimeStart}, {"completed_at", "second", model.TimeEnd}} {
		if raw, ok := payload[entry.field]; ok {
			times = append(times, nativeStamp(raw, "payload."+entry.field, entry.unit, entry.meaning, scope, ref))
		}
	}
	return append(times, callerTimes(input, scope, ref)...)
}
func wireTimes(input model.Input, capture json.RawMessage, scope model.Scope, ref string) []model.TimeObservation {
	times := []model.TimeObservation{{Scope: scope, Class: model.TimeNative, Meaning: model.TimeEvent, SourceField: "envelope", ObservationRef: ref}}
	if len(capture) > 0 {
		stamp := nativeStamp(capture, "capture.ts", "native_string", model.TimeEvent, scope, ref)
		stamp.Class = model.TimeCapture
		times = append(times, stamp)
	}
	return append(times, callerTimes(input, scope, ref)...)
}
func nativeItemTimes(payload map[string]json.RawMessage, scope model.Scope, ref string) []model.TimeObservation {
	var times []model.TimeObservation
	for _, entry := range []struct {
		field   string
		meaning model.TimeMeaning
	}{{"startedAtMs", model.TimeStart}, {"completedAtMs", model.TimeEnd}, {"started_at_ms", model.TimeStart}, {"completed_at_ms", model.TimeEnd}} {
		if raw, ok := payload[entry.field]; ok {
			times = append(times, nativeStamp(raw, entry.field, "millisecond", entry.meaning, scope, ref))
		}
	}
	return times
}
func nativeTurnTimes(payload map[string]json.RawMessage, scope model.Scope, ref string) []model.TimeObservation {
	var times []model.TimeObservation
	for _, entry := range []struct {
		field   string
		meaning model.TimeMeaning
	}{{"startedAt", model.TimeStart}, {"completedAt", model.TimeEnd}} {
		if raw, ok := payload[entry.field]; ok {
			times = append(times, nativeStamp(raw, entry.field, "second", entry.meaning, scope, ref))
		}
	}
	return times
}
