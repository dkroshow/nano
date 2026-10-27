package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestMessageBlockIdentityAgreesAcrossReaders(t *testing.T) {
	file, _, err := parseFileView(parsedView{}, inputFor([]byte(`{"type":"assistant","uuid":"row","apiBlockIndex":2,"message":{"id":"message:part","content":[{"type":"text","text":"Answer"},{"type":"tool_use","id":"call","name":"Read","input":{"file_path":"example"}}]}}`+"\n")))
	if err != nil {
		t.Fatal(err)
	}
	var wire parsedView
	for _, row := range []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"message:part"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":2,"content_block":{"type":"text","text":"Answer"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call","name":"Read","input":{"file_path":"example"}}}}`,
	} {
		wire, _, err = parseWireView(wire, inputFor([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
		checkpoint, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		wire = parsedView{}
		if err := json.Unmarshal(checkpoint, &wire); err != nil {
			t.Fatal(err)
		}
	}
	fitems, witems := orderedItems(file), orderedItems(wire)
	if len(fitems) != 2 || len(witems) != 2 {
		t.Fatal("missing file/wire content")
	}
	for i, want := range []string{"message:part:2", "message:part:3"} {
		for _, item := range []model.Item{fitems[i], witems[i]} {
			if item.NativeItemID != want {
				t.Fatalf("native item identity %q, want %q", item.NativeItemID, want)
			}
			aliases := map[string]model.Alias{}
			for _, alias := range item.Aliases {
				aliases[alias.Namespace] = alias
			}
			composite := aliases["claude.messageBlock"]
			if string(composite.Value) != string(mustJSON(want)) || composite.Authority != model.BasisNative || len(composite.ObservationRefs) == 0 {
				t.Fatal("composite identity lacks derivation evidence")
			}
			if string(aliases["claude.apiMessageId"].Value) != `"message:part"` || string(aliases["claude.blockIndex"].Value) != string(mustJSON(i+2)) {
				t.Fatal("composite replaced native components")
			}
		}
	}
	if fitems[1].CallID != "call" || witems[1].CallID != "call" {
		t.Fatal("call identity was replaced by message identity")
	}
}

func TestMissingAPIMessageIDDoesNotInventComposite(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor([]byte(`{"type":"assistant","uuid":"row-only","message":{"content":[{"type":"text","text":"Answer"}]}}`+"\n")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].NativeItemID != "" {
		t.Fatal("row UUID became an API message identity")
	}
}

func TestCountedBlockIdentityIsProvisionalAfterEviction(t *testing.T) {
	raw, err := os.ReadFile("testdata/replay-block-index-reset.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []bool{false, true} {
		var view parsedView
		var last model.Batch
		for _, row := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
			parse, suffix := parseFileView, []byte{'\n'}
			if wire {
				parse, suffix = parseWireView, nil
			}
			view, last, err = parse(view, inputFor(append(append([]byte(nil), row...), suffix...)))
			if err != nil {
				t.Fatal(err)
			}
		}
		items := orderedItems(view)
		var blocks []model.Item
		for _, item := range items {
			if item.CallID != "" {
				blocks = append(blocks, item)
			}
		}
		if len(blocks) != 3 {
			t.Fatalf("wire=%t blocks=%d", wire, len(blocks))
		}
		for i, want := range []string{"message:0", "message:1", "message:0"} {
			item := blocks[i]
			found := false
			for _, alias := range item.Aliases {
				if alias.Namespace == "claude.messageBlock" {
					found = alias.Authority == model.BasisDerived
				}
			}
			if item.NativeItemID != want || !found {
				t.Fatalf("wire=%t block=%d counted identity claims exactness", wire, i)
			}
		}
		found := false
		for _, d := range last.Diagnostics {
			found = found || d.Code == "provisional_block_index"
		}
		if !found {
			t.Fatal("counting without retained message lacks diagnostic")
		}
	}
}
