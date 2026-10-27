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

func (s *Store) ListRoots(ctx context.Context) ([]PersistedRoot, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, provider, root_kind, canonical_root, archived, enabled
FROM source_roots ORDER BY provider, root_kind, canonical_root`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []PersistedRoot
	for rows.Next() {
		var root PersistedRoot
		if err := rows.Scan(&root.ID, &root.Root.Provider, &root.Root.Kind,
			&root.Root.Path, &root.Root.Archived, &root.Enabled); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

func (s *Store) LoadSource(ctx context.Context, rootID int64, path string) (StoredSource, bool, error) {
	source, err := scanStoredSource(s.db.QueryRowContext(ctx, storedSourceSelect+`
WHERE sf.source_root_id = ? AND sf.canonical_path = ?`, rootID, path))
	if errors.Is(err, sql.ErrNoRows) {
		return StoredSource{}, false, nil
	}
	if err != nil {
		return StoredSource{}, false, err
	}
	return source, true, nil
}

// LoadThreadSources returns the durable source snapshots currently associated
// with one catalog thread. Callers that must act on provider files can re-open
// and re-parse these snapshots instead of trusting catalog presence alone.
func (s *Store) LoadThreadSources(ctx context.Context, threadID string) ([]StoredSource, error) {
	rows, err := s.db.QueryContext(ctx, storedSourceSelect+`
WHERE t.id = ? ORDER BY sf.canonical_path`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []StoredSource
	for rows.Next() {
		source, err := scanStoredSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// LoadSourcesByRoot loads every stored source for a root in one query, keyed by
// canonical path. Scanners use it to compare a whole walk against committed
// state in memory instead of issuing one LoadSource per candidate — the cycle
// cost then tracks the number of changed files, not the catalog size.
func (s *Store) LoadSourcesByRoot(ctx context.Context, rootID int64) (map[string]StoredSource, error) {
	rows, err := s.db.QueryContext(ctx, storedSourceSelect+`
WHERE sf.source_root_id = ?`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := make(map[string]StoredSource)
	for rows.Next() {
		source, err := scanStoredSource(rows)
		if err != nil {
			return nil, err
		}
		sources[source.Source.Path] = source
	}
	return sources, rows.Err()
}

// LoadTranscriptCheckpointsByRoot loads the active transcript checkpoint for
// every thread under a root in one query, keyed by thread ID. It is the batch
// companion to LoadTranscriptCheckpoint so the repair pass can skip an unchanged
// transcript without a per-thread checkpoint read.
func (s *Store) LoadTranscriptCheckpointsByRoot(ctx context.Context, rootID int64) (map[string]TranscriptCheckpoint, error) {
	return s.loadTranscriptCheckpointsByRoot(ctx, rootID, true)
}

// LoadTranscriptCheckpointHeadersByRoot plans recovery without loading every
// source's retained parser state. Load a full checkpoint only for an append.
func (s *Store) LoadTranscriptCheckpointHeadersByRoot(ctx context.Context, rootID int64) (map[string]TranscriptCheckpoint, error) {
	return s.loadTranscriptCheckpointsByRoot(ctx, rootID, false)
}

func (s *Store) loadTranscriptCheckpointsByRoot(ctx context.Context, rootID int64, includeState bool) (map[string]TranscriptCheckpoint, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stateColumn := "NULL"
	if includeState {
		stateColumn = "cp.parser_state_json"
	}
	rows, err := tx.QueryContext(ctx, `
SELECT cp.id, cp.projection_key, cp.thread_id, cp.source_file_id, cp.source_generation, cp.provider,
       cp.parser_version, cp.completion_rule_version, cp.complete_offset, cp.complete_lines,
       cp.latest_seq, `+stateColumn+`, cp.error_summary, COALESCE(NULLIF(cp.read_offset,0),cp.complete_offset)
FROM conversation_projections cp
JOIN threads t ON t.id = cp.thread_id
WHERE t.source_root_id = ? AND cp.state = 'active'`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	checkpoints := make(map[string]TranscriptCheckpoint)
	projectionIDs := make(map[string]int64)
	for rows.Next() {
		var checkpoint TranscriptCheckpoint
		var projectionID int64
		var parserState []byte
		var errorSummary sql.NullString
		if err := rows.Scan(&projectionID, &checkpoint.ProjectionKey, &checkpoint.ThreadID, &checkpoint.SourceID,
			&checkpoint.SourceGeneration, &checkpoint.Provider, &checkpoint.ParserVersion,
			&checkpoint.CompletionRuleVersion, &checkpoint.CompleteOffset, &checkpoint.CompleteLines,
			&checkpoint.LatestSeq, &parserState, &errorSummary, &checkpoint.ReadOffset); err != nil {
			return nil, err
		}
		checkpoint.ParserState = parserState
		checkpoint.ErrorSummary = pointerFromNullString(errorSummary)
		checkpoints[checkpoint.ThreadID] = checkpoint
		projectionIDs[checkpoint.ThreadID] = projectionID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if includeState {
		entriesByProjection := make(map[int64][]continuationEntry)
		entryRows, err := tx.QueryContext(ctx, `
SELECT e.projection_id,e.field,e.entry_key,e.entry_json
FROM conversation_parser_state_entries e
JOIN conversation_projections cp ON cp.id=e.projection_id
JOIN threads t ON t.id=cp.thread_id
WHERE t.source_root_id=? AND cp.state='active'
ORDER BY e.projection_id,e.entry_sequence`, rootID)
		if err != nil {
			return nil, err
		}
		for entryRows.Next() {
			var projectionID int64
			var entry continuationEntry
			if err := entryRows.Scan(&projectionID, &entry.field, &entry.key, &entry.value); err != nil {
				entryRows.Close()
				return nil, err
			}
			entriesByProjection[projectionID] = append(entriesByProjection[projectionID], entry)
		}
		if err := entryRows.Err(); err != nil {
			entryRows.Close()
			return nil, err
		}
		if err := entryRows.Close(); err != nil {
			return nil, err
		}
		for threadID, checkpoint := range checkpoints {
			entries := entriesByProjection[projectionIDs[threadID]]
			checkpoint.ParserState, err = restoreTranscriptContinuation(checkpoint.ParserState, entries)
			if err != nil {
				return nil, err
			}
			checkpoints[threadID] = checkpoint
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return checkpoints, nil
}

func (s *Store) ListSources(ctx context.Context, rootID int64) ([]StoredSource, error) {
	rows, err := s.db.QueryContext(ctx, storedSourceSelect+`
WHERE sf.source_root_id = ? ORDER BY sf.canonical_path`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []StoredSource
	for rows.Next() {
		source, err := scanStoredSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func (s *Store) ListThreads(ctx context.Context, params ListThreadsParams) (ThreadPage, error) {
	if params.Limit == 0 {
		params.Limit = 100
	}
	if len(params.Identities) > MaxThreadIdentities {
		return ThreadPage{}, fmt.Errorf("thread identity filter holds %d threads, above %d", len(params.Identities), MaxThreadIdentities)
	}
	// An identity read may return two rows per thread (one per source root).
	if params.Limit < 1 || (params.Limit > 250 && params.Limit > 2*len(params.Identities)) {
		return ThreadPage{}, errors.New("thread list limit must be between 1 and 250")
	}
	if params.Archived == "" {
		params.Archived = ArchivedInclude
	}
	if params.Archived != ArchivedInclude && params.Archived != ArchivedExclude && params.Archived != ArchivedOnly {
		return ThreadPage{}, fmt.Errorf("invalid archived filter %q", params.Archived)
	}
	if params.After != nil && params.After.ID == "" {
		return ThreadPage{}, errors.New("thread cursor requires an ID")
	}

	where, args := baseThreadFilters(params)
	if params.ThreadID != "" {
		where = append(where, "(id = ? OR native_thread_id = ?)")
		args = append(args, params.ThreadID, params.ThreadID)
	}
	if len(params.Identities) > 0 {
		clause, identityArgs := identityFilter(params.Identities)
		where, args = append(where, clause), append(args, identityArgs...)
	}
	// Cursor predicates: rows strictly after the cursor in the fixed
	// updated-desc order. NULL updated_at sorts last, so a non-NULL cursor
	// still admits the NULL tail and a NULL cursor pages within it.
	if params.After != nil {
		if params.After.UpdatedAt == nil {
			where = append(where, "updated_at IS NULL AND id < ?")
			args = append(args, params.After.ID)
		} else {
			where = append(where, "(updated_at < ? OR updated_at IS NULL OR (updated_at = ? AND id < ?))")
			value := formatTime(*params.After.UpdatedAt)
			args = append(args, value, value, params.After.ID)
		}
	}
	query := threadSelect
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY updated_at IS NULL ASC, updated_at DESC, id DESC LIMIT ?"
	args = append(args, params.Limit+1)

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ThreadPage{}, fmt.Errorf("begin thread page snapshot: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return ThreadPage{}, fmt.Errorf("list history threads: %w", err)
	}
	defer rows.Close()
	items := make([]Thread, 0, params.Limit+1)
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return ThreadPage{}, err
		}
		items = append(items, thread)
	}
	if err := rows.Err(); err != nil {
		return ThreadPage{}, err
	}
	if err := rows.Close(); err != nil {
		return ThreadPage{}, err
	}
	if err := attachThreadNamesTx(ctx, tx, items); err != nil {
		return ThreadPage{}, err
	}
	var next *ThreadCursor
	if len(items) > params.Limit {
		items = items[:params.Limit]
		last := items[len(items)-1]
		next = &ThreadCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
	}
	version, err := catalogVersionTx(ctx, tx)
	if err != nil {
		return ThreadPage{}, err
	}
	if err := tx.Commit(); err != nil {
		return ThreadPage{}, fmt.Errorf("commit thread page snapshot: %w", err)
	}
	return ThreadPage{Items: items, NextCursor: next, CatalogVersion: version}, nil
}

func (s *Store) GetThread(ctx context.Context, id string) (ThreadDetail, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ThreadDetail{}, fmt.Errorf("begin thread detail snapshot: %w", err)
	}
	defer tx.Rollback()
	thread, err := scanThread(tx.QueryRowContext(ctx, threadSelect+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadDetail{}, ErrThreadNotFound
	}
	if err != nil {
		return ThreadDetail{}, err
	}
	items := []Thread{thread}
	if err := attachThreadNamesTx(ctx, tx, items); err != nil {
		return ThreadDetail{}, err
	}
	thread = items[0]
	rows, err := tx.QueryContext(ctx, `
SELECT sr.root_kind, sf.canonical_path, sf.generation, sf.parser_version,
       sf.status, sf.archived, ts.identity_evidence, sf.complete_offset,
       sf.complete_lines, sf.diagnostic_summary_json, sf.diagnostics_json, sf.error_summary,
       sf.last_indexed_at
FROM thread_sources ts
JOIN source_files sf ON sf.id = ts.source_file_id
JOIN source_roots sr ON sr.id = sf.source_root_id
WHERE ts.thread_id = ? ORDER BY sf.canonical_path`, id)
	if err != nil {
		return ThreadDetail{}, err
	}
	defer rows.Close()
	detail := ThreadDetail{Thread: thread, Sources: []ThreadSource{}}
	for rows.Next() {
		var source ThreadSource
		var summaryJSON, diagnosticsJSON []byte
		var errorSummary, lastIndexed sql.NullString
		if err := rows.Scan(&source.RootKind, &source.Path, &source.Generation,
			&source.ParserVersion, &source.Status, &source.Archived,
			&source.IdentityEvidence, &source.CompleteOffset, &source.CompleteLines, &summaryJSON,
			&diagnosticsJSON, &errorSummary, &lastIndexed); err != nil {
			return ThreadDetail{}, err
		}
		if err := json.Unmarshal(summaryJSON, &source.DiagnosticSummary); err != nil {
			return ThreadDetail{}, fmt.Errorf("decode diagnostic summary: %w", err)
		}
		if err := json.Unmarshal(diagnosticsJSON, &source.Diagnostics); err != nil {
			return ThreadDetail{}, fmt.Errorf("decode diagnostics: %w", err)
		}
		source.LastIndexedAt, err = parseNullTime(lastIndexed)
		if err != nil {
			return ThreadDetail{}, err
		}
		source.ErrorSummary = pointerFromNullString(errorSummary)
		detail.Sources = append(detail.Sources, source)
	}
	if err := rows.Err(); err != nil {
		return ThreadDetail{}, err
	}
	if err := rows.Close(); err != nil {
		return ThreadDetail{}, err
	}
	if err := tx.Commit(); err != nil {
		return ThreadDetail{}, fmt.Errorf("commit thread detail snapshot: %w", err)
	}
	return detail, nil
}

// CatalogVersion returns the current monotonic catalog version without the full
// IndexStatus snapshot — the lightweight read a caller needs when it only wants
// to know whether the catalog advanced.
func (s *Store) CatalogVersion(ctx context.Context) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, fmt.Errorf("begin catalog version read: %w", err)
	}
	defer tx.Rollback()
	return catalogVersionTx(ctx, tx)
}

func (s *Store) IndexStatus(ctx context.Context) (IndexStatus, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return IndexStatus{}, fmt.Errorf("begin index status snapshot: %w", err)
	}
	defer tx.Rollback()
	version, err := catalogVersionTx(ctx, tx)
	if err != nil {
		return IndexStatus{}, err
	}
	counts, err := s.interactionCounts(ctx, tx, version)
	if err != nil {
		return IndexStatus{}, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT sr.id, sr.provider, sr.root_kind, sr.canonical_root, sr.archived, sr.enabled,
       COALESCE(ix.state, 'starting'), COALESCE(ix.source_count, 0),
       ix.last_completed_at, ix.error_summary
FROM source_roots sr LEFT JOIN index_state ix ON ix.source_root_id = sr.id
ORDER BY sr.provider, sr.root_kind, sr.canonical_root`)
	if err != nil {
		return IndexStatus{}, err
	}
	defer rows.Close()
	status := IndexStatus{State: IndexStarting, CatalogVersion: version, Roots: []RootIndexStatus{}, InteractionCounts: counts}
	for rows.Next() {
		var root RootIndexStatus
		var completed, errorSummary sql.NullString
		if err := rows.Scan(&root.Root.ID, &root.Root.Root.Provider, &root.Root.Root.Kind,
			&root.Root.Root.Path, &root.Root.Root.Archived, &root.Root.Enabled,
			&root.State, &root.SourceCount, &completed, &errorSummary); err != nil {
			return IndexStatus{}, err
		}
		root.LastCompletedAt, err = parseNullTime(completed)
		if err != nil {
			return IndexStatus{}, err
		}
		if errorSummary.Valid {
			root.Error = &errorSummary.String
		}
		status.Roots = append(status.Roots, root)
	}
	if err := rows.Err(); err != nil {
		return IndexStatus{}, err
	}
	if err := rows.Close(); err != nil {
		return IndexStatus{}, err
	}
	if len(status.Roots) == 0 {
		if err := tx.Commit(); err != nil {
			return IndexStatus{}, fmt.Errorf("commit index status snapshot: %w", err)
		}
		return status, nil
	}
	status.State = IndexReady
	for _, root := range status.Roots {
		if root.State == IndexDegraded {
			status.State = IndexDegraded
			break
		}
		if root.State == IndexStarting || root.State == IndexScanning {
			status.State = IndexScanning
		}
	}
	if err := tx.Commit(); err != nil {
		return IndexStatus{}, fmt.Errorf("commit index status snapshot: %w", err)
	}
	return status, nil
}

// baseThreadFilters builds the list query's WHERE fragments: subagent,
// Guardian diagnostic visibility, programmatic, archived, and the time bounds.
func baseThreadFilters(params ListThreadsParams) ([]string, []any) {
	var where []string
	var args []any
	if !params.IncludeSubagents {
		where = append(where, "kind <> ?")
		args = append(args, ThreadSubagent)
	}
	// A specific id is a diagnostic lookup, so it may inspect a Guardian
	// without widening ordinary catalog listings. includeSubagents never
	// affects this rule.
	if !params.IncludeGuardians && params.ThreadID == "" {
		where = append(where, "kind <> ?")
		args = append(args, ThreadGuardian)
	}
	if !params.IncludeProgrammatic {
		where = append(where, "interaction <> ?")
		args = append(args, InteractionProgrammatic)
	}
	if params.ParentNativeID != "" {
		where = append(where, "parent_native_id = ?")
		args = append(args, params.ParentNativeID)
	}
	switch params.Archived {
	case ArchivedExclude:
		where = append(where, "archived = 0")
	case ArchivedOnly:
		where = append(where, "archived = 1")
	}
	bounds := []struct {
		col, op string
		at      *time.Time
	}{
		{"created_at", ">=", params.CreatedFrom},
		{"created_at", "<=", params.CreatedTo},
		{"updated_at", ">=", params.UpdatedFrom},
		{"updated_at", "<=", params.UpdatedTo},
	}
	for _, b := range bounds {
		if b.at != nil {
			where = append(where, b.col+" "+b.op+" ?")
			args = append(args, formatTime(*b.at))
		}
	}
	return where, args
}

// interactionCounts returns the tally for version, recounting only when the
// cached one belongs to another version. tx must have read version.
func (s *Store) interactionCounts(ctx context.Context, tx *sql.Tx, version uint64) (InteractionCounts, error) {
	s.countsMu.Lock()
	cached, ok := s.counts, s.countsValid && s.countsVersion == version
	s.countsMu.Unlock()
	if ok {
		return cached, nil
	}
	counts, err := interactionCountsTx(ctx, tx)
	if err != nil {
		return InteractionCounts{}, err
	}
	s.countsMu.Lock()
	s.counts, s.countsVersion, s.countsValid = counts, version, true
	s.countsMu.Unlock()
	return counts, nil
}

// interactionCountsTx tallies ordinary catalog threads by interaction class.
// Guardians are internal permission reviewers, not delegated-work activity.
func interactionCountsTx(ctx context.Context, tx *sql.Tx) (InteractionCounts, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT interaction, COUNT(*) FROM threads WHERE kind NOT IN (?, ?) GROUP BY interaction`, ThreadSubagent, ThreadGuardian)
	if err != nil {
		return InteractionCounts{}, err
	}
	defer rows.Close()
	var counts InteractionCounts
	for rows.Next() {
		var class string
		var count int
		if err := rows.Scan(&class, &count); err != nil {
			return InteractionCounts{}, err
		}
		switch Interaction(class) {
		case InteractionInteractive:
			counts.Interactive = count
		case InteractionProgrammatic:
			counts.Programmatic = count
		case InteractionUnclassified:
			counts.Unclassified = count
		}
	}
	return counts, rows.Err()
}

const threadSelect = `
SELECT id, provider, native_thread_id, kind, parent_native_id, title, preview, created_at,
       updated_at, archived, provider_version, metadata_json, interaction, source_status, conflict,
       diagnostic_count, last_indexed_at
FROM threads`

type rowScanner interface {
	Scan(...any) error
}

func scanThread(row rowScanner) (Thread, error) {
	var thread Thread
	var parentNativeID, title, preview, createdAt, updatedAt, providerVersion, lastIndexed sql.NullString
	var metadataJSON []byte
	if err := row.Scan(&thread.ID, &thread.Provider, &thread.NativeID, &thread.Kind,
		&parentNativeID, &title, &preview, &createdAt, &updatedAt, &thread.Archived, &providerVersion,
		&metadataJSON,
		&thread.Interaction, &thread.Source.Status, &thread.Source.Conflict, &thread.Source.DiagnosticCount,
		&lastIndexed); err != nil {
		return Thread{}, err
	}
	thread.ParentNativeID = parentNativeID.String
	thread.ProviderVersion = pointerFromNullString(providerVersion)
	thread.Title = pointerFromNullString(title)
	thread.Preview = pointerFromNullString(preview)
	var err error
	thread.CreatedAt, err = parseNullTime(createdAt)
	if err != nil {
		return Thread{}, err
	}
	thread.UpdatedAt, err = parseNullTime(updatedAt)
	if err != nil {
		return Thread{}, err
	}
	thread.Source.LastIndexedAt, err = parseNullTime(lastIndexed)
	if err != nil {
		return Thread{}, err
	}
	if err := json.Unmarshal(metadataJSON, &thread.Metadata); err != nil {
		return Thread{}, fmt.Errorf("decode thread metadata: %w", err)
	}
	return thread, nil
}

// attachThreadNamesTx overlays the derived name copy onto a page of threads,
// reading only the page's own names.
func attachThreadNamesTx(ctx context.Context, tx *sql.Tx, items []Thread) error {
	identities := make([]ThreadIdentity, len(items))
	for index, item := range items {
		identities[index] = ThreadIdentity{Provider: item.Provider, NativeThreadID: item.NativeID}
	}
	refs, err := threadNameRefs(ctx, tx, identities)
	if err != nil {
		return err
	}
	for index, identity := range identities {
		if ref, ok := refs[identity]; ok {
			items[index].Name = &ref
		}
	}
	return nil
}

// ThreadNameRefFor returns the durable name and its truth-file provenance for
// one provider/native identity. It is a narrow read seam for live hosted
// views; name ownership remains with the thread-names truth file.
func (s *Store) ThreadNameRefFor(ctx context.Context, provider Provider, nativeThreadID string) (*ThreadNameRef, error) {
	identity := ThreadIdentity{Provider: provider, NativeThreadID: nativeThreadID}
	refs, err := s.ThreadNameRefs(ctx, []ThreadIdentity{identity})
	if ref, ok := refs[identity]; ok {
		return &ref, nil
	}
	return nil, err
}

// ThreadNameRefs is ThreadNameRefFor for many identities: list paths read
// every row's name in bounded batches, not one query per row. An identity
// with no name is absent.
func (s *Store) ThreadNameRefs(ctx context.Context, identities []ThreadIdentity) (map[ThreadIdentity]ThreadNameRef, error) {
	refs, err := threadNameRefs(ctx, s.db, identities)
	if err != nil {
		return nil, fmt.Errorf("read thread names: %w", err)
	}
	return refs, nil
}

// ThreadNameFor returns the durable user-assigned name for one thread, "" when
// unnamed — the read-by-(provider, nativeID) accessor live-session boot needs:
// a resumed session knows only its provider identity, not a catalog thread id.
func (s *Store) ThreadNameFor(ctx context.Context, provider Provider, nativeThreadID string) (string, error) {
	ref, err := s.ThreadNameRefFor(ctx, provider, nativeThreadID)
	if err != nil || ref == nil {
		return "", err
	}
	return ref.Value, nil
}

// ThreadNamesNamed returns every derived name record with this exact display
// name. Identity resolve uses it so duplicate names can pick the latest thread
// without paging the whole catalog.
func (s *Store) ThreadNamesNamed(ctx context.Context, name string) ([]ThreadNameRecord, error) {
	if name == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider, native_thread_id, name, provenance, updated_at FROM thread_names WHERE name = ? ORDER BY native_thread_id, provider`,
		name)
	if err != nil {
		return nil, fmt.Errorf("lookup thread names %q: %w", name, err)
	}
	defer rows.Close()
	records := make([]ThreadNameRecord, 0)
	for rows.Next() {
		var record ThreadNameRecord
		var provider, provenance string
		var updated sql.NullString
		if err := rows.Scan(&provider, &record.NativeThreadID, &record.Name, &provenance, &updated); err != nil {
			return nil, err
		}
		record.Provider = Provider(provider)
		record.Provenance = ThreadNameProvenance(provenance)
		parsed, err := parseNullTime(updated)
		if err != nil {
			return nil, err
		}
		record.UpdatedAt = parsed
		records = append(records, record)
	}
	return records, rows.Err()
}

// queryer is the read seam threadNameRefs needs, satisfied by both *sql.DB and
// *sql.Tx so the keyed name query has a single copy.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

const threadNameSelect = `SELECT provider, native_thread_id, name, provenance FROM thread_names WHERE `

// threadNameRefs reads the derived names of identities, deduplicated, in reads
// of at most MaxThreadIdentities pairs; an identity with no name is absent.
// Every name read (page overlays, thread detail, the one-id and batch readers)
// goes through it, and none reads the whole table.
func threadNameRefs(ctx context.Context, q queryer, identities []ThreadIdentity) (map[ThreadIdentity]ThreadNameRef, error) {
	refs, seen, unique := map[ThreadIdentity]ThreadNameRef{}, map[ThreadIdentity]bool{}, []ThreadIdentity{}
	for _, identity := range identities {
		if identity.NativeThreadID != "" && !seen[identity] {
			seen[identity], unique = true, append(unique, identity)
		}
	}
	for start := 0; start < len(unique); start += MaxThreadIdentities {
		clause, args := identityFilter(unique[start:min(start+MaxThreadIdentities, len(unique))])
		rows, err := q.QueryContext(ctx, threadNameSelect+clause, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var identity ThreadIdentity
			var name, provenance string
			if err := rows.Scan(&identity.Provider, &identity.NativeThreadID, &name, &provenance); err != nil {
				rows.Close()
				return nil, err
			}
			refs[identity] = ThreadNameRef{Value: name, Provenance: ThreadNameProvenance(provenance)}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// identityFilter is a (provider, native_thread_id) row-value IN filter over
// identities, with its arguments.
func identityFilter(identities []ThreadIdentity) (string, []any) {
	pairs := make([]string, len(identities))
	args := make([]any, 0, 2*len(identities))
	for i, identity := range identities {
		pairs[i] = "(?, ?)"
		args = append(args, string(identity.Provider), identity.NativeThreadID)
	}
	return "(provider, native_thread_id) IN (VALUES " + strings.Join(pairs, ", ") + ")", args
}

func pointerFromNullString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func parseNullTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, fmt.Errorf("parse stored history timestamp: %w", err)
	}
	return &parsed, nil
}

const storedSourceSelect = `
SELECT sf.id, sf.source_root_id, sr.provider, sr.root_kind, sr.canonical_root,
       sr.archived, sf.canonical_path, sf.classification, sf.archived, sf.size,
       sf.mtime, sf.device, sf.inode, sf.complete_offset, sf.complete_lines, sf.generation,
       sf.parser_version, sf.status, sf.diagnostics_json,
       sf.diagnostic_summary_json, sf.error_summary, sf.last_indexed_at,
       t.id, t.provider, t.native_thread_id, t.kind, t.parent_native_id,
       t.provider_version, t.title, t.preview, t.created_at, t.updated_at,
       t.metadata_json, ts.identity_evidence,
       (sf.id = COALESCE(
         (SELECT cp.source_file_id FROM conversation_projections cp
          JOIN source_files active_sf ON active_sf.id = cp.source_file_id
          WHERE cp.thread_id = t.id AND cp.state = 'active'
            AND active_sf.status = 'present' LIMIT 1),
         (SELECT MIN(ts2.source_file_id) FROM thread_sources ts2
          JOIN source_files present_sf ON present_sf.id = ts2.source_file_id
          WHERE ts2.thread_id = t.id AND present_sf.status = 'present'),
         (SELECT cp.source_file_id FROM conversation_projections cp
          WHERE cp.thread_id = t.id AND cp.state = 'active' LIMIT 1),
         (SELECT MIN(ts2.source_file_id) FROM thread_sources ts2 WHERE ts2.thread_id = t.id)
       ))
FROM source_files sf
JOIN source_roots sr ON sr.id = sf.source_root_id
LEFT JOIN thread_sources ts ON ts.source_file_id = sf.id
LEFT JOIN threads t ON t.id = ts.thread_id `

func scanStoredSource(row rowScanner) (StoredSource, error) {
	var source StoredSource
	var mtime, lastIndexed, errorSummary sql.NullString
	var diagnosticsJSON, summaryJSON []byte
	var threadID, provider, nativeID, kind, parentID, providerVersion sql.NullString
	var title, preview, createdAt, updatedAt, identityEvidence sql.NullString
	var canonicalTranscriptSource sql.NullBool
	var metadataJSON []byte
	if err := row.Scan(&source.ID, &source.RootID, &source.Source.Root.Provider,
		&source.Source.Root.Kind, &source.Source.Root.Path, &source.Source.Root.Archived,
		&source.Source.Path, &source.Source.Kind, &source.Source.Archived,
		&source.Source.Size, &mtime, &source.Source.Device, &source.Source.Inode,
		&source.CompleteOffset, &source.CompleteLines, &source.Generation, &source.ParserVersion,
		&source.Status, &diagnosticsJSON, &summaryJSON, &errorSummary, &lastIndexed,
		&threadID, &provider, &nativeID, &kind, &parentID, &providerVersion,
		&title, &preview, &createdAt, &updatedAt, &metadataJSON, &identityEvidence,
		&canonicalTranscriptSource); err != nil {
		return StoredSource{}, err
	}
	if mtime.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, mtime.String)
		if err != nil {
			return StoredSource{}, err
		}
		source.Source.ModTime = parsed
	}
	source.Source.RelativePath, _ = filepath.Rel(source.Source.Root.Path, source.Source.Path)
	if err := json.Unmarshal(diagnosticsJSON, &source.Diagnostics); err != nil {
		return StoredSource{}, err
	}
	if err := json.Unmarshal(summaryJSON, &source.DiagnosticSummary); err != nil {
		return StoredSource{}, err
	}
	source.ErrorSummary = pointerFromNullString(errorSummary)
	var err error
	source.LastIndexedAt, err = parseNullTime(lastIndexed)
	if err != nil {
		return StoredSource{}, err
	}
	if threadID.Valid {
		source.ThreadID = threadID.String
		source.CanonicalTranscriptSource = canonicalTranscriptSource.Valid && canonicalTranscriptSource.Bool
		projection := ThreadProjection{
			Provider: Provider(provider.String), NativeThreadID: nativeID.String,
			IdentityEvidence: Evidence(identityEvidence.String), Kind: ThreadKind(kind.String),
			ParentNativeID: parentID.String, Title: pointerFromNullString(title),
			Preview: pointerFromNullString(preview), ProviderVersion: pointerFromNullString(providerVersion),
		}
		projection.CreatedAt, err = parseNullTime(createdAt)
		if err != nil {
			return StoredSource{}, err
		}
		projection.UpdatedAt, err = parseNullTime(updatedAt)
		if err != nil {
			return StoredSource{}, err
		}
		if err := json.Unmarshal(metadataJSON, &projection.Metadata); err != nil {
			return StoredSource{}, err
		}
		source.Projection = &projection
	}
	return source, nil
}

// SidebarGraphKey names a row to rescue into the compact sidebar graph even
// when it sits outside the eligibility window.
type SidebarGraphKey struct {
	ID       string
	Provider Provider
	NativeID string
}

// SidebarGraphParams is the compact sidebar read: the catalog eligibility
// window plus optional identity rescues. Limit and cursor are ignored — the
// graph is the complete eligible-and-rescued population.
type SidebarGraphParams struct {
	List      ListThreadsParams
	Extra     []SidebarGraphKey
	OnlyExtra bool
}

// SidebarGraphRow is one compact graph node: identity and sidebar count
// fields plus the court-deciding activity used to classify fleet labels.
type SidebarGraphRow struct {
	Thread Thread
	Court  FleetCourtMessage
}

// SidebarGraph is the eligible catalog identity graph from one read-only
// transaction, including the catalog version observed in that snapshot.
type SidebarGraph struct {
	Rows           []SidebarGraphRow
	CatalogVersion uint64
}

const sidebarGraphINBatch = 400

// ListSidebarGraph returns the eligible catalog's identity graph, sidebar
// count fields, and court-deciding activity in one transaction. Extra keys
// are included even when they fall outside the time or interaction window.
func (s *Store) ListSidebarGraph(ctx context.Context, params SidebarGraphParams) (SidebarGraph, error) {
	if params.List.Archived == "" {
		params.List.Archived = ArchivedInclude
	}
	if params.List.Archived != ArchivedInclude && params.List.Archived != ArchivedExclude && params.List.Archived != ArchivedOnly {
		return SidebarGraph{}, fmt.Errorf("invalid archived filter %q", params.List.Archived)
	}
	where, args := baseThreadFilters(params.List)
	extraWhere, extraArgs := sidebarGraphExtraPredicates(params.Extra)
	query := threadSelect
	switch {
	case params.OnlyExtra:
		if extraWhere == "" {
			return SidebarGraph{}, nil
		}
		query += " WHERE " + extraWhere
		args = extraArgs
	case len(where) > 0 && len(extraWhere) > 0:
		query += " WHERE ((" + strings.Join(where, " AND ") + ") OR " + extraWhere + ")"
		args = append(args, extraArgs...)
	case len(where) > 0:
		query += " WHERE " + strings.Join(where, " AND ")
	case len(extraWhere) > 0:
		query += " WHERE " + extraWhere
		args = extraArgs
	}
	query += " ORDER BY updated_at IS NULL ASC, updated_at DESC, id DESC"

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SidebarGraph{}, fmt.Errorf("begin sidebar graph snapshot: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return SidebarGraph{}, fmt.Errorf("list sidebar graph: %w", err)
	}
	defer rows.Close()
	items := make([]Thread, 0)
	seen := map[string]bool{}
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return SidebarGraph{}, err
		}
		if seen[thread.ID] {
			continue
		}
		seen[thread.ID] = true
		items = append(items, thread)
	}
	if err := rows.Err(); err != nil {
		return SidebarGraph{}, err
	}
	if err := rows.Close(); err != nil {
		return SidebarGraph{}, err
	}
	if err := attachThreadNamesTx(ctx, tx, items); err != nil {
		return SidebarGraph{}, err
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	courts, err := fleetCourtMessagesTx(ctx, tx, ids)
	if err != nil {
		return SidebarGraph{}, err
	}
	version, err := catalogVersionTx(ctx, tx)
	if err != nil {
		return SidebarGraph{}, err
	}
	if err := tx.Commit(); err != nil {
		return SidebarGraph{}, fmt.Errorf("commit sidebar graph snapshot: %w", err)
	}
	graph := SidebarGraph{Rows: make([]SidebarGraphRow, 0, len(items)), CatalogVersion: version}
	for _, item := range items {
		graph.Rows = append(graph.Rows, SidebarGraphRow{Thread: item, Court: courts[item.ID]})
	}
	return graph, nil
}

func sidebarGraphExtraPredicates(extra []SidebarGraphKey) (string, []any) {
	var parts []string
	var args []any
	var ids []string
	for _, key := range extra {
		if id := strings.TrimSpace(key.ID); id != "" {
			ids = append(ids, id)
			continue
		}
		if key.Provider != "" && strings.TrimSpace(key.NativeID) != "" {
			parts = append(parts, "(provider = ? AND native_thread_id = ?)")
			args = append(args, string(key.Provider), strings.TrimSpace(key.NativeID))
		}
	}
	if len(ids) > 0 {
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, id)
		}
		parts = append(parts, "id IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// LoadThreadsByIDs loads full catalog thread rows for the included sidebar
// set. Unknown ids are omitted rather than failing the page.
func (s *Store) LoadThreadsByIDs(ctx context.Context, ids []string) ([]Thread, error) {
	unique := uniqueNonEmpty(ids)
	if len(unique) == 0 {
		return []Thread{}, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin sidebar thread load: %w", err)
	}
	defer tx.Rollback()
	byID := map[string]Thread{}
	for start := 0; start < len(unique); start += sidebarGraphINBatch {
		end := start + sidebarGraphINBatch
		if end > len(unique) {
			end = len(unique)
		}
		batch := unique[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, threadSelect+" WHERE id IN ("+strings.Join(placeholders, ", ")+")", args...)
		if err != nil {
			return nil, fmt.Errorf("load sidebar threads: %w", err)
		}
		for rows.Next() {
			thread, err := scanThread(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			byID[thread.ID] = thread
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	items := make([]Thread, 0, len(byID))
	for _, id := range unique {
		if thread, ok := byID[id]; ok {
			items = append(items, thread)
		}
	}
	if err := attachThreadNamesTx(ctx, tx, items); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit sidebar thread load: %w", err)
	}
	return items, nil
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func fleetCourtMessagesTx(ctx context.Context, tx *sql.Tx, threadIDs []string) (map[string]FleetCourtMessage, error) {
	out := make(map[string]FleetCourtMessage, len(threadIDs))
	if len(threadIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(threadIDs); start += sidebarGraphINBatch {
		end := start + sidebarGraphINBatch
		if end > len(threadIDs) {
			end = len(threadIDs)
		}
		batch := threadIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch)+3)
		args = append(args, fleetCourtHeadRunes, -fleetCourtTailRunes, string(ThreadSubagent))
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, `
SELECT t.id, t.updated_at,
       COALESCE(ci.item_type, ''),
       COALESCE(substr(ci.display_text, 1, ?), ''),
       COALESCE(substr(ci.display_text, ?), ''),
       COALESCE((
         SELECT latest.status = 'interrupted' FROM conversation_turns latest
         WHERE latest.projection_id = cp.id
         ORDER BY latest.ordinal DESC LIMIT 1
       ), 0)
FROM threads t
LEFT JOIN conversation_projections cp ON cp.thread_id = t.id AND cp.state = 'active'
LEFT JOIN conversation_items ci ON ci.projection_id = cp.id AND ci.projected_seq = (
  SELECT MAX(court.projected_seq) FROM conversation_items court
  WHERE court.projection_id = cp.id
    AND (court.sidechain = 0 OR t.kind = ?)
    AND (court.item_type = 'user_message'
         OR (court.item_type = 'agent_message' AND court.is_final_answer = 1))
)
WHERE t.id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("read sidebar court messages: %w", err)
		}
		for rows.Next() {
			var id, itemType, head, tail string
			var interrupted bool
			var updatedAt sql.NullString
			if err := rows.Scan(&id, &updatedAt, &itemType, &head, &tail, &interrupted); err != nil {
				rows.Close()
				return nil, err
			}
			lastActivity, err := parseNullTime(updatedAt)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = FleetCourtMessage{
				ItemType: TranscriptItemType(itemType), Head: head, Tail: tail,
				LastActivity: lastActivity, Interrupted: interrupted,
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
