package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CodexWorkspaceResolution is a disposable current-environment cache row.
// Payload is owned and versioned by the provider adapter in the main package;
// the history store deliberately treats it as opaque JSON.
type CodexWorkspaceResolution struct {
	TargetCWD       string
	ResolverVersion string
	ResolvedAt      time.Time
	Payload         []byte
}

// ClaudeWorkspaceResolution is the equivalent disposable cache row for
// Claude's documented, filesystem-derived current workspace setup.
type ClaudeWorkspaceResolution struct {
	TargetCWD       string
	ResolverVersion string
	ResolvedAt      time.Time
	Payload         []byte
}

func (s *Store) LoadCodexWorkspaceResolution(
	ctx context.Context,
	targetCWD, resolverVersion string,
) (CodexWorkspaceResolution, bool, error) {
	var row CodexWorkspaceResolution
	var resolvedAt string
	err := s.db.QueryRowContext(ctx, `
SELECT target_cwd, resolver_version, resolved_at, payload_json
FROM codex_workspace_resolutions
WHERE target_cwd = ? AND resolver_version = ?`,
		targetCWD, resolverVersion,
	).Scan(&row.TargetCWD, &row.ResolverVersion, &resolvedAt, &row.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return CodexWorkspaceResolution{}, false, nil
	}
	if err != nil {
		return CodexWorkspaceResolution{}, false, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, resolvedAt)
	if err != nil {
		return CodexWorkspaceResolution{}, false, fmt.Errorf("parse Codex workspace resolution time: %w", err)
	}
	row.ResolvedAt = parsed
	return row, true, nil
}

func (s *Store) SaveCodexWorkspaceResolution(ctx context.Context, row CodexWorkspaceResolution) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if row.TargetCWD == "" || row.ResolverVersion == "" || row.ResolvedAt.IsZero() {
		return errors.New("incomplete Codex workspace resolution")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO codex_workspace_resolutions(target_cwd, resolver_version, resolved_at, payload_json)
VALUES (?, ?, ?, ?)
ON CONFLICT(target_cwd) DO UPDATE SET
  resolver_version = excluded.resolver_version,
  resolved_at = excluded.resolved_at,
  payload_json = excluded.payload_json`,
		row.TargetCWD, row.ResolverVersion, formatTime(row.ResolvedAt.UTC()), row.Payload,
	)
	return err
}

func (s *Store) LoadClaudeWorkspaceResolution(
	ctx context.Context,
	targetCWD, resolverVersion string,
) (ClaudeWorkspaceResolution, bool, error) {
	var row ClaudeWorkspaceResolution
	var resolvedAt string
	err := s.db.QueryRowContext(ctx, `
SELECT target_cwd, resolver_version, resolved_at, payload_json
FROM claude_workspace_resolutions
WHERE target_cwd = ? AND resolver_version = ?`,
		targetCWD, resolverVersion,
	).Scan(&row.TargetCWD, &row.ResolverVersion, &resolvedAt, &row.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return ClaudeWorkspaceResolution{}, false, nil
	}
	if err != nil {
		return ClaudeWorkspaceResolution{}, false, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, resolvedAt)
	if err != nil {
		return ClaudeWorkspaceResolution{}, false, fmt.Errorf("parse Claude workspace resolution time: %w", err)
	}
	row.ResolvedAt = parsed
	return row, true, nil
}

func (s *Store) SaveClaudeWorkspaceResolution(ctx context.Context, row ClaudeWorkspaceResolution) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if row.TargetCWD == "" || row.ResolverVersion == "" || row.ResolvedAt.IsZero() {
		return errors.New("incomplete Claude workspace resolution")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO claude_workspace_resolutions(target_cwd, resolver_version, resolved_at, payload_json)
VALUES (?, ?, ?, ?)
ON CONFLICT(target_cwd) DO UPDATE SET
  resolver_version = excluded.resolver_version,
  resolved_at = excluded.resolved_at,
  payload_json = excluded.payload_json`,
		row.TargetCWD, row.ResolverVersion, formatTime(row.ResolvedAt.UTC()), row.Payload,
	)
	return err
}
