package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func resolveTranscriptRelationsTx(ctx context.Context, tx *sql.Tx, projectionID int64, batch model.Batch) (model.Batch, error) {
	// Explicit replays remain evidence but cannot introduce a second target
	// for the native identity whose earlier occurrence they reference.
	replays := make(map[string]bool)
	for _, change := range batch.Changes {
		if change.Relation != nil && change.Relation.Kind == model.RelationReplayOf {
			replays[change.Relation.FromObservation] = true
		}
	}
	index := func(scope model.Scope, namespace string, raw []byte, observationID string) error {
		value, valid := transcriptNativeScalar(raw)
		if !valid || namespace == "" || observationID == "" || replays[observationID] {
			return nil
		}
		scopeID, err := internTranscriptValueTx(ctx, tx, projectionID, scope)
		if err != nil {
			return err
		}
		valueID, err := internTranscriptValueTx(ctx, tx, projectionID, value)
		if err != nil {
			return err
		}
		observation, err := internTranscriptValueTx(ctx, tx, projectionID, observationID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_observation_aliases(projection_id, namespace, value_id, observation_value_id, scope_id) VALUES (?, ?, ?, ?, ?)`, projectionID, namespace, valueID, observation, scopeID)
		return err
	}
	for _, obs := range batch.Observations {
		if obs.NativeEventID != "" {
			value, _ := json.Marshal(obs.NativeEventID)
			if err := index(obs.Scope, "native.eventId", value, obs.ID); err != nil {
				return batch, fmt.Errorf("index native observation: %w", err)
			}
		}
	}
	for _, change := range batch.Changes {
		if change.Item == nil {
			continue
		}
		for _, alias := range change.Item.Aliases {
			if alias.Authority != model.BasisNative && alias.Authority != model.BasisDerivedExact {
				continue
			}
			for _, id := range alias.ObservationRefs {
				if err := index(change.Item.Scope, alias.Namespace, alias.Value, id); err != nil {
					return batch, fmt.Errorf("index native item alias: %w", err)
				}
			}
		}
	}
	next := batch
	next.Changes = append([]model.Change(nil), batch.Changes...)
	next.Diagnostics = append([]model.Diagnostic(nil), batch.Diagnostics...)
	for i, change := range batch.Changes {
		if change.Relation == nil || change.Relation.ToNativeObservation == nil || change.Relation.ToObservation != "" {
			continue
		}
		ref := change.Relation.ToNativeObservation
		value, valid := transcriptNativeScalar(ref.Value)
		var targets []transcriptAliasTarget
		if valid {
			valueID, err := findTranscriptValueTx(ctx, tx, projectionID, value)
			if err != nil {
				return batch, err
			}
			rows, err := tx.QueryContext(ctx, `SELECT o.value_key,s.value_key FROM conversation_observation_aliases a JOIN conversation_parse_values o ON o.id=a.observation_value_id JOIN conversation_parse_values s ON s.id=a.scope_id WHERE a.projection_id=? AND a.namespace=? AND a.value_id=? ORDER BY a.observation_value_id`, projectionID, ref.Namespace, valueID)
			if err != nil {
				return batch, fmt.Errorf("query native observation targets: %w", err)
			}
			// One compatible target resolves; two make the reference ambiguous.
			// Value insertion order cannot select among distinct native targets.
			seen := make(map[string]bool)
			for rows.Next() {
				target := transcriptAliasTarget{Namespace: ref.Namespace, Value: ref.Value}
				var raw, observation []byte
				if err := rows.Scan(&observation, &raw); err != nil {
					rows.Close()
					return batch, err
				}
				if err := unmarshalTranscriptValueKeyTx(ctx, tx, projectionID, raw, &target.Scope); err != nil {
					rows.Close()
					return batch, fmt.Errorf("decode native observation scope: %w", err)
				}
				if err := unmarshalTranscriptValueKeyTx(ctx, tx, projectionID, observation, &target.ObservationID); err != nil {
					rows.Close()
					return batch, err
				}
				if target.ObservationID != change.Relation.FromObservation && !seen[target.ObservationID] && transcriptReferenceScopeMatches(ref.Scope, target.Scope) {
					seen[target.ObservationID] = true
					targets = append(targets, target)
					if len(targets) == 2 {
						break
					}
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return batch, err
			}
		}
		resolved := resolveTranscriptRelations(model.Batch{Observations: batch.Observations, Changes: []model.Change{change}}, targets)
		next.Changes[i] = resolved.Changes[0]
		next.Diagnostics = append(next.Diagnostics, resolved.Diagnostics...)
	}
	return next, nil
}
