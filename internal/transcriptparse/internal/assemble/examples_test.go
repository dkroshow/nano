package assemble

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type examples struct {
	GrokReplay   []model.Observation
	ClaudeCall   []model.Observation
	ClaudeResult model.Observation
	CodexUsage   []model.UsageObservation
	NullWireTime model.Observation
}

func readExamples(t *testing.T) examples {
	t.Helper()
	raw, err := os.ReadFile("testdata/model-examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture examples
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func testScope(provider model.Provider) model.Scope {
	thread, turn := "thread-1", "turn-1"
	return model.Scope{Kind: model.ScopeItem, Provider: provider, Source: "fixture", Incarnation: "capture-1", NativeThreadID: &thread, NativeTurnID: &turn, TurnKey: "t1"}
}

func TestLateNativeBindingRequiresStableLocalIdentity(t *testing.T) {
	known := testScope(model.ProviderClaude)
	unknown := known
	unknown.NativeThreadID = nil
	if !sameTurn(known, unknown) {
		t.Fatal("explicit local binding lost prior source scope")
	}
	unknown.TurnKey = "different-local-turn"
	if sameTurn(known, unknown) {
		t.Fatal("unknown thread joined without a local identity proof")
	}
	conflict := known
	other := "other-thread"
	conflict.NativeThreadID = &other
	if sameTurn(known, conflict) {
		t.Fatal("local key overrode conflicting native thread IDs")
	}
	conflict = known
	conflict.NativeTurnID = &other
	if sameTurn(known, conflict) {
		t.Fatal("local key overrode conflicting native turn IDs")
	}
	conflict = known
	conflict.NativeParentCallID = &other
	if sameTurn(known, conflict) {
		t.Fatal("local key overrode nested call scope")
	}
}

func TestModelExampleHahaReplay(t *testing.T) {
	item := model.Item{ID: "message", Scope: testScope(model.ProviderGrok)}
	for _, observation := range readExamples(t).GrokReplay {
		observation.Scope = item.Scope
		var diagnostics []model.Diagnostic
		item, diagnostics = Observe(item, observation)
		if len(diagnostics) != 0 {
			t.Fatal(diagnostics)
		}
	}
	if item.Text != "haha" || len(item.Observations) != 3 {
		t.Fatalf("got text %q and %d observations", item.Text, len(item.Observations))
	}
	if len(item.Relations) != 1 || item.Relations[0].Kind != model.RelationReplayOf || item.Relations[0].FromObservation != "o3" || item.Relations[0].ToObservation != "o1" {
		t.Fatalf("replay evidence: %+v", item.Relations)
	}
	if item.Observations[1].NativeEventID != "E2" {
		t.Fatal("equal text lost distinct native event")
	}
}

func TestModelExampleClaudeBlockStopAndErrorResult(t *testing.T) {
	fixture := readExamples(t)
	scope := testScope(model.ProviderClaude)
	call := model.Item{ID: "call", Scope: scope, Type: model.ItemToolCall, CallID: "call-1"}
	for _, observation := range fixture.ClaudeCall {
		observation.Scope = scope
		call, _ = Observe(call, observation)
	}
	if !call.Complete || call.Outcome != model.OutcomeUnknown {
		t.Fatalf("block stop must complete arguments only: %+v", call)
	}
	fixture.ClaudeResult.Scope = scope
	result, _ := Observe(model.Item{ID: "result", Scope: scope, Type: model.ItemToolResult, CallID: "call-1"}, fixture.ClaudeResult)
	result, diagnostics := LinkResult(call, result)
	if len(diagnostics) != 0 || !result.Complete || result.Outcome != model.OutcomeFailed {
		t.Fatalf("error result lost: %+v, %v", result, diagnostics)
	}
	if len(result.Relations) != 1 || result.Relations[0].Kind != model.RelationResultOf || result.Relations[0].FromItem != "result" || result.Relations[0].ToItem != "call" {
		t.Fatalf("result relation: %+v", result.Relations)
	}
	if call.Outcome != model.OutcomeUnknown || result.Finality.State != model.FinalityUnknown {
		t.Fatal("result changed invocation or inferred answer finality")
	}
	// The API returns only the result item. Parent completion requires a distinct
	// turn-scoped terminal; this result's failed outcome has item scope.
	if result.Observations[0].Scope.Kind != model.ScopeItem {
		t.Fatal("tool failure promoted to turn failure")
	}
}

func TestModelExampleUsageGrains(t *testing.T) {
	scope := testScope(model.ProviderCodex)
	scope.Kind = model.ScopeTurn
	turn := model.Turn{Key: "t1", Scope: scope, UsageObservations: readExamples(t).CodexUsage}
	for i := range turn.UsageObservations {
		turn.UsageObservations[i].Scope = scope
	}
	turn.UsageObservations[2].Scope.Kind = model.ScopeThread
	turn.UsageObservations[2].Scope.NativeTurnID = nil
	turn.UsageObservations[2].Scope.TurnKey = ""
	got, diagnostics := SummarizeUsage(turn)
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got.Usage == nil || got.Usage.Total == nil || *got.Usage.Total != 10 {
		t.Fatalf("10/10/100 summary: %+v", got.Usage)
	}
	if got.UsageBasis != "native_turn_snapshot" || !reflect.DeepEqual(got.UsageRefs, []string{"turn-usage"}) {
		t.Fatalf("basis %q refs %v", got.UsageBasis, got.UsageRefs)
	}
	if len(got.UsageObservations) != 3 || *got.UsageObservations[2].Values.Total != 100 {
		t.Fatal("unselected native grain discarded")
	}
	if got.Usage.Input != nil || got.UsageCoverage["total"] != (model.CounterCoverage{Reported: 1, Samples: 1}) {
		t.Fatal("missing counter or coverage changed")
	}
	if turn.Usage != nil {
		t.Fatal("prior turn mutated")
	}
}

func TestModelExampleMissingWireNativeTime(t *testing.T) {
	observation := readExamples(t).NullWireTime
	observation.Scope = testScope(model.ProviderCodex)
	for i := range observation.Times {
		observation.Times[i].Scope = observation.Scope
	}
	item, diagnostics := Observe(model.Item{ID: "message", Scope: observation.Scope}, observation)
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if item.CreatedAt != nil || len(item.Times) != 3 || item.Times[0].Value != nil || item.Times[1].Value == nil || item.Times[2].Value == nil {
		t.Fatalf("time sources conflated: %+v", item.Times)
	}
	raw, err := json.Marshal(item.Times[0])
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatal(err)
	}
	if string(encoded["value"]) != "null" {
		t.Fatalf("missing native time must serialize as null: %s", raw)
	}
}

func TestConflictingNativeEventRetainsExactNumbers(t *testing.T) {
	scope := testScope(model.ProviderGrok)
	item, _ := Observe(model.Item{ID: "i", Scope: scope}, model.Observation{ID: "o1", Scope: scope, Kind: model.ObservationDelta, NativeEventID: "E1", Text: "ha", Detail: json.RawMessage(`{"n":9007199254740992}`)})
	got, diagnostics := Observe(item, model.Observation{ID: "o2", Scope: scope, Kind: model.ObservationDelta, NativeEventID: "E1", Text: "ha", Detail: json.RawMessage(`{"n":9007199254740993}`)})
	if len(diagnostics) != 1 || diagnostics[0].Code != "native_event_conflict" || len(got.Relations) != 0 || len(got.Observations) != 2 || got.Text != "ha" {
		t.Fatalf("conflicting evidence merged: %+v, %v", got, diagnostics)
	}
}
