package statestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
)

const transcriptBoundariesRetirement = "transcript-boundaries-retired-v1"

type transcriptBoundariesReport struct {
	BackupPath string `json:"backupPath"`
	Rows       int    `json:"rows"`
}

// retireTranscriptBoundaries runs after the store is prepared and before any
// writer opens it. A store without the table is left alone.
func retireTranscriptBoundaries(ctx context.Context, destination, backupRoot string) (err error) {
	db, err := statedb.Open(ctx, destination)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var present, version int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='transcript_boundaries'`).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if err := os.MkdirAll(backupRoot, 0700); err != nil {
		return err
	}
	run, err := os.MkdirTemp(backupRoot, "transcript-boundaries-")
	if err != nil {
		return err
	}
	report := transcriptBoundariesReport{BackupPath: filepath.Join(run, fmt.Sprintf("nano-threads-v%d.sqlite", version))}
	if err := threads.Backup(ctx, db, report.BackupPath); err != nil {
		return fmt.Errorf("threads: back up before dropping transcript_boundaries: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM transcript_boundaries`).Scan(&report.Rows); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE transcript_boundaries`); err != nil {
		return fmt.Errorf("threads: drop transcript_boundaries: %w", err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	// After an older Nano recreated the table, a second retirement keeps the
	// first receipt; its own backup still lands beside the first.
	if _, err := tx.ExecContext(ctx, `INSERT INTO threads_store_migrations(name,report_json,manifest_json) VALUES(?,?,'{}') ON CONFLICT(name) DO NOTHING`, transcriptBoundariesRetirement, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
