package history

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestLateNativeReferenceUsesExactScopeAndScalar(t *testing.T) {
	thread, turn, other := "thread", "turn", "other"
	scope := model.Scope{Provider: model.ProviderClaude, Kind: model.ScopeTurn, Source: "source", Incarnation: "1", ConnectionID: "connection", NativeThreadID: &thread, NativeTurnID: &turn}
	observation := model.Observation{ID: "late", Scope: scope, Kind: model.ObservationMetadata, Source: model.SourceRef{Label: "source", Incarnation: "1", Line: 20}}
	relation := model.Relation{Kind: model.RelationReplayOf, Scope: scope, FromObservation: "late", ToNativeObservation: &model.NativeObservationRef{Scope: scope, Namespace: "native_event", Value: json.RawMessage(`1`)}, ObservationRefs: []string{"late"}}
	batch := model.Batch{Observations: []model.Observation{observation}, Changes: []model.Change{{Relation: &relation}}}
	base := transcriptAliasTarget{Scope: scope, Namespace: "native_event", Value: []byte(`1`), ObservationID: "earlier"}
	wrongTurn, wrongThread, wrongSource, wrongConnection := base, base, base, base
	wrongTurn.Scope.NativeTurnID = &other
	wrongThread.Scope.NativeThreadID = &other
	wrongSource.Scope.Incarnation = "2"
	wrongConnection.Scope.ConnectionID = "other"
	wrongScalar, wrongNamespace := base, base
	wrongScalar.Value = []byte(`"1"`)
	wrongNamespace.Namespace = "request"
	targets := []transcriptAliasTarget{wrongTurn, wrongThread, wrongSource, wrongConnection, wrongScalar, wrongNamespace, base, base}
	before, _ := json.Marshal(batch)
	got := resolveTranscriptRelations(batch, targets)
	if len(got.Diagnostics) != 0 || got.Changes[0].Relation.ToObservation != "earlier" {
		t.Fatal("late reference did not resolve by exact native evidence")
	}
	if !reflect.DeepEqual(got.Observations, batch.Observations) || len(got.Changes) != 1 || got.Changes[0].Item != nil || got.Changes[0].Turn != nil {
		t.Fatal("late replay reopened or replaced settled output")
	}
	after, _ := json.Marshal(batch)
	if string(after) != string(before) {
		t.Fatal("resolution mutated its input")
	}
	for _, test := range []struct {
		name    string
		targets []transcriptAliasTarget
		code    string
	}{
		{"absent", targets[:6], "unresolved_native_reference"},
		{"ambiguous", append(targets, transcriptAliasTarget{Scope: scope, Namespace: "native_event", Value: []byte(`1`), ObservationID: "second"}), "ambiguous_native_reference"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := resolveTranscriptRelations(batch, test.targets)
			if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != test.code || got.Changes[0].Relation.ToObservation != "" {
				t.Fatal("uncertain target was silently selected")
			}
			if !reflect.DeepEqual(got.Observations, batch.Observations) {
				t.Fatal("unresolved observation was dropped")
			}
		})
	}
}

func TestUnknownNativeScopeDoesNotSelectLastMatch(t *testing.T) {
	first, second := "first", "second"
	scope := model.Scope{Provider: model.ProviderGrok, Source: "capture", Incarnation: "1"}
	ref := &model.NativeObservationRef{Scope: scope, Namespace: "native_event", Value: json.RawMessage(`"event"`)}
	a, b := scope, scope
	a.NativeTurnID, b.NativeTurnID = &first, &second
	batch := model.Batch{Observations: []model.Observation{{ID: "late", Scope: scope}}, Changes: []model.Change{{Relation: &model.Relation{Kind: model.RelationReplayOf, FromObservation: "late", ToNativeObservation: ref}}}}
	targets := []transcriptAliasTarget{{a, "native_event", []byte(`"event"`), "one"}, {b, "native_event", []byte(`"event"`), "two"}}
	got := resolveTranscriptRelations(batch, targets)
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != "ambiguous_native_reference" || len(got.Observations) != 1 {
		t.Fatal("unknown turn was treated as the latest turn")
	}
}

func TestNativeReferenceRejectsWrongScopeAndMalformedIdentity(t *testing.T) {
	thread, turn, parent, call, other := "thread", "turn", "parent", "call", "other"
	scope := model.Scope{Provider: model.ProviderClaude, Source: "source", Incarnation: "1", ConnectionID: "connection", NativeThreadID: &thread, NativeTurnID: &turn, ParentNativeID: &parent, NativeParentCallID: &call}
	base := model.NativeObservationRef{Scope: scope, Namespace: "claude.rowUUID", Value: json.RawMessage(`"event"`)}
	for _, test := range []struct {
		name string
		edit func(*model.NativeObservationRef)
	}{
		{"provider", func(r *model.NativeObservationRef) { r.Scope.Provider = model.ProviderCodex }},
		{"source", func(r *model.NativeObservationRef) { r.Scope.Source = "other" }},
		{"parent thread", func(r *model.NativeObservationRef) { r.Scope.ParentNativeID = &other }},
		{"parent call", func(r *model.NativeObservationRef) { r.Scope.NativeParentCallID = &other }},
		{"local item", func(r *model.NativeObservationRef) { r.Scope.ItemID = "local" }},
		{"local turn", func(r *model.NativeObservationRef) { r.Scope.TurnKey = "local" }},
		{"missing provider", func(r *model.NativeObservationRef) { r.Scope.Provider = "" }},
		{"missing source", func(r *model.NativeObservationRef) { r.Scope.Source = "" }},
		{"missing incarnation", func(r *model.NativeObservationRef) { r.Scope.Incarnation = "" }},
		{"missing namespace", func(r *model.NativeObservationRef) { r.Namespace = "" }},
		{"null", func(r *model.NativeObservationRef) { r.Value = json.RawMessage(`null`) }},
		{"array", func(r *model.NativeObservationRef) { r.Value = json.RawMessage(`["event"]`) }},
		{"object", func(r *model.NativeObservationRef) { r.Value = json.RawMessage(`{"id":"event"}`) }},
		{"invalid JSON", func(r *model.NativeObservationRef) { r.Value = json.RawMessage(`"event`) }},
		{"trailing JSON", func(r *model.NativeObservationRef) { r.Value = json.RawMessage(`"event" true`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ref := base
			test.edit(&ref)
			observation := model.Observation{ID: "late", Scope: scope, Source: model.SourceRef{Label: "source", Incarnation: "1", Line: 90}, Text: "retained evidence"}
			batch := model.Batch{Observations: []model.Observation{observation}, Changes: []model.Change{{Relation: &model.Relation{Kind: model.RelationReplayOf, FromObservation: "late", ToNativeObservation: &ref}}}}
			got := resolveTranscriptRelations(batch, []transcriptAliasTarget{{Scope: scope, Namespace: base.Namespace, Value: base.Value, ObservationID: "earlier"}})
			if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != "unresolved_native_reference" || got.Diagnostics[0].Source != observation.Source || got.Changes[0].Relation.ToObservation != "" || !reflect.DeepEqual(got.Observations, batch.Observations) {
				t.Fatal("invalid endpoint joined unrelated evidence or lost diagnostic provenance")
			}
		})
	}
}

func TestNativeReferenceResolutionIsIdempotent(t *testing.T) {
	scope := model.Scope{Provider: model.ProviderGrok, Source: "source", Incarnation: "1"}
	ref := &model.NativeObservationRef{Scope: scope, Namespace: "event", Value: json.RawMessage(`  "\u0065vent" `)}
	batch := model.Batch{Changes: []model.Change{{Relation: &model.Relation{Kind: model.RelationReplayOf, FromObservation: "late", ToNativeObservation: ref, ObservationRefs: []string{"late"}}}}}
	targets := []transcriptAliasTarget{{Scope: scope, Namespace: "event", Value: []byte(`"event"`), ObservationID: "earlier"}}
	once := resolveTranscriptRelations(batch, targets)
	if once.Changes[0].Relation.ToObservation != "earlier" || len(once.Diagnostics) != 0 {
		t.Fatal("equivalent string encoding did not resolve")
	}
	if !reflect.DeepEqual(once, resolveTranscriptRelations(once, targets)) {
		t.Fatal("repeated resolution duplicated evidence")
	}
}
