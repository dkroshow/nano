package mail

import (
	"database/sql"
	"fmt"
)

// MigrateLegacyStateSchema retains ownership of the legacy mail copy while it
// remains in nano_threads.sqlite. S2 removes these tables after the soak.
func MigrateLegacyStateSchema(tx *sql.Tx, version int) error {
	if version <= 15 {
		for _, statement := range legacyStateMailSchema {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("create legacy agent inbox schema: %w", err)
			}
		}
	}
	if version != 16 {
		return nil
	}
	for _, statement := range legacyStateMailV17Rebuild {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("widen legacy agent inbox schema: %w", err)
		}
	}
	var violations int
	if err := tx.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return fmt.Errorf("verify legacy agent inbox rebuild: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("legacy agent inbox rebuild left %d foreign key violations", violations)
	}
	return nil
}

func CheckLegacyStateIntegrity(db *sql.DB) error {
	var invalid int
	err := db.QueryRow(`SELECT
		(SELECT count(*) FROM agent_message_refs r LEFT JOIN agent_messages m ON m.message_id=r.message_id WHERE m.message_id IS NULL) +
		(SELECT count(*) FROM agent_message_events e LEFT JOIN agent_messages m ON m.message_id=e.message_id WHERE m.message_id IS NULL) +
		(SELECT count(*) FROM agent_messages WHERE status NOT IN ('unread','seen','acted','declined','deferred')) +
		(SELECT count(*) FROM (SELECT recipient_hosted_id,seq FROM agent_messages GROUP BY recipient_hosted_id,seq HAVING count(*) > 1)) +
		(SELECT count(*) FROM agent_messages m LEFT JOIN agent_message_seq s ON s.recipient_hosted_id=m.recipient_hosted_id WHERE s.recipient_hosted_id IS NULL OR s.next_seq <= m.seq)`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("control metadata integrity failure: %d agent message rows have invalid references or sequence state", invalid)
	}
	return nil
}

var legacyStateMailSchema = []string{
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
	`CREATE INDEX IF NOT EXISTS agent_messages_inbox_order ON agent_messages(recipient_hosted_id,seq)`,
	`CREATE INDEX IF NOT EXISTS agent_messages_inbox_status ON agent_messages(recipient_hosted_id,status,seq)`,
	`CREATE INDEX IF NOT EXISTS agent_messages_outbox_order ON agent_messages(sender_hosted_id,message_order)`,
	`CREATE INDEX IF NOT EXISTS agent_message_refs_lookup ON agent_message_refs(kind,id,message_id)`,
	`CREATE INDEX IF NOT EXISTS agent_message_events_message_order ON agent_message_events(message_id,event_id)`,
}

var legacyStateMailV17Rebuild = []string{
	`CREATE TABLE agent_messages_rebuilt (
		message_order INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT NOT NULL UNIQUE,
		client_message_id TEXT NOT NULL, request_hash TEXT NOT NULL, sender_key TEXT NOT NULL,
		sender_kind TEXT NOT NULL CHECK(sender_kind IN ('thread','system','operator')),
		origin TEXT NOT NULL CHECK(origin IN ('agent','system','operator')),
		sender_hosted_id TEXT, sender_native_id TEXT, sender_provider TEXT, sender_name TEXT,
		recipient_hosted_id TEXT NOT NULL, seq INTEGER NOT NULL CHECK(seq > 0),
		subject TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, feedback_for_message_id TEXT,
		status TEXT NOT NULL CHECK(status IN ('unread','seen','acted','declined','deferred')),
		status_note TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		UNIQUE(sender_key,client_message_id), UNIQUE(recipient_hosted_id,seq))`,
	`INSERT INTO agent_messages_rebuilt SELECT * FROM agent_messages`,
	`DROP TABLE agent_messages`,
	`ALTER TABLE agent_messages_rebuilt RENAME TO agent_messages`,
	`CREATE INDEX agent_messages_inbox_order ON agent_messages(recipient_hosted_id,seq)`,
	`CREATE INDEX agent_messages_inbox_status ON agent_messages(recipient_hosted_id,status,seq)`,
	`CREATE INDEX agent_messages_outbox_order ON agent_messages(sender_hosted_id,message_order)`,
	`CREATE TABLE agent_message_events_rebuilt (
		event_id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('created','seen','acted','declined','deferred','edited')),
		actor_kind TEXT NOT NULL CHECK(actor_kind IN ('thread','system','operator')),
		actor_hosted_id TEXT, detail_json TEXT, at TEXT NOT NULL,
		FOREIGN KEY(message_id) REFERENCES agent_messages(message_id) ON DELETE CASCADE)`,
	`INSERT INTO agent_message_events_rebuilt SELECT * FROM agent_message_events`,
	`DROP TABLE agent_message_events`,
	`ALTER TABLE agent_message_events_rebuilt RENAME TO agent_message_events`,
	`CREATE INDEX agent_message_events_message_order ON agent_message_events(message_id,event_id)`,
}
