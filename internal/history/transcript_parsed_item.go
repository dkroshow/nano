package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// The caller resolves the owning turn before writing the item. Existing rows
// keep their first position and owner even when an evicted replay arrives under
// a new parser-local item or turn key.
func writeParsedTranscriptItemTx(ctx context.Context, tx *sql.Tx, projectionID int64, nativeThreadID string, turn TranscriptTurn, item model.Item) (TranscriptItem, []model.Relation, error) {
	prefix := fmt.Sprintf("line:%d:", item.SourceLine)
	if item.SourceLine == 0 {
		prefix = "local:" + item.ID + ":"
	}
	var emission int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(emission_index),-1)+1 FROM conversation_items WHERE projection_id=? AND source_line=?`, projectionID, item.SourceLine).Scan(&emission); err != nil {
		return TranscriptItem{}, nil, err
	}
	sourceKey := fmt.Sprintf("%s%d", prefix, emission)
	proposed := canonicalTranscriptItemID(Provider(item.Scope.Provider), nativeThreadID, sourceKey)
	resolution, err := resolveTranscriptItemIdentityTx(ctx, tx, projectionID, proposed, "", item)
	if err != nil {
		return TranscriptItem{}, nil, err
	}
	if resolution.Diagnostic == "ambiguous_call_identity" {
		d := model.Diagnostic{Code: resolution.Diagnostic, Severity: "warning", Scope: item.Scope, Message: "Call identity matches several retained items; exact event or source-local identity is required"}
		if len(item.Observations) > 0 {
			d.Source = item.Observations[0].Source
		}
		if err := writeParsedDiagnosticTx(ctx, tx, projectionID, d); err != nil {
			return TranscriptItem{}, nil, err
		}
	} else if resolution.Diagnostic != "" {
		return TranscriptItem{}, nil, fmt.Errorf("%s: %w", resolution.Diagnostic, ErrTranscriptConflict)
	}
	position := TranscriptItem{ID: resolution.ItemID, SourceKey: sourceKey, TurnKey: turn.Key, TurnOrdinal: turn.Ordinal, SourceLine: item.SourceLine, SourceLineSHA256: item.SourceLineSHA256, EmissionIndex: emission, SourceRowType: item.SourceRowType, SourcePayloadType: item.SourcePayloadType}
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT projected_seq, source_key, turn_key, turn_ordinal, source_line, source_line_sha256, emission_index, source_row_type, COALESCE(source_payload_type,''), parsed_json FROM conversation_items WHERE projection_id=? AND item_id=?`, projectionID, resolution.ItemID).Scan(&position.ProjectedSeq, &position.SourceKey, &position.TurnKey, &position.TurnOrdinal, &position.SourceLine, &position.SourceLineSHA256, &position.EmissionIndex, &position.SourceRowType, &position.SourcePayloadType, &previous)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TranscriptItem{}, nil, err
	}
	var prior model.Item
	if exists {
		if prior, err = decodeParsedItemTx(ctx, tx, projectionID, previous); err != nil {
			return TranscriptItem{}, nil, fmt.Errorf("decode prior parsed item: %w", err)
		}
	} else {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(projected_seq),0)+1 FROM conversation_items WHERE projection_id=?`, projectionID).Scan(&position.ProjectedSeq); err != nil {
			return TranscriptItem{}, nil, err
		}
	}
	normalized := item
	keepView := false
	if exists && prior.ID != "" {
		replayOnly := item.ID != prior.ID && len(resolution.Replay) > 0
		if replayOnly {
			replayRefs := make(map[string]bool)
			for _, relation := range resolution.Replay {
				replayRefs[relation.FromObservation] = true
			}
			for _, obs := range item.Observations {
				if replayRefs[obs.ID] {
					continue
				}
				var known int
				value, err := internTranscriptValueTx(ctx, tx, projectionID, obs.ID)
				if err != nil {
					return TranscriptItem{}, nil, err
				}
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_item_observations m JOIN conversation_observations o ON o.id=m.observation_id WHERE m.projection_id=? AND m.item_id=? AND o.key_id=?)`, projectionID, position.ID, value).Scan(&known); err != nil {
					return TranscriptItem{}, nil, err
				}
				if known == 0 {
					replayOnly = false
					break
				}
			}
		}
		if replayOnly {
			normalized = prior
			keepView = true
		}
		normalized.ID, normalized.SourceSeq = prior.ID, prior.SourceSeq
		normalized.TurnKey, normalized.TurnOrdinal = prior.TurnKey, prior.TurnOrdinal
		normalized.Scope.ItemID, normalized.Scope.TurnKey = prior.Scope.ItemID, prior.Scope.TurnKey
		if item.ExactNativeItemID() == "" && prior.NativeItemID != "" {
			normalized.NativeItemID = prior.NativeItemID
		}
		normalized.Aliases = mergeParsedAliases(prior.Aliases, item.Aliases)
	}
	visible, body := parsedItemView(normalized, position)
	if keepView {
		// A replay adds evidence without rebuilding content from private state.
		// The retained view/body may be bounded and is the existing display authority.
		retained, _, err := readTranscriptItemRange(ctx, tx, projectionID, position.ProjectedSeq, position.ProjectedSeq+1, nil)
		if err != nil {
			return TranscriptItem{}, nil, err
		}
		if len(retained) != 1 {
			return TranscriptItem{}, nil, fmt.Errorf("missing retained replay view: %w", ErrTranscriptConflict)
		}
		visible = retained[0]
		visible.SourceKey, visible.SourceLine, visible.SourceLineSHA256 = position.SourceKey, position.SourceLine, position.SourceLineSHA256
		visible.EmissionIndex, visible.SourceRowType, visible.SourcePayloadType = position.EmissionIndex, position.SourceRowType, position.SourcePayloadType
	}
	if !exists {
		batch := TranscriptProjectionResult{Items: []TranscriptItem{visible}}
		if body != nil {
			batch.Bodies = []TranscriptBody{*body}
		}
		if err := writeTranscriptBatch(ctx, tx, projectionID, batch); err != nil {
			return TranscriptItem{}, nil, err
		}
	} else if !keepView {
		if err := reviseParsedItemTx(ctx, tx, projectionID, visible, body); err != nil {
			return TranscriptItem{}, nil, err
		}
	}
	for _, obs := range item.Observations {
		if err := writeTranscriptObservationTx(ctx, tx, projectionID, obs); err != nil {
			return TranscriptItem{}, nil, err
		}
	}
	if err := linkTranscriptObservationsTx(ctx, tx, projectionID, "item", position.ID, item.Observations); err != nil {
		return TranscriptItem{}, nil, err
	}
	for _, relation := range append(append([]model.Relation(nil), item.Relations...), resolution.Replay...) {
		if err := writeTranscriptRelationTx(ctx, tx, projectionID, relation); err != nil {
			return TranscriptItem{}, nil, err
		}
	}
	raw, err := encodeParsedItemTx(ctx, tx, projectionID, normalized)
	if err != nil {
		return TranscriptItem{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_items SET parsed_json=? WHERE projection_id=? AND item_id=?`, raw, projectionID, position.ID); err != nil {
		return TranscriptItem{}, nil, err
	}
	return visible, resolution.Replay, nil
}

func parsedItemView(item model.Item, position TranscriptItem) (TranscriptItem, *TranscriptBody) {
	text, body := boundTranscriptText(position.ID, item.Text)
	position.NativeItemID = item.NativeItemID
	position.Type, position.Category, position.Status, position.Role = TranscriptItemType(item.Type), TranscriptCategory(item.Category), TranscriptItemStatus(item.Status), TranscriptRole(item.Role)
	position.Text, position.ToolName, position.CallID = text, item.ToolName, item.CallID
	position.Detail = safeDetail(item.Detail)
	position.CreatedAt, position.Phase = cloneTime(item.CreatedAt), item.Phase
	position.FilterTags = append([]string(nil), item.FilterTags...)
	position.Synthetic, position.Sidechain = item.Synthetic, item.Sidechain
	position.IsFinalAnswer = item.IsFinalAnswer
	position.ContentTruncated = body != nil || item.ContentTruncated
	position.OriginalBytes = int64(len(item.Text))
	if item.OriginalBytes > position.OriginalBytes {
		position.OriginalBytes = item.OriginalBytes
	}
	if body != nil {
		// Revisions can change content without changing byte length. A cached
		// body reference must identify the actual content, not only its size.
		body.ContentRef = opaqueID("ncb_", position.ID+":"+strings.TrimSpace(item.Text))
		position.ContentRef = body.ContentRef
	}
	return position, body
}

func mergeParsedAliases(prior, incoming []model.Alias) []model.Alias {
	merged := append([]model.Alias(nil), prior...)
	for _, candidate := range incoming {
		found := false
		for i, alias := range merged {
			if alias.Namespace == candidate.Namespace && bytes.Equal(alias.Value, candidate.Value) && alias.Authority == candidate.Authority {
				refs := append([]string(nil), alias.ObservationRefs...)
				for _, ref := range candidate.ObservationRefs {
					seen := false
					for _, priorRef := range refs {
						seen = seen || priorRef == ref
					}
					if !seen {
						refs = append(refs, ref)
					}
				}
				merged[i].ObservationRefs = refs
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, candidate)
		}
	}
	return merged
}

func reviseParsedItemTx(ctx context.Context, tx *sql.Tx, projectionID int64, item TranscriptItem, body *TranscriptBody) error {
	tags, err := json.Marshal(item.FilterTags)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE conversation_items SET native_item_id=?, item_type=?, item_status=?, role=?, display_text=?, tool_name=?, call_id=?, detail_json=?, created_at=?, phase=?, filter_tags_json=?, synthetic=?, sidechain=?, is_final_answer=?, content_ref=?, content_truncated=?, original_bytes=? WHERE projection_id=? AND item_id=?`, nullableText(item.NativeItemID), item.Type, item.Status, nullableText(string(item.Role)), item.Text, nullableText(item.ToolName), nullableText(item.CallID), []byte(item.Detail), nullableTime(item.CreatedAt), nullableText(item.Phase), tags, item.Synthetic, item.Sidechain, item.IsFinalAnswer, nullableText(item.ContentRef), item.ContentTruncated, item.OriginalBytes, projectionID, item.ID)
	if err != nil {
		return fmt.Errorf("revise parsed transcript item: %w", err)
	}
	if body == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM conversation_item_bodies WHERE projection_id=? AND item_id=?`, projectionID, item.ID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_item_bodies(projection_id,item_id,content_ref,body_text,body_json,original_bytes,stored_bytes,truncated) VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(projection_id,item_id) DO UPDATE SET content_ref=excluded.content_ref,body_text=excluded.body_text,body_json=excluded.body_json,original_bytes=excluded.original_bytes,stored_bytes=excluded.stored_bytes,truncated=excluded.truncated`, projectionID, item.ID, body.ContentRef, nullableText(body.Text), nullableBytes(body.JSON), body.OriginalBytes, body.StoredBytes, body.Truncated)
	}
	if err != nil {
		return fmt.Errorf("revise parsed transcript body: %w", err)
	}
	// Existing FTS rowids are append-only in the ordinary upsert helper. Replace
	// this item's derived document before applying the shared eligibility rule.
	removed, err := removeConversationSearchDocumentTx(ctx, tx, projectionID, item.ID)
	if err != nil {
		return err
	}
	changed, err := maintainConversationSearchForProjectionSequenceRangeTx(ctx, tx, projectionID, item.ProjectedSeq-1, item.ProjectedSeq)
	if err != nil || !removed || changed {
		return err
	}
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return err
	}
	return updateMaintainedConversationSearchStateTx(ctx, tx, statuses, time.Now().UTC())
}
