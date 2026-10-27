package history

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// transcriptIdentityKey selects indexed receipts; its value is normalized model
// evidence, never a decoded provider envelope or text-equality fingerprint.
type transcriptIdentityKey struct {
	Kind, Value           string
	Scope                 model.Scope
	ObservationID, Digest string
}

type transcriptIdentityReceipt struct {
	Key                  transcriptIdentityKey
	ItemID               string
	NativeItemID, CallID string
	ToolName             string
	Components           []model.Alias
	Type                 model.ItemType
	Role                 model.Role
}

type transcriptIdentityResolution struct {
	ItemID     string
	Replay     []model.Relation
	Diagnostic string
}

func transcriptItemIdentityKeys(item model.Item) []transcriptIdentityKey {
	var keys []transcriptIdentityKey
	add := func(kind, value string, scope model.Scope, observation, digest string) {
		if value != "" {
			keys = append(keys, transcriptIdentityKey{Kind: kind, Value: value, Scope: scope, ObservationID: observation, Digest: digest})
		}
	}
	add("item", item.ExactNativeItemID(), item.Scope, "", "")
	// A permission/question request can address the same tool invocation as
	// another independent request. Its call ID is an endpoint, not identity.
	if item.Type != model.ItemInteraction {
		add("call", item.CallID, item.Scope, "", "")
	}
	for _, obs := range item.Observations {
		if obs.NativeEventID != "" {
			value, _ := json.Marshal([]string{obs.NativeEventID, obs.Source.BlockPath})
			add("event", string(value), obs.Scope, obs.ID, obs.NativeEventDigest)
		}
	}
	add("local", item.ID, item.Scope, "", "")
	return keys
}

func transcriptIdentityReceiptKeys(item model.Item, replay bool) []transcriptIdentityKey {
	var retained []transcriptIdentityKey
	for _, key := range transcriptItemIdentityKeys(item) {
		// A retired UUID observed in a later provisional turn cannot make that
		// turn claim the old call. Both durable and live consumers use this rule.
		if replay && (key.Kind == "item" || key.Kind == "call") {
			continue
		}
		key.Scope.Kind = ""
		if key.Kind != "call" {
			key.Scope.ItemID, key.Scope.TurnKey = "", ""
		}
		retained = append(retained, key)
	}
	return retained
}

// resolveTranscriptItemIdentity is shared by durable and live conversion.
// Receipts come from indexed history lookups, never parser continuation Input.
func resolveTranscriptItemIdentity(item model.Item, boundID string, receipts []transcriptIdentityReceipt) transcriptIdentityResolution {
	result := transcriptIdentityResolution{ItemID: boundID}
	keys := transcriptItemIdentityKeys(item)
	nativeID := item.ExactNativeItemID()
	components := item.ExactNativeItemComponents()
	for _, kind := range []string{"item", "call", "event", "local"} {
		matches := make(map[string]bool)
		for _, key := range keys {
			if key.Kind != kind {
				continue
			}
			for _, receipt := range receipts {
				candidateScope := key.Scope
				if kind == "call" && transcriptIdentitySharesMessage(components, receipt.Components) {
					// Exact native message/call identity outlives a counted source
					// turn. Native turn and parent scopes still have to agree.
					candidateScope.TurnKey = receipt.Key.Scope.TurnKey
				}
				if receipt.ItemID == "" || receipt.Key.Kind != kind || receipt.Key.Value != key.Value || kind != "local" && (receipt.Type != item.Type || receipt.Role != item.Role) || !transcriptIdentityScopeMatches(candidateScope, receipt.Key.Scope, kind) {
					continue
				}
				if kind == "event" && key.Digest != "" && receipt.Key.Digest != "" && key.Digest != receipt.Key.Digest {
					return transcriptIdentityResolution{Diagnostic: "conflicting_native_event"}
				}
				// A reused call ID cannot collapse two explicitly distinct native
				// blocks. Their result endpoint remains ambiguous in the model.
				// Within a retained source turn, distinct parser item IDs also
				// prove independent facets. Eviction changes the source turn key;
				// native identity can still resolve those later provisional items.
				if kind == "call" && key.Scope.TurnKey != "" && key.Scope.TurnKey == receipt.Key.Scope.TurnKey && key.Scope.ItemID != "" && receipt.Key.Scope.ItemID != "" && key.Scope.ItemID != receipt.Key.Scope.ItemID {
					continue
				}
				if kind == "call" && (nativeID != "" && receipt.NativeItemID != "" && nativeID != receipt.NativeItemID || transcriptIdentityComponentsContradict(components, receipt.Components)) {
					continue
				}
				// A call can address independently typed facets. In particular,
				// a named invocation and an unnamed execution observation are
				// distinct even after their source turn has been evicted.
				if kind == "call" && item.ToolName != receipt.ToolName {
					continue
				}
				if nativeID != "" && receipt.NativeItemID != "" && nativeID != receipt.NativeItemID || item.CallID != "" && receipt.CallID != "" && item.CallID != receipt.CallID {
					return transcriptIdentityResolution{Diagnostic: "conflicting_item_identity"}
				}
				matches[receipt.ItemID] = true
			}
		}
		if result.ItemID == "" {
			if len(matches) > 1 {
				if kind == "call" {
					result.Diagnostic = "ambiguous_call_identity"
					continue
				}
				return transcriptIdentityResolution{Diagnostic: "ambiguous_item_identity"}
			}
			for id := range matches {
				result.ItemID = id
			}
		}
	}
	if result.ItemID == "" {
		return result
	}
	seen := make(map[string]bool)
	for _, key := range keys {
		if key.Kind != "event" || key.Digest == "" || key.ObservationID == "" {
			continue
		}
		for _, receipt := range receipts {
			prior := receipt.Key
			if receipt.ItemID != result.ItemID || prior.Kind != "event" || prior.Value != key.Value || prior.Digest != key.Digest || prior.ObservationID == "" || prior.ObservationID == key.ObservationID || seen[key.ObservationID] || !transcriptIdentityScopeMatches(key.Scope, prior.Scope, "event") {
				continue
			}
			seen[key.ObservationID] = true
			result.Replay = append(result.Replay, model.Relation{Kind: model.RelationReplayOf, Scope: key.Scope, FromObservation: key.ObservationID, ToObservation: prior.ObservationID, ObservationRefs: []string{prior.ObservationID, key.ObservationID}})
		}
	}
	return result
}

func transcriptIdentityScopeMatches(a, b model.Scope, kind string) bool {
	if a.Provider == "" || a.Source == "" || a.Incarnation == "" || a.Provider != b.Provider || a.Source != b.Source || a.Incarnation != b.Incarnation || a.ConnectionID != b.ConnectionID {
		return false
	}
	if kind == "call" && a.NativeTurnID == nil && b.NativeTurnID == nil && a.TurnKey != b.TurnKey {
		return false
	}
	for i, pair := range [][2]*string{{a.NativeThreadID, b.NativeThreadID}, {a.NativeTurnID, b.NativeTurnID}, {a.ParentNativeID, b.ParentNativeID}, {a.NativeParentCallID, b.NativeParentCallID}} {
		if pair[0] == nil || pair[1] == nil {
			// The same parser-local item can gain an explicit native binding.
			// Parent scope remains exact so root and child traffic cannot join.
			if pair[0] != pair[1] && (kind != "local" || i > 1) {
				return false
			}
		} else if *pair[0] != *pair[1] {
			return false
		}
	}
	return true
}

func transcriptIdentityComponentsContradict(incoming, retained []model.Alias) bool {
	for _, a := range incoming {
		for _, b := range retained {
			if a.Namespace == b.Namespace && !bytes.Equal(a.Value, b.Value) {
				return true
			}
		}
	}
	return false
}

func transcriptIdentitySharesMessage(incoming, retained []model.Alias) bool {
	for _, a := range incoming {
		if !strings.HasSuffix(a.Namespace, ".apiMessageId") {
			continue
		}
		for _, b := range retained {
			if a.Namespace == b.Namespace && bytes.Equal(a.Value, b.Value) {
				return true
			}
		}
	}
	return false
}
