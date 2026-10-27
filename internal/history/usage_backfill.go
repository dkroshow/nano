package history

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const defaultTranscriptParserBackfillBatchSize = 2

// TranscriptParserBackfillCandidate is one active projection whose transcript
// parser is behind the current adapter and whose canonical source is still
// present, so a rebuild can terminate.
type TranscriptParserBackfillCandidate struct {
	ThreadID              string
	NativeThreadID        string
	Provider              Provider
	ParserVersion         string
	CompletionRuleVersion string
	UpdatedAt             time.Time
	Source                StoredSource
}

// ListTranscriptParserBackfillCandidates returns rebuildable parser-behind
// threads. source_missing, source error, and transcript error_summary rows
// are excluded so one failed rebuild cannot stall the rest of the pass.
func (s *Store) ListTranscriptParserBackfillCandidates(ctx context.Context, limit int) ([]TranscriptParserBackfillCandidate, error) {
	if limit == 0 {
		limit = defaultTranscriptParserBackfillBatchSize
	}
	claude := CurrentTranscriptParserVersion(ProviderClaude)
	codex := CurrentTranscriptParserVersion(ProviderCodex)
	grok := CurrentTranscriptParserVersion(ProviderGrok)
	claudeCompletion := CurrentTranscriptCompletionRuleVersion(ProviderClaude)
	codexCompletion := CurrentTranscriptCompletionRuleVersion(ProviderCodex)
	grokCompletion := CurrentTranscriptCompletionRuleVersion(ProviderGrok)
	rows, err := s.db.QueryContext(ctx, `
SELECT t.id, t.native_thread_id, t.provider, cp.parser_version, cp.completion_rule_version, COALESCE(t.updated_at, t.created_at, ''),
       sf.id, sf.source_root_id, sr.provider, sr.root_kind, sr.canonical_root, sr.archived,
       sf.canonical_path, sf.classification, sf.archived, sf.size, sf.mtime,
       COALESCE(sf.device, 0), COALESCE(sf.inode, 0),
       sf.complete_offset, sf.complete_lines, sf.generation, sf.status
FROM conversation_projections cp
JOIN threads t ON t.id = cp.thread_id
JOIN source_files sf ON sf.id = cp.source_file_id
JOIN source_roots sr ON sr.id = sf.source_root_id
WHERE cp.state = 'active' AND sf.status = ?
  AND (cp.error_summary IS NULL OR cp.error_summary = '')
  AND NOT `+projectionReaderCurrent(`CASE t.provider
    WHEN 'claude' THEN ?
    WHEN 'codex' THEN ?
    WHEN 'grok' THEN ?
    ELSE cp.parser_version
  END`, `CASE t.provider
    WHEN 'claude' THEN ?
    WHEN 'codex' THEN ?
    WHEN 'grok' THEN ?
    ELSE cp.completion_rule_version
  END`)+`
ORDER BY COALESCE(t.updated_at, t.created_at, '') DESC, t.id
LIMIT ?`, SourcePresent, claude, codex, grok, claude, codex, grok, claudeCompletion, codexCompletion, grokCompletion, limit)
	if err != nil {
		return nil, fmt.Errorf("list transcript parser backfill candidates: %w", err)
	}
	defer rows.Close()
	var out []TranscriptParserBackfillCandidate
	for rows.Next() {
		var candidate TranscriptParserBackfillCandidate
		source := &candidate.Source
		var mtime sql.NullString
		var updatedAt string
		if err := rows.Scan(&candidate.ThreadID, &candidate.NativeThreadID, &candidate.Provider, &candidate.ParserVersion, &candidate.CompletionRuleVersion, &updatedAt,
			&source.ID, &source.RootID, &source.Source.Root.Provider, &source.Source.Root.Kind,
			&source.Source.Root.Path, &source.Source.Root.Archived, &source.Source.Path,
			&source.Source.Kind, &source.Source.Archived, &source.Source.Size, &mtime,
			&source.Source.Device, &source.Source.Inode, &source.CompleteOffset,
			&source.CompleteLines, &source.Generation, &source.Status); err != nil {
			return nil, err
		}
		if mtime.Valid {
			parsed, err := time.Parse(time.RFC3339Nano, mtime.String)
			if err != nil {
				return nil, err
			}
			source.Source.ModTime = parsed
		}
		if updatedAt != "" {
			candidate.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		}
		source.ThreadID = candidate.ThreadID
		source.CanonicalTranscriptSource = true
		out = append(out, candidate)
	}
	return out, rows.Err()
}

// projectionReaderCurrent is the SQL condition that an active projection (cp,
// over its source sf) is what the current reader would build: that reader
// built it, or an upgrade to it proved this exact source unaffected
// (RecordTranscriptParserUnaffected). parser and completion are SQL
// expressions for the current parser and completion rule versions; parser is
// used twice, so its placeholders come first, then parser's again, then
// completion's.
func projectionReaderCurrent(parser, completion string) string {
	return `(cp.parser_version = ` + parser + ` OR EXISTS (
    SELECT 1 FROM conversation_parser_checks check_row
    WHERE check_row.source_file_id = sf.id
      AND check_row.source_generation = sf.generation
      AND check_row.source_size = sf.size
      AND check_row.source_mtime = COALESCE(sf.mtime, '')
      AND check_row.from_parser_version = cp.parser_version
      AND check_row.to_parser_version = ` + parser + `
      AND check_row.from_completion_rule_version = cp.completion_rule_version
      AND check_row.to_completion_rule_version = ` + completion + `
      AND check_row.outcome = 'unaffected'))`
}

// RecordTranscriptParserUnaffected persists an exact-source decision: this
// source, at this generation, size and mtime, needs no re-read for the reader
// upgrade from one version to the other. It does not touch the projection or
// its checkpoint. A later append no longer matches the row and is rebuilt by
// the ordinary changed-source path.
func (s *Store) RecordTranscriptParserUnaffected(ctx context.Context, candidate TranscriptParserBackfillCandidate, toParserVersion, toCompletionRuleVersion string, checkedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	mtime := ""
	if !candidate.Source.Source.ModTime.IsZero() {
		mtime = formatTime(candidate.Source.Source.ModTime)
	}
	result, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO conversation_parser_checks(
  source_file_id,source_generation,source_size,source_mtime,
  from_parser_version,to_parser_version,
  from_completion_rule_version,to_completion_rule_version,outcome,checked_at
)
SELECT id,generation,size,COALESCE(mtime,''),?,?,?,?, 'unaffected',?
FROM source_files
WHERE id=? AND generation=? AND size=? AND COALESCE(mtime,'')=? AND status=?`,
		candidate.ParserVersion, toParserVersion, candidate.CompletionRuleVersion, toCompletionRuleVersion, formatTime(checkedAt.UTC()),
		candidate.Source.ID, candidate.Source.Generation, candidate.Source.Source.Size,
		mtime, SourcePresent)
	if err != nil {
		return false, fmt.Errorf("record transcript parser unaffected: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

// A reader upgrade that changes only how rows carrying one marker are read
// names that marker here, by the reader tokens it moves between; a source
// without the bytes is marked current without a re-read. An upgrade not
// listed re-reads every source. Reader v34 (merged Claude prompts) is the
// first: it queued 6,000 sources, 7 of which carried the marker.
var transcriptUpgradeMarkers = []struct {
	from, to string
	marker   []byte
}{
	{from: "claude-reader-v33", to: "claude-reader-v34", marker: []byte(`"mergedSendUuids"`)},
	// v35 groups saved turns by Claude's promptId, which only a row carrying
	// one can change. Every current file has one, so in practice all re-read.
	{from: "claude-reader-v34", to: "claude-reader-v35", marker: []byte(`"promptId"`)},
	// v36 reads a slash command a sender sent from the content Claude saved
	// for it, whose text opens with a command tag: a JSON string that begins
	// with one, whatever the content's shape.
	{from: "claude-reader-v35", to: "claude-reader-v36", marker: []byte(`"<command-`)},
}

// TranscriptUpgradeMarker returns the bytes a source must contain for the
// upgrade from one composite parser version to the other to affect it. The two
// versions must differ only by the listed reader tokens.
func TranscriptUpgradeMarker(fromParserVersion, toParserVersion string) ([]byte, bool) {
	for _, rule := range transcriptUpgradeMarkers {
		if strings.Contains(toParserVersion, rule.to) && strings.Replace(toParserVersion, rule.to, rule.from, 1) == fromParserVersion {
			return rule.marker, true
		}
	}
	return nil, false
}

// SourceContainsMarker reads the file once in chunks, keeping the tail of each
// so a marker split across two chunks is still found.
func SourceContainsMarker(path string, marker []byte) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	keep := len(marker) - 1
	buffer := make([]byte, 1<<20+keep)
	carry := 0
	for {
		n, err := file.Read(buffer[carry:])
		if n > 0 {
			window := buffer[:carry+n]
			if bytes.Contains(window, marker) {
				return true, nil
			}
			if len(window) > keep {
				copy(buffer, window[len(window)-keep:])
				carry = keep
			} else {
				carry = len(window)
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// CurrentTranscriptParserVersion is the converted file reader version for a provider.
func CurrentTranscriptParserVersion(provider Provider) string {
	version, _, err := ParsedFileVersions(provider)
	if err != nil {
		return ""
	}
	return version
}

// CurrentTranscriptCompletionRuleVersion is the converted file assembly
// version for a provider, tracked independently from the composite parser
// version.
func CurrentTranscriptCompletionRuleVersion(provider Provider) string {
	_, completion, err := ParsedFileVersions(provider)
	if err != nil {
		return ""
	}
	return completion
}
