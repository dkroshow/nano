package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Turn aliases persist only in history. The parser can discard settled turns
// without loading previous history rows into its continuation state.
func ensureParsedTranscriptTurnTx(ctx context.Context, tx *sql.Tx, projectionID int64, turn model.Turn) (TranscriptTurn, error) {
	if turn.Key == "" || turn.Scope.Provider == "" || turn.Scope.Source == "" || turn.Scope.Incarnation == "" {
		return TranscriptTurn{}, fmt.Errorf("parsed turn requires a scoped local identity")
	}
	if turn.NativeTurnID != "" {
		if turn.Scope.NativeTurnID != nil && *turn.Scope.NativeTurnID != turn.NativeTurnID {
			return TranscriptTurn{}, fmt.Errorf("conflicting native turn scope: %w", ErrTranscriptConflict)
		}
		turn.Scope.NativeTurnID = &turn.NativeTurnID
	}
	aliasScope := turn.Scope
	aliasScope.Kind, aliasScope.ItemID, aliasScope.TurnKey, aliasScope.NativeTurnID = model.ScopeTurn, "", "", nil
	localID, err := internTranscriptValueTx(ctx, tx, projectionID, turn.Key)
	if err != nil {
		return TranscriptTurn{}, err
	}
	var key string
	rows, err := tx.QueryContext(ctx, `SELECT s.value_key, a.turn_key, COALESCE(t.native_turn_id,'') FROM conversation_turn_aliases a JOIN conversation_turns t ON t.projection_id=a.projection_id AND t.turn_key=a.turn_key JOIN conversation_parse_values s ON s.id=a.scope_id WHERE a.projection_id=? AND a.local_value_id=?`, projectionID, localID)
	if err != nil {
		return TranscriptTurn{}, err
	}
	for rows.Next() {
		var raw []byte
		var candidate, native string
		var scope model.Scope
		if err = rows.Scan(&raw, &candidate, &native); err == nil {
			err = unmarshalTranscriptValueKeyTx(ctx, tx, projectionID, raw, &scope)
		}
		if err != nil {
			break
		}
		if !transcriptIdentityScopeMatches(aliasScope, scope, "local") {
			continue
		}
		if native != "" && turn.NativeTurnID != "" && native != turn.NativeTurnID || key != "" && key != candidate {
			err = fmt.Errorf("conflicting local turn binding: %w", ErrTranscriptConflict)
			break
		}
		key = candidate
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return TranscriptTurn{}, err
	}
	if key == "" && turn.NativeTurnID != "" {
		rows, err = tx.QueryContext(ctx, `SELECT turn_key, parsed_json FROM conversation_turns WHERE projection_id=? AND native_turn_id=?`, projectionID, turn.NativeTurnID)
		if err != nil {
			return TranscriptTurn{}, err
		}
		for rows.Next() {
			var candidate string
			var raw []byte
			var prior model.Turn
			if err = rows.Scan(&candidate, &raw); err == nil {
				prior, err = decodeParsedTurnHeadTx(ctx, tx, projectionID, raw)
			}
			if err != nil {
				break
			}
			if !transcriptIdentityScopeMatches(turn.Scope, prior.Scope, "event") {
				continue
			}
			if key != "" && key != candidate {
				err = fmt.Errorf("ambiguous native turn binding: %w", ErrTranscriptConflict)
				break
			}
			key = candidate
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return TranscriptTurn{}, err
		}
	}
	encodedScope, err := json.Marshal(aliasScope)
	if err != nil {
		return TranscriptTurn{}, err
	}
	var view TranscriptTurn
	if key == "" {
		key = opaqueID("ntr_", fmt.Sprintf("%d:%s:%s", projectionID, encodedScope, turn.Key))
		var ordinal int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal),0)+1 FROM conversation_turns WHERE projection_id=?`, projectionID).Scan(&ordinal); err != nil {
			return TranscriptTurn{}, err
		}
		view = parsedTurnView(turn, TranscriptTurn{Key: key, Ordinal: ordinal})
		if err := writeTranscriptBatch(ctx, tx, projectionID, TranscriptProjectionResult{Turns: []TranscriptTurn{view}}); err != nil {
			return TranscriptTurn{}, err
		}
		if err := saveParsedTurnModelTx(ctx, tx, projectionID, key, turn); err != nil {
			return TranscriptTurn{}, err
		}
	} else {
		var prior model.Turn
		view, prior, err = loadParsedTranscriptTurnTx(ctx, tx, projectionID, key, false)
		if err != nil {
			return TranscriptTurn{}, err
		}
		// Binding adds identity without rewriting completion or usage. A later
		// full snapshot write below owns changes to those source facts.
		if turn.NativeTurnID != "" && prior.NativeTurnID == "" {
			if _, prior, err = loadParsedTranscriptTurnTx(ctx, tx, projectionID, key, true); err != nil {
				return TranscriptTurn{}, err
			}
			prior.NativeTurnID, prior.Scope.NativeTurnID = turn.NativeTurnID, turn.Scope.NativeTurnID
			view.NativeTurnID = turn.NativeTurnID
			if _, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET native_turn_id=? WHERE projection_id=? AND turn_key=?`, turn.NativeTurnID, projectionID, key); err != nil {
				return TranscriptTurn{}, err
			}
			if err := saveParsedTurnModelTx(ctx, tx, projectionID, key, prior); err != nil {
				return TranscriptTurn{}, err
			}
		}
	}
	scopeID, err := internTranscriptValueTx(ctx, tx, projectionID, aliasScope)
	if err != nil {
		return TranscriptTurn{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_turn_aliases(projection_id,local_value_id,scope_id,turn_key) VALUES (?,?,?,?)`, projectionID, localID, scopeID, key); err != nil {
		return TranscriptTurn{}, err
	}
	return view, nil
}

func writeParsedTranscriptTurnTx(ctx context.Context, tx *sql.Tx, projectionID int64, turn model.Turn, firstNativeTime *time.Time) (TranscriptTurn, error) {
	position, err := ensureParsedTranscriptTurnTx(ctx, tx, projectionID, turn)
	if err != nil {
		return TranscriptTurn{}, err
	}
	_, prior, err := loadParsedTranscriptTurnTx(ctx, tx, projectionID, position.Key, false)
	if err != nil {
		return TranscriptTurn{}, err
	}
	// A turn recreated after source-state eviction is a partial snapshot of
	// the same native turn. Absence of a new completion fact cannot retract
	// the completion already recorded by that turn's earlier observations.
	if turn.Key != prior.Key {
		// A late result contains no turn context. Retain the same native
		// turn's earlier context; never inherit from a different turn.
		if len(turn.Config) == 0 {
			turn.Config = prior.Config
		}
		if turn.Model == "" {
			turn.Model = prior.Model
		}
		if turn.Effort == "" {
			turn.Effort = prior.Effort
		}
		if turn.ServiceTier == "" {
			turn.ServiceTier = prior.ServiceTier
		}
	}
	retainedCompletion := turn.Key != prior.Key && len(turn.Completion) == 0 && (turn.Outcome == "" || turn.Outcome == model.OutcomeUnknown)
	if retainedCompletion {
		turn.Status, turn.Outcome, turn.CompletionEvidence = prior.Status, prior.Outcome, prior.CompletionEvidence
		turn.Completion = prior.Completion
		turn.CompletedAt, turn.DurationMS = prior.CompletedAt, prior.DurationMS
		if turn.StartedAt == nil {
			turn.StartedAt = prior.StartedAt
		}
		if len(turn.Times) == 0 {
			turn.Times = prior.Times
		}
		if len(turn.Durations) == 0 {
			turn.Durations = prior.Durations
		}
		if turn.Capture == "" || turn.Capture == model.CaptureUnknown {
			turn.Capture = prior.Capture
		}
		if len(turn.FinalItemRefs) == 0 {
			turn.FinalItemID, turn.FinalItemRefs = prior.FinalItemID, prior.FinalItemRefs
		}
		if turn.Usage == nil {
			if _, prior, err = loadParsedTranscriptTurnTx(ctx, tx, projectionID, position.Key, true); err != nil {
				return TranscriptTurn{}, err
			}
			turn.Usage, turn.UsageBasis, turn.UsageRefs = prior.Usage, prior.UsageBasis, prior.UsageRefs
			turn.UsageObservations, turn.UsageCoverage, turn.UsageIncomplete = prior.UsageObservations, prior.UsageCoverage, prior.UsageIncomplete
		}
	}
	if turn.NativeTurnID == "" {
		turn.NativeTurnID, turn.Scope.NativeTurnID = prior.NativeTurnID, prior.Scope.NativeTurnID
	} else {
		turn.Scope.NativeTurnID = &turn.NativeTurnID
	}
	view := parsedTurnView(turn, position)
	if view.StartedAt == nil {
		view.StartedAt = cloneTime(firstNativeTime)
	}
	if view.CompletedAt == nil {
		view.CompletedAt = parsedTurnDisplayCompletion(turn)
		if retainedCompletion {
			view.CompletedAt = cloneTime(position.CompletedAt)
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(projected_seq),0),COALESCE(MAX(projected_seq),0) FROM conversation_items WHERE projection_id=? AND turn_key=?`, projectionID, view.Key).Scan(&view.StartSeq, &view.EndSeq); err != nil {
		return TranscriptTurn{}, err
	}
	view.CompletionSeq = 0
	if turn.Status != model.TurnInProgress && len(turn.Completion) > 0 {
		view.CompletionSeq = view.EndSeq
	}
	view.FinalItemID, view.FinalitySource, view.FinalityConfidence = "", "", ""
	if turn.FinalItemID != "" {
		var raw []byte
		var count int
		value, err := findTranscriptValueTx(ctx, tx, projectionID, turn.FinalItemID)
		if err != nil {
			return TranscriptTurn{}, err
		}
		err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT i.item_id),COALESCE(MIN(i.item_id),''),COALESCE(MIN(i.parsed_json),'{}') FROM conversation_parse_identities a JOIN conversation_items i ON i.projection_id=a.projection_id AND i.item_id=a.item_id WHERE a.projection_id=? AND a.identity_kind='local' AND a.identity_value_id=? AND i.turn_key=?`, projectionID, value, view.Key).Scan(&count, &view.FinalItemID, &raw)
		if err != nil {
			return TranscriptTurn{}, err
		}
		if count > 1 {
			return TranscriptTurn{}, fmt.Errorf("ambiguous final item reference: %w", ErrTranscriptConflict)
		}
		if count == 1 {
			item, err := decodeParsedItemTx(ctx, tx, projectionID, raw)
			if err != nil {
				return TranscriptTurn{}, err
			}
			// These labels exist only in the old compatibility DTO. The model
			// keeps the exact finality state, basis and supporting references.
			switch item.Finality.Basis {
			case model.BasisNative, model.BasisDerivedExact:
				view.FinalitySource, view.FinalityConfidence = "native", "high"
			case model.BasisDerived:
				view.FinalitySource, view.FinalityConfidence = "derived", "medium"
			}
		}
	}
	if err := writeTranscriptBatch(ctx, tx, projectionID, TranscriptProjectionResult{Turns: []TranscriptTurn{view}}); err != nil {
		return TranscriptTurn{}, err
	}
	for _, observation := range turn.Observations {
		if err := writeTranscriptObservationTx(ctx, tx, projectionID, observation); err != nil {
			return TranscriptTurn{}, err
		}
	}
	if err := linkTranscriptObservationsTx(ctx, tx, projectionID, "turn", view.Key, turn.Observations); err != nil {
		return TranscriptTurn{}, err
	}
	turn.Key, turn.Ordinal, turn.Scope.TurnKey = prior.Key, prior.Ordinal, prior.Scope.TurnKey
	if prior.StartSourceSeq != 0 {
		turn.StartSourceSeq = prior.StartSourceSeq
	}
	if err := saveParsedTurnModelTx(ctx, tx, projectionID, view.Key, turn); err != nil {
		return TranscriptTurn{}, err
	}
	return view, nil
}

func parsedTurnView(turn model.Turn, position TranscriptTurn) TranscriptTurn {
	position.NativeTurnID = turn.NativeTurnID
	position.Status, position.CompletionEvidence = TranscriptTurnStatus(turn.Status), TranscriptCompletionEvidence(turn.CompletionEvidence)
	if position.Status == "" {
		position.Status = TurnUnknown
	}
	if position.CompletionEvidence == "" {
		position.CompletionEvidence = CompletionNone
	}
	if turn.StartedAt != nil || position.StartedAt == nil {
		position.StartedAt = cloneTime(turn.StartedAt)
	}
	position.CompletedAt = cloneTime(turn.CompletedAt)
	position.DurationMS, position.Config = turn.DurationMS, safeDetail(turn.Config)
	position.Model, position.Effort, position.ServiceTier = turn.Model, turn.Effort, turn.ServiceTier
	position.Usage = (*TranscriptTokenUsage)(turn.Usage)
	return position
}

func saveParsedTurnModelTx(ctx context.Context, tx *sql.Tx, projectionID int64, key string, turn model.Turn) error {
	var known []int64
	var stored []byte
	if err := tx.QueryRowContext(ctx, `SELECT parsed_json FROM conversation_turns WHERE projection_id=? AND turn_key=?`, projectionID, key).Scan(&stored); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if decoded, err := decodeTranscriptContinuation(stored); err == nil {
		var saved struct {
			UsageValues []int64 `json:"uv"`
		}
		if json.Unmarshal(decoded, &saved) == nil {
			known = saved.UsageValues
		}
	}
	raw, err := encodeParsedTurnOverTx(ctx, tx, projectionID, turn, known)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE conversation_turns SET parsed_json=? WHERE projection_id=? AND turn_key=?`, raw, projectionID, key)
	return err
}

func loadParsedTranscriptTurnTx(ctx context.Context, tx *sql.Tx, projectionID int64, key string, usage bool) (TranscriptTurn, model.Turn, error) {
	position := TranscriptTurn{Key: key}
	var raw []byte
	var turn model.Turn
	var started, completed sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT ordinal,start_seq,end_seq,COALESCE(completion_seq,0),COALESCE(final_item_id,''),COALESCE(finality_source,''),COALESCE(finality_confidence,''),parsed_json,started_at,completed_at FROM conversation_turns WHERE projection_id=? AND turn_key=?`, projectionID, key).Scan(&position.Ordinal, &position.StartSeq, &position.EndSeq, &position.CompletionSeq, &position.FinalItemID, &position.FinalitySource, &position.FinalityConfidence, &raw, &started, &completed)
	if err == nil {
		turn, err = decodeStoredTurnTx(ctx, tx, projectionID, raw, usage)
	}
	view := parsedTurnView(turn, position)
	if err == nil {
		view.StartedAt, err = parseNullTime(started)
	}
	if err == nil {
		view.CompletedAt, err = parseNullTime(completed)
	}
	return view, turn, err
}
