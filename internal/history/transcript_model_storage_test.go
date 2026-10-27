package history

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedItemStorageKeepsPayloadOnlyInRetainedView(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "private-payload")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	turn := TranscriptTurn{Key: "view-turn", Ordinal: 1, Status: TurnInProgress, CompletionEvidence: CompletionNone}
	if err := writeTranscriptBatch(ctx, tx, projection, TranscriptProjectionResult{Turns: []TranscriptTurn{turn}}); err != nil {
		t.Fatal(err)
	}
	item := identityTestItem()
	item.Text = strings.Repeat("native-payload-only ", 10000) + "end"
	item.Detail = json.RawMessage(`{"payload":"private-detail-must-not-be-copied"}`)
	item.Parts = []model.Part{{Type: "text", Text: item.Text, Detail: item.Detail, Availability: model.AvailableInline, Source: model.SourceRef{Kind: "file", Label: "native.jsonl", Incarnation: "generation", Line: 3, Start: 22, End: 240000, BlockPath: "message.content[0]", SHA256: strings.Repeat("a", 64)}}}
	visible, _, err := writeParsedTranscriptItemTx(ctx, tx, projection, "native", turn, item)
	if err != nil {
		t.Fatal(err)
	}
	var raw, detail []byte
	var text, body string
	if err := tx.QueryRow(`SELECT parsed_json,display_text,detail_json FROM conversation_items WHERE projection_id=? AND item_id=?`, projection, visible.ID).Scan(&raw, &text, &detail); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("native-payload-only")) || bytes.Contains(raw, []byte("private-detail-must-not-be-copied")) || len(raw) > 4096 {
		t.Fatalf("private state copied payload: %d bytes", len(raw))
	}
	if text != visible.Text || !bytes.Equal(detail, item.Detail) || visible.ContentRef == "" {
		t.Fatal("retained view lost content")
	}
	if err := tx.QueryRow(`SELECT body_text FROM conversation_item_bodies WHERE projection_id=? AND item_id=?`, projection, visible.ID).Scan(&body); err != nil || body != item.Text {
		t.Fatalf("retained body changed: %v", err)
	}
	saved, err := decodeParsedItemTx(ctx, tx, projection, raw)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Text != "" || len(saved.Detail) != 0 || len(saved.Parts) != 1 || saved.Parts[0].Text != "" || len(saved.Parts[0].Detail) != 0 || saved.Parts[0].Source != item.Parts[0].Source {
		t.Fatal("private part lost source address or copied payload")
	}
	var duplicated int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_parse_values WHERE projection_id=? AND (instr(value_key,'native-payload-only')>0 OR instr(value_key,'private-detail-must-not-be-copied')>0)`, projection).Scan(&duplicated); err != nil || duplicated != 0 {
		t.Fatalf("payload moved into dictionary: count=%d err=%v", duplicated, err)
	}
}

func TestParsedModelStoragePreservesTypedFacts(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "private-facts")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	empty := ""
	zero := int64(0)
	scope := model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, Source: "native", Incarnation: "1", NativeThreadID: &empty, NativeTurnID: nil, TurnKey: "local-turn"}
	item := model.Item{Scope: scope, ID: "item", SourceSeq: 400, TurnKey: "local-turn", TurnOrdinal: 80, Capture: model.CapturePartial, Outcome: model.OutcomeFailed, Complete: true,
		Finality: model.Finality{State: model.FinalityCandidate, Basis: model.BasisDerivedExact, ObservationRefs: []string{"later", "first", "later"}},
		Aliases:  []model.Alias{{Namespace: "rpc", Value: json.RawMessage(`1`), Authority: model.BasisNative, ObservationRefs: []string{}}, {Namespace: "rpc", Value: json.RawMessage(`"1"`), Authority: model.BasisNative, ObservationRefs: nil}},
		Times:    []model.TimeObservation{{Scope: scope, Value: nil, Raw: json.RawMessage(`null`), Class: model.TimeNative, Meaning: model.TimeEnd, ObservationRef: "time"}}}
	raw, err := encodeParsedItemTx(ctx, tx, projection, item)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decodeParsedItemTx(ctx, tx, projection, raw)
	if err != nil || !reflect.DeepEqual(item, restored) {
		t.Fatalf("item facts changed: err=%v\nwant=%+v\ngot=%+v", err, item, restored)
	}
	turn := model.Turn{Scope: scope, Key: "turn", Ordinal: 80, StartSourceSeq: 400, EndSourceSeq: 700, FinalItemID: "item", FinalItemRefs: []string{"first", "item"}, Times: item.Times,
		Completion: []model.CompletionFact{{Scope: scope, Outcome: model.OutcomeFailed, Basis: model.BasisNative, ObservationRefs: []string{"result"}, Reason: "native error"}},
		Durations:  []model.Duration{{Scope: scope, Value: 0, Unit: "ms", Basis: "native", SourceField: "duration", ObservationRef: "duration"}},
		Usage:      &model.TokenUsage{Input: &zero, Native: json.RawMessage(`{"input":0}`)}, UsageRefs: []string{"turn"}, UsageBasis: "native", UsageCoverage: map[string]model.CounterCoverage{"input": {Reported: 1, Samples: 2}}, UsageIncomplete: true,
		UsageObservations: []model.UsageObservation{{ID: "response", Scope: scope, Grain: model.UsageResponse, SampleID: "sample", Revision: "2", Values: model.TokenUsage{Input: &zero}, ObservationRefs: []string{"response"}}, {ID: "thread", Scope: scope, Grain: model.UsageThread, SampleID: "total", Values: model.TokenUsage{Input: nil}, ObservationRefs: []string{}}}}
	encoded, err := encodeParsedTurnTx(ctx, tx, projection, turn)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeParsedTurnTx(ctx, tx, projection, encoded)
	if err != nil || !reflect.DeepEqual(turn, decoded) {
		t.Fatalf("turn facts changed: err=%v\nwant=%+v\ngot=%+v", err, turn, decoded)
	}
}
