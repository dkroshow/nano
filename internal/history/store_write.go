package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ApplySourceOptions is used when a pre-indexed shadow is about to replace a
// thread: maintaining that thread's old projection would do a full search pass.
type ApplySourceOptions struct{ SkipThreadSearch string }

func (s *Store) ApplySource(ctx context.Context, input SourceProjection, options ...ApplySourceOptions) (ApplyResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := validateSourceProjection(input); err != nil {
		return ApplyResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyResult{}, err
	}
	defer tx.Rollback()
	root, _, err := ensureRootTx(ctx, tx, input.Source.Root)
	if err != nil {
		return ApplyResult{}, err
	}
	result, err := applySourceTx(ctx, tx, root.ID, input)
	if err != nil {
		return ApplyResult{}, err
	}
	searchThreads := result.searchThreads
	if len(options) > 0 && options[0].SkipThreadSearch != "" {
		searchThreads = nil
		for _, id := range result.searchThreads {
			if id != options[0].SkipThreadSearch {
				searchThreads = append(searchThreads, id)
			}
		}
	}
	if _, err := maintainConversationSearchForThreadsTx(ctx, tx, searchThreads); err != nil {
		return ApplyResult{}, fmt.Errorf("maintain source conversation search rows: %w", err)
	}
	version, err := catalogVersionTx(ctx, tx)
	if err != nil {
		return ApplyResult{}, err
	}
	if result.changed {
		version, err = bumpCatalogVersion(ctx, tx)
		if err != nil {
			return ApplyResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{SourceID: result.sourceID, ThreadID: result.threadID,
		CatalogVersion: version, Changed: result.changed}, nil
}

// SyncThreadNames rebuilds the derived thread_names table from the truth-file
// snapshot: rows absent from records are deleted, present rows are diff-upserted.
// It bumps the catalog version only when a row actually changed, so a no-op
// refresh (the common case) never churns open browsers.
func (s *Store) SyncThreadNames(ctx context.Context, records []ThreadNameRecord) (bool, error) {
	for _, record := range records {
		if err := validateThreadNameRecord(record); err != nil {
			return false, fmt.Errorf("invalid thread name (%s, %s): %w", record.Provider, record.NativeThreadID, err)
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	changed, err := syncThreadNamesTx(ctx, tx, records)
	if err != nil {
		return false, err
	}
	if changed {
		if _, err := bumpCatalogVersion(ctx, tx); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

func syncThreadNamesTx(ctx context.Context, tx *sql.Tx, records []ThreadNameRecord) (bool, error) {
	desired := make(map[string]bool, len(records))
	for _, record := range records {
		desired[threadNameKey(record.Provider, record.NativeThreadID)] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT provider, native_thread_id, name, provenance FROM thread_names`)
	if err != nil {
		return false, err
	}
	type key struct {
		provider, nativeThreadID string
		name, provenance         string
	}
	existingByKey := make(map[string]key)
	var stale []key
	for rows.Next() {
		var existing key
		if err := rows.Scan(&existing.provider, &existing.nativeThreadID, &existing.name, &existing.provenance); err != nil {
			rows.Close()
			return false, err
		}
		nameKey := threadNameKey(Provider(existing.provider), existing.nativeThreadID)
		existingByKey[nameKey] = existing
		if !desired[nameKey] {
			stale = append(stale, existing)
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	changed := false
	for _, existing := range stale {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM thread_names WHERE provider = ? AND native_thread_id = ?`,
			existing.provider, existing.nativeThreadID)
		if err != nil {
			return false, err
		}
		rowChanged, err := resultChanged(result)
		if err != nil {
			return false, err
		}
		changed = changed || rowChanged
	}
	for _, record := range records {
		nameKey := threadNameKey(record.Provider, record.NativeThreadID)
		existing, exists := existingByKey[nameKey]
		visibleChanged := !exists || existing.name != record.Name || existing.provenance != string(record.Provenance)
		_, err := tx.ExecContext(ctx, `
INSERT INTO thread_names(provider, native_thread_id, name, provenance, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(provider, native_thread_id) DO UPDATE SET
  name = excluded.name, provenance = excluded.provenance, updated_at = excluded.updated_at
WHERE name IS NOT excluded.name OR provenance IS NOT excluded.provenance
   OR updated_at IS NOT excluded.updated_at
`,
			string(record.Provider), record.NativeThreadID, record.Name,
			string(record.Provenance), nullableTime(record.UpdatedAt))
		if err != nil {
			return false, fmt.Errorf("upsert thread name: %w", err)
		}
		// updated_at is refreshed by the derived-copy sync, but it is not a
		// catalog-visible field. Only name/provenance changes publish a bump.
		changed = changed || visibleChanged
	}
	return changed, nil
}

// SetIndexState publishes scan lifecycle without changing the last completed
// reconciliation's source count/timestamp or the catalog version. Ready marks
// the saved catalog available without claiming a completed walk.
func (s *Store) SetIndexState(ctx context.Context, rootID int64, state IndexStateName, errorSummary *string) error {
	if rootID <= 0 {
		return errors.New("index state requires a root ID")
	}
	if state != IndexStarting && state != IndexScanning && state != IndexReady && state != IndexDegraded {
		return fmt.Errorf("invalid index state %q", state)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
UPDATE index_state SET state = ?, error_summary = ? WHERE source_root_id = ?`,
		state, nullableString(errorSummary), rootID)
	if err != nil {
		return err
	}
	changed, err := resultChanged(result)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("history root %d not found", rootID)
	}
	return nil
}

func (s *Store) ReconcileRoot(ctx context.Context, input RootReconciliation) (ReconcileResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if input.RootID <= 0 {
		return ReconcileResult{}, errors.New("root reconciliation requires a root ID")
	}
	if input.State != IndexStarting && input.State != IndexScanning &&
		input.State != IndexReady && input.State != IndexDegraded {
		return ReconcileResult{}, fmt.Errorf("invalid root reconciliation state %q", input.State)
	}
	if input.CompletedAt.IsZero() {
		input.CompletedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconcileResult{}, err
	}
	defer tx.Rollback()
	root, err := loadRootTx(ctx, tx, input.RootID)
	if err != nil {
		return ReconcileResult{}, err
	}

	// ObservedPaths is the authority for absence on a complete sweep. A partial
	// event reconciliation validates its projections against the reported paths
	// but cannot prove anything about other files in the root.
	observed := make(map[string]struct{}, len(input.ObservedPaths))
	for _, path := range input.ObservedPaths {
		if path == "" {
			return ReconcileResult{}, errors.New("root reconciliation observed an empty path")
		}
		if _, duplicate := observed[path]; duplicate {
			return ReconcileResult{}, fmt.Errorf("path %q appears twice in observed set", path)
		}
		observed[path] = struct{}{}
	}

	changed := false
	structural := false
	changedThreadIDs := make(map[string]struct{})
	projected := make(map[string]struct{}, len(input.Sources))
	for _, source := range input.Sources {
		if err := validateSourceProjection(source); err != nil {
			return ReconcileResult{}, err
		}
		if source.Source.Root.Provider != root.Root.Provider ||
			source.Source.Root.Kind != root.Root.Kind || source.Source.Root.Path != root.Root.Path {
			return ReconcileResult{}, fmt.Errorf("source %q does not belong to reconciled root", source.Source.Path)
		}
		if _, duplicate := projected[source.Source.Path]; duplicate {
			return ReconcileResult{}, fmt.Errorf("source %q appears twice in root reconciliation", source.Source.Path)
		}
		if _, seen := observed[source.Source.Path]; !seen {
			return ReconcileResult{}, fmt.Errorf("projected source %q was not in the observed set", source.Source.Path)
		}
		projected[source.Source.Path] = struct{}{}
		applied, err := applySourceTx(ctx, tx, input.RootID, source)
		if err != nil {
			return ReconcileResult{}, err
		}
		changed = changed || applied.changed
		if applied.changed {
			for _, id := range uniqueStrings(applied.oldThreadID, applied.threadID) {
				changedThreadIDs[id] = struct{}{}
			}
			structural = structural || applied.structural
		}
		if _, err := maintainConversationSearchForThreadsTx(ctx, tx, applied.searchThreads); err != nil {
			return ReconcileResult{}, fmt.Errorf("maintain reconciled source conversation search rows: %w", err)
		}
	}

	// Only a complete, successful sweep proves absence. Degraded, scanning, and
	// watcher-scoped updates preserve the previous source-health projection.
	if input.State == IndexReady && !input.Partial {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, canonical_path FROM source_files WHERE source_root_id = ?`, input.RootID)
		if err != nil {
			return ReconcileResult{}, err
		}
		type missingCandidate struct {
			id   int64
			path string
		}
		var missing []missingCandidate
		for rows.Next() {
			var candidate missingCandidate
			if err := rows.Scan(&candidate.id, &candidate.path); err != nil {
				rows.Close()
				return ReconcileResult{}, err
			}
			if _, ok := observed[candidate.path]; !ok {
				missing = append(missing, candidate)
			}
		}
		if err := rows.Close(); err != nil {
			return ReconcileResult{}, err
		}
		for _, candidate := range missing {
			result, err := tx.ExecContext(ctx, `
UPDATE source_files SET status = ?, last_indexed_at = ?
WHERE id = ? AND status IS NOT ?`, SourceMissing, formatTime(input.CompletedAt), candidate.id, SourceMissing)
			if err != nil {
				return ReconcileResult{}, err
			}
			rowChanged, err := resultChanged(result)
			if err != nil {
				return ReconcileResult{}, err
			}
			if rowChanged {
				changed = true
				threadID, err := sourceThreadID(ctx, tx, candidate.id)
				if err != nil {
					return ReconcileResult{}, err
				}
				if threadID != "" {
					changedThreadIDs[threadID] = struct{}{}
					structural = true
					aggregateChanged, err := recomputeThreadSummary(ctx, tx, threadID)
					if err != nil {
						return ReconcileResult{}, err
					}
					changed = changed || aggregateChanged
				}
			}
		}
	}

	// A partial reconciliation proves nothing about the root, so it leaves
	// the root's state, source count and completion time alone.
	if !input.Partial {
		if _, err = upsertIndexState(ctx, tx, input); err != nil {
			return ReconcileResult{}, err
		}
	}
	version, err := catalogVersionTx(ctx, tx)
	if err != nil {
		return ReconcileResult{}, err
	}
	if changed {
		version, err = bumpCatalogVersion(ctx, tx)
		if err != nil {
			return ReconcileResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ReconcileResult{}, err
	}
	threadIDs := make([]string, 0, len(changedThreadIDs))
	for id := range changedThreadIDs {
		threadIDs = append(threadIDs, id)
	}
	if changed && len(threadIDs) == 0 {
		structural = true
	}
	return ReconcileResult{CatalogVersion: version, Changed: changed, ChangedThreadIDs: threadIDs,
		Structural: structural, SourceCount: len(input.ObservedPaths)}, nil
}

type appliedSource struct {
	sourceID      int64
	oldThreadID   string
	threadID      string
	changed       bool
	structural    bool
	searchThreads []string
}

func applySourceTx(ctx context.Context, tx *sql.Tx, rootID int64, input SourceProjection) (appliedSource, error) {
	if input.IndexedAt.IsZero() {
		input.IndexedAt = time.Now().UTC()
	}
	if input.Status == "" {
		input.Status = SourcePresent
	}
	if err := migrateGrokSummarySource(ctx, tx, rootID, input.Source); err != nil {
		return appliedSource{}, err
	}
	old, err := readStoredSource(ctx, tx, rootID, input.Source.Path)
	if err != nil {
		return appliedSource{}, err
	}
	generation := int64(1)
	completeOffset := int64(0)
	completeLines := 0
	parserVersion := input.ParserVersion
	diagnostics := []Diagnostic(nil)
	summary := DiagnosticSummary{}
	if old.exists {
		generation = old.generation
		completeOffset = old.completeOffset
		completeLines = old.completeLines
		diagnostics = old.diagnostics
		summary = old.summary
		if parserVersion == "" && input.Result == nil {
			parserVersion = old.parserVersion
		}
		replaced := (old.inode != 0 && input.Source.Inode != 0 &&
			(old.device != input.Source.Device || old.inode != input.Source.Inode)) ||
			input.Source.Size < old.size
		parserChanged := input.Result != nil && old.parserVersion != "" &&
			parserVersion != "" && old.parserVersion != parserVersion
		if replaced || input.Rewritten {
			generation++
			completeOffset = 0
			completeLines = 0
		} else if parserChanged {
			// Parser changes invalidate only the metadata checkpoint. Source
			// generation changes only with the file itself (a replacement,
			// shrink or in-place rewrite) and fences the independent transcript
			// projection lifecycle.
			completeOffset = 0
			completeLines = 0
		}
	}
	if input.Result != nil {
		completeOffset = input.Result.CompleteOffset
		completeLines = input.Result.CompleteLines
		diagnostics = input.Result.Diagnostics
		summary = input.Result.Summary
	}
	diagnosticsJSON, err := json.Marshal(diagnostics)
	if err != nil {
		return appliedSource{}, err
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return appliedSource{}, err
	}

	result, err := tx.ExecContext(ctx, `
INSERT INTO source_files(
  source_root_id, canonical_path, device, inode, size, mtime, complete_offset,
  complete_lines, generation, classification, status, archived, parser_version,
  diagnostics_json, diagnostic_summary_json, error_summary, last_indexed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_root_id, canonical_path) DO UPDATE SET
  device = excluded.device, inode = excluded.inode, size = excluded.size,
  mtime = excluded.mtime, complete_offset = excluded.complete_offset,
  complete_lines = excluded.complete_lines,
  generation = excluded.generation, classification = excluded.classification,
  status = excluded.status, archived = excluded.archived,
  parser_version = excluded.parser_version,
  diagnostics_json = excluded.diagnostics_json,
  diagnostic_summary_json = excluded.diagnostic_summary_json,
  error_summary = excluded.error_summary, last_indexed_at = excluded.last_indexed_at
WHERE device IS NOT excluded.device OR inode IS NOT excluded.inode OR size IS NOT excluded.size
   OR mtime IS NOT excluded.mtime OR complete_offset IS NOT excluded.complete_offset
   OR complete_lines IS NOT excluded.complete_lines
   OR generation IS NOT excluded.generation OR classification IS NOT excluded.classification
   OR status IS NOT excluded.status OR archived IS NOT excluded.archived
   OR parser_version IS NOT excluded.parser_version
   OR diagnostics_json IS NOT excluded.diagnostics_json
   OR diagnostic_summary_json IS NOT excluded.diagnostic_summary_json
   OR error_summary IS NOT excluded.error_summary`,
		rootID, input.Source.Path, input.Source.Device, input.Source.Inode,
		input.Source.Size, formatTime(input.Source.ModTime), completeOffset,
		completeLines, generation, input.Source.Kind, input.Status, input.Source.Archived,
		parserVersion, diagnosticsJSON, summaryJSON, nullableString(input.ErrorSummary),
		formatTime(input.IndexedAt))
	if err != nil {
		return appliedSource{}, fmt.Errorf("upsert history source: %w", err)
	}
	changed, err := resultChanged(result)
	if err != nil {
		return appliedSource{}, err
	}
	var sourceID int64
	if err := tx.QueryRowContext(ctx, `
SELECT id FROM source_files WHERE source_root_id = ? AND canonical_path = ?`,
		rootID, input.Source.Path).Scan(&sourceID); err != nil {
		return appliedSource{}, err
	}

	oldThreadID, err := sourceThreadID(ctx, tx, sourceID)
	if err != nil {
		return appliedSource{}, err
	}
	threadID := oldThreadID
	kindChanged := false
	structural := false
	projectionIsUsable := input.Result != nil && input.Result.Projection.NativeThreadID != ""
	if projectionIsUsable {
		var rowChanged, threadStructural bool
		threadID, rowChanged, kindChanged, threadStructural, err = upsertThread(ctx, tx, rootID, sourceID, input)
		if err != nil {
			return appliedSource{}, err
		}
		changed = changed || rowChanged
		structural = structural || threadStructural
		if oldThreadID != "" && oldThreadID != threadID {
			// Preserve normalized evidence when durable identity is corrected.
			// The destination's existing active projection remains authoritative;
			// otherwise the moved projection keeps its active state.
			if err := reassociateTranscriptProjections(ctx, tx, oldThreadID, threadID, sourceID); err != nil {
				return appliedSource{}, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM thread_sources WHERE source_file_id = ?`, sourceID); err != nil {
				return appliedSource{}, err
			}
			changed = true
			structural = true
			kindChanged = true // reassociation changes projection eligibility ownership.
		}
		attach, err := tx.ExecContext(ctx, `
INSERT INTO thread_sources(thread_id, source_file_id, identity_evidence)
VALUES (?, ?, ?)
ON CONFLICT(source_file_id) DO UPDATE SET
  thread_id = excluded.thread_id, identity_evidence = excluded.identity_evidence
WHERE thread_id IS NOT excluded.thread_id OR identity_evidence IS NOT excluded.identity_evidence`,
			threadID, sourceID, input.Result.Projection.IdentityEvidence)
		if err != nil {
			return appliedSource{}, fmt.Errorf("attach history source: %w", err)
		}
		attachChanged, err := resultChanged(attach)
		if err != nil {
			return appliedSource{}, err
		}
		changed = changed || attachChanged
	}
	for _, id := range uniqueStrings(oldThreadID, threadID) {
		aggregateChanged, err := recomputeThreadSummary(ctx, tx, id)
		if err != nil {
			return appliedSource{}, err
		}
		changed = changed || aggregateChanged
	}
	searchThreads := []string(nil)
	if kindChanged {
		searchThreads = uniqueStrings(oldThreadID, threadID)
	}
	return appliedSource{sourceID: sourceID, oldThreadID: oldThreadID, threadID: threadID,
		changed: changed, structural: structural, searchThreads: searchThreads}, nil
}

// migrateGrokSummarySource preserves one logical source identity when a
// metadata-only Grok session gains its first updates.jsonl. Discovery switches
// from summary.json to the append-only update stream at that moment. Renaming
// the derived source row before normal apply logic runs keeps the thread and
// transcript association stable, while the new inode advances its generation
// and forces a clean transcript rebuild. Provider-owned files are untouched.
func migrateGrokSummarySource(ctx context.Context, tx *sql.Tx, rootID int64, source SourceCandidate) error {
	if source.Root.Provider != ProviderGrok || filepath.Base(source.Path) != "updates.jsonl" {
		return nil
	}
	var targetExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
  SELECT 1 FROM source_files WHERE source_root_id = ? AND canonical_path = ?
)`, rootID, source.Path).Scan(&targetExists); err != nil {
		return fmt.Errorf("inspect Grok update source migration: %w", err)
	}
	if targetExists {
		return nil
	}
	summaryPath := filepath.Join(filepath.Dir(source.Path), "summary.json")
	if _, err := tx.ExecContext(ctx, `
UPDATE source_files SET canonical_path = ?
WHERE source_root_id = ? AND canonical_path = ?`, source.Path, rootID, summaryPath); err != nil {
		return fmt.Errorf("migrate Grok summary source: %w", err)
	}
	return nil
}

func upsertThread(ctx context.Context, tx *sql.Tx, rootID, sourceID int64, input SourceProjection) (string, bool, bool, bool, error) {
	projection := input.Result.Projection
	metadataJSON, err := json.Marshal(projection.Metadata)
	if err != nil {
		return "", false, false, false, err
	}
	var id string
	var previousKind ThreadKind
	var previousParent sql.NullString
	var previousInteraction Interaction
	var previousArchived bool
	existed := true
	err = tx.QueryRowContext(ctx, `
SELECT id, kind, parent_native_id, interaction, archived
FROM threads WHERE source_root_id = ? AND provider = ? AND native_thread_id = ?`,
		rootID, projection.Provider, projection.NativeThreadID).Scan(
		&id, &previousKind, &previousParent, &previousInteraction, &previousArchived)
	if errors.Is(err, sql.ErrNoRows) {
		existed = false
		id, err = mintThreadID()
	} else if err == nil {
		var otherClaims int
		if countErr := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM thread_sources WHERE thread_id = ? AND source_file_id <> ?`,
			id, sourceID).Scan(&otherClaims); countErr != nil {
			return "", false, false, false, countErr
		}
		if otherClaims > 0 {
			// A second native file claiming this identity is an explicit
			// conflict, not another metadata authority. Freeze the canonical
			// projection at its last unambiguous value.
			return id, false, false, false, nil
		}
	}
	if err != nil {
		return "", false, false, false, err
	}
	if existed && projection.Provider == ProviderGrok {
		conversationUpdatedAt, recencyErr := grokThreadConversationRecency(ctx, tx, id)
		if recencyErr != nil {
			return "", false, false, false, recencyErr
		}
		if conversationUpdatedAt != nil {
			projection.UpdatedAt = conversationUpdatedAt
		}
	}
	interaction := ClassifyInteraction(projection.Provider, projection.Metadata.Origin)
	result, err := tx.ExecContext(ctx, `
INSERT INTO threads(id, source_root_id, provider, native_thread_id, kind,
  parent_native_id, provider_version, title, preview, created_at, updated_at,
  metadata_json, interaction, archived, source_status,
  conflict, diagnostic_count, last_indexed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
ON CONFLICT(source_root_id, provider, native_thread_id) DO UPDATE SET
  kind = excluded.kind, parent_native_id = excluded.parent_native_id,
  provider_version = excluded.provider_version,
  title = excluded.title, preview = excluded.preview,
  created_at = excluded.created_at, updated_at = excluded.updated_at,
  metadata_json = excluded.metadata_json, interaction = excluded.interaction,
  archived = excluded.archived
WHERE kind IS NOT excluded.kind OR parent_native_id IS NOT excluded.parent_native_id
   OR provider_version IS NOT excluded.provider_version OR title IS NOT excluded.title
   OR preview IS NOT excluded.preview OR created_at IS NOT excluded.created_at
   OR updated_at IS NOT excluded.updated_at OR metadata_json IS NOT excluded.metadata_json
   OR interaction IS NOT excluded.interaction OR archived IS NOT excluded.archived`,
		id, rootID, projection.Provider, projection.NativeThreadID, projection.Kind,
		nullableText(projection.ParentNativeID), nullableString(projection.ProviderVersion),
		nullableString(projection.Title), nullableString(projection.Preview),
		nullableTime(projection.CreatedAt), nullableTime(projection.UpdatedAt), metadataJSON,
		interaction, input.Source.Archived, input.Status, input.Result.Summary.Total, formatTime(input.IndexedAt))
	if err != nil {
		return "", false, false, false, fmt.Errorf("upsert history thread: %w", err)
	}
	changed, err := resultChanged(result)
	parent := ""
	if previousParent.Valid {
		parent = previousParent.String
	}
	structural := !existed || previousKind != projection.Kind || parent != projection.ParentNativeID ||
		previousInteraction != interaction || previousArchived != input.Source.Archived
	return id, changed, existed && previousKind != projection.Kind, structural, err
}

func recomputeThreadSummary(ctx context.Context, tx *sql.Tx, threadID string) (bool, error) {
	if threadID == "" {
		return false, nil
	}
	var total, present, failed, diagnosticCount int
	var lastIndexed sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN sf.status = 'present' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN sf.status = 'error' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CAST(json_extract(sf.diagnostic_summary_json, '$.total') AS INTEGER)), 0),
       MAX(sf.last_indexed_at)
FROM thread_sources ts JOIN source_files sf ON sf.id = ts.source_file_id
WHERE ts.thread_id = ?`, threadID).Scan(&total, &present, &failed, &diagnosticCount, &lastIndexed)
	if err != nil {
		return false, err
	}
	if total == 0 {
		// Reassignment must have moved all source-owned projections before the
		// last metadata attachment disappears. Refuse destructive cleanup if a
		// corrupt/orphan projection remains; retained transcript evidence is
		// preferable to silently deleting it.
		var projectionCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM conversation_projections WHERE thread_id = ?`, threadID).Scan(&projectionCount); err != nil {
			return false, err
		}
		if projectionCount != 0 {
			return false, fmt.Errorf("history thread %q has %d orphan transcript projections", threadID, projectionCount)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM threads WHERE id = ?`, threadID)
		if err != nil {
			return false, err
		}
		return resultChanged(result)
	}
	status := SourceMissing
	if present > 0 {
		status = SourcePresent
	} else if failed > 0 {
		status = SourceError
	}
	result, err := tx.ExecContext(ctx, `
UPDATE threads SET source_status = ?, conflict = ?, diagnostic_count = ?, last_indexed_at = ?
WHERE id = ? AND (source_status IS NOT ? OR conflict IS NOT ?
  OR diagnostic_count IS NOT ? OR last_indexed_at IS NOT ?)`,
		status, total > 1, diagnosticCount, nullStringValue(lastIndexed), threadID,
		status, total > 1, diagnosticCount, nullStringValue(lastIndexed))
	if err != nil {
		return false, err
	}
	return resultChanged(result)
}

func reassociateTranscriptProjections(ctx context.Context, tx *sql.Tx, oldThreadID, newThreadID string, sourceID int64) error {
	var targetHasActive bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
  SELECT 1 FROM conversation_projections WHERE thread_id = ? AND state = 'active'
)`, newThreadID).Scan(&targetHasActive); err != nil {
		return fmt.Errorf("inspect reassigned transcript target: %w", err)
	}
	if targetHasActive {
		if _, err := tx.ExecContext(ctx, `
UPDATE conversation_projections SET state = 'superseded'
WHERE thread_id = ? AND source_file_id = ? AND state = 'active'`, oldThreadID, sourceID); err != nil {
			return fmt.Errorf("supersede reassigned transcript projection: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE conversation_projections SET thread_id = ?
WHERE thread_id = ? AND source_file_id = ?`, newThreadID, oldThreadID, sourceID); err != nil {
		return fmt.Errorf("reassociate transcript projection: %w", err)
	}
	return nil
}

func upsertIndexState(ctx context.Context, tx *sql.Tx, input RootReconciliation) (bool, error) {
	result, err := tx.ExecContext(ctx, `
INSERT INTO index_state(source_root_id, state, source_count, last_completed_at, error_summary)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(source_root_id) DO UPDATE SET state = excluded.state,
  source_count = excluded.source_count, last_completed_at = excluded.last_completed_at,
  error_summary = excluded.error_summary
WHERE state IS NOT excluded.state OR source_count IS NOT excluded.source_count
   OR last_completed_at IS NOT excluded.last_completed_at
   OR error_summary IS NOT excluded.error_summary`, input.RootID, input.State,
		len(input.ObservedPaths), formatTime(input.CompletedAt), nullableString(input.Error))
	if err != nil {
		return false, err
	}
	return resultChanged(result)
}

type storedSource struct {
	exists         bool
	device, inode  uint64
	size           int64
	completeOffset int64
	completeLines  int
	generation     int64
	parserVersion  string
	diagnostics    []Diagnostic
	summary        DiagnosticSummary
}

func readStoredSource(ctx context.Context, tx *sql.Tx, rootID int64, path string) (storedSource, error) {
	var source storedSource
	var diagnosticsJSON, summaryJSON []byte
	err := tx.QueryRowContext(ctx, `
SELECT device, inode, size, complete_offset, complete_lines, generation, parser_version,
       diagnostics_json, diagnostic_summary_json
FROM source_files WHERE source_root_id = ? AND canonical_path = ?`, rootID, path).Scan(
		&source.device, &source.inode, &source.size, &source.completeOffset,
		&source.completeLines, &source.generation, &source.parserVersion, &diagnosticsJSON, &summaryJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return source, nil
	}
	if err != nil {
		return source, err
	}
	source.exists = true
	if err := json.Unmarshal(diagnosticsJSON, &source.diagnostics); err != nil {
		return source, fmt.Errorf("decode stored source diagnostics: %w", err)
	}
	if err := json.Unmarshal(summaryJSON, &source.summary); err != nil {
		return source, fmt.Errorf("decode stored diagnostic summary: %w", err)
	}
	return source, nil
}

func sourceThreadID(ctx context.Context, tx *sql.Tx, sourceID int64) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT thread_id FROM thread_sources WHERE source_file_id = ?`, sourceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func loadRootTx(ctx context.Context, tx *sql.Tx, id int64) (PersistedRoot, error) {
	var root PersistedRoot
	root.ID = id
	err := tx.QueryRowContext(ctx, `
SELECT provider, root_kind, canonical_root, archived, enabled FROM source_roots WHERE id = ?`, id).Scan(
		&root.Root.Provider, &root.Root.Kind, &root.Root.Path, &root.Root.Archived, &root.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return PersistedRoot{}, fmt.Errorf("history root %d not found", id)
	}
	return root, err
}

func validateSourceProjection(input SourceProjection) error {
	if err := validateRoot(input.Source.Root); err != nil {
		return err
	}
	if input.Source.Path == "" {
		return errors.New("history source path is empty")
	}
	if !validSourceThreadKind(input.Source.Kind) {
		return fmt.Errorf("invalid source thread kind %q", input.Source.Kind)
	}
	if input.Status != "" && input.Status != SourcePresent && input.Status != SourceMissing && input.Status != SourceError {
		return fmt.Errorf("invalid source status %q", input.Status)
	}
	if !filepath.IsAbs(input.Source.Path) {
		return errors.New("history source path must be absolute")
	}
	relative, err := filepath.Rel(input.Source.Root.Path, input.Source.Path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("history source path is outside its configured root")
	}
	if input.Result != nil && input.Result.Projection.Provider != input.Source.Root.Provider {
		return errors.New("history projection provider does not match source root")
	}
	if input.Result != nil && !validThreadKind(input.Result.Projection.Kind) {
		return fmt.Errorf("invalid projected thread kind %q", input.Result.Projection.Kind)
	}
	return nil
}

func validSourceThreadKind(kind ThreadKind) bool {
	return kind == ThreadMain || kind == ThreadSubagent || kind == ThreadFork
}

func validThreadKind(kind ThreadKind) bool {
	return validSourceThreadKind(kind) || kind == ThreadGuardian
}

func uniqueStrings(values ...string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func nullStringValue(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
