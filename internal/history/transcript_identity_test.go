package history

import (
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func identityTestItem() model.Item {
	thread := "native-thread"
	scope := model.Scope{Provider: model.ProviderClaude, Source: "source", Incarnation: "1", NativeThreadID: &thread, Kind: model.ScopeItem, ItemID: "local-late"}
	return model.Item{ID: "local-late", Scope: scope, NativeItemID: "message:0", CallID: "call", Type: model.ItemToolResult, Role: model.RoleTool, Observations: []model.Observation{{ID: "late", Scope: scope, NativeEventID: "event", NativeEventDigest: "digest", Source: model.SourceRef{BlockPath: "message.content[0]"}}}}
}

func identityTestReceipt(item model.Item, kind, id string) transcriptIdentityReceipt {
	for _, key := range transcriptItemIdentityKeys(item) {
		if key.Kind == kind {
			key.ObservationID = "earlier"
			return transcriptIdentityReceipt{Key: key, ItemID: id, NativeItemID: item.NativeItemID, CallID: item.CallID, ToolName: item.ToolName, Components: item.ExactNativeItemComponents(), Type: item.Type, Role: item.Role}
		}
	}
	return transcriptIdentityReceipt{}
}

func TestTranscriptIdentityUsesFixedPrecedenceAndRejectsConflict(t *testing.T) {
	item := identityTestItem()
	local := identityTestReceipt(item, "local", "local-canonical")
	native := identityTestReceipt(item, "item", "native-canonical")
	call := identityTestReceipt(item, "call", "call-canonical")
	event := identityTestReceipt(item, "event", "event-canonical")
	for _, tc := range []struct {
		name, binding, want, diagnostic string
		receipts                        []transcriptIdentityReceipt
	}{
		{"local", "", local.ItemID, "", []transcriptIdentityReceipt{local}},
		{"event before local", "", event.ItemID, "", []transcriptIdentityReceipt{local, event}},
		{"call before event", "", call.ItemID, "", []transcriptIdentityReceipt{local, event, call}},
		{"native before call", "", native.ItemID, "", []transcriptIdentityReceipt{local, event, call, native}},
		{"persisted binding", "bound", "bound", "", []transcriptIdentityReceipt{local, event, call, native}},
		{"ambiguous native", "", "", "ambiguous_item_identity", []transcriptIdentityReceipt{native, identityTestReceipt(item, "item", "other")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTranscriptItemIdentity(item, tc.binding, tc.receipts)
			if got.ItemID != tc.want || got.Diagnostic != tc.diagnostic {
				t.Fatalf("resolution = %+v, want id=%q diagnostic=%q", got, tc.want, tc.diagnostic)
			}
		})
	}
	conflict := call
	conflict.NativeItemID = "other-message:0"
	if got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{conflict}); got.ItemID != "" || got.Diagnostic != "" {
		t.Fatalf("distinct native blocks must remain independent despite a shared call: %+v", got)
	}
	conflict = native
	conflict.CallID = "other-call"
	if got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{conflict}); got.ItemID != "" || got.Diagnostic != "conflicting_item_identity" {
		t.Fatalf("one native block's conflicting call identity joined: %+v", got)
	}
}

func TestTranscriptIdentityRetainsReplayEvidenceAfterEviction(t *testing.T) {
	item := identityTestItem()
	item.NativeItemID = ""
	item.CallID = ""
	prior := identityTestReceipt(item, "event", "canonical")
	before := item
	got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{prior})
	if got.ItemID != "canonical" || len(got.Replay) != 1 || got.Replay[0].Kind != model.RelationReplayOf || got.Replay[0].FromObservation != "late" || got.Replay[0].ToObservation != "earlier" {
		t.Fatalf("late replay = %+v", got)
	}
	if !reflect.DeepEqual(before, item) {
		t.Fatal("identity resolution mutated native evidence")
	}
	prior.Key.Digest = "changed"
	if got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{prior}); got.ItemID != "" || got.Diagnostic != "conflicting_native_event" {
		t.Fatalf("conflicting UUID reuse joined: %+v", got)
	}
}

func TestTranscriptIdentityDoesNotJoinTextOrWrongScopeOrFacet(t *testing.T) {
	item := identityTestItem()
	for _, tc := range []struct {
		name string
		edit func(*transcriptIdentityReceipt)
	}{
		{"source", func(r *transcriptIdentityReceipt) { r.Key.Scope.Source = "other" }},
		{"incarnation", func(r *transcriptIdentityReceipt) { r.Key.Scope.Incarnation = "2" }},
		{"provider", func(r *transcriptIdentityReceipt) { r.Key.Scope.Provider = model.ProviderCodex }},
		{"thread", func(r *transcriptIdentityReceipt) { v := "other"; r.Key.Scope.NativeThreadID = &v }},
		{"parent call", func(r *transcriptIdentityReceipt) { v := "child"; r.Key.Scope.NativeParentCallID = &v }},
		{"facet", func(r *transcriptIdentityReceipt) { r.Type = model.ItemToolCall }},
		{"role", func(r *transcriptIdentityReceipt) { r.Role = model.RoleAssistant }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := identityTestReceipt(item, "event", "earlier")
			tc.edit(&r)
			if got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{r}); got.ItemID != "" {
				t.Fatalf("unrelated evidence joined: %+v", got)
			}
		})
	}
	other := item
	other.ID = "other-local"
	other.NativeItemID = "different-native"
	other.CallID = "different-call"
	other.Observations = nil
	if got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{identityTestReceipt(other, "local", "other")}); got.ItemID != "" {
		t.Fatal("missing native identity joined equal text")
	}
	multi := item
	multi.Observations = append([]model.Observation(nil), item.Observations...)
	multi.Observations[0].Source.BlockPath = "message.content[1]"
	if got := resolveTranscriptItemIdentity(multi, "", []transcriptIdentityReceipt{identityTestReceipt(item, "event", "other-block")}); got.ItemID != "" {
		t.Fatal("different blocks of one event collapsed")
	}
}

func TestTranscriptIdentityScopesCallsToTheKnownTurn(t *testing.T) {
	item := identityTestItem()
	item.NativeItemID = ""
	item.Observations = nil
	item.Scope.TurnKey = "second-turn"
	earlier := item
	earlier.ID = "earlier-local"
	earlier.Scope.TurnKey = "first-turn"
	receipt := identityTestReceipt(earlier, "call", "earlier")
	if got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{receipt}); got.ItemID != "" {
		t.Fatal("reused call with unknown native turn joined another local turn")
	}
	native := "proven-native-turn"
	item.Scope.NativeTurnID = &native
	receipt.Key.Scope.NativeTurnID = &native
	if got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{receipt}); got.ItemID != "earlier" {
		t.Fatal("exact native turn failed across provisional local turn keys")
	}
}

func TestTranscriptIdentityKeepsLocalClassificationRefinement(t *testing.T) {
	item := identityTestItem()
	receipt := identityTestReceipt(item, "local", "canonical")
	item.Type = model.ItemNotice
	item.Role = model.RoleSystem
	got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{receipt})
	if got.ItemID != "canonical" {
		t.Fatal("classification refinement created a second identity for an established local item")
	}
}

func TestTranscriptProvisionalCompositeUsesCallThenEvent(t *testing.T) {
	item := identityTestItem()
	item.Aliases = []model.Alias{{Namespace: "claude.messageBlock", Value: []byte(`"message:0"`), Authority: model.BasisDerived}}
	first := identityTestItem()
	first.CallID = "other-call"
	second := identityTestItem()
	second.NativeItemID = "message:1"
	call := identityTestReceipt(second, "call", "second")
	event := identityTestReceipt(second, "event", "second")
	got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{identityTestReceipt(first, "item", "first"), call, event})
	if got.ItemID != "second" || got.Diagnostic != "" || len(got.Replay) != 1 {
		t.Fatalf("provisional composite overruled exact receipts: %+v", got)
	}
	if got := resolveTranscriptItemIdentity(item, "", []transcriptIdentityReceipt{identityTestReceipt(first, "item", "first")}); got.ItemID != "" || got.Diagnostic != "" {
		t.Fatal("provisional identity resolved or rejected without exact evidence")
	}
}

func TestTranscriptCallIdentityChecksExactComponents(t *testing.T) {
	first := identityTestItem()
	first.Aliases = []model.Alias{{Namespace: "claude.apiMessageId", Value: []byte(`"first"`), Authority: model.BasisNative}}
	second := identityTestItem()
	second.Aliases = []model.Alias{{Namespace: "claude.messageBlock", Value: []byte(`"message:0"`), Authority: model.BasisDerived}, {Namespace: "claude.apiMessageId", Value: []byte(`"second"`), Authority: model.BasisNative}}
	if got := resolveTranscriptItemIdentity(second, "", []transcriptIdentityReceipt{identityTestReceipt(first, "call", "first")}); got.ItemID != "" || got.Diagnostic != "" {
		t.Fatal("exact message components were ignored")
	}
	unknown := identityTestItem()
	unknown.NativeItemID = ""
	unknown.Observations = nil
	got := resolveTranscriptItemIdentity(unknown, "", []transcriptIdentityReceipt{identityTestReceipt(first, "call", "first"), identityTestReceipt(second, "call", "second")})
	if got.ItemID != "" || got.Diagnostic != "ambiguous_call_identity" {
		t.Fatalf("ambiguous call selected a row: %+v", got)
	}
}

func TestTranscriptExactMessageAnchorsCallAcrossProvisionalTurns(t *testing.T) {
	first := identityTestItem()
	first.Scope.TurnKey = "first-source-turn"
	first.Aliases = []model.Alias{{Namespace: "claude.apiMessageId", Value: []byte(`"message"`), Authority: model.BasisNative}}
	late := identityTestItem()
	late.Scope.TurnKey = "later-source-turn"
	late.Observations = nil
	late.NativeItemID = "message:1"
	late.Aliases = []model.Alias{{Namespace: "claude.apiMessageId", Value: []byte(`"message"`), Authority: model.BasisNative}, {Namespace: "claude.messageBlock", Value: []byte(`"message:1"`), Authority: model.BasisDerived}}
	got := resolveTranscriptItemIdentity(late, "", []transcriptIdentityReceipt{identityTestReceipt(first, "call", "first")})
	if got.ItemID != "first" || got.Diagnostic != "" {
		t.Fatalf("exact message/call pair lost to a provisional turn: %+v", got)
	}
}
