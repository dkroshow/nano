package prompts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
)

// LegacyTurnSchemaVersion is the lossless import schema before prompts owns its
// transactions. It remains readable only as migration input.
const LegacyTurnSchemaVersion = 20
const MigrationName = "turn-records-v20"

// LegacyTurnPaths names the three sources consumed once at the store cutover.
// Missing files mean an unused record family; an existing empty/corrupt file is
// an error. The caller must stop all application writers until this returns.
type LegacyTurnPaths struct{ Queue, Attempts, Completed string }

type TurnMigrationReport struct {
	BackupDir         string `json:"backupDir"`
	Queued            int    `json:"queued"`
	Attempts          int    `json:"attempts"`
	Completed         int    `json:"completed"`
	CompletedSequence int64  `json:"completedSequence"`
}

type migrationBackupFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type turnMigrationManifest struct {
	Version int                   `json:"version"`
	Report  TurnMigrationReport   `json:"report"`
	Files   []migrationBackupFile `json:"files"`
	Sources []SourceFingerprint   `json:"sources,omitempty"`
}

// SourceFingerprint records one legacy source copied into the canonical store.
type SourceFingerprint struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// MigrateTurnRecords absorbs queue, journal and completion facts into a schema
// 19 threads database. It backs up every source before the single migration
// transaction. It never changes provider evidence, confirms a submission,
// imports observer state, deletes a source, or prunes a retained record.
//
// Use a disposable control copy until the final path is ready to publish. The
// committed receipt makes reruns independent of legacy files. C1 retains
// accepted prompt rows permanently; legacy sources never overwrite them.
func MigrateTurnRecords(ctx context.Context, db *sql.DB, paths LegacyTurnPaths, backupDir string) (TurnMigrationReport, error) {
	return MigrateTurnRecordsWithSources(ctx, db, paths, backupDir, nil)
}

func MigrateTurnRecordsWithSources(ctx context.Context, db *sql.DB, paths LegacyTurnPaths, backupDir string, sources []SourceFingerprint) (TurnMigrationReport, error) {
	var report TurnMigrationReport
	if db == nil {
		return report, errors.New("threads: migration database is required")
	}
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return report, err
	}
	if version == LegacyTurnSchemaVersion {
		var raw string
		if err := db.QueryRowContext(ctx, `SELECT report_json FROM threads_store_migrations WHERE name=?`, MigrationName).Scan(&raw); err != nil {
			return report, fmt.Errorf("threads: missing turn migration receipt: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			return report, err
		}
		for _, table := range []string{"queued_turns", "send_attempts", "completed_turns"} {
			if _, err := db.ExecContext(ctx, "SELECT 1 FROM "+table+" LIMIT 0"); err != nil {
				return TurnMigrationReport{}, err
			}
		}
		return report, nil
	}
	if version != 19 {
		return report, fmt.Errorf("threads: turn migration requires schema 19, found %d", version)
	}
	queueBytes, queue, err := readLegacyRecords[QueuedTurn](paths.Queue)
	if err != nil {
		return report, err
	}
	attemptBytes, attempts, err := readLegacyRecords[SendAttempt](paths.Attempts)
	if err != nil {
		return report, err
	}
	seen := map[string]bool{}
	for _, rec := range queue {
		if rec.ClientActionID == "" || rec.Provider == "" || (rec.HostedThreadID == "" && rec.NativeThreadID == "") || rec.Text == "" || seen[rec.ClientActionID] {
			return report, errors.New("threads: queued records have missing identity/text or duplicate action ids")
		}
		seen[rec.ClientActionID] = true
	}
	seen = map[string]bool{}
	for _, rec := range attempts {
		if rec.ClientActionID == "" || rec.Provider == "" || seen[rec.ClientActionID] {
			return report, errors.New("threads: attempt records have missing identity or duplicate action ids")
		}
		seen[rec.ClientActionID] = true
	}
	if backupDir == "" {
		return report, errors.New("threads: a new pre-migration backup directory is required")
	}
	backupDir, err = filepath.Abs(backupDir)
	if err != nil {
		return report, err
	}
	if err := os.Mkdir(backupDir, 0700); err != nil {
		return report, fmt.Errorf("threads: create pre-migration backup: %w", err)
	}
	report.BackupDir = backupDir
	if err := threads.Backup(ctx, db, filepath.Join(backupDir, "control.sqlite")); err != nil {
		return report, err
	}
	files := []string{"control.sqlite"}
	for _, item := range []struct {
		name string
		data []byte
	}{{"queued-turns.json", queueBytes}, {"send-attempts.json", attemptBytes}} {
		if item.data == nil {
			continue
		}
		if err := writePrivateFile(filepath.Join(backupDir, item.name), item.data); err != nil {
			return report, err
		}
		files = append(files, item.name)
	}
	var completions []CompletedTurn
	if paths.Completed != "" {
		info, err := os.Stat(paths.Completed)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() == 0 {
				return report, errors.New("threads: invalid completed-turn source")
			}
			source, err := sql.Open("sqlite", statedb.URI(paths.Completed, "ro"))
			if err != nil {
				return report, err
			}
			copied := filepath.Join(backupDir, "turn-completed.sqlite")
			copyErr := threads.Backup(ctx, source, copied)
			if err := errors.Join(copyErr, source.Close()); err != nil {
				return report, err
			}
			copyDB, err := threads.OpenCheckedCopy(ctx, copied)
			if err != nil {
				return report, err
			}
			completions, report.CompletedSequence, err = LoadLegacyCompletions(ctx, copyDB)
			err = errors.Join(err, copyDB.Close())
			if err != nil {
				return report, err
			}
			files = append(files, "turn-completed.sqlite")
		}
	}
	report.Queued, report.Attempts, report.Completed = len(queue), len(attempts), len(completions)
	manifest := turnMigrationManifest{Version: LegacyTurnSchemaVersion, Report: report, Sources: sources}
	for _, name := range files {
		file, err := os.Open(filepath.Join(backupDir, name))
		if err != nil {
			return report, err
		}
		hash := sha256.New()
		size, err := io.Copy(hash, file)
		if err := errors.Join(err, file.Close()); err != nil {
			return report, err
		}
		manifest.Files = append(manifest.Files, migrationBackupFile{Name: name, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: size})
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return report, err
	}
	if err := writePrivateFile(filepath.Join(backupDir, "manifest.json"), rawManifest); err != nil {
		return report, err
	}
	if err := syncDirectory(backupDir); err != nil {
		return report, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, statement := range []string{queuedTurnSchema, sendAttemptSchema, CompletedTurnSchema, `CREATE TABLE threads_store_migrations(name TEXT PRIMARY KEY, report_json TEXT NOT NULL, manifest_json TEXT NOT NULL)`} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return report, err
		}
	}
	if err := insertQueuedTurns(ctx, tx, queue); err != nil {
		return report, err
	}
	if err := insertSendAttempts(ctx, tx, attempts); err != nil {
		return report, err
	}
	for _, rec := range completions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO completed_turns(id,target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at) VALUES(?,?,?,?,?,?,?,?)`, rec.ID, rec.TargetHostedThreadID, rec.Provider, rec.TurnID, rec.FinalMessageID, rec.CompletedAt, rec.Source, rec.CreatedAt); err != nil {
			return report, err
		}
	}
	// AUTOINCREMENT's high watermark may exceed every surviving row. Cursors
	// and later insertions must not reuse those historical ids.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name='completed_turns'`); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence(name,seq) VALUES('completed_turns',?)`, report.CompletedSequence); err != nil {
		return report, err
	}
	for _, item := range []struct {
		table string
		count int
	}{{"queued_turns", len(queue)}, {"send_attempts", len(attempts)}, {"completed_turns", len(completions)}} {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+item.table).Scan(&count); err != nil {
			return report, err
		}
		if count != item.count {
			return report, fmt.Errorf("threads: %s count mismatch", item.table)
		}
	}
	rawReport, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO threads_store_migrations(name,report_json,manifest_json) VALUES(?,?,?)`, MigrationName, string(rawReport), string(rawManifest)); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version=20`); err != nil {
		return report, err
	}
	return report, tx.Commit()
}

func readLegacyRecords[T any](path string) ([]byte, []T, error) {
	if path == "" {
		return nil, nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, nil, fmt.Errorf("threads: %s must contain a JSON array", path)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var rows []T
	if err := decoder.Decode(&rows); err != nil {
		return nil, nil, fmt.Errorf("threads: parse %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, nil, fmt.Errorf("threads: trailing data in %s", path)
	}
	return data, rows, nil
}

const CompletedTurnSchema = `CREATE TABLE completed_turns (id INTEGER PRIMARY KEY AUTOINCREMENT,target_hosted_thread_id TEXT NOT NULL,provider TEXT NOT NULL,turn_id TEXT NOT NULL,final_message_id TEXT NOT NULL,completed_at TEXT NOT NULL,source TEXT NOT NULL,created_at TEXT NOT NULL,UNIQUE(target_hosted_thread_id,turn_id)); CREATE INDEX completed_turns_target_order ON completed_turns(target_hosted_thread_id,id);`

func LoadLegacyCompletions(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) ([]CompletedTurn, int64, error) {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return nil, 0, err
	}
	if version != 0 {
		return nil, 0, fmt.Errorf("threads: unsupported completed-turn source version %d", version)
	}
	// The old completion store has no versioned migrations. Refuse additional
	// columns instead of silently losing future evidence in a selective copy.
	columns, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('completed_turns') ORDER BY cid`)
	if err != nil {
		return nil, 0, err
	}
	expected := []string{"id", "target_hosted_thread_id", "provider", "turn_id", "final_message_id", "completed_at", "source", "created_at"}
	index := 0
	for columns.Next() {
		var name string
		if err := columns.Scan(&name); err != nil {
			columns.Close()
			return nil, 0, err
		}
		if index >= len(expected) || name != expected[index] {
			columns.Close()
			return nil, 0, errors.New("threads: unsupported completed-turn source columns")
		}
		index++
	}
	if err := errors.Join(columns.Err(), columns.Close()); err != nil {
		return nil, 0, err
	}
	if index == 0 {
		// A fresh stage-4 install can create only the retained observer tables.
		// Re-migration of that source has no legacy completion facts to copy.
		return nil, 0, nil
	}
	if index != len(expected) {
		return nil, 0, errors.New("threads: incomplete completed-turn source columns")
	}
	rows, err := db.QueryContext(ctx, `SELECT id,target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at FROM completed_turns ORDER BY id`)
	if err != nil {
		return nil, 0, err
	}
	var result []CompletedTurn
	var maxID int64
	for rows.Next() {
		var rec CompletedTurn
		if err := rows.Scan(&rec.ID, &rec.TargetHostedThreadID, &rec.Provider, &rec.TurnID, &rec.FinalMessageID, &rec.CompletedAt, &rec.Source, &rec.CreatedAt); err != nil {
			rows.Close()
			return nil, 0, err
		}
		id, err := strconv.ParseInt(rec.ID, 10, 64)
		if err != nil || id <= 0 || rec.TargetHostedThreadID == "" || rec.TurnID == "" || rec.FinalMessageID == "" {
			rows.Close()
			return nil, 0, errors.New("threads: invalid completion identity")
		}
		maxID = id
		result = append(result, rec)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, 0, err
	}
	var sequence int64
	err = db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name='completed_turns'`).Scan(&sequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, err
	}
	if sequence < maxID {
		return nil, 0, errors.New("threads: completion sequence precedes a retained row")
	}
	return result, sequence, nil
}

func writePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
