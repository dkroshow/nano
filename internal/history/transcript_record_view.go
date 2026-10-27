package history

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Source-only records still need the legacy Dev view's native-evidence link.
// These rows are a history projection, not additional model Items. Keeping a
// terminal record's position also preserves the existing fixed-page cursor.
type parsedFileEntry struct {
	item         *model.Item
	record       *model.Record
	observations []model.Observation
	scope        model.Scope
}

func parsedRecordKey(source model.SourceRef) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d", source.Label, source.Incarnation, source.Line, source.Frame)
}

func parsedFileEntries(batch model.Batch, changes []model.Change, recordSource string) []parsedFileEntry {
	var entries []parsedFileEntry
	for _, change := range changes {
		if change.Item != nil {
			entries = append(entries, parsedFileEntry{item: change.Item})
		}
	}
	if recordSource == "" {
		return entries
	}
	observations := make(map[string][]model.Observation)
	represented := make(map[string]bool)
	for _, obs := range batch.Observations {
		key := parsedRecordKey(obs.Source)
		observations[key] = append(observations[key], obs)
		if obs.Scope.ItemID != "" {
			represented[key] = true
		}
	}
	for _, entry := range entries {
		for _, obs := range entry.item.Observations {
			represented[parsedRecordKey(obs.Source)] = true
		}
	}
	for i := range batch.Records {
		record := &batch.Records[i]
		if record.Source.Kind != "file" || record.Source.Label != recordSource || record.Source.Line == 0 {
			continue
		}
		key := parsedRecordKey(record.Source)
		if represented[key] {
			continue
		}
		facts := observations[key]
		// Blank records carry neither observations nor a malformed/unknown finding.
		if len(facts) == 0 && record.Disposition != model.RecordMalformed && record.Disposition != model.RecordUnknown {
			continue
		}
		scope := model.Scope{Kind: model.ScopeThread, Source: record.Source.Label, Incarnation: record.Source.Incarnation}
		for _, obs := range facts {
			if scope.Provider == "" || obs.Scope.TurnKey != "" {
				scope = obs.Scope
			}
		}
		entries = append(entries, parsedFileEntry{record: record, observations: facts, scope: scope})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		line := func(entry parsedFileEntry) int64 {
			if entry.item != nil {
				return int64(entry.item.SourceLine)
			}
			return entry.record.Source.Line
		}
		return line(entries[i]) < line(entries[j])
	})
	return entries
}

func parsedRecordOwnerTx(ctx context.Context, tx *sql.Tx, projectionID int64, entry parsedFileEntry, owners map[string]TranscriptTurn, changes []model.Change) (TranscriptTurn, error) {
	if entry.scope.TurnKey != "" {
		if owner, ok := owners[entry.scope.TurnKey]; ok {
			return owner, nil
		}
		return parsedItemOwnerTx(ctx, tx, projectionID, model.Item{Scope: entry.scope, TurnKey: entry.scope.TurnKey})
	}
	// This is only the legacy row grouping. It supplies no model/native turn
	// assertion for an otherwise unattributed source record.
	var previous TranscriptTurn
	err := tx.QueryRowContext(ctx, `SELECT turn_key,turn_ordinal FROM conversation_items WHERE projection_id=? ORDER BY projected_seq DESC LIMIT 1`, projectionID).Scan(&previous.Key, &previous.Ordinal)
	if err == nil {
		return previous, nil
	}
	if err != sql.ErrNoRows {
		return TranscriptTurn{}, err
	}
	for _, change := range changes {
		if change.Item != nil && change.Item.Scope.Source == entry.scope.Source && change.Item.Scope.Incarnation == entry.scope.Incarnation {
			return parsedItemOwnerTx(ctx, tx, projectionID, *change.Item)
		}
	}
	for _, change := range changes {
		if change.Turn != nil {
			return owners[change.Turn.Key], nil
		}
	}
	return parsedItemOwnerTx(ctx, tx, projectionID, model.Item{Scope: entry.scope})
}

func writeParsedRecordViewTx(ctx context.Context, tx *sql.Tx, projectionID int64, nativeThreadID string, owner TranscriptTurn, entry parsedFileEntry) error {
	source := entry.record.Source
	key := fmt.Sprintf("line:%d:0", source.Line)
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_items WHERE projection_id=? AND source_key=?)`, projectionID, key).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	var provider Provider
	if err := tx.QueryRowContext(ctx, `SELECT provider FROM conversation_projections WHERE id=?`, projectionID).Scan(&provider); err != nil {
		return err
	}
	item := TranscriptItem{ID: canonicalTranscriptItemID(provider, nativeThreadID, key), SourceKey: key, SourceLine: int(source.Line), SourceLineSHA256: source.SHA256, TurnKey: owner.Key, TurnOrdinal: owner.Ordinal, Type: ItemProviderEvent, Role: RoleSystem, Status: ItemStatusCompleted, Text: "Provider housekeeping record", FilterTags: []string{"housekeeping"}}
	switch entry.record.Disposition {
	case model.RecordMalformed:
		item.Text = "Malformed provider record"
		item.Status = ItemStatusUnknown
		item.FilterTags = []string{"unclassified"}
	case model.RecordUnknown:
		item.Text = "Unclassified provider record"
		item.Status = ItemStatusUnknown
		item.FilterTags = []string{"unclassified"}
	}
	item.OriginalBytes = int64(len(item.Text))
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(projected_seq),0)+1 FROM conversation_items WHERE projection_id=?`, projectionID).Scan(&item.ProjectedSeq); err != nil {
		return err
	}
	if err := writeTranscriptBatch(ctx, tx, projectionID, TranscriptProjectionResult{Items: []TranscriptItem{item}}); err != nil {
		return err
	}
	return linkTranscriptObservationsTx(ctx, tx, projectionID, "item", item.ID, entry.observations)
}
