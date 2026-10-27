package history

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Payload text and native JSON remain at Source. These fields are derived
// parser facts; integer keys refer to exact values in this projection.
type storedObservationFacts struct {
	Kind        model.ObservationKind   `json:"k"`
	NativeEvent int64                   `json:"n,omitempty"`
	NativeHash  string                  `json:"h,omitempty"`
	Complete    *bool                   `json:"c,omitempty"`
	Outcome     model.Outcome           `json:"o,omitempty"`
	Capture     model.CaptureState      `json:"a,omitempty"`
	Times       []storedObservationTime `json:"t,omitempty"`
	Metadata    model.Metadata          `json:"m,omitempty"`
}

type storedTimeFact struct {
	Scope     int64             `json:"s"`
	Ref       int64             `json:"r"`
	Value     *time.Time        `json:"v"`
	Raw       json.RawMessage   `json:"j,omitempty"`
	Class     model.TimeClass   `json:"c"`
	Field     string            `json:"f"`
	Precision string            `json:"p,omitempty"`
	Meaning   model.TimeMeaning `json:"m"`
}

func storeTranscriptTimesTx(ctx context.Context, tx *sql.Tx, projectionID int64, times []model.TimeObservation) ([]storedTimeFact, error) {
	var stored []storedTimeFact
	for _, fact := range times {
		scope, err := internTranscriptValueTx(ctx, tx, projectionID, fact.Scope)
		if err != nil {
			return nil, err
		}
		ref, err := internTranscriptValueTx(ctx, tx, projectionID, fact.ObservationRef)
		if err != nil {
			return nil, err
		}
		stored = append(stored, storedTimeFact{Scope: scope, Ref: ref, Value: fact.Value, Raw: fact.Raw, Class: fact.Class, Field: fact.SourceField, Precision: fact.Precision, Meaning: fact.Meaning})
	}
	return stored, nil
}

func loadTranscriptTimesTx(ctx context.Context, tx *sql.Tx, projectionID int64, stored []storedTimeFact) ([]model.TimeObservation, error) {
	var times []model.TimeObservation
	for _, fact := range stored {
		value := model.TimeObservation{Value: fact.Value, Raw: fact.Raw, Class: fact.Class, SourceField: fact.Field, Precision: fact.Precision, Meaning: fact.Meaning}
		if err := loadTranscriptValueTx(ctx, tx, projectionID, fact.Scope, &value.Scope); err != nil {
			return nil, err
		}
		if err := loadTranscriptValueTx(ctx, tx, projectionID, fact.Ref, &value.ObservationRef); err != nil {
			return nil, err
		}
		times = append(times, value)
	}
	return times, nil
}

// loadTranscriptObservationTx loads facts and a source address. Text and Detail
// are deliberately absent: callers needing bytes must use a guarded source read.
func loadTranscriptObservationTx(ctx context.Context, tx *sql.Tx, projectionID int64, observationID string) (model.Observation, error) {
	var observation model.Observation
	var scopeID, sourceID, pathID int64
	var byteLength int64
	var facts, sourceHash []byte
	key, err := findTranscriptValueTx(ctx, tx, projectionID, observationID)
	if err != nil {
		return observation, err
	}
	err = tx.QueryRowContext(ctx, `SELECT o.scope_id,o.source_id,o.block_path_id,o.source_line,o.source_frame,o.byte_start,o.byte_length,o.source_sha256,o.facts_blob
FROM conversation_observations o
WHERE o.projection_id=? AND o.key_id=?`, projectionID, key).Scan(&scopeID, &sourceID, &pathID, &observation.Source.Line, &observation.Source.Frame, &observation.Source.Start, &byteLength, &sourceHash, &facts)
	if err != nil {
		return observation, err
	}
	observation.Source.SHA256 = hex.EncodeToString(sourceHash)
	position := observation.Source
	if err := loadTranscriptValueTx(ctx, tx, projectionID, scopeID, &observation.Scope); err != nil {
		return observation, err
	}
	if err := loadTranscriptValueTx(ctx, tx, projectionID, sourceID, &observation.Source); err != nil {
		return observation, err
	}
	if err := loadTranscriptValueTx(ctx, tx, projectionID, pathID, &observation.Source.BlockPath); err != nil {
		return observation, err
	}
	observation.ID = observationID
	observation.Source.Line, observation.Source.Frame = position.Line, position.Frame
	observation.Source.Start, observation.Source.End = position.Start, position.Start+byteLength
	observation.Source.SHA256 = position.SHA256
	var saved storedObservationFacts
	facts, err = decodeTranscriptFacts(facts)
	if err != nil {
		return observation, err
	}
	if err := json.Unmarshal(facts, &saved); err != nil {
		return observation, err
	}
	if saved.NativeEvent != 0 {
		if err := loadTranscriptValueTx(ctx, tx, projectionID, saved.NativeEvent, &observation.NativeEventID); err != nil {
			return observation, err
		}
	}
	observation.Kind, observation.NativeEventDigest = saved.Kind, saved.NativeHash
	observation.Complete, observation.Outcome, observation.Capture = saved.Complete, saved.Outcome, saved.Capture
	observation.Metadata = saved.Metadata
	times, err := expandObservationTimes(saved.Times, scopeID, key)
	if err != nil {
		return observation, err
	}
	observation.Times, err = loadTranscriptTimesTx(ctx, tx, projectionID, times)
	return observation, err
}
