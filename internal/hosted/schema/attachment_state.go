package schema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/threads"
)

const attachmentStateMigrationName = "hosted-attachments-v25"

// attachmentStateSchema is the schema-25 table that keeps every hosted thread
// Nano can resume, one row per thread, with its explicit state. Invoker and
// presentation stay in hosted_threads. No row is ever deleted, so it carries
// no foreign key: a finished row outlives even an explicit delete of its
// hosted thread. Only an explicit close or a cleanup agent finishes a thread,
// and nothing reopens it except an explicit resume. The partial unique index
// both keeps one active owner per native id and serves the one query that
// reads every active row.
var attachmentStateSchema = []string{
	`CREATE TABLE hosted_attachments (
		hosted_thread_id TEXT PRIMARY KEY,
		provider TEXT NOT NULL, native_thread_id TEXT NOT NULL,
		state TEXT NOT NULL CHECK(state IN ('attached','resumable','finished')),
		bin TEXT NOT NULL DEFAULT '', args_json TEXT NOT NULL DEFAULT '[]', shim_session_id TEXT NOT NULL DEFAULT '',
		pending_name TEXT NOT NULL DEFAULT '', continued_from TEXT NOT NULL DEFAULT '',
		turn_started_at_ms INTEGER NOT NULL DEFAULT 0, turn_anchor_seq INTEGER NOT NULL DEFAULT 0,
		attached_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '', finished_by TEXT NOT NULL DEFAULT '',
		CHECK((state = 'finished') = (finished_at <> '')))`,
	`CREATE UNIQUE INDEX hosted_attachments_active ON hosted_attachments(provider, native_thread_id) WHERE state <> 'finished'`,
}

// AttachmentStateMigrationReport is the schema-25 receipt.
type AttachmentStateMigrationReport struct {
	BackupPath   string `json:"backupPath"`
	Source       string `json:"source"`
	SourceSHA256 string `json:"sourceSha256,omitempty"`
	Records      int    `json:"records"`
	Attached     int    `json:"attached"`
	Resumable    int    `json:"resumable"`
	// Skipped counts records the table cannot hold: no hosted identity, or a
	// second record for one thread. The source file is retained.
	Skipped int `json:"skipped"`
}

// legacyAttachment is the attachments.json record shape, read only here.
type legacyAttachment struct {
	Provider        string   `json:"provider"`
	NativeThreadID  string   `json:"nativeThreadId"`
	HostedThreadID  string   `json:"hostedThreadId"`
	AgentSessionID  string   `json:"agentSessionId"`
	Cwd             string   `json:"cwd"`
	Model           string   `json:"model"`
	TurnServiceTier string   `json:"turnServiceTier"`
	PendingName     string   `json:"pendingName"`
	AttachedAt      string   `json:"attachedAt"`
	Bin             string   `json:"bin"`
	Args            []string `json:"args"`
	ShimSessionID   string   `json:"shimSessionId"`
	ContinuedFrom   string   `json:"continuedFrom"`
	TurnStartedAtMs int64    `json:"turnStartedAtMs"`
	TurnAnchorSeq   int64    `json:"turnAnchorSeq"`
}

// MigrateAttachmentState takes a backed-up schema-24 private candidate to
// schema 25, importing attachments.json once. A record whose thread was live
// when Nano stopped (its execution status still ready) becomes attached; every
// other record becomes resumable. With an empty backup path it only verifies a
// schema-25 store. The legacy file is never written.
func MigrateAttachmentState(ctx context.Context, db *sql.DB, backupPath, legacyPath string) (AttachmentStateMigrationReport, error) {
	var report AttachmentStateMigrationReport
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return report, err
	}
	if version == turns.TurnSchemaVersion {
		var raw string
		if err := db.QueryRowContext(ctx, `SELECT report_json FROM threads_store_migrations WHERE name=?`, attachmentStateMigrationName).Scan(&raw); err != nil {
			return report, fmt.Errorf("threads: missing hosted attachment migration receipt: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			return report, err
		}
		var tables int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name IN ('hosted_attachments','hosted_attachments_active')`).Scan(&tables); err != nil {
			return report, err
		}
		if tables != 2 {
			return report, errors.New("threads: hosted_attachments schema is incomplete")
		}
		return report, nil
	}
	if version != turns.ParkedDiscardTurnSchemaVersion {
		return report, fmt.Errorf("threads: hosted attachment migration requires schema %d, found %d", turns.ParkedDiscardTurnSchemaVersion, version)
	}
	if backupPath == "" {
		return report, errors.New("threads: hosted attachment migration requires a new backup path")
	}
	records, sum, err := readLegacyAttachments(legacyPath)
	if err != nil {
		return report, err
	}
	if err := threads.Backup(ctx, db, backupPath); err != nil {
		return report, fmt.Errorf("threads: back up hosted attachment source: %w", err)
	}
	report.BackupPath, report.Source, report.SourceSHA256, report.Records = backupPath, legacyPath, sum, len(records)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, statement := range attachmentStateSchema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return report, fmt.Errorf("threads: create hosted_attachments: %w", err)
		}
	}
	for _, record := range preferredAttachments(records) {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM hosted_execution_sessions WHERE hosted_thread_id=?`, record.HostedThreadID).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return report, err
		}
		state := "resumable"
		if status == "ready" {
			state = "attached"
			report.Attached++
		} else {
			report.Resumable++
		}
		if record.Args == nil {
			record.Args = []string{}
		}
		args, err := json.Marshal(record.Args)
		if err != nil {
			return report, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_attachments(hosted_thread_id,provider,native_thread_id,state,bin,args_json,shim_session_id,pending_name,continued_from,turn_started_at_ms,turn_anchor_seq,attached_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			record.HostedThreadID, record.Provider, record.NativeThreadID, state, record.Bin, string(args), record.ShimSessionID, record.PendingName, record.ContinuedFrom, record.TurnStartedAtMs, record.TurnAnchorSeq, record.AttachedAt); err != nil {
			return report, fmt.Errorf("threads: import attachment %s: %w", record.HostedThreadID, err)
		}
	}
	report.Skipped = len(records) - report.Attached - report.Resumable
	raw, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO threads_store_migrations(name,report_json,manifest_json) VALUES(?,?,?)`, attachmentStateMigrationName, string(raw), `{}`); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, turns.TurnSchemaVersion)); err != nil {
		return report, err
	}
	return report, tx.Commit()
}

// readLegacyAttachments parses attachments.json as the last Nano to write it
// did. A missing file is an install with nothing to import. A record still
// carrying cwd, model or tier has not had its posture imported into hosted
// metadata yet, and importing it here would drop those facts.
func readLegacyAttachments(path string) ([]legacyAttachment, string, error) {
	if path == "" {
		return nil, "", nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var records []legacyAttachment
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, "", fmt.Errorf("threads: parse attachments file %s: %w", path, err)
	}
	out := records[:0]
	for _, record := range records {
		if record.NativeThreadID == "" {
			continue
		}
		if record.Cwd != "" || record.Model != "" || record.TurnServiceTier != "" {
			return nil, "", fmt.Errorf("threads: attachments file %s still holds posture for %s; start the previous Nano once to import it", path, record.NativeThreadID)
		}
		if record.Provider == "" {
			record.Provider = "codex" // the store was codex-only before records named a provider
		}
		if record.HostedThreadID == "" {
			record.HostedThreadID = record.AgentSessionID
		}
		out = append(out, record)
	}
	sum := sha256.Sum256(data)
	return out, hex.EncodeToString(sum[:]), nil
}

// preferredAttachments keeps one record per hosted thread. An interrupted
// Claude re-key can leave a provisional record (named by its shim handle)
// beside the record for the resolved native id; the resolved one is current.
// Otherwise the latest attach wins. Records with no hosted identity are left
// out; the caller counts them as skipped.
func preferredAttachments(records []legacyAttachment) []legacyAttachment {
	chosen := map[string]int{}
	var order []string
	for i, record := range records {
		if record.HostedThreadID == "" {
			continue
		}
		j, seen := chosen[record.HostedThreadID]
		if !seen {
			chosen[record.HostedThreadID] = i
			order = append(order, record.HostedThreadID)
			continue
		}
		current := records[j]
		provisional := func(r legacyAttachment) bool { return r.ShimSessionID != "" && r.NativeThreadID == r.ShimSessionID }
		if provisional(current) != provisional(record) {
			if provisional(current) {
				chosen[record.HostedThreadID] = i
			}
			continue
		}
		if threads.TimestampBefore(current.AttachedAt, record.AttachedAt) {
			chosen[record.HostedThreadID] = i
		}
	}
	out := make([]legacyAttachment, 0, len(order))
	for _, id := range order {
		out = append(out, records[chosen[id]])
	}
	return out
}
