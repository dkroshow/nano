package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	statedb "github.com/dkroshow/nano/internal/state/db"
	_ "modernc.org/sqlite"
)

const agentMailSchemaVersion = 1

type Store struct {
	DB *sql.DB
}

type MigrationReport struct {
	Messages  int `json:"messages"`
	Events    int `json:"events"`
	Refs      int `json:"refs"`
	Sequences int `json:"sequences"`
}

func OpenStore(path, legacyPath string) (*Store, MigrationReport, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, MigrationReport{}, fmt.Errorf("resolve mail path: %w", err)
	}
	db, err := statedb.Open(context.Background(), abs)
	if err != nil {
		return nil, MigrationReport{}, err
	}
	store := &Store{DB: db}
	report, err := store.migrate(legacyPath)
	if err != nil {
		db.Close()
		return nil, report, err
	}
	return store, report, nil
}

func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}

func (s *Store) migrate(legacyPath string) (MigrationReport, error) {
	var version int
	if err := s.DB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return MigrationReport{}, err
	}
	if version != 0 && version != agentMailSchemaVersion {
		return MigrationReport{}, fmt.Errorf("mail schema %d is unsupported", version)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return MigrationReport{}, err
	}
	defer tx.Rollback()
	for _, statement := range agentMailSchema {
		if _, err := tx.Exec(statement); err != nil {
			return MigrationReport{}, err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", agentMailSchemaVersion)); err != nil {
		return MigrationReport{}, err
	}
	if err := tx.Commit(); err != nil {
		return MigrationReport{}, err
	}
	if legacyPath == "" {
		return MigrationReport{}, nil
	}
	return s.copyLegacy(legacyPath)
}

var agentMailSchema = []string{
	`CREATE TABLE IF NOT EXISTS agent_messages (
		message_order INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT NOT NULL UNIQUE,
		client_message_id TEXT NOT NULL,
		request_hash TEXT NOT NULL,
		sender_key TEXT NOT NULL,
		sender_kind TEXT NOT NULL CHECK(sender_kind IN ('thread','system','operator')),
		origin TEXT NOT NULL CHECK(origin IN ('agent','system','operator')),
		sender_hosted_id TEXT,
		sender_native_id TEXT,
		sender_provider TEXT,
		sender_name TEXT,
		recipient_hosted_id TEXT NOT NULL,
		seq INTEGER NOT NULL CHECK(seq > 0),
		subject TEXT NOT NULL DEFAULT '',
		body TEXT NOT NULL,
		feedback_for_message_id TEXT,
		disposition_feedback INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL CHECK(status IN ('unread','seen','acted','declined','deferred')),
		status_note TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(sender_key,client_message_id),
		UNIQUE(recipient_hosted_id,seq))`,
	`CREATE TABLE IF NOT EXISTS agent_message_events (
		event_id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('created','seen','acted','declined','deferred','edited')),
		actor_kind TEXT NOT NULL CHECK(actor_kind IN ('thread','system','operator')),
		actor_hosted_id TEXT,
		detail_json TEXT,
		at TEXT NOT NULL,
		FOREIGN KEY(message_id) REFERENCES agent_messages(message_id) ON DELETE CASCADE)`,
	`CREATE TABLE IF NOT EXISTS agent_message_refs (
		message_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		id TEXT NOT NULL,
		PRIMARY KEY(message_id,kind,id),
		FOREIGN KEY(message_id) REFERENCES agent_messages(message_id) ON DELETE CASCADE)`,
	`CREATE TABLE IF NOT EXISTS agent_message_seq (
		recipient_hosted_id TEXT PRIMARY KEY,
		next_seq INTEGER NOT NULL CHECK(next_seq > 0))`,
	`CREATE TABLE IF NOT EXISTS agent_mail_migrations (
		name TEXT PRIMARY KEY,
		report_json TEXT NOT NULL,
		completed_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS agent_mail_settings (
		id INTEGER PRIMARY KEY CHECK(id=1),
		dispositioned_retention_days INTEGER CHECK(dispositioned_retention_days > 0))`,
	`INSERT OR IGNORE INTO agent_mail_settings(id,dispositioned_retention_days) VALUES(1,NULL)`,
	`CREATE INDEX IF NOT EXISTS agent_messages_inbox_order ON agent_messages(recipient_hosted_id,seq)`,
	`CREATE INDEX IF NOT EXISTS agent_messages_inbox_status ON agent_messages(recipient_hosted_id,status,seq)`,
	`CREATE INDEX IF NOT EXISTS agent_messages_outbox_order ON agent_messages(sender_hosted_id,message_order)`,
	`CREATE INDEX IF NOT EXISTS agent_message_refs_lookup ON agent_message_refs(kind,id,message_id)`,
	`CREATE INDEX IF NOT EXISTS agent_message_events_message_order ON agent_message_events(message_id,event_id)`,
}

// configureRetention applies the only mail retention setting. A negative
// override preserves the stored value, zero disables it, and a positive value
// keeps dispositioned mail for that many days. Unread and seen mail never
// qualify for deletion.
func (s *Store) ConfigureRetention(overrideDays int, now time.Time) (days int, enabled bool, deleted int64, err error) {
	if s == nil || s.DB == nil {
		return 0, false, 0, fmt.Errorf("mail store is unavailable")
	}
	if overrideDays < -1 {
		return 0, false, 0, fmt.Errorf("mail retention days must be -1, 0, or a positive integer")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, false, 0, err
	}
	defer tx.Rollback()
	if overrideDays >= 0 {
		var value any
		if overrideDays > 0 {
			value = overrideDays
		}
		if _, err := tx.Exec(`UPDATE agent_mail_settings SET dispositioned_retention_days=? WHERE id=1`, value); err != nil {
			return 0, false, 0, err
		}
	}
	var stored sql.NullInt64
	if err := tx.QueryRow(`SELECT dispositioned_retention_days FROM agent_mail_settings WHERE id=1`).Scan(&stored); err != nil {
		return 0, false, 0, err
	}
	if stored.Valid {
		days, enabled = int(stored.Int64), true
		cutoff := now.UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
		result, err := tx.Exec(`DELETE FROM agent_messages WHERE status IN ('acted','declined','deferred') AND updated_at < ?`, cutoff)
		if err != nil {
			return 0, false, 0, err
		}
		deleted, _ = result.RowsAffected()
	}
	if err := tx.Commit(); err != nil {
		return 0, false, 0, err
	}
	return days, enabled, deleted, nil
}

func StartRetention(ctx context.Context, store *Store, logf func(string, ...any)) {
	if store == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_, enabled, deleted, err := store.ConfigureRetention(-1, now)
				if err != nil {
					logf("mail retention: %v", err)
				} else if enabled && deleted > 0 {
					logf("mail retention deleted %d dispositioned messages", deleted)
				}
			}
		}
	}()
}

func (s *Store) copyLegacy(path string) (report MigrationReport, err error) {
	var existing string
	if err := s.DB.QueryRow(`SELECT report_json FROM agent_mail_migrations WHERE name='control-v21'`).Scan(&existing); err == nil {
		if err := json.Unmarshal([]byte(existing), &report); err != nil {
			return report, err
		}
		return report, nil
	} else if err != sql.ErrNoRows {
		return report, err
	}
	if _, err := s.DB.Exec(`ATTACH DATABASE ? AS legacy_mail`, path); err != nil {
		return report, err
	}
	defer func() {
		if _, detachErr := s.DB.Exec(`DETACH DATABASE legacy_mail`); err == nil && detachErr != nil {
			err = detachErr
		}
	}()
	for query, destination := range map[string]*int{
		`SELECT count(*) FROM legacy_mail.agent_messages`:       &report.Messages,
		`SELECT count(*) FROM legacy_mail.agent_message_events`: &report.Events,
		`SELECT count(*) FROM legacy_mail.agent_message_refs`:   &report.Refs,
		`SELECT count(*) FROM legacy_mail.agent_message_seq`:    &report.Sequences,
	} {
		if err := s.DB.QueryRow(query).Scan(destination); err != nil {
			return report, fmt.Errorf("read legacy mail counts: %w", err)
		}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	statements := []string{
		`INSERT INTO agent_messages(message_order,message_id,client_message_id,request_hash,sender_key,sender_kind,origin,sender_hosted_id,sender_native_id,sender_provider,sender_name,recipient_hosted_id,seq,subject,body,feedback_for_message_id,status,status_note,created_at,updated_at)
		 SELECT message_order,message_id,client_message_id,request_hash,sender_key,sender_kind,origin,sender_hosted_id,sender_native_id,sender_provider,sender_name,recipient_hosted_id,seq,subject,body,feedback_for_message_id,status,status_note,created_at,updated_at FROM legacy_mail.agent_messages`,
		`INSERT INTO agent_message_events SELECT * FROM legacy_mail.agent_message_events`,
		`INSERT INTO agent_message_refs SELECT * FROM legacy_mail.agent_message_refs`,
		`INSERT INTO agent_message_seq SELECT * FROM legacy_mail.agent_message_seq`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return report, fmt.Errorf("copy legacy mail: %w", err)
		}
	}
	for table, want := range map[string]int{"agent_messages": report.Messages, "agent_message_events": report.Events, "agent_message_refs": report.Refs, "agent_message_seq": report.Sequences} {
		var got int
		if err := tx.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
			return report, fmt.Errorf("verify copied %s count: %w", table, err)
		}
		if got != want {
			return report, fmt.Errorf("verify copied %s count: got %d want %d", table, got, want)
		}
	}
	encoded, _ := json.Marshal(report)
	if _, err := tx.Exec(`INSERT INTO agent_mail_migrations(name,report_json,completed_at) VALUES('control-v21',?,?)`, string(encoded), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return report, err
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, nil
}
