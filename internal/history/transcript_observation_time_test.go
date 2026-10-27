package history

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestObservationTimesKeepAbsentNullAndNativeClocks(t *testing.T) {
	at := time.Date(2026, 9, 10, 8, 0, 1, 120000000, time.UTC)
	raw, err := json.Marshal(at)
	if err != nil {
		t.Fatal(err)
	}
	facts := []storedTimeFact{
		{Scope: 10, Ref: 20, Class: model.TimeNative, Meaning: model.TimeWrite},
		{Scope: 10, Ref: 20, Raw: json.RawMessage(`null`), Class: model.TimeNative, Meaning: model.TimeStart},
		{Scope: 10, Ref: 20, Value: &at, Raw: raw, Class: model.TimeNative, Field: "timestamp", Meaning: model.TimeEnd},
		{Scope: 30, Ref: 40, Value: &at, Raw: json.RawMessage(`"2026-09-10T08:00:01.120000Z"`), Class: model.TimeCapture, Precision: "microseconds", Meaning: model.TimeWrite},
		{Scope: 10, Ref: 40, Raw: json.RawMessage(`0`), Class: model.TimeNative, Meaning: model.TimeWrite},
	}
	compact, err := compactObservationTimes(facts, 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	for i, mode := range []uint8{0, 1, 1, 2, 2} {
		if compact[i].RawMode != mode {
			t.Fatalf("clock %d lost raw mode", i)
		}
	}
	if compact[0].Scope != 0 || compact[0].Ref != 0 || compact[3].Scope != 30 || compact[3].Ref != 40 {
		t.Fatal("inherited and distinct source references changed")
	}
	encoded, err := json.Marshal(compact)
	if err != nil {
		t.Fatal(err)
	}
	var restored []storedObservationTime
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	expanded, err := expandObservationTimes(restored, 10, 20)
	if err != nil || !reflect.DeepEqual(facts, expanded) {
		t.Fatalf("clock order, source or native representation changed: %v", err)
	}
}

func TestObservationTimesRejectMalformedTuple(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `[0,0,null,3,null,"native","","","write"]`, `[-1,0,null,0,null,"native","","","write"]`, `[0,0,null,1,0,"native","","","write"]`, `[0,0,"bad-time",0,null,"native","","","write"]`} {
		var fact storedObservationTime
		if err := json.Unmarshal([]byte(raw), &fact); err == nil {
			t.Fatalf("invalid clock tuple accepted: %s", raw)
		}
	}
}
