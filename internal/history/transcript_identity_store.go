package history

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// newID is the conversion boundary's proposed ID for a first appearance.
// Existing native or local receipts take precedence over that proposal.
func resolveTranscriptItemIdentityTx(ctx context.Context, tx *sql.Tx, projectionID int64, newID, boundID string, item model.Item) (transcriptIdentityResolution, error) {
	keys := transcriptItemIdentityKeys(item)
	var receipts []transcriptIdentityReceipt
	queried := make(map[string]bool)
	for _, key := range keys {
		queryKey := key.Kind + ":" + key.Value
		if queried[queryKey] {
			continue
		}
		queried[queryKey] = true
		value, err := findTranscriptValueTx(ctx, tx, projectionID, key.Value)
		if err != nil {
			return transcriptIdentityResolution{}, err
		}
		if value == 0 {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT s.value_key,a.item_id,n.value_key,c.value_key,a.item_type,a.role,o.value_key,a.native_event_digest,COALESCE(i.tool_name,'')
FROM conversation_parse_identities a
JOIN conversation_parse_values s ON s.id=a.scope_id
LEFT JOIN conversation_parse_values n ON n.id=a.native_item_value_id
LEFT JOIN conversation_parse_values c ON c.id=a.call_value_id
LEFT JOIN conversation_parse_values o ON o.id=a.observation_value_id
LEFT JOIN conversation_items i ON i.projection_id=a.projection_id AND i.item_id=a.item_id
WHERE a.projection_id=? AND a.identity_kind=? AND a.identity_value_id=? ORDER BY a.id`, projectionID, key.Kind, value)
		if err != nil {
			return transcriptIdentityResolution{}, fmt.Errorf("query parser identity receipts: %w", err)
		}
		for rows.Next() {
			receipt := transcriptIdentityReceipt{Key: transcriptIdentityKey{Kind: key.Kind, Value: key.Value}}
			var scope, native, call, observation []byte
			if err := rows.Scan(&scope, &receipt.ItemID, &native, &call, &receipt.Type, &receipt.Role, &observation, &receipt.Key.Digest, &receipt.ToolName); err != nil {
				rows.Close()
				return transcriptIdentityResolution{}, err
			}
			if err := unmarshalTranscriptValueKeyTx(ctx, tx, projectionID, scope, &receipt.Key.Scope); err != nil {
				rows.Close()
				return transcriptIdentityResolution{}, fmt.Errorf("decode parser identity scope: %w", err)
			}
			for _, pair := range []struct {
				raw   []byte
				value *string
			}{{native, &receipt.NativeItemID}, {call, &receipt.CallID}, {observation, &receipt.Key.ObservationID}} {
				if pair.raw == nil {
					continue
				}
				if err := unmarshalTranscriptValueKeyTx(ctx, tx, projectionID, pair.raw, pair.value); err != nil {
					rows.Close()
					return transcriptIdentityResolution{}, err
				}
			}
			receipts = append(receipts, receipt)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return transcriptIdentityResolution{}, err
		}
	}
	// Compare normalized exact component aliases only for call candidates.
	// No native envelope or composite string is decoded in history.
	if len(item.ExactNativeItemComponents()) != 0 {
		components := make(map[string][]model.Alias)
		for i, receipt := range receipts {
			if receipt.Key.Kind != "call" {
				continue
			}
			aliases, loaded := components[receipt.ItemID]
			if !loaded {
				var raw []byte
				err := tx.QueryRowContext(ctx, `SELECT parsed_json FROM conversation_items WHERE projection_id=? AND item_id=?`, projectionID, receipt.ItemID).Scan(&raw)
				if err != nil && err != sql.ErrNoRows {
					return transcriptIdentityResolution{}, err
				}
				if err == nil {
					retained, err := decodeParsedItemTx(ctx, tx, projectionID, raw)
					if err != nil {
						return transcriptIdentityResolution{}, fmt.Errorf("decode retained identity components: %w", err)
					}
					aliases = retained.Aliases
				}
				aliases = (model.Item{Scope: item.Scope, Aliases: aliases}).ExactNativeItemComponents()
				components[receipt.ItemID] = aliases
			}
			receipts[i].Components = aliases
		}
	}
	result := resolveTranscriptItemIdentity(item, boundID, receipts)
	if result.Diagnostic != "" && result.Diagnostic != "ambiguous_call_identity" {
		return result, nil
	}
	if result.ItemID == "" {
		if newID == "" || item.ID == "" {
			return transcriptIdentityResolution{}, fmt.Errorf("new parser item requires canonical and local identities")
		}
		result.ItemID = newID
	}
	for _, key := range transcriptIdentityReceiptKeys(item, len(result.Replay) != 0) {
		scope, err := internTranscriptValueTx(ctx, tx, projectionID, key.Scope)
		if err != nil {
			return transcriptIdentityResolution{}, err
		}
		value, err := internTranscriptValueTx(ctx, tx, projectionID, key.Value)
		if err != nil {
			return transcriptIdentityResolution{}, err
		}
		nativeItem, err := optionalTranscriptStringTx(ctx, tx, projectionID, item.ExactNativeItemID())
		if err != nil {
			return transcriptIdentityResolution{}, err
		}
		call, err := optionalTranscriptStringTx(ctx, tx, projectionID, item.CallID)
		if err != nil {
			return transcriptIdentityResolution{}, err
		}
		observation, err := optionalTranscriptStringTx(ctx, tx, projectionID, key.ObservationID)
		if err != nil {
			return transcriptIdentityResolution{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_parse_identities(
projection_id, identity_kind, identity_value_id, scope_id, item_id, native_item_value_id, call_value_id, item_type, role, observation_value_id, native_event_digest)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(projection_id, identity_kind, identity_value_id, scope_id, item_id) DO UPDATE SET
native_item_value_id=COALESCE(native_item_value_id,excluded.native_item_value_id),
call_value_id=COALESCE(call_value_id,excluded.call_value_id),
item_type=excluded.item_type, role=excluded.role`, projectionID, key.Kind, value, scope, result.ItemID, nativeItem, call, item.Type, item.Role, observation, key.Digest)
		if err != nil {
			return transcriptIdentityResolution{}, fmt.Errorf("record parser identity receipt: %w", err)
		}
	}
	return result, nil
}
