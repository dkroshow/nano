package statestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

// ErrLegacySourceChanged prevents the one-time legacy copy from publishing a
// mixed snapshot when a source changes during that copy.
var ErrLegacySourceChanged = errors.New("threads_legacy_source_changed")

type sourceFingerprintVerifier func(context.Context, *sql.DB, StorePaths) error

func verifyCopiedLegacySources(ctx context.Context, db *sql.DB, paths StorePaths, preCopyVersion int, verify sourceFingerprintVerifier) error {
	if preCopyVersion >= turns.LegacyTurnSchemaVersion {
		return nil
	}
	return verify(ctx, db, paths)
}

func sourceChange(paths StorePaths, source string, cause error) error {
	return fmt.Errorf("%w: %s (%v); stop legacy writers and retry the one-time copy before publishing %s", ErrLegacySourceChanged, source, cause, paths.Destination)
}

func fingerprintSources(ctx context.Context, paths StorePaths) ([]turns.SourceFingerprint, error) {
	var result []turns.SourceFingerprint
	for _, source := range []struct{ name, path, kind string }{
		{"control.sqlite", paths.Control, "control"},
		{"queued-turns.json", paths.Turns.Queue, "json"},
		{"send-attempts.json", paths.Turns.Attempts, "json"},
		{"turn-completed.sqlite", paths.Turns.Completed, "completed"},
	} {
		digest, err := fingerprintSource(ctx, source.path, source.kind)
		if err != nil {
			return nil, sourceChange(paths, source.name, err)
		}
		result = append(result, turns.SourceFingerprint{Name: source.name, SHA256: digest})
	}
	return result, nil
}

func verifySourceFingerprints(ctx context.Context, db *sql.DB, paths StorePaths) error {
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT manifest_json FROM threads_store_migrations WHERE name=?`, turns.MigrationName).Scan(&raw); err != nil {
		return sourceChange(paths, "migration receipt", err)
	}
	var manifest struct {
		Sources []turns.SourceFingerprint `json:"sources"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return sourceChange(paths, "migration receipt", err)
	}
	current, err := fingerprintSources(ctx, paths)
	if err != nil {
		return err
	}
	return compareSourceFingerprints(paths, manifest.Sources, current)
}

func compareSourceFingerprints(paths StorePaths, recorded, current []turns.SourceFingerprint) error {
	if len(recorded) != 4 || len(current) != 4 {
		return sourceChange(paths, "migration receipt", errors.New("missing source fingerprints"))
	}
	for i, source := range current {
		if recorded[i] != source {
			return sourceChange(paths, source.Name, errors.New("copied facts differ from the migration receipt"))
		}
	}
	return nil
}

func fingerprintSource(ctx context.Context, path, kind string) (string, error) {
	hash := sha256.New()
	encode := json.NewEncoder(hash)
	var info os.FileInfo
	var err error
	if path != "" {
		info, err = os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	present := info != nil
	if present && !info.Mode().IsRegular() {
		return "", errors.New("source is not a regular file")
	}
	if kind == "json" && !present {
		// An absent file has no digest. Do not hash a marker that could itself
		// be written as malformed journal contents and pass the restart check.
		return "", nil
	}
	if kind == "control" {
		// Absent JSON and control sources must also be checked on later boots.
		// A legacy completion file may appear after a fresh migration.
		_ = encode.Encode(present)
	}
	if !present && path != "" && kind != "json" {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(path + suffix); err == nil {
				return "", errors.New("absent SQLite source has a stranded journal")
			} else if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		}
	}
	if kind == "json" && present {
		body, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		_, _ = hash.Write(body)
	} else if kind != "json" {
		var tx *sql.Tx
		if present {
			if info.Size() == 0 {
				return "", errors.New("empty SQLite source")
			}
			db, err := sql.Open("sqlite", statedb.URI(path, "ro"))
			if err != nil {
				return "", err
			}
			defer db.Close()
			tx, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return "", err
			}
			defer tx.Rollback()
		}
		if kind == "completed" {
			records := []turns.CompletedTurn{}
			var sequence int64
			if tx != nil {
				var count int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name='completed_turns' AND type='table'`).Scan(&count); err != nil {
					return "", err
				}
				if count != 0 {
					records, sequence, err = turns.LoadLegacyCompletions(ctx, tx)
					if err != nil {
						return "", err
					}
				}
			}
			if records == nil {
				records = []turns.CompletedTurn{}
			}
			if err := encode.Encode(struct {
				Rows     []turns.CompletedTurn
				Sequence int64
			}{records, sequence}); err != nil {
				return "", err
			}
		} else if tx != nil {
			if err := fingerprintControl(ctx, tx, encode); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// The control backup copies every table. Hash its schema and typed row content
// in a stable order, so WAL checkpointing and page layout cannot change it.
func fingerprintControl(ctx context.Context, tx *sql.Tx, encode *json.Encoder) error {
	if err := fingerprintQuery(ctx, tx, encode, `PRAGMA user_version`); err != nil {
		return err
	}
	if err := fingerprintQuery(ctx, tx, encode, `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	for _, table := range tables {
		query := `SELECT * FROM ` + quote(table)
		rows, err := tx.QueryContext(ctx, query+` LIMIT 0`)
		if err != nil {
			return err
		}
		columns, err := rows.Columns()
		if err := errors.Join(err, rows.Close()); err != nil {
			return err
		}
		var order []string
		for _, column := range columns {
			order = append(order, `typeof(`+quote(column)+`)`, quote(column)+` COLLATE BINARY`)
		}
		if err := fingerprintQuery(ctx, tx, encode, query+` ORDER BY `+strings.Join(order, ",")); err != nil {
			return err
		}
	}
	return nil
}

func fingerprintQuery(ctx context.Context, tx *sql.Tx, encode *json.Encoder, query string) error {
	if err := encode.Encode(query); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	values, pointers := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		for _, value := range values {
			// The type tag distinguishes SQL text, blobs, integers, reals and NULL.
			if err := encode.Encode(fmt.Sprintf("%T:%#v", value, value)); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}
