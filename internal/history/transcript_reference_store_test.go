package history

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestNativeEndpointUsesPersistedObservationIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	projection := identityStoreProjection(t, store, "endpoint")
	scope := model.Scope{Provider: model.ProviderGrok, Source: "wire", Incarnation: "1", ConnectionID: "connection"}
	source := model.SourceRef{Label: "wire", Incarnation: "1", Frame: 1}
	first := model.Observation{ID: "first", NativeEventID: "event", Scope: scope, Source: source}
	apply := func(batch model.Batch) model.Batch {
		t.Helper()
		ctx := context.Background()
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		got, err := resolveTranscriptRelationsTx(ctx, tx, projection, batch)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return got
	}
	apply(model.Batch{Observations: []model.Observation{first}})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	later := model.Observation{ID: "late", NativeEventID: "event", Scope: scope, Source: model.SourceRef{Label: "wire", Incarnation: "1", Frame: 20}}
	ref := &model.NativeObservationRef{Scope: scope, Namespace: "native.eventId", Value: []byte(`"event"`)}
	batch := model.Batch{Observations: []model.Observation{later}, Changes: []model.Change{{Relation: &model.Relation{Kind: model.RelationReplayOf, Scope: scope, FromObservation: later.ID, ToNativeObservation: ref}}}}
	got := apply(batch)
	if len(got.Diagnostics) != 0 || got.Changes[0].Relation.ToObservation != "first" {
		t.Fatalf("persisted endpoint not resolved: %+v", got)
	}
	again := batch
	again.Observations = []model.Observation{{ID: "another-replay", NativeEventID: "event", Scope: scope}}
	again.Changes = []model.Change{{Relation: &model.Relation{Kind: model.RelationReplayOf, Scope: scope, FromObservation: "another-replay", ToNativeObservation: ref}}}
	got = apply(again)
	if len(got.Diagnostics) != 0 || got.Changes[0].Relation.ToObservation != "first" {
		t.Fatal("a replay introduced a competing native endpoint")
	}
	aliased := model.Item{Scope: scope, Aliases: []model.Alias{{Namespace: "request", Value: []byte(`1`), Authority: model.BasisNative, ObservationRefs: []string{"native-request"}}, {Namespace: "request", Value: []byte(`1`), Authority: model.BasisDerived, ObservationRefs: []string{"inferred-request"}}}}
	apply(model.Batch{Changes: []model.Change{{Item: &aliased}}})
	for _, scalar := range []string{`1`, `"1"`} {
		target := &model.NativeObservationRef{Scope: scope, Namespace: "request", Value: []byte(scalar)}
		got := apply(model.Batch{Changes: []model.Change{{Relation: &model.Relation{Kind: model.RelationResultOf, FromObservation: "response", ToNativeObservation: target}}}})
		if scalar == `1` {
			if len(got.Diagnostics) != 0 || got.Changes[0].Relation.ToObservation != "native-request" {
				t.Fatal("native scalar item alias failed or derived alias competed")
			}
		} else if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != "unresolved_native_reference" {
			t.Fatal("numeric and string native aliases collapsed")
		}
	}
	// A second distinct observation with the same exact alias stays ambiguous.
	second := first
	second.ID = "second"
	apply(model.Batch{Observations: []model.Observation{second}})
	got = apply(batch)
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != "ambiguous_native_reference" || got.Changes[0].Relation.ToObservation != "" {
		t.Fatalf("ambiguous endpoint selected a row: %+v", got)
	}
	// Known source generations never borrow evidence from another projection.
	projection = identityStoreProjection(t, store, "endpoint-other")
	got = apply(batch)
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != "unresolved_native_reference" {
		t.Fatalf("projection leaked endpoint receipts: %+v", got)
	}
}
