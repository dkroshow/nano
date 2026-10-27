package prompts

import (
	"context"
	"database/sql"
	"fmt"
)

const queuedTurnSchema = `CREATE TABLE queued_turns(
 client_action_id TEXT NOT NULL PRIMARY KEY,
 provider TEXT NOT NULL,
 hosted_thread_id TEXT NOT NULL,
 native_thread_id TEXT NOT NULL,
 text TEXT NOT NULL,
 presentation_text TEXT NOT NULL,
 text_hash TEXT NOT NULL,
 request_hash TEXT NOT NULL,
 state TEXT NOT NULL,
 origin TEXT NOT NULL,
 created_at TEXT NOT NULL,
 provider_turn_id TEXT NOT NULL,
 provider_terminal_status TEXT NOT NULL,
 provider_terminal_at TEXT NOT NULL,
 position INTEGER NOT NULL UNIQUE
)`

const queuedTurnColumns = "client_action_id,provider,hosted_thread_id,native_thread_id,text,presentation_text,text_hash,request_hash,state,origin,created_at,provider_turn_id,provider_terminal_status,provider_terminal_at"

// LoadQueuedTurns retains the stored arrival order and every durable evidence field.
func LoadQueuedTurns(ctx context.Context, db *sql.DB) ([]QueuedTurn, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+queuedTurnColumns+" FROM queued_turns ORDER BY position")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []QueuedTurn
	for rows.Next() {
		var rec QueuedTurn
		if err := rows.Scan(&rec.ClientActionID, &rec.Provider, &rec.HostedThreadID, &rec.NativeThreadID, &rec.Text, &rec.PresentationText, &rec.TextHash, &rec.RequestHash, &rec.State, &rec.Origin, &rec.CreatedAt, &rec.ProviderTurnID, &rec.ProviderTerminalStatus, &rec.ProviderTerminalAt); err != nil {
			return nil, err
		}
		result = append(result, rec)
	}
	return result, rows.Err()
}

// SaveQueuedTurns replaces only this record family, in one transaction. Delivery
// decisions and exact-turn confirmation remain with the existing coordinator.
func SaveQueuedTurns(ctx context.Context, db *sql.DB, records []QueuedTurn) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM queued_turns"); err != nil {
		return err
	}
	if err := insertQueuedTurns(ctx, tx, records); err != nil {
		return err
	}
	return tx.Commit()
}

func insertQueuedTurns(ctx context.Context, tx *sql.Tx, records []QueuedTurn) error {
	statement, err := tx.PrepareContext(ctx, "INSERT INTO queued_turns("+queuedTurnColumns+",position) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer statement.Close()
	for position, rec := range records {
		if rec.ClientActionID == "" {
			return fmt.Errorf("threads: queued_turns record has no action id")
		}
		if _, err := statement.ExecContext(ctx, rec.ClientActionID, rec.Provider, rec.HostedThreadID, rec.NativeThreadID, rec.Text, rec.PresentationText, rec.TextHash, rec.RequestHash, rec.State, rec.Origin, rec.CreatedAt, rec.ProviderTurnID, rec.ProviderTerminalStatus, rec.ProviderTerminalAt, position); err != nil {
			return err
		}
	}
	return nil
}

const sendAttemptSchema = `CREATE TABLE send_attempts(
 client_action_id TEXT NOT NULL PRIMARY KEY,
 provider TEXT NOT NULL,
 native_thread_id TEXT NOT NULL,
 text_hash TEXT NOT NULL,
 status TEXT NOT NULL,
 run_id TEXT NOT NULL,
 reason TEXT NOT NULL,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 confirmed INTEGER NOT NULL,
 confirmed_at TEXT NOT NULL,
 provider_turn_id TEXT NOT NULL,
 provider_terminal_status TEXT NOT NULL,
 provider_terminal_at TEXT NOT NULL,
 canonical_message_id TEXT NOT NULL,
 identity_conflict TEXT NOT NULL,
 position INTEGER NOT NULL UNIQUE
)`

const sendAttemptColumns = "client_action_id,provider,native_thread_id,text_hash,status,run_id,reason,created_at,updated_at,confirmed,confirmed_at,provider_turn_id,provider_terminal_status,provider_terminal_at,canonical_message_id,identity_conflict"

// LoadSendAttempts retains the stored arrival order and every durable evidence field.
func LoadSendAttempts(ctx context.Context, db *sql.DB) ([]SendAttempt, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+sendAttemptColumns+" FROM send_attempts ORDER BY position")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SendAttempt
	for rows.Next() {
		var rec SendAttempt
		if err := rows.Scan(&rec.ClientActionID, &rec.Provider, &rec.NativeThreadID, &rec.TextHash, &rec.Status, &rec.RunID, &rec.Reason, &rec.CreatedAt, &rec.UpdatedAt, &rec.Confirmed, &rec.ConfirmedAt, &rec.ProviderTurnID, &rec.ProviderTerminalStatus, &rec.ProviderTerminalAt, &rec.CanonicalMessageID, &rec.IdentityConflict); err != nil {
			return nil, err
		}
		result = append(result, rec)
	}
	return result, rows.Err()
}

// SaveSendAttempts replaces only this record family, in one transaction. Delivery
// decisions and exact-turn confirmation remain with the existing coordinator.
func SaveSendAttempts(ctx context.Context, db *sql.DB, records []SendAttempt) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM send_attempts"); err != nil {
		return err
	}
	if err := insertSendAttempts(ctx, tx, records); err != nil {
		return err
	}
	return tx.Commit()
}

func insertSendAttempts(ctx context.Context, tx *sql.Tx, records []SendAttempt) error {
	statement, err := tx.PrepareContext(ctx, "INSERT INTO send_attempts("+sendAttemptColumns+",position) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer statement.Close()
	for position, rec := range records {
		if rec.ClientActionID == "" {
			return fmt.Errorf("threads: send_attempts record has no action id")
		}
		if _, err := statement.ExecContext(ctx, rec.ClientActionID, rec.Provider, rec.NativeThreadID, rec.TextHash, rec.Status, rec.RunID, rec.Reason, rec.CreatedAt, rec.UpdatedAt, rec.Confirmed, rec.ConfirmedAt, rec.ProviderTurnID, rec.ProviderTerminalStatus, rec.ProviderTerminalAt, rec.CanonicalMessageID, rec.IdentityConflict, position); err != nil {
			return err
		}
	}
	return nil
}
