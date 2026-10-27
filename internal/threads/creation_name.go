package threads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const CreationNameField = "creationName"

// RecordCreationName stores the immutable name from a hosted-thread creation
// request. Repeating the same request is a no-op; a different value is a
// conflict rather than a rename.
func RecordCreationName(ctx context.Context, db *sql.DB, hostedID, name, evidence string) (int64, bool, error) {
	if db == nil {
		return 0, false, errors.New("threads: creation name store is unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	revision, changed, err := RecordCreationNameTx(ctx, tx, hostedID, name, evidence, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return revision, changed, nil
}

// RecordCreationNameTx performs the same immutable insert in the caller's
// hosted-thread creation or maintenance transaction.
func RecordCreationNameTx(ctx context.Context, tx *sql.Tx, hostedID, name, evidence, now string) (int64, bool, error) {
	hostedID, name = strings.TrimSpace(hostedID), strings.TrimSpace(name)
	if tx == nil || hostedID == "" || name == "" {
		return 0, false, errors.New("threads: hosted id and creation name are required")
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT effective_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, CreationNameField).Scan(&raw)
	if err == nil {
		var existing string
		if json.Unmarshal([]byte(raw), &existing) != nil || existing == "" {
			return 0, false, errors.New("threads: stored creation name is invalid")
		}
		if existing != name {
			return 0, false, fmt.Errorf("threads: hosted thread %s has a different creation name", hostedID)
		}
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, hostedID).Scan(&revision); err != nil {
			return 0, false, err
		}
		return revision, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	encoded, err := json.Marshal(name)
	if err != nil {
		return 0, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO thread_metadata_events(hosted_thread_id,field,value_json,disposition,source,evidence,attachment_generation,evidence_freshness,source_event_key,observed_at,committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, hostedID, CreationNameField, string(encoded), "confirmed", "creation", evidence, 0, "live", "creation-name:"+hostedID, now, now)
	if err != nil {
		return 0, false, err
	}
	eventID, err := result.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `UPDATE hosted_threads SET revision=revision+1,updated_at=? WHERE hosted_thread_id=? RETURNING revision`, now, hostedID).Scan(&revision); err != nil {
		return 0, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO thread_metadata_current(hosted_thread_id,field,desired_value_json,desired_disposition,desired_source,desired_event_id,effective_value_json,effective_disposition,effective_source,effective_event_id,thread_revision,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, hostedID, CreationNameField, string(encoded), "confirmed", "creation", eventID, string(encoded), "confirmed", "creation", eventID, revision, now)
	return revision, err == nil, err
}

func ReadCreationName(ctx context.Context, db *sql.DB, hostedID string) (string, bool, error) {
	if db == nil || strings.TrimSpace(hostedID) == "" {
		return "", false, nil
	}
	var raw string
	err := db.QueryRowContext(ctx, `SELECT effective_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, CreationNameField).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var name string
	if err := json.Unmarshal([]byte(raw), &name); err != nil || strings.TrimSpace(name) == "" {
		return "", false, errors.New("threads: stored creation name is invalid")
	}
	return name, true, nil
}
