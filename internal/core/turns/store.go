package turns

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	TransactionalTurnSchemaVersion = 21
	UsageTurnSchemaVersion         = 22
	TurnSchemaVersion              = 24
)

type Store struct{ db *sql.DB }

type Start struct {
	HostedThreadID string
	Provider       string
	NativeThreadID string
	TurnID         string
	ActionID       string
	At             time.Time
	Source         string
}

type Usage struct {
	InputTokens       *int64
	CachedInputTokens *int64
	OutputTokens      *int64
}

type Finish struct {
	HostedThreadID string
	Provider       string
	NativeThreadID string
	TurnID         string
	ActionID       string
	FinalMessageID string
	Outcome        string
	At             time.Time
	Source         string
	Usage          Usage
}

func OpenStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("turns: threads database is required")
	}
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return nil, err
	}
	if version < TransactionalTurnSchemaVersion || version > TurnSchemaVersion {
		return nil, fmt.Errorf("turns: schema %d is not supported", version)
	}
	return &Store{db: db}, nil
}

func (s *Store) Started(ctx context.Context, start Start) (Record, bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(start.HostedThreadID) == "" || strings.TrimSpace(start.Provider) == "" || strings.TrimSpace(start.TurnID) == "" {
		return Record{}, false, errors.New("turns: start requires owner, provider, and turn id")
	}
	at := eventTime(start.At)
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO completed_turns(
		target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at,
		client_action_id,native_thread_id,binding_key,outcome)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, start.HostedThreadID, start.Provider, start.TurnID, "", "", source(start.Source), at,
		start.ActionID, start.NativeThreadID, "", "")
	if err != nil {
		return Record{}, false, err
	}
	created, _ := result.RowsAffected()
	record, err := s.Get(ctx, start.HostedThreadID, start.TurnID)
	return record, created == 1, err
}

func (s *Store) Finished(ctx context.Context, finish Finish) (Record, bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(finish.HostedThreadID) == "" || strings.TrimSpace(finish.Provider) == "" || strings.TrimSpace(finish.TurnID) == "" || strings.TrimSpace(finish.Outcome) == "" {
		return Record{}, false, errors.New("turns: finish requires owner, provider, turn id, and outcome")
	}
	at := eventTime(finish.At)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO completed_turns(
		target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at,
		client_action_id,native_thread_id,binding_key,outcome)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, finish.HostedThreadID, finish.Provider, finish.TurnID, finish.FinalMessageID, at, source(finish.Source), at,
		finish.ActionID, finish.NativeThreadID, "", finish.Outcome)
	if err != nil {
		return Record{}, false, err
	}
	created, _ := result.RowsAffected()
	alreadyTerminal, savedPlaceholder := false, false
	if created == 0 {
		result, err = tx.ExecContext(ctx, `UPDATE completed_turns SET
			provider=?,final_message_id=?,completed_at=?,source=?,
			client_action_id=CASE WHEN client_action_id='' THEN ? ELSE client_action_id END,
			native_thread_id=CASE WHEN native_thread_id='' THEN ? ELSE native_thread_id END,
			outcome=?
			WHERE target_hosted_thread_id=? AND turn_id=? AND outcome=''`, finish.Provider, finish.FinalMessageID, at, source(finish.Source),
			finish.ActionID, finish.NativeThreadID, finish.Outcome, finish.HostedThreadID, finish.TurnID)
		if err != nil {
			return Record{}, false, err
		}
		if changed, _ := result.RowsAffected(); changed == 0 {
			prior, loadErr := get(ctx, tx, finish.HostedThreadID, finish.TurnID)
			if loadErr != nil {
				return Record{}, false, loadErr
			}
			if prior.Outcome != finish.Outcome || prior.FinalMessageID != finish.FinalMessageID {
				return Record{}, false, errors.New("turns: terminal event conflicts with retained turn")
			}
			alreadyTerminal = true
			savedPlaceholder = prior.Source == "saved" && source(finish.Source) == "adapter"
		}
	}
	// The finish that ends a turn records its usage. A repeated finish of an
	// ended turn only fills usage still unknown: a recorded value, zero
	// included, is kept, so usage is never replaced or counted twice. The one
	// exception is the provider's own result after a saved finish: the saved
	// transcript decides the answer but reports no usage, so its placeholder
	// zeros give way, once, to the counts the result reports; the row then
	// records the provider's counts, and a count the saved finish kept stays.
	usage := `UPDATE completed_turns SET usage_input_tokens=?,usage_cached_input_tokens=?,usage_output_tokens=? WHERE target_hosted_thread_id=? AND turn_id=?`
	switch {
	case savedPlaceholder:
		usage = `UPDATE completed_turns SET source='adapter',
			usage_input_tokens=CASE WHEN COALESCE(usage_input_tokens,0)=0 THEN COALESCE(?,usage_input_tokens) ELSE usage_input_tokens END,
			usage_cached_input_tokens=CASE WHEN COALESCE(usage_cached_input_tokens,0)=0 THEN COALESCE(?,usage_cached_input_tokens) ELSE usage_cached_input_tokens END,
			usage_output_tokens=CASE WHEN COALESCE(usage_output_tokens,0)=0 THEN COALESCE(?,usage_output_tokens) ELSE usage_output_tokens END
			WHERE target_hosted_thread_id=? AND turn_id=?`
	case alreadyTerminal:
		usage = `UPDATE completed_turns SET usage_input_tokens=COALESCE(usage_input_tokens,?),usage_cached_input_tokens=COALESCE(usage_cached_input_tokens,?),usage_output_tokens=COALESCE(usage_output_tokens,?) WHERE target_hosted_thread_id=? AND turn_id=?`
	}
	if _, err := tx.ExecContext(ctx, usage, finish.Usage.InputTokens, finish.Usage.CachedInputTokens, finish.Usage.OutputTokens, finish.HostedThreadID, finish.TurnID); err != nil {
		return Record{}, false, err
	}
	record, err := get(ctx, tx, finish.HostedThreadID, finish.TurnID)
	if err != nil {
		return Record{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, false, err
	}
	return record, created == 1, nil
}

func (s *Store) Get(ctx context.Context, hostedThreadID, turnID string) (Record, error) {
	return get(ctx, s.db, hostedThreadID, turnID)
}

// Ended returns the ids of the thread's turns that have a recorded outcome, in
// one read: a caller checking every turn of a long thread would otherwise
// queue once per turn behind the writer connection.
func (s *Store) Ended(ctx context.Context, hostedThreadID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT turn_id FROM completed_turns WHERE target_hosted_thread_id=? AND outcome<>''`, hostedThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ended := map[string]bool{}
	for rows.Next() {
		var turnID string
		if err := rows.Scan(&turnID); err != nil {
			return nil, err
		}
		ended[turnID] = true
	}
	return ended, rows.Err()
}

func (s *Store) Latest(ctx context.Context, hostedThreadID string) (Record, error) {
	return scanRecord(s.db.QueryRowContext(ctx, selectRecord+` WHERE target_hosted_thread_id=? ORDER BY id DESC LIMIT 1`, hostedThreadID))
}

func (s *Store) After(ctx context.Context, hostedThreadID, bookmark string, limit int) ([]Record, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx, selectRecord+` WHERE target_hosted_thread_id=? AND outcome<>'' AND id>COALESCE((SELECT id FROM completed_turns WHERE target_hosted_thread_id=? AND turn_id=?),0) ORDER BY id LIMIT ?`, hostedThreadID, hostedThreadID, bookmark, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

const selectRecord = `SELECT id,target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at,client_action_id,native_thread_id,outcome,usage_input_tokens,usage_cached_input_tokens,usage_output_tokens FROM completed_turns`

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func get(ctx context.Context, db queryer, hostedThreadID, turnID string) (Record, error) {
	return scanRecord(db.QueryRowContext(ctx, selectRecord+` WHERE target_hosted_thread_id=? AND turn_id=?`, hostedThreadID, turnID))
}

type scanner interface{ Scan(...any) error }

func scanRecord(row scanner) (Record, error) {
	var record Record
	err := row.Scan(&record.ID, &record.TargetHostedThreadID, &record.Provider, &record.TurnID, &record.FinalMessageID,
		&record.CompletedAt, &record.Source, &record.CreatedAt, &record.ClientActionID, &record.NativeThreadID, &record.Outcome,
		&record.UsageInputTokens, &record.UsageCachedInputTokens, &record.UsageOutputTokens)
	return record, err
}

func eventTime(at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func source(value string) string {
	if value == "" {
		return "adapter"
	}
	return value
}
