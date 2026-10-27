package history

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type storedRelation struct {
	Scope           int64                       `json:"s"`
	FromItem        int64                       `json:"fi,omitempty"`
	ToItem          int64                       `json:"ti,omitempty"`
	FromObservation int64                       `json:"fo,omitempty"`
	ToObservation   int64                       `json:"to,omitempty"`
	Native          *storedNativeObservationRef `json:"n,omitempty"`
	Refs            []int64                     `json:"r"`
}

type storedNativeObservationRef struct {
	Scope     int64  `json:"s"`
	Namespace string `json:"n"`
	Value     int64  `json:"v"`
}

func nullablePositiveID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func storeTranscriptRefsTx(ctx context.Context, tx *sql.Tx, projectionID int64, refs []string) ([]int64, error) {
	if refs == nil {
		return nil, nil
	}
	stored := make([]int64, len(refs))
	for i, ref := range refs {
		id, err := internTranscriptValueTx(ctx, tx, projectionID, ref)
		if err != nil {
			return nil, err
		}
		stored[i] = id
	}
	return stored, nil
}

func loadTranscriptRefsTx(ctx context.Context, tx *sql.Tx, projectionID int64, stored []int64) ([]string, error) {
	if stored == nil {
		return nil, nil
	}
	refs := make([]string, len(stored))
	for i, id := range stored {
		if err := loadTranscriptValueTx(ctx, tx, projectionID, id, &refs[i]); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

func compactTranscriptRelationTx(ctx context.Context, tx *sql.Tx, projectionID int64, relation model.Relation) (storedRelation, error) {
	var stored storedRelation
	var err error
	stored.Scope, err = internTranscriptValueTx(ctx, tx, projectionID, relation.Scope)
	if err != nil {
		return stored, err
	}
	for _, pair := range []struct {
		value string
		id    *int64
	}{{relation.FromItem, &stored.FromItem}, {relation.ToItem, &stored.ToItem}, {relation.FromObservation, &stored.FromObservation}, {relation.ToObservation, &stored.ToObservation}} {
		if pair.value != "" {
			*pair.id, err = internTranscriptValueTx(ctx, tx, projectionID, pair.value)
			if err != nil {
				return stored, err
			}
		}
	}
	stored.Refs, err = storeTranscriptRefsTx(ctx, tx, projectionID, relation.ObservationRefs)
	if err != nil {
		return stored, err
	}
	if ref := relation.ToNativeObservation; ref != nil {
		stored.Native = &storedNativeObservationRef{Namespace: ref.Namespace}
		stored.Native.Scope, err = internTranscriptValueTx(ctx, tx, projectionID, ref.Scope)
		if err != nil {
			return stored, err
		}
		stored.Native.Value, err = internTranscriptValueTx(ctx, tx, projectionID, ref.Value)
	}
	return stored, err
}

func loadTranscriptRelationTx(ctx context.Context, tx *sql.Tx, projectionID int64, kind model.RelationKind, raw []byte) (model.Relation, error) {
	relation := model.Relation{Kind: kind}
	var stored storedRelation
	if err := json.Unmarshal(raw, &stored); err != nil {
		return relation, err
	}
	if err := loadTranscriptValueTx(ctx, tx, projectionID, stored.Scope, &relation.Scope); err != nil {
		return relation, err
	}
	for _, pair := range []struct {
		id    int64
		value *string
	}{{stored.FromItem, &relation.FromItem}, {stored.ToItem, &relation.ToItem}, {stored.FromObservation, &relation.FromObservation}, {stored.ToObservation, &relation.ToObservation}} {
		if pair.id != 0 {
			if err := loadTranscriptValueTx(ctx, tx, projectionID, pair.id, pair.value); err != nil {
				return relation, err
			}
		}
	}
	var err error
	relation.ObservationRefs, err = loadTranscriptRefsTx(ctx, tx, projectionID, stored.Refs)
	if err != nil {
		return relation, err
	}
	if ref := stored.Native; ref != nil {
		relation.ToNativeObservation = &model.NativeObservationRef{Namespace: ref.Namespace}
		if err := loadTranscriptValueTx(ctx, tx, projectionID, ref.Scope, &relation.ToNativeObservation.Scope); err != nil {
			return relation, err
		}
		err = loadTranscriptValueTx(ctx, tx, projectionID, ref.Value, &relation.ToNativeObservation.Value)
	}
	return relation, err
}
