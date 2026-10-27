package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGenuinePairedAssistantExcerpt(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile("testdata/captured/paired-assistant-excerpt/" + name + ".jsonl")
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	input := func(raw []byte) model.Input {
		return model.Input{Bytes: raw, Source: model.SourceRef{Label: "genuine-pair", Incarnation: "excerpt"}, Closed: true}
	}
	file, batch, err := parseFileView(parsedView{}, input(read("file")))
	if err != nil || len(batch.Diagnostics) != 0 {
		t.Fatalf("file: %v, %+v", err, batch.Diagnostics)
	}
	var wire parsedView
	for _, raw := range bytes.Split(bytes.TrimSpace(read("wire")), []byte{'\n'}) {
		wire, batch, err = parseWireView(wire, input(raw))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("wire: %v, %+v", err, batch.Diagnostics)
		}
	}
	fitems, witems := orderedItems(file), orderedItems(wire)
	if len(fitems) != 2 || len(witems) != 2 || len(file.Turns) != 1 || len(wire.Turns) != 1 {
		t.Fatal("split assistant snapshots duplicated streamed blocks or invented prompt turns")
	}
	for i, expected := range []struct {
		kind model.ItemType
		text string
	}{{model.ItemReasoning, ""}, {model.ItemAgentMessage, "OK"}} {
		for _, item := range []model.Item{fitems[i], witems[i]} {
			if item.Type != expected.kind || item.Text != expected.text || !item.Complete || item.NativeItemID != fmt.Sprintf("fixture-id-2:%d", i) {
				t.Errorf("block %d lost its native content or generation state", i)
			}
			if item.Scope.NativeThreadID == nil || *item.Scope.NativeThreadID != "fixture-id-5" || item.Scope.NativeTurnID != nil {
				t.Errorf("block %d lost native session or invented native turn", i)
			}
			aliases := map[string]json.RawMessage{}
			for _, alias := range item.Aliases {
				if alias.Authority == model.BasisNative {
					aliases[alias.Namespace] = alias.Value
				}
			}
			if string(aliases["claude.apiMessageId"]) != `"fixture-id-2"` || string(aliases["claude.apiRequestId"]) != `"fixture-id-3"` || string(aliases["claude.blockIndex"]) != string(mustJSON(i)) {
				t.Errorf("block %d lost file/wire native aliases: %v", i, aliases)
			}
		}
		if !reflect.DeepEqual(fitems[i].CreatedAt, witems[i].CreatedAt) || fitems[i].CreatedAt == nil {
			t.Errorf("block %d did not retain matching native snapshot time", i)
		}
		first := witems[i].Observations[0]
		if len(first.Times) != 2 || first.Times[0].Class != model.TimeNative || first.Times[0].Value != nil || first.Times[1].Class != model.TimeCapture || first.Times[1].Value == nil {
			t.Errorf("block %d replaced absent native stream time with capture time", i)
		}
	}
	if fitems[1].Finality.State != model.FinalityCandidate || fitems[1].IsFinalAnswer {
		t.Error("file stop reason became authoritative finality")
	}
	if witems[1].Finality.State != model.FinalityFinal || !witems[1].IsFinalAnswer {
		t.Error("captured completed terminal did not finalize the wire answer")
	}
	for _, state := range []parsedView{file, wire} {
		for _, turn := range state.Turns {
			if turn.Outcome != model.OutcomeCompleted || turn.Model != "fixture-model" || turn.Usage == nil || turn.Usage.Input == nil || *turn.Usage.Input != 10 || turn.Usage.Output == nil || *turn.Usage.Output != 52 || turn.Usage.Reasoning == nil || *turn.Usage.Reasoning != 45 || turn.Usage.CacheWrite == nil || *turn.Usage.CacheWrite != 43065 {
				t.Error("captured usage revisions or completion did not converge at their own grain")
			}
		}
	}
	for _, turn := range wire.Turns {
		found := false
		for _, usage := range turn.UsageObservations {
			if usage.Semantics == "result_aggregate" {
				bucket := usage.ModelUsage["fixture-model"]
				found = bucket.Input != nil && *bucket.Input == 914 && bucket.Output != nil && *bucket.Output == 63
			}
		}
		if !found {
			t.Error("result model bucket was lost or replaced with response usage")
		}
	}
}
