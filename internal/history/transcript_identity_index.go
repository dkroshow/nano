package history

import (
	"reflect"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// ParsedItemIdentities lets a bounded live view use the same identity rules as
// the durable store. It retains identity receipts only, never content or parser
// continuation. The caller supplies its current local binding, serializes access,
// and forgets rows when its view does.
type ParsedItemIdentities struct {
	receipts map[string][]transcriptIdentityReceipt
}

// Resolve returns an existing view ID, replay evidence, or a conflict code.
// Empty ID and code mean that this view has not retained the item before.
func (index *ParsedItemIdentities) Resolve(item model.Item, boundID string) (string, []model.Relation, string) {
	var receipts []transcriptIdentityReceipt
	seen := make(map[string]bool)
	for _, key := range transcriptItemIdentityKeys(item) {
		bucket := key.Kind + "\x00" + key.Value
		if !seen[bucket] {
			receipts = append(receipts, index.receipts[bucket]...)
			seen[bucket] = true
		}
	}
	result := resolveTranscriptItemIdentity(item, boundID, receipts)
	return result.ItemID, result.Replay, result.Diagnostic
}

// Remember associates accepted native evidence with the consumer's view ID.
// Identical receipts retain their first observation for later replay references.
func (index *ParsedItemIdentities) Remember(item model.Item, id string, replay bool) {
	if id == "" {
		return
	}
	if index.receipts == nil {
		index.receipts = make(map[string][]transcriptIdentityReceipt)
	}
	for _, key := range transcriptIdentityReceiptKeys(item, replay) {
		if key.Kind == "local" {
			continue // The caller owns its active local-to-view bindings.
		}
		bucket := key.Kind + "\x00" + key.Value
		receipt := transcriptIdentityReceipt{Key: key, ItemID: id, NativeItemID: item.ExactNativeItemID(), CallID: item.CallID, ToolName: item.ToolName, Components: item.ExactNativeItemComponents(), Type: item.Type, Role: item.Role}
		duplicate := false
		for _, earlier := range index.receipts[bucket] {
			if earlier.ItemID == id && earlier.Type == receipt.Type && earlier.Role == receipt.Role && earlier.NativeItemID == receipt.NativeItemID && earlier.CallID == receipt.CallID && earlier.ToolName == receipt.ToolName && earlier.Key.Digest == key.Digest && reflect.DeepEqual(earlier.Components, receipt.Components) && reflect.DeepEqual(earlier.Key.Scope, key.Scope) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			index.receipts[bucket] = append(index.receipts[bucket], receipt)
		}
	}
}

// Retain drops receipts for rows that have left the live view's retained tail.
func (index *ParsedItemIdentities) Retain(ids []string) {
	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
	}
	for key, receipts := range index.receipts {
		retained := receipts[:0]
		for _, receipt := range receipts {
			if keep[receipt.ItemID] {
				retained = append(retained, receipt)
			}
		}
		if len(retained) == 0 {
			delete(index.receipts, key)
		} else {
			index.receipts[key] = retained
		}
	}
}

// ReplayOnly accepts a retained snapshot's already-recorded observations plus
// newly proven replay observations. A genuinely new revision must still update
// the view even when the item retains an older replay relation.
func (index *ParsedItemIdentities) ReplayOnly(item model.Item, id string, replays []model.Relation) bool {
	if id == "" || len(replays) == 0 || len(item.Observations) == 0 {
		return false
	}
	known := make(map[string]bool)
	for _, relation := range replays {
		known[relation.FromObservation] = true
	}
	for _, key := range transcriptItemIdentityKeys(item) {
		if key.Kind != "event" {
			continue
		}
		for _, receipt := range index.receipts[key.Kind+"\x00"+key.Value] {
			if receipt.ItemID == id && transcriptIdentityScopeMatches(key.Scope, receipt.Key.Scope, "event") {
				known[receipt.Key.ObservationID] = true
			}
		}
	}
	for _, observation := range item.Observations {
		if !known[observation.ID] {
			return false
		}
	}
	return true
}
