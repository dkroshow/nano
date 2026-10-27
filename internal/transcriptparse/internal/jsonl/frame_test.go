package jsonl

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestEveryByteSplitPreservesRecordsAndCheckpoint(t *testing.T) {
	input := []byte("{\"text\":\"雪\"}\r\n\n{broken}\n{\"pending\":")
	firstEnd := bytes.IndexByte(input, '\n') + 1
	want := []Record{
		{Line: 1, Start: 0, End: int64(firstEnd), Bytes: input[:firstEnd-2], Terminated: true},
		{Line: 2, Start: int64(firstEnd), End: int64(firstEnd + 1), Bytes: []byte{}, Terminated: true},
		{Line: 3, Start: int64(firstEnd + 1), End: int64(firstEnd + 10), Bytes: []byte("{broken}"), Terminated: true},
	}
	for split := 0; split <= len(input); split++ {
		state, left, err := Feed(State{}, input[:split], 1024, false)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		var restored State
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal(err)
		}
		next, right, err := Feed(restored, input[split:], 1024, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := append(left, right...); !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d records: %#v; want %#v", split, got, want)
		}
		if next.Offset != int64(len(input)) || next.Checkpoint != int64(firstEnd+10) || next.Line != 4 || string(next.Pending) != "{\"pending\":" {
			t.Fatalf("split %d checkpoint: %+v", split, next)
		}
	}
}

func TestTailRequiresExplicitClose(t *testing.T) {
	state, records, err := Feed(State{}, []byte(`{"ok":true}`), 128, false)
	if err != nil || len(records) != 0 || state.Checkpoint != 0 {
		t.Fatalf("premature tail consumption: %+v %v %v", state, records, err)
	}
	closed, records, err := Feed(state, nil, 128, true)
	if err != nil || len(records) != 1 || records[0].Terminated || string(records[0].Bytes) != `{"ok":true}` || closed.Checkpoint != 11 {
		t.Fatalf("closed tail: %+v %+v %v", closed, records, err)
	}
	if _, _, err := Feed(closed, []byte("\n"), 128, false); err == nil {
		t.Fatal("accepted append after explicit close")
	}
}

func TestOversizedLineIsBoundedAndNextLineSurvives(t *testing.T) {
	input := []byte("123456789\n{}\n")
	for split := 0; split <= len(input); split++ {
		state, left, err := Feed(State{}, input[:split], 4, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Pending) > 4 {
			t.Fatal("unbounded pending data")
		}
		next, right, err := Feed(state, input[split:], 4, false)
		if err != nil {
			t.Fatal(err)
		}
		got := append(left, right...)
		if len(got) != 2 || got[0].Problem != "row_too_large" || got[0].Start != 0 || got[0].End != 10 || string(got[0].Bytes) != "1234" || got[1].Problem != "" || string(got[1].Bytes) != "{}" || next.Checkpoint != 13 {
			t.Fatalf("split %d: %+v %+v", split, got, next)
		}
	}
}

func TestLimitExcludesCRLFAndFramingRetainsInvalidBytes(t *testing.T) {
	for _, input := range [][]byte{[]byte("1234\r\n"), []byte("1234\n"), {0xff, '\n'}} {
		for split := 0; split <= len(input); split++ {
			state, a, err := Feed(State{}, input[:split], 4, false)
			if err != nil {
				t.Fatal(err)
			}
			_, b, err := Feed(state, input[split:], 4, false)
			if err != nil {
				t.Fatal(err)
			}
			rows := append(a, b...)
			if len(rows) != 1 || rows[0].Problem != "" || !bytes.Equal(rows[0].Bytes, bytes.TrimSuffix(bytes.TrimSuffix(input, []byte{'\n'}), []byte{'\r'})) {
				t.Fatalf("split %d changed source bytes: %+v", split, rows)
			}
		}
	}
}

func TestFeedDoesNotMutateInputOrPriorState(t *testing.T) {
	input := []byte("abc")
	prior, _, err := Feed(State{}, input, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = 'z'
	before, _ := json.Marshal(prior)
	next, rows, err := Feed(prior, []byte("def\nx"), 100, false)
	if err != nil {
		t.Fatal(err)
	}
	next.Pending[0] = 'y'
	rows[0].Bytes[0] = 'z'
	after, _ := json.Marshal(prior)
	if !bytes.Equal(before, after) || string(prior.Pending) != "abc" {
		t.Fatalf("prior state mutated: %s -> %s", before, after)
	}
}

func TestRejectsIncompatibleContinuation(t *testing.T) {
	state, _, err := Feed(State{}, []byte("part"), 10, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*State){
		func(s *State) { s.Version++ },
		func(s *State) { s.Checkpoint = s.Offset + 1 },
		func(s *State) { s.Line = 0 },
		func(s *State) { s.Pending = []byte("short") },
	} {
		bad := state
		mutate(&bad)
		if _, _, err := Feed(bad, nil, 10, false); err == nil {
			t.Fatalf("accepted invalid state: %+v", bad)
		}
	}
	if _, _, err := Feed(state, nil, 20, false); err == nil {
		t.Fatal("accepted changed limit")
	}
	if _, _, err := Feed(State{}, nil, 0, false); err == nil {
		t.Fatal("accepted nonpositive limit")
	}
}
