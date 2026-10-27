package jsonl

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func FuzzChunkingMatchesPhysicalRecords(f *testing.F) {
	for _, seed := range [][]byte{nil, []byte("a\r\nb\n"), []byte("1234\r\nnext\nlast"), []byte("\n\r\n\r"), {0xff, 0xfe, '\n'}, []byte("雪\n{\"incomplete\":")} {
		f.Add(seed, uint8(4), uint8(3), true)
	}
	f.Fuzz(func(t *testing.T, input []byte, size, stride uint8, close bool) {
		limit, chunk := int(size)+1, int(stride)+1
		var state State
		var got []Record
		for offset := 0; offset < len(input); {
			end := offset + chunk
			if end > len(input) {
				end = len(input)
			}
			next, records, err := Feed(state, input[offset:end], limit, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(next.Pending) > limit {
				t.Fatal("unbounded state")
			}
			encoded, err := json.Marshal(next)
			if err != nil {
				t.Fatal(err)
			}
			var restored State
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			state = restored
			got = append(got, records...)
			offset = end
		}
		next, records, err := Feed(state, nil, limit, close)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, records...)
		var want []Record
		start := 0
		for i := 0; i < len(input); i++ {
			if input[i] != '\n' && !(close && i == len(input)-1) {
				continue
			}
			terminated := input[i] == '\n'
			data := input[start : i+1]
			if terminated {
				data = bytes.TrimSuffix(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\r'})
			}
			row := Record{Line: int64(len(want) + 1), Start: int64(start), End: int64(i + 1), Terminated: terminated}
			if len(data) > limit {
				row.Problem = "row_too_large"
				data = data[:limit]
			}
			row.Bytes = append([]byte{}, data...)
			want = append(want, row)
			start = i + 1
		}
		if !reflect.DeepEqual(got, want) || next.Checkpoint != int64(start) || next.Offset != int64(len(input)) {
			t.Fatalf("chunked records differ from physical input: got %+v, want %+v; state %+v", got, want, next)
		}
	})
}
