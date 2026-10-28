package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func writeTranscriptResultTx(ctx context.Context, tx *sql.Tx, projectionID int64, result TranscriptProjectionResult) error {
	if result.parsedBatches == nil {
		return writeTranscriptBatch(ctx, tx, projectionID, result)
	}
	var nativeThreadID string
	if err := tx.QueryRowContext(ctx, `SELECT t.native_thread_id FROM conversation_projections p JOIN threads t ON t.id=p.thread_id WHERE p.id=?`, projectionID).Scan(&nativeThreadID); err != nil {
		return err
	}
	err := result.parsedBatches(func(batch model.Batch) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return writeParsedBatchTx(ctx, tx, projectionID, nativeThreadID, batch, result.recordSource)
	})
	if err != nil {
		return err
	}
	return writeTranscriptSessionUsageTx(ctx, tx, projectionID, result.sessionUsage)
}

func writeTranscriptSessionUsageTx(ctx context.Context, tx *sql.Tx, projectionID int64, usage *Metadata) error {
	if usage == nil {
		return nil
	}
	// Preserve the SQL/API display contract without turning a current summary
	// into historical per-turn provider evidence. parsed_json stays untouched.
	value := func(field MetadataField) any {
		if field.State == MetadataKnown && field.Value != nil {
			return *field.Value
		}
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET usage_model=?, usage_effort=? WHERE projection_id=? AND usage_contract=1`, value(usage.Model), value(usage.Effort), projectionID)
	return err
}

func writeParsedTranscriptBatchTx(ctx context.Context, tx *sql.Tx, projectionID int64, nativeThreadID string, batch model.Batch) error {
	return writeParsedBatchTx(ctx, tx, projectionID, nativeThreadID, batch, "")
}

func writeParsedBatchTx(ctx context.Context, tx *sql.Tx, projectionID int64, nativeThreadID string, batch model.Batch, recordSource string) error {
	// Keep source arrival order before attaching observations to changing views.
	for _, observation := range batch.Observations {
		if err := writeTranscriptObservationTx(ctx, tx, projectionID, observation); err != nil {
			return err
		}
	}
	// A reader emits full unit snapshots. Write the latest snapshot once at
	// its first appearance in this batch, retaining every independent fact.
	changes := compactParsedChanges(batch.Changes)
	starts := parsedTurnDisplayStarts(batch.Observations)
	owners := make(map[string]TranscriptTurn)
	// Saved turns this batch adds rows to, and those with a snapshot in it,
	// which that snapshot's write positions.
	joined, snapshots := map[string]bool{}, map[string]bool{}
	for _, change := range changes {
		if change.Turn == nil {
			continue
		}
		view, err := ensureParsedTranscriptTurnTx(ctx, tx, projectionID, *change.Turn)
		if err != nil {
			return err
		}
		owners[change.Turn.Key] = view
		snapshots[view.Key] = true
	}
	for _, entry := range parsedFileEntries(batch, changes, recordSource) {
		if entry.record != nil {
			owner, err := parsedRecordOwnerTx(ctx, tx, projectionID, entry, owners, changes)
			if err != nil {
				return err
			}
			if err := writeParsedRecordViewTx(ctx, tx, projectionID, nativeThreadID, owner, entry); err != nil {
				return err
			}
			joined[owner.Key] = true
			continue
		}
		item := *entry.item
		owner, ok := owners[item.TurnKey]
		if !ok || item.TurnKey == "" {
			var err error
			owner, err = parsedItemOwnerTx(ctx, tx, projectionID, item)
			if err != nil {
				return err
			}
			if item.TurnKey != "" {
				owners[item.TurnKey] = owner
			}
			joined[owner.Key] = true
		}
		_, replay, err := writeParsedTranscriptItemTx(ctx, tx, projectionID, nativeThreadID, owner, item)
		if err != nil {
			return err
		}
		for _, relation := range replay {
			relation := relation
			batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
		}
	}
	for _, change := range changes {
		if change.Turn == nil {
			continue
		}
		if _, err := writeParsedTranscriptTurnTx(ctx, tx, projectionID, *change.Turn, starts[change.Turn.Key]); err != nil {
			return err
		}
	}
	// A record after a turn's completion can add an item to a turn saved by an
	// earlier append, with no new snapshot of that turn; its positions follow
	// its items here, as a snapshot write would set them. A completed turn's
	// completion position is its end.
	for key := range joined {
		if key == "" || snapshots[key] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET start_seq=COALESCE((SELECT MIN(projected_seq) FROM conversation_items i WHERE i.projection_id=conversation_turns.projection_id AND i.turn_key=conversation_turns.turn_key),0),end_seq=COALESCE((SELECT MAX(projected_seq) FROM conversation_items i WHERE i.projection_id=conversation_turns.projection_id AND i.turn_key=conversation_turns.turn_key),0) WHERE projection_id=? AND turn_key=?`, projectionID, key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET completion_seq=end_seq WHERE projection_id=? AND turn_key=? AND completion_seq>0`, projectionID, key); err != nil {
			return err
		}
	}
	// A metadata/control item may have no module turn. Its history-only group
	// has no native identity, completion or source-model snapshot.
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET start_seq=COALESCE((SELECT MIN(projected_seq) FROM conversation_items i WHERE i.projection_id=conversation_turns.projection_id AND i.turn_key=conversation_turns.turn_key),0),end_seq=COALESCE((SELECT MAX(projected_seq) FROM conversation_items i WHERE i.projection_id=conversation_turns.projection_id AND i.turn_key=conversation_turns.turn_key),0) WHERE projection_id=? AND parsed_json='{}'`, projectionID); err != nil {
		return err
	}
	for _, change := range changes {
		if change.Item != nil {
			for _, relation := range change.Item.Relations {
				relation := relation
				batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
			}
		}
	}
	resolved, err := resolveTranscriptRelationsTx(ctx, tx, projectionID, batch)
	if err != nil {
		return err
	}
	for i, change := range resolved.Changes {
		if change.Relation != nil {
			if err := writeParsedRelationTx(ctx, tx, projectionID, *batch.Changes[i].Relation, *change.Relation); err != nil {
				return err
			}
		}
	}
	for _, diagnostic := range resolved.Diagnostics {
		if err := writeParsedDiagnosticTx(ctx, tx, projectionID, diagnostic); err != nil {
			return err
		}
	}
	return nil
}

func writeParsedDiagnosticTx(ctx context.Context, tx *sql.Tx, projectionID int64, diagnostic model.Diagnostic) error {
	raw, err := json.Marshal(diagnostic)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_parse_diagnostics(projection_id,diagnostic_key,diagnostic_json) VALUES (?,?,?)`, projectionID, fmt.Sprintf("%x", sha256.Sum256(raw)), raw)
	return err
}

func compactParsedChanges(changes []model.Change) []model.Change {
	compacted := make([]model.Change, 0, len(changes))
	items, turns := make(map[string]int), make(map[string]int)
	for _, change := range changes {
		var key string
		var indices map[string]int
		if change.Item != nil {
			key, indices = change.Item.ID, items
		} else if change.Turn != nil {
			key, indices = change.Turn.Key, turns
		}
		if indices != nil {
			if index, ok := indices[key]; ok {
				compacted[index] = change
				continue
			}
			indices[key] = len(compacted)
		}
		compacted = append(compacted, change)
	}
	return compacted
}

func compactParsedTurnChanges(changes []model.Change) []model.Change {
	compacted := make([]model.Change, 0, len(changes))
	turns := make(map[string]int)
	for _, change := range changes {
		if change.Turn != nil {
			key := change.Turn.Key
			if index, ok := turns[key]; ok {
				compacted[index] = change
				continue
			}
			turns[key] = len(compacted)
		}
		compacted = append(compacted, change)
	}
	return compacted
}

func parsedItemOwnerTx(ctx context.Context, tx *sql.Tx, projectionID int64, item model.Item) (TranscriptTurn, error) {
	if item.TurnKey != "" {
		scope := item.Scope
		scope.Kind, scope.ItemID = model.ScopeTurn, ""
		turn := model.Turn{Key: item.TurnKey, Scope: scope, Status: model.TurnUnknown, CompletionEvidence: model.CompletionNone}
		if scope.NativeTurnID != nil {
			turn.NativeTurnID = *scope.NativeTurnID
		}
		return ensureParsedTranscriptTurnTx(ctx, tx, projectionID, turn)
	}
	scope := item.Scope
	scope.Kind, scope.ItemID = model.ScopeThread, ""
	raw, err := json.Marshal(scope)
	if err != nil {
		return TranscriptTurn{}, err
	}
	group := TranscriptTurn{Key: opaqueID("ntr_", fmt.Sprintf("%d:unscoped:%s", projectionID, raw)), Status: TurnUnknown, CompletionEvidence: CompletionNone}
	err = tx.QueryRowContext(ctx, `SELECT ordinal FROM conversation_turns WHERE projection_id=? AND turn_key=?`, projectionID, group.Key).Scan(&group.Ordinal)
	if err == nil {
		return group, nil
	}
	if err != sql.ErrNoRows {
		return TranscriptTurn{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal),0)+1 FROM conversation_turns WHERE projection_id=?`, projectionID).Scan(&group.Ordinal); err != nil {
		return TranscriptTurn{}, err
	}
	return group, writeTranscriptBatch(ctx, tx, projectionID, TranscriptProjectionResult{Turns: []TranscriptTurn{group}})
}

// The JSON retains source-local endpoints. Indexed endpoint columns resolve
// to history rows only when the scoped local alias has a unique target.
func writeParsedRelationTx(ctx context.Context, tx *sql.Tx, projectionID int64, source, relation model.Relation) error {
	if err := writeTranscriptRelationTx(ctx, tx, projectionID, source); err != nil {
		return err
	}
	resolve := func(local string) (string, error) {
		if local == "" {
			return "", nil
		}
		value, err := findTranscriptValueTx(ctx, tx, projectionID, local)
		if err != nil {
			return "", err
		}
		rows, err := tx.QueryContext(ctx, `SELECT a.item_id,s.value_key FROM conversation_parse_identities a JOIN conversation_parse_values s ON s.id=a.scope_id WHERE a.projection_id=? AND a.identity_kind='local' AND a.identity_value_id=?`, projectionID, value)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		var target string
		for rows.Next() {
			var id string
			var raw []byte
			var scope model.Scope
			if err := rows.Scan(&id, &raw); err != nil {
				return "", err
			}
			if err := unmarshalTranscriptValueKeyTx(ctx, tx, projectionID, raw, &scope); err != nil {
				return "", err
			}
			if !transcriptIdentityScopeMatches(relation.Scope, scope, "local") {
				continue
			}
			if target != "" && target != id {
				return "", nil
			}
			target = id
		}
		return target, rows.Err()
	}
	from, err := resolve(relation.FromItem)
	if err != nil {
		return err
	}
	to, err := resolve(relation.ToItem)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return err
	}
	fromObservation, err := optionalTranscriptStringTx(ctx, tx, projectionID, relation.FromObservation)
	if err != nil {
		return err
	}
	toObservation, err := optionalTranscriptStringTx(ctx, tx, projectionID, relation.ToObservation)
	if err != nil {
		return err
	}
	key := sha256.Sum256(raw)
	_, err = tx.ExecContext(ctx, `UPDATE conversation_relations SET from_item_id=?,to_item_id=?,from_observation_value_id=?,to_observation_value_id=? WHERE projection_id=? AND relation_key=?`, from, to, fromObservation, toObservation, projectionID, key[:])
	return err
}
