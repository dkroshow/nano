package heartbeat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	nanofiles "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/interact"
	statedb "github.com/dkroshow/nano/internal/state/db"
	_ "modernc.org/sqlite"
)

const (
	SchemaVersion = 2

	StateActive  = "active"
	StatePaused  = "paused"
	StateRemoved = "removed"

	GateManualPause = "manual_pause"
	GateHardFacts   = "hard_facts"
	GatePrePing     = "pre_ping"
	GateDedupeSkip  = "dedupe_skip"
	GateGlobalCap   = "global_cap"
	GateDelivery    = "delivery"
	GateLegacy      = "legacy"

	OutcomeSkipped   = "skipped"
	OutcomeDelivered = "delivered"
	OutcomeFailed    = "failed"

	ActionPrefix = "nano:heartbeat:"
	Footer       = "[from nano heartbeat]"

	UsagePending     = "pending"
	UsageReported    = "reported"
	UsageUnavailable = "unavailable"
)

var (
	ErrAttachmentExists = errors.New("heartbeat attachment already exists for target")
	ErrNotFound         = errors.New("heartbeat attachment not found")
	ErrScriptNotFound   = errors.New("heartbeat script not found")
)

type Actor struct {
	Kind           string `json:"kind"`
	HostedThreadID string `json:"hostedThreadId,omitempty"`
	Name           string `json:"name,omitempty"`
}

// PrePingReference pins an attachment to one immutable script
// revision. Settings are the only attachment-controlled processor input.
// They must be a bounded JSON object; attachments never provide commands,
// paths, arguments, URLs, or environment variables.
type PrePingReference struct {
	ScriptID string          `json:"scriptId"`
	Revision int64           `json:"revision"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

type Script struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Owner           Actor  `json:"owner"`
	CurrentRevision int64  `json:"currentRevision"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
}

type ScriptRevision struct {
	ScriptID         string          `json:"scriptId"`
	Revision         int64           `json:"revision"`
	Language         string          `json:"language"`
	Source           string          `json:"source,omitempty"`
	SourceSHA256     string          `json:"sourceSha256"`
	DefaultSettings  json.RawMessage `json:"defaultSettings"`
	DefaultPrompt    string          `json:"defaultPrompt,omitempty"`
	TemplateID       string          `json:"templateId,omitempty"`
	TemplateRevision string          `json:"templateRevision,omitempty"`
	Authority        string          `json:"authority,omitempty"`
	Actor            Actor           `json:"actor"`
	CreatedAt        string          `json:"createdAt"`
}

type ScriptRevisionInput struct {
	Name, Source, DefaultPrompt, TemplateID, TemplateRevision, Authority string
	DefaultSettings                                                      json.RawMessage
	Actor                                                                Actor
}

// Attachment is the durable per-target configuration and explicit
// administrative pause state.
type Attachment struct {
	ID                      string            `json:"id"`
	TargetHostedThreadID    string            `json:"targetHostedThreadId"`
	CadenceMinutes          int               `json:"cadenceMinutes"`
	OffsetMinutes           int               `json:"offsetMinutes"`
	Prompt                  string            `json:"prompt"`
	PrePing                 *PrePingReference `json:"prePing,omitempty"`
	PrePingFailureScriptID  string            `json:"prePingFailureScriptId,omitempty"`
	PrePingFailureRevision  int64             `json:"prePingFailureRevision,omitempty"`
	PrePingFailureAt        string            `json:"prePingFailureAt,omitempty"`
	PrePingSuspended        bool              `json:"prePingSuspended,omitempty"`
	State                   string            `json:"state"`
	PauseReason             string            `json:"pauseReason,omitempty"`
	PauseActor              Actor             `json:"pauseActor,omitempty"`
	Authority               string            `json:"authority,omitempty"`
	Creator                 Actor             `json:"creator"`
	Revision                int64             `json:"revision"`
	CreatedAt               string            `json:"createdAt"`
	UpdatedAt               string            `json:"updatedAt"`
	RemovedAt               string            `json:"removedAt,omitempty"`
	ConsecutiveWakes        int               `json:"consecutiveWakes"`
	EffectiveCadenceMinutes int               `json:"effectiveCadenceMinutes"`
	LastHumanMessageAt      string            `json:"lastHumanMessageAt,omitempty"`
	LastResumeAt            string            `json:"lastResumeAt,omitempty"`
	Usage                   UsageSummary      `json:"usage"`
}

type AttachmentInput struct {
	TargetHostedThreadID string
	CadenceMinutes       int
	OffsetMinutes        int
	Prompt               string
	PrePing              *PrePingReference
	Authority            string
	Actor                Actor
}

type AttachmentPatch struct {
	CadenceMinutes *int
	OffsetMinutes  *int
	Prompt         *string
	PrePingSet     bool
	PrePing        *PrePingReference
}

// WakeDecision is one durable per-due-minute (or legacy per-tick)
// decision row. Exactly one row exists per (attachment, dueEpochMinute); it is
// claimed once and then settled with the final gate/outcome.
type WakeDecision struct {
	ID                    int64                `json:"id"`
	AttachmentID          string               `json:"attachmentId"`
	DueEpochMinute        *int64               `json:"dueEpochMinute,omitempty"`
	Gate                  string               `json:"gate"`
	AvailabilityState     string               `json:"availabilityState,omitempty"`
	AvailabilityReasons   []string             `json:"availabilityReasons,omitempty"`
	Outcome               string               `json:"outcome"`
	OperationID           string               `json:"operationId,omitempty"`
	ErrorClass            string               `json:"errorClass,omitempty"`
	PrePingScriptID       string               `json:"prePingScriptId,omitempty"`
	PrePingRevision       int64                `json:"prePingRevision,omitempty"`
	PrePingResult         string               `json:"prePingResult,omitempty"`
	PrePingReason         string               `json:"prePingReason,omitempty"`
	PrePingDurationMS     int64                `json:"prePingDurationMs,omitempty"`
	PrePingPromptChanged  bool                 `json:"prePingPromptChanged,omitempty"`
	PrePingFailureLatched bool                 `json:"prePingFailureLatched,omitempty"`
	PrePingDedupeKey      string               `json:"prePingDedupeKey,omitempty"`
	OccurredAt            string               `json:"occurredAt"`
	LegacyTickSequence    *int64               `json:"legacyTickSequence,omitempty"`
	UsageState            string               `json:"usageState,omitempty"`
	Usage                 *interact.TokenUsage `json:"usage,omitempty"`
}

type UsageSummary struct {
	WakeCount         int64 `json:"wakeCount"`
	InputTokens       int64 `json:"inputTokens"`
	CachedInputTokens int64 `json:"cachedInputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	PendingWakes      int64 `json:"pendingWakes"`
	UnavailableWakes  int64 `json:"unavailableWakes"`
}

type Store struct{ DB *sql.DB }

type BackupFunc func(context.Context, *sql.DB, string) error

type sqlScanner interface {
	Scan(...any) error
}

// OpenStore resolves and opens the durable heartbeat database. Resolving the
// prospective path here gives every caller the same file through symlinks.
func OpenStore(path string, backup BackupFunc) (*Store, error) {
	absolute, err := nanofiles.CanonicalProspective(path)
	if err != nil {
		return nil, err
	}
	info, statErr := os.Stat(absolute)
	existing := statErr == nil && info.Mode().IsRegular() && info.Size() > 0
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect heartbeat database: %w", statErr)
	}
	db, err := statedb.Open(context.Background(), absolute)
	if err != nil {
		return nil, err
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("read heartbeat schema version: %w", err)
	}
	if version > SchemaVersion {
		db.Close()
		return nil, fmt.Errorf("heartbeat database schema %d is newer than this Nano", version)
	}
	if existing && version < SchemaVersion {
		if backup == nil {
			db.Close()
			return nil, errors.New("heartbeat migration requires a backup function")
		}
		stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
		if err := backup(context.Background(), db, absolute+".before-stage6-"+stamp); err != nil {
			db.Close()
			return nil, fmt.Errorf("back up heartbeat database before migration: %w", err)
		}
	}
	store := &Store{DB: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, SchemaVersion)); err != nil {
		db.Close()
		return nil, fmt.Errorf("record heartbeat schema version: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}

func (s *Store) migrate() error {
	if err := s.migrateLegacyAttachments(); err != nil {
		return fmt.Errorf("migrate legacy heartbeat attachments: %w", err)
	}
	if err := s.migrateLegacyWakeLog(); err != nil {
		return fmt.Errorf("migrate legacy heartbeat wake log: %w", err)
	}
	if _, err := s.DB.Exec(`
		CREATE TABLE IF NOT EXISTS heartbeat_attachments (
			id TEXT PRIMARY KEY, target_hosted_thread_id TEXT NOT NULL,
			cadence_minutes INTEGER NOT NULL CHECK(cadence_minutes > 0), offset_minutes INTEGER NOT NULL DEFAULT 0,
			prompt TEXT NOT NULL, condition_type TEXT NOT NULL DEFAULT '',
			pre_ping_script_id TEXT NOT NULL DEFAULT '', pre_ping_script_revision INTEGER NOT NULL DEFAULT 0,
			pre_ping_settings TEXT NOT NULL DEFAULT '{}', pre_ping_failure_script_id TEXT NOT NULL DEFAULT '',
			pre_ping_failure_revision INTEGER NOT NULL DEFAULT 0, pre_ping_failure_at TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'active',
			pause_reason TEXT NOT NULL DEFAULT '', pause_actor_kind TEXT NOT NULL DEFAULT '',
			pause_actor_hosted_thread_id TEXT NOT NULL DEFAULT '', pause_actor_name TEXT NOT NULL DEFAULT '',
			agent_paused INTEGER NOT NULL DEFAULT 0, agent_pause_source_turn_id TEXT NOT NULL DEFAULT '',
			agent_pause_source_final_message_id TEXT NOT NULL DEFAULT '', agent_pause_at TEXT NOT NULL DEFAULT '',
			authority TEXT NOT NULL DEFAULT '',
			creator_kind TEXT NOT NULL, creator_hosted_thread_id TEXT NOT NULL DEFAULT '', creator_name TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, removed_at TEXT NOT NULL DEFAULT '',
				consecutive_wakes INTEGER NOT NULL DEFAULT 0, last_human_message_at TEXT NOT NULL DEFAULT '', last_resume_at TEXT NOT NULL DEFAULT '',
				pre_ping_suspended INTEGER NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX IF NOT EXISTS heartbeat_one_live_target
			ON heartbeat_attachments(target_hosted_thread_id) WHERE state!='removed';
		CREATE TABLE IF NOT EXISTS heartbeat_attachment_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, attachment_id TEXT NOT NULL, event_type TEXT NOT NULL,
			actor_kind TEXT NOT NULL, actor_hosted_thread_id TEXT NOT NULL DEFAULT '', actor_name TEXT NOT NULL DEFAULT '',
			authority TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', occurred_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS heartbeat_wake_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT, attachment_id TEXT NOT NULL, due_epoch_minute INTEGER,
			gate TEXT NOT NULL DEFAULT '', availability_state TEXT NOT NULL DEFAULT '',
			availability_reasons TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL,
				operation_id TEXT NOT NULL DEFAULT '', error_class TEXT NOT NULL DEFAULT '',
				condition_type TEXT NOT NULL DEFAULT '', condition_result TEXT NOT NULL DEFAULT '',
				pre_ping_script_id TEXT NOT NULL DEFAULT '', pre_ping_revision INTEGER NOT NULL DEFAULT 0,
				pre_ping_result TEXT NOT NULL DEFAULT '', pre_ping_reason TEXT NOT NULL DEFAULT '',
				pre_ping_duration_ms INTEGER NOT NULL DEFAULT 0, pre_ping_prompt_changed INTEGER NOT NULL DEFAULT 0,
				pre_ping_failure_latched INTEGER NOT NULL DEFAULT 0,
				pre_ping_dedupe_key TEXT NOT NULL DEFAULT '', occurred_at TEXT NOT NULL,
				usage_state TEXT NOT NULL DEFAULT '', usage_input_tokens INTEGER,
				usage_cached_input_tokens INTEGER, usage_output_tokens INTEGER,
			legacy_tick_sequence INTEGER
		);
		CREATE TABLE IF NOT EXISTS heartbeat_scripts (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, owner_kind TEXT NOT NULL,
			owner_hosted_thread_id TEXT NOT NULL DEFAULT '', owner_name TEXT NOT NULL DEFAULT '',
			current_revision INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS heartbeat_script_revisions (
			script_id TEXT NOT NULL, revision INTEGER NOT NULL, language TEXT NOT NULL DEFAULT 'javascript',
			source TEXT NOT NULL, source_sha256 TEXT NOT NULL, default_settings TEXT NOT NULL DEFAULT '{}',
			default_prompt TEXT NOT NULL DEFAULT '', template_id TEXT NOT NULL DEFAULT '',
			template_revision TEXT NOT NULL DEFAULT '', authority TEXT NOT NULL DEFAULT '',
			actor_kind TEXT NOT NULL, actor_hosted_thread_id TEXT NOT NULL DEFAULT '', actor_name TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, PRIMARY KEY(script_id,revision),
			FOREIGN KEY(script_id) REFERENCES heartbeat_scripts(id)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS heartbeat_wake_log_due_once
			ON heartbeat_wake_log(attachment_id, due_epoch_minute) WHERE due_epoch_minute IS NOT NULL;
	`); err != nil {
		return fmt.Errorf("migrate heartbeat database: %w", err)
	}
	for _, column := range []struct {
		table, name, definition string
	}{
		{"heartbeat_attachments", "condition_type", "condition_type TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_attachments", "pre_ping_script_id", "pre_ping_script_id TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_attachments", "pre_ping_script_revision", "pre_ping_script_revision INTEGER NOT NULL DEFAULT 0"},
		{"heartbeat_attachments", "pre_ping_settings", "pre_ping_settings TEXT NOT NULL DEFAULT '{}'"},
		{"heartbeat_attachments", "pre_ping_failure_script_id", "pre_ping_failure_script_id TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_attachments", "pre_ping_failure_revision", "pre_ping_failure_revision INTEGER NOT NULL DEFAULT 0"},
		{"heartbeat_attachments", "pre_ping_failure_at", "pre_ping_failure_at TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_attachments", "consecutive_wakes", "consecutive_wakes INTEGER NOT NULL DEFAULT 0"},
		{"heartbeat_attachments", "last_human_message_at", "last_human_message_at TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_attachments", "last_resume_at", "last_resume_at TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_attachments", "pre_ping_suspended", "pre_ping_suspended INTEGER NOT NULL DEFAULT 0"},
		{"heartbeat_wake_log", "condition_type", "condition_type TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_wake_log", "condition_result", "condition_result TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_wake_log", "pre_ping_script_id", "pre_ping_script_id TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_wake_log", "pre_ping_revision", "pre_ping_revision INTEGER NOT NULL DEFAULT 0"},
		{"heartbeat_wake_log", "pre_ping_result", "pre_ping_result TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_wake_log", "pre_ping_reason", "pre_ping_reason TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_wake_log", "pre_ping_duration_ms", "pre_ping_duration_ms INTEGER NOT NULL DEFAULT 0"},
		{"heartbeat_wake_log", "pre_ping_prompt_changed", "pre_ping_prompt_changed INTEGER NOT NULL DEFAULT 0"},
		{"heartbeat_wake_log", "pre_ping_failure_latched", "pre_ping_failure_latched INTEGER NOT NULL DEFAULT 0"},
		{"heartbeat_wake_log", "pre_ping_dedupe_key", "pre_ping_dedupe_key TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_wake_log", "usage_state", "usage_state TEXT NOT NULL DEFAULT ''"},
		{"heartbeat_wake_log", "usage_input_tokens", "usage_input_tokens INTEGER"},
		{"heartbeat_wake_log", "usage_cached_input_tokens", "usage_cached_input_tokens INTEGER"},
		{"heartbeat_wake_log", "usage_output_tokens", "usage_output_tokens INTEGER"},
	} {
		hasColumn, err := tableHasColumn(s.DB, column.table, column.name)
		if err != nil {
			return fmt.Errorf("inspect heartbeat condition column %s.%s: %w", column.table, column.name, err)
		}
		if !hasColumn {
			if _, err := s.DB.Exec(`ALTER TABLE ` + column.table + ` ADD COLUMN ` + column.definition); err != nil {
				return fmt.Errorf("add heartbeat condition column %s.%s: %w", column.table, column.name, err)
			}
		}
	}
	if err := s.retireClosedConditions(time.Now()); err != nil {
		return fmt.Errorf("retire closed heartbeat conditions: %w", err)
	}
	if err := s.migratePrePingTemplates(time.Now()); err != nil {
		return fmt.Errorf("migrate heartbeat pre-ping templates: %w", err)
	}
	if _, err := s.DB.Exec(`
		DROP TABLE IF EXISTS heartbeat_state;
		DROP TABLE IF EXISTS status_agents;
		DROP TABLE IF EXISTS heartbeat_status_events;
		DROP TABLE IF EXISTS status_judgments;
	`); err != nil {
		return fmt.Errorf("drop retired heartbeat status-agent tables: %w", err)
	}
	return nil
}

func (s *Store) retireClosedConditions(at time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := at.UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO heartbeat_attachment_events(attachment_id,event_type,actor_kind,actor_name,detail,occurred_at)
		SELECT id,'condition_retired','system','migration','closed condition removed; configure a repository pre-ping template',? FROM heartbeat_attachments WHERE condition_type!=''`, stamp); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE heartbeat_attachments SET
		state=CASE WHEN state='active' THEN 'paused' ELSE state END,
		pause_reason=CASE WHEN state='active' THEN 'closed condition removed; configure a repository pre-ping template' ELSE pause_reason END,
		pause_actor_kind=CASE WHEN state='active' THEN 'system' ELSE pause_actor_kind END,
		pause_actor_name=CASE WHEN state='active' THEN 'migration' ELSE pause_actor_name END,
		condition_type='',revision=revision+1,updated_at=? WHERE condition_type!=''`, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

// migratePrePingTemplates removes executable legacy-script references without
// deleting their audit rows. Each affected attachment receives its own copy of
// the repository inbox template. Unsupported legacy attachments remain paused
// and suppress even that template until an explicit resume.
func (s *Store) migratePrePingTemplates(at time.Time) error {
	template, err := ScriptTemplateByID(InboxTemplateID)
	if err != nil {
		return err
	}
	type attachedScript struct {
		attachmentID, state, prompt, settings, scriptID string
		scriptRevision                                  int64
		templateID                                      sql.NullString
	}
	rows, err := s.DB.Query(`SELECT a.id,a.state,a.prompt,a.pre_ping_settings,a.pre_ping_script_id,a.pre_ping_script_revision,r.template_id
		FROM heartbeat_attachments a LEFT JOIN heartbeat_script_revisions r
		ON r.script_id=a.pre_ping_script_id AND r.revision=a.pre_ping_script_revision
		WHERE a.state!='removed' AND a.pre_ping_script_id!=''
		ORDER BY a.pre_ping_script_id,a.pre_ping_script_revision,a.id`)
	if err != nil {
		return err
	}
	var attached []attachedScript
	for rows.Next() {
		var value attachedScript
		if err := rows.Scan(&value.attachmentID, &value.state, &value.prompt, &value.settings, &value.scriptID, &value.scriptRevision, &value.templateID); err != nil {
			rows.Close()
			return err
		}
		attached = append(attached, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	seen := make(map[string]int)
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := at.UTC().Format(time.RFC3339Nano)
	actor := Actor{Kind: "system", Name: "migration"}
	const authority = "retire legacy pre-ping attachments"
	for _, value := range attached {
		key := fmt.Sprintf("%s:%d", value.scriptID, value.scriptRevision)
		unsupported := !value.templateID.Valid || value.templateID.String != template.ID
		duplicate := seen[key] > 0
		seen[key]++
		if !unsupported && !duplicate {
			continue
		}
		settings, err := NormalizeJSONObject(json.RawMessage(value.settings), 16<<10)
		if err != nil {
			return fmt.Errorf("attachment %s settings: %w", value.attachmentID, err)
		}
		suffix, err := newHeartbeatID()
		if err != nil {
			return err
		}
		script := Script{ID: "hbs_" + suffix, Name: template.Name + " attachment copy", Owner: actor, CurrentRevision: 1, CreatedAt: stamp, UpdatedAt: stamp}
		revision := ScriptRevision{ScriptID: script.ID, Revision: 1, Language: template.Language, Source: template.Source,
			SourceSHA256: SourceSHA256(template.Source), DefaultSettings: settings, DefaultPrompt: value.prompt,
			TemplateID: template.ID, TemplateRevision: template.Revision, Authority: authority, Actor: actor, CreatedAt: stamp}
		if _, err := tx.Exec(`INSERT INTO heartbeat_scripts(id,name,owner_kind,owner_hosted_thread_id,owner_name,current_revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			script.ID, script.Name, script.Owner.Kind, script.Owner.HostedThreadID, script.Owner.Name, 1, stamp, stamp); err != nil {
			return err
		}
		if err := insertHeartbeatScriptRevisionTx(tx, revision); err != nil {
			return err
		}
		if unsupported {
			_, err = tx.Exec(`UPDATE heartbeat_attachments SET pre_ping_script_id=?,pre_ping_script_revision=1,
				pre_ping_failure_script_id='',pre_ping_failure_revision=0,pre_ping_failure_at='',pre_ping_suspended=1,
				state=CASE WHEN state='active' THEN 'paused' ELSE state END,
				pause_reason=CASE WHEN state='active' THEN 'legacy pre-ping retired; explicitly resume the repository inbox template' ELSE pause_reason END,
				pause_actor_kind=CASE WHEN state='active' THEN 'system' ELSE pause_actor_kind END,
				pause_actor_hosted_thread_id=CASE WHEN state='active' THEN '' ELSE pause_actor_hosted_thread_id END,
				pause_actor_name=CASE WHEN state='active' THEN 'migration' ELSE pause_actor_name END,
				revision=revision+1,updated_at=? WHERE id=?`, script.ID, stamp, value.attachmentID)
		} else {
			_, err = tx.Exec(`UPDATE heartbeat_attachments SET pre_ping_script_id=?,pre_ping_script_revision=1,
				pre_ping_failure_script_id='',pre_ping_failure_revision=0,pre_ping_failure_at='',
				revision=revision+1,updated_at=? WHERE id=?`, script.ID, stamp, value.attachmentID)
		}
		if err != nil {
			return err
		}
		event := "pre_ping_private_copy"
		detail := fmt.Sprintf("shared repository template copied for one attachment; old script %s revision %d", value.scriptID, value.scriptRevision)
		if unsupported {
			event = "legacy_pre_ping_retired"
			detail = fmt.Sprintf("legacy script %s revision %d retained as audit evidence; executable reference replaced by a suspended repository inbox template copy", value.scriptID, value.scriptRevision)
		}
		if err := appendHeartbeatAttachmentEvent(tx, value.attachmentID, event, actor, authority, detail, stamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func TableExists(db *sql.DB, table string) (bool, error) {
	var exists int
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&exists)
	return exists != 0, err
}

func tableHasColumn(db *sql.DB, table, column string) (bool, error) {
	if ok, err := TableExists(db, table); err != nil || !ok {
		return false, err
	}
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) migrateLegacyAttachments() error {
	legacy, err := tableHasColumn(s.DB, "heartbeat_attachments", "phase_tick")
	if err != nil || !legacy {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		CREATE TABLE heartbeat_attachments_v2 (
			id TEXT PRIMARY KEY, target_hosted_thread_id TEXT NOT NULL,
			cadence_minutes INTEGER NOT NULL CHECK(cadence_minutes > 0), offset_minutes INTEGER NOT NULL DEFAULT 0,
			prompt TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'active',
			pause_reason TEXT NOT NULL DEFAULT '', pause_actor_kind TEXT NOT NULL DEFAULT '',
			pause_actor_hosted_thread_id TEXT NOT NULL DEFAULT '', pause_actor_name TEXT NOT NULL DEFAULT '',
			agent_paused INTEGER NOT NULL DEFAULT 0, agent_pause_source_turn_id TEXT NOT NULL DEFAULT '',
			agent_pause_source_final_message_id TEXT NOT NULL DEFAULT '', agent_pause_at TEXT NOT NULL DEFAULT '',
			authority TEXT NOT NULL DEFAULT '',
			creator_kind TEXT NOT NULL, creator_hosted_thread_id TEXT NOT NULL DEFAULT '', creator_name TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, removed_at TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id,target_hosted_thread_id,cadence_minutes,phase_tick,prompt,enabled,held,hold_reason,authority,creator_kind,creator_hosted_thread_id,creator_name,revision,created_at,updated_at,removed_at FROM heartbeat_attachments`)
	if err != nil {
		return err
	}
	type legacyAttachment struct {
		id, target, prompt, holdReason, authority, creatorKind, creatorHosted, creatorName string
		createdAt, updatedAt, removedAt                                                    string
		cadence                                                                            int
		phaseTick                                                                          int64
		enabled, held                                                                      int
		revision                                                                           int64
	}
	var legacyRows []legacyAttachment
	for rows.Next() {
		var r legacyAttachment
		if err := rows.Scan(&r.id, &r.target, &r.cadence, &r.phaseTick, &r.prompt, &r.enabled, &r.held, &r.holdReason, &r.authority, &r.creatorKind, &r.creatorHosted, &r.creatorName, &r.revision, &r.createdAt, &r.updatedAt, &r.removedAt); err != nil {
			rows.Close()
			return err
		}
		legacyRows = append(legacyRows, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range legacyRows {
		cadence := r.cadence
		if cadence < 1 {
			cadence = 1
		}
		offset := int(((r.phaseTick % int64(cadence)) + int64(cadence)) % int64(cadence))
		state, pauseReason, pauseActorKind, pauseActorName := StateActive, "", "", ""
		switch {
		case r.removedAt != "":
			state = StateRemoved
		case r.held != 0:
			state, pauseReason, pauseActorKind, pauseActorName = StatePaused, r.holdReason, "unknown", "migrated manual hold"
		case r.enabled == 0:
			state, pauseReason, pauseActorKind, pauseActorName = StatePaused, "migrated disabled attachment", "system", "migration"
		}
		if _, err := tx.Exec(`INSERT INTO heartbeat_attachments_v2(id,target_hosted_thread_id,cadence_minutes,offset_minutes,prompt,state,pause_reason,pause_actor_kind,pause_actor_hosted_thread_id,pause_actor_name,agent_paused,agent_pause_source_turn_id,agent_pause_source_final_message_id,agent_pause_at,authority,creator_kind,creator_hosted_thread_id,creator_name,revision,created_at,updated_at,removed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,0,'','','',?,?,?,?,?,?,?,?)`,
			r.id, r.target, cadence, offset, r.prompt, state, pauseReason, pauseActorKind, "", pauseActorName,
			r.authority, r.creatorKind, r.creatorHosted, r.creatorName, r.revision, r.createdAt, r.updatedAt, r.removedAt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP TABLE heartbeat_attachments`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE heartbeat_attachments_v2 RENAME TO heartbeat_attachments`); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateLegacyWakeLog rebuilds the pre-redesign tick-sequence wake log into
// the v2 due-epoch-minute decision schema, retaining every legacy row with a
// null due_epoch_minute and its original tick sequence preserved for history.
func (s *Store) migrateLegacyWakeLog() error {
	legacy, err := tableHasColumn(s.DB, "heartbeat_wake_log", "tick_sequence")
	if err != nil || !legacy {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		CREATE TABLE heartbeat_wake_log_v2 (
			id INTEGER PRIMARY KEY AUTOINCREMENT, attachment_id TEXT NOT NULL, due_epoch_minute INTEGER,
			gate TEXT NOT NULL DEFAULT '', availability_state TEXT NOT NULL DEFAULT '',
			availability_reasons TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL,
			operation_id TEXT NOT NULL DEFAULT '', error_class TEXT NOT NULL DEFAULT '', occurred_at TEXT NOT NULL,
			legacy_tick_sequence INTEGER
		)`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT attachment_id,tick_sequence,availability_state,availability_reasons,outcome,operation_id,error_class,occurred_at FROM heartbeat_wake_log ORDER BY id`)
	if err != nil {
		return err
	}
	type legacyWake struct {
		attachmentID, availabilityState, availabilityReasons, outcome, operationID, errorClass, occurredAt string
		tick                                                                                               int64
	}
	var legacyRows []legacyWake
	for rows.Next() {
		var r legacyWake
		if err := rows.Scan(&r.attachmentID, &r.tick, &r.availabilityState, &r.availabilityReasons, &r.outcome, &r.operationID, &r.errorClass, &r.occurredAt); err != nil {
			rows.Close()
			return err
		}
		legacyRows = append(legacyRows, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range legacyRows {
		if _, err := tx.Exec(`INSERT INTO heartbeat_wake_log_v2(attachment_id,due_epoch_minute,gate,availability_state,availability_reasons,outcome,operation_id,error_class,occurred_at,legacy_tick_sequence) VALUES(?,NULL,?,?,?,?,?,?,?,?)`,
			r.attachmentID, legacyWakeGate(r.outcome, r.errorClass), r.availabilityState, r.availabilityReasons, r.outcome, r.operationID, r.errorClass, r.occurredAt, r.tick); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP TABLE heartbeat_wake_log`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE heartbeat_wake_log_v2 RENAME TO heartbeat_wake_log`); err != nil {
		return err
	}
	return tx.Commit()
}

func legacyWakeGate(outcome, errorClass string) string {
	switch {
	case outcome == "held" || outcome == "disabled":
		return GateManualPause
	case outcome == "skipped" && errorClass == "hard_facts":
		return GateHardFacts
	case outcome == "submitted" || outcome == "failed":
		return GateDelivery
	default:
		return GateLegacy
	}
}

func normalizeHeartbeatInput(input AttachmentInput) (AttachmentInput, error) {
	input.TargetHostedThreadID = strings.TrimSpace(input.TargetHostedThreadID)
	input.Prompt = strings.TrimSpace(input.Prompt)
	input.Actor.Kind = strings.TrimSpace(input.Actor.Kind)
	if input.TargetHostedThreadID == "" || input.CadenceMinutes < 1 || input.Prompt == "" || input.Actor.Kind == "" {
		return input, errors.New("targetHostedThreadId, positive cadenceMinutes, prompt, and actor.kind are required")
	}
	if input.OffsetMinutes < 0 {
		return input, errors.New("offsetMinutes must not be negative")
	}
	input.OffsetMinutes = input.OffsetMinutes % input.CadenceMinutes
	prePing, err := normalizeHeartbeatPrePing(input.PrePing)
	if err != nil {
		return input, err
	}
	input.PrePing = prePing
	return input, nil
}

func NormalizeJSONObject(raw json.RawMessage, max int) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if len(raw) > max {
		return nil, fmt.Errorf("JSON object exceeds %d bytes", max)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("value must be one JSON object")
	}
	canonical, err := json.Marshal(value)
	return canonical, err
}

func normalizeHeartbeatPrePing(value *PrePingReference) (*PrePingReference, error) {
	if value == nil {
		return nil, nil
	}
	normalized := &PrePingReference{ScriptID: strings.TrimSpace(value.ScriptID), Revision: value.Revision}
	if normalized.ScriptID == "" || normalized.Revision < 1 {
		return nil, errors.New("prePing requires scriptId and positive revision")
	}
	settings, err := NormalizeJSONObject(value.Settings, 16<<10)
	if err != nil {
		return nil, fmt.Errorf("invalid prePing settings: %w", err)
	}
	normalized.Settings = settings
	return normalized, nil
}

func (s *Store) CreateAttachment(input AttachmentInput, now time.Time) (Attachment, error) {
	input, err := normalizeHeartbeatInput(input)
	if err != nil {
		return Attachment{}, err
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	suffix, err := newHeartbeatID()
	if err != nil {
		return Attachment{}, err
	}
	rec := Attachment{ID: "hba_" + suffix, TargetHostedThreadID: input.TargetHostedThreadID,
		CadenceMinutes: input.CadenceMinutes, OffsetMinutes: input.OffsetMinutes, Prompt: input.Prompt,
		PrePing: input.PrePing,
		State:   StateActive, Authority: strings.TrimSpace(input.Authority),
		Creator: input.Actor, Revision: 1, CreatedAt: nowText, UpdatedAt: nowText}
	rec.EffectiveCadenceMinutes = EffectiveCadence(rec.CadenceMinutes, 0)
	tx, err := s.DB.Begin()
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback()
	if rec.PrePing != nil {
		if _, err := scriptRevisionTx(tx, rec.PrePing.ScriptID, rec.PrePing.Revision); err != nil {
			return Attachment{}, fmt.Errorf("prePing script revision: %w", err)
		}
	}
	prePingID, prePingRevision, prePingSettings := heartbeatPrePingColumns(rec.PrePing)
	_, err = tx.Exec(`INSERT INTO heartbeat_attachments(id,target_hosted_thread_id,cadence_minutes,offset_minutes,prompt,pre_ping_script_id,pre_ping_script_revision,pre_ping_settings,pre_ping_failure_script_id,pre_ping_failure_revision,pre_ping_failure_at,state,pause_reason,pause_actor_kind,pause_actor_hosted_thread_id,pause_actor_name,agent_paused,agent_pause_source_turn_id,agent_pause_source_final_message_id,agent_pause_at,authority,creator_kind,creator_hosted_thread_id,creator_name,revision,created_at,updated_at,removed_at)
		VALUES(?,?,?,?,?,?,?,?,'',0,'',?,?,?,?,?,0,'','','',?,?,?,?,?,?,?,?)`,
		rec.ID, rec.TargetHostedThreadID, rec.CadenceMinutes, rec.OffsetMinutes, rec.Prompt, prePingID, prePingRevision, prePingSettings, rec.State, "", "", "", "",
		rec.Authority, rec.Creator.Kind, rec.Creator.HostedThreadID, rec.Creator.Name, rec.Revision, rec.CreatedAt, rec.UpdatedAt, "")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Attachment{}, ErrAttachmentExists
		}
		return Attachment{}, err
	}
	if err := appendHeartbeatAttachmentEvent(tx, rec.ID, "created", input.Actor, rec.Authority, "", nowText); err != nil {
		return Attachment{}, err
	}
	return rec, tx.Commit()
}

func appendHeartbeatAttachmentEvent(tx *sql.Tx, attachmentID, eventType string, actor Actor, authority, detail, at string) error {
	_, err := tx.Exec(`INSERT INTO heartbeat_attachment_events(attachment_id,event_type,actor_kind,actor_hosted_thread_id,actor_name,authority,detail,occurred_at) VALUES(?,?,?,?,?,?,?,?)`, attachmentID, eventType, actor.Kind, actor.HostedThreadID, actor.Name, authority, detail, at)
	return err
}

func scanHeartbeatAttachment(scanner interface{ Scan(...any) error }) (Attachment, error) {
	var rec Attachment
	var retiredAgentPaused, prePingSuspended int
	var retiredAgentPauseSourceTurnID, retiredAgentPauseSourceFinalMessageID, retiredAgentPauseAt string
	var prePingScriptID, prePingSettings string
	var prePingRevision int64
	err := scanner.Scan(&rec.ID, &rec.TargetHostedThreadID, &rec.CadenceMinutes, &rec.OffsetMinutes, &rec.Prompt,
		&prePingScriptID, &prePingRevision, &prePingSettings, &rec.PrePingFailureScriptID, &rec.PrePingFailureRevision, &rec.PrePingFailureAt, &rec.State,
		&rec.PauseReason, &rec.PauseActor.Kind, &rec.PauseActor.HostedThreadID, &rec.PauseActor.Name,
		&retiredAgentPaused, &retiredAgentPauseSourceTurnID, &retiredAgentPauseSourceFinalMessageID, &retiredAgentPauseAt,
		&rec.Authority, &rec.Creator.Kind, &rec.Creator.HostedThreadID, &rec.Creator.Name,
		&rec.Revision, &rec.CreatedAt, &rec.UpdatedAt, &rec.RemovedAt,
		&rec.ConsecutiveWakes, &rec.LastHumanMessageAt, &rec.LastResumeAt, &prePingSuspended)
	_, _, _, _ = retiredAgentPaused, retiredAgentPauseSourceTurnID, retiredAgentPauseSourceFinalMessageID, retiredAgentPauseAt
	if prePingScriptID != "" {
		rec.PrePing = &PrePingReference{ScriptID: prePingScriptID, Revision: prePingRevision, Settings: json.RawMessage(prePingSettings)}
	}
	rec.PrePingSuspended = prePingSuspended != 0
	return rec, err
}

const heartbeatAttachmentColumns = `id,target_hosted_thread_id,cadence_minutes,offset_minutes,prompt,pre_ping_script_id,pre_ping_script_revision,pre_ping_settings,pre_ping_failure_script_id,pre_ping_failure_revision,pre_ping_failure_at,state,pause_reason,pause_actor_kind,pause_actor_hosted_thread_id,pause_actor_name,agent_paused,agent_pause_source_turn_id,agent_pause_source_final_message_id,agent_pause_at,authority,creator_kind,creator_hosted_thread_id,creator_name,revision,created_at,updated_at,removed_at,consecutive_wakes,last_human_message_at,last_resume_at,pre_ping_suspended`

func heartbeatPrePingColumns(value *PrePingReference) (string, int64, string) {
	if value == nil {
		return "", 0, "{}"
	}
	return value.ScriptID, value.Revision, string(value.Settings)
}

func EffectiveCadence(configured, consecutive int) int {
	floor := 1
	if consecutive >= 20 {
		floor = 5
	} else if consecutive >= 10 {
		floor = 2
	}
	if configured > floor {
		return configured
	}
	return floor
}

func (s *Store) enrichAttachment(rec *Attachment) error {
	rec.EffectiveCadenceMinutes = EffectiveCadence(rec.CadenceMinutes, rec.ConsecutiveWakes)
	return s.DB.QueryRow(`SELECT count(*),
		COALESCE(sum(usage_input_tokens),0),COALESCE(sum(usage_cached_input_tokens),0),COALESCE(sum(usage_output_tokens),0),
		COALESCE(sum(CASE WHEN usage_state=? THEN 1 ELSE 0 END),0),COALESCE(sum(CASE WHEN usage_state=? THEN 1 ELSE 0 END),0)
		FROM heartbeat_wake_log WHERE attachment_id=? AND operation_id!=''`,
		UsagePending, UsageUnavailable, rec.ID).Scan(
		&rec.Usage.WakeCount, &rec.Usage.InputTokens, &rec.Usage.CachedInputTokens, &rec.Usage.OutputTokens,
		&rec.Usage.PendingWakes, &rec.Usage.UnavailableWakes)
}

func (s *Store) Attachment(id string) (Attachment, error) {
	rec, err := scanHeartbeatAttachment(s.DB.QueryRow(`SELECT `+heartbeatAttachmentColumns+` FROM heartbeat_attachments WHERE id=? AND state!='removed'`, id))
	if err == nil {
		err = s.enrichAttachment(&rec)
	}
	return rec, err
}

func (s *Store) AttachmentForTarget(target string) (Attachment, error) {
	rec, err := scanHeartbeatAttachment(s.DB.QueryRow(`SELECT `+heartbeatAttachmentColumns+` FROM heartbeat_attachments WHERE target_hosted_thread_id=? AND state!='removed'`, target))
	if err == nil {
		err = s.enrichAttachment(&rec)
	}
	return rec, err
}

func (s *Store) ListAttachments(includeRemoved bool) ([]Attachment, error) {
	query := `SELECT ` + heartbeatAttachmentColumns + ` FROM heartbeat_attachments`
	if !includeRemoved {
		query += ` WHERE state!='removed'`
	}
	query += ` ORDER BY created_at,id`
	rows, err := s.DB.Query(query)
	if err != nil {
		return nil, err
	}
	out := []Attachment{}
	for rows.Next() {
		rec, err := scanHeartbeatAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.enrichAttachment(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) DueAttachments(epochMinute int64) ([]Attachment, error) {
	rows, err := s.ListAttachments(false)
	if err != nil {
		return nil, err
	}
	out := make([]Attachment, 0)
	for _, rec := range rows {
		if rec.CadenceMinutes <= 0 {
			continue
		}
		cadence := int64(rec.EffectiveCadenceMinutes)
		if ((epochMinute%cadence)+cadence)%cadence == int64(rec.OffsetMinutes) {
			out = append(out, rec)
		}
	}
	return out, nil
}

// mutateLifecycle applies one administrative lifecycle transition (pause,
// resume, or remove) and appends its audit event. It is the only path that
// changes State.
func (s *Store) MutateLifecycle(id, eventType, reason string, actor Actor, newState string, now time.Time) (Attachment, error) {
	reason = strings.TrimSpace(reason)
	if (newState == StatePaused || newState == StateActive) && reason == "" {
		return Attachment{}, errors.New("pause and resume require a reason")
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.Begin()
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback()
	rec, err := scanHeartbeatAttachment(tx.QueryRow(`SELECT `+heartbeatAttachmentColumns+` FROM heartbeat_attachments WHERE id=? AND state!='removed'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	rec.State = newState
	switch newState {
	case StatePaused:
		rec.PauseReason, rec.PauseActor = reason, actor
	case StateActive:
		rec.PauseReason, rec.PauseActor = "", Actor{}
		rec.PrePingFailureScriptID, rec.PrePingFailureRevision, rec.PrePingFailureAt = "", 0, ""
		rec.PrePingSuspended = false
		rec.ConsecutiveWakes, rec.LastResumeAt = 0, nowText
	case StateRemoved:
		rec.RemovedAt = nowText
	}
	rec.Revision++
	rec.UpdatedAt = nowText
	rec.EffectiveCadenceMinutes = EffectiveCadence(rec.CadenceMinutes, rec.ConsecutiveWakes)
	_, err = tx.Exec(`UPDATE heartbeat_attachments SET state=?,pause_reason=?,pause_actor_kind=?,pause_actor_hosted_thread_id=?,pause_actor_name=?,pre_ping_failure_script_id=?,pre_ping_failure_revision=?,pre_ping_failure_at=?,pre_ping_suspended=?,consecutive_wakes=?,last_resume_at=?,revision=?,updated_at=?,removed_at=? WHERE id=?`,
		rec.State, rec.PauseReason, rec.PauseActor.Kind, rec.PauseActor.HostedThreadID, rec.PauseActor.Name,
		rec.PrePingFailureScriptID, rec.PrePingFailureRevision, rec.PrePingFailureAt, boolInt(rec.PrePingSuspended), rec.ConsecutiveWakes, rec.LastResumeAt, rec.Revision, rec.UpdatedAt, rec.RemovedAt, rec.ID)
	if err != nil {
		return Attachment{}, err
	}
	if err := appendHeartbeatAttachmentEvent(tx, rec.ID, eventType, actor, rec.Authority, reason, nowText); err != nil {
		return Attachment{}, err
	}
	return rec, tx.Commit()
}

func (s *Store) PauseAttachment(id, reason string, actor Actor, now time.Time) (Attachment, error) {
	return s.MutateLifecycle(id, "paused", reason, actor, StatePaused, now)
}

func (s *Store) ResumeAttachment(id, reason string, actor Actor, now time.Time) (Attachment, error) {
	return s.MutateLifecycle(id, "resumed", reason, actor, StateActive, now)
}

func (s *Store) RemoveAttachment(id, reason string, actor Actor, now time.Time) (Attachment, error) {
	return s.MutateLifecycle(id, "removed", reason, actor, StateRemoved, now)
}

func (s *Store) UpdateAttachment(id string, patch AttachmentPatch, reason string, actor Actor, now time.Time) (Attachment, error) {
	nowText := now.UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.Begin()
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback()
	rec, err := scanHeartbeatAttachment(tx.QueryRow(`SELECT `+heartbeatAttachmentColumns+` FROM heartbeat_attachments WHERE id=? AND state!='removed'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	changed := false
	if patch.CadenceMinutes != nil {
		rec.CadenceMinutes, changed = *patch.CadenceMinutes, true
	}
	if patch.OffsetMinutes != nil {
		rec.OffsetMinutes, changed = *patch.OffsetMinutes, true
	}
	if patch.Prompt != nil {
		rec.Prompt, changed = strings.TrimSpace(*patch.Prompt), true
	}
	if patch.PrePingSet {
		rec.PrePing, changed = patch.PrePing, true
	}
	if !changed {
		return Attachment{}, errors.New("heartbeat patch contains no configurable fields")
	}
	if rec.CadenceMinutes < 1 {
		return Attachment{}, errors.New("cadenceMinutes must be positive")
	}
	if rec.OffsetMinutes < 0 {
		return Attachment{}, errors.New("offsetMinutes must not be negative")
	}
	rec.OffsetMinutes = rec.OffsetMinutes % rec.CadenceMinutes
	if rec.Prompt == "" {
		return Attachment{}, errors.New("prompt must not be empty")
	}
	prePing, err := normalizeHeartbeatPrePing(rec.PrePing)
	if err != nil {
		return Attachment{}, err
	}
	rec.PrePing = prePing
	if rec.PrePing != nil {
		if _, err := scriptRevisionTx(tx, rec.PrePing.ScriptID, rec.PrePing.Revision); err != nil {
			return Attachment{}, fmt.Errorf("prePing script revision: %w", err)
		}
	}
	rec.Revision++
	rec.UpdatedAt = nowText
	rec.EffectiveCadenceMinutes = EffectiveCadence(rec.CadenceMinutes, rec.ConsecutiveWakes)
	prePingID, prePingRevision, prePingSettings := heartbeatPrePingColumns(rec.PrePing)
	rec.PrePingFailureScriptID, rec.PrePingFailureRevision, rec.PrePingFailureAt = "", 0, ""
	if rec.State == StatePaused && rec.PauseActor.Kind == "system" && rec.PauseActor.Name == "pre-ping" {
		rec.State, rec.PauseReason, rec.PauseActor = StateActive, "", Actor{}
	}
	_, err = tx.Exec(`UPDATE heartbeat_attachments SET cadence_minutes=?,offset_minutes=?,prompt=?,pre_ping_script_id=?,pre_ping_script_revision=?,pre_ping_settings=?,pre_ping_failure_script_id='',pre_ping_failure_revision=0,pre_ping_failure_at='',state=?,pause_reason=?,pause_actor_kind=?,pause_actor_hosted_thread_id=?,pause_actor_name=?,revision=?,updated_at=? WHERE id=?`,
		rec.CadenceMinutes, rec.OffsetMinutes, rec.Prompt, prePingID, prePingRevision, prePingSettings,
		rec.State, rec.PauseReason, rec.PauseActor.Kind, rec.PauseActor.HostedThreadID, rec.PauseActor.Name, rec.Revision, rec.UpdatedAt, rec.ID)
	if err != nil {
		return Attachment{}, err
	}
	if err := appendHeartbeatAttachmentEvent(tx, rec.ID, "updated", actor, rec.Authority, strings.TrimSpace(reason), nowText); err != nil {
		return Attachment{}, err
	}
	return rec, tx.Commit()
}

// latchPrePingFailure pauses an active attachment before Nano attempts the
// single diagnostic wake. An already-paused attachment keeps its human reason
// and actor but still latches the broken revision so it cannot run repeatedly.
// An attachment update or explicit resume clears the latch.
func (s *Store) LatchPrePingFailure(id string, expectedAttachmentRevision int64, scriptID string, scriptRevision int64, reason string, now time.Time) (Attachment, bool, error) {
	nowText := now.UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.Begin()
	if err != nil {
		return Attachment{}, false, err
	}
	defer tx.Rollback()
	rec, err := scanHeartbeatAttachment(tx.QueryRow(`SELECT `+heartbeatAttachmentColumns+` FROM heartbeat_attachments WHERE id=? AND state!='removed'`, id))
	if err != nil {
		return Attachment{}, false, err
	}
	if rec.Revision != expectedAttachmentRevision || rec.PrePing == nil || rec.PrePing.ScriptID != scriptID || rec.PrePing.Revision != scriptRevision {
		return rec, false, nil
	}
	if rec.State != StatePaused {
		rec.State = StatePaused
		rec.PauseReason = "pre-ping script failed: " + strings.TrimSpace(reason)
		rec.PauseActor = Actor{Kind: "system", Name: "pre-ping"}
	}
	rec.PrePingFailureScriptID, rec.PrePingFailureRevision, rec.PrePingFailureAt = scriptID, scriptRevision, nowText
	rec.Revision++
	rec.UpdatedAt = nowText
	result, err := tx.Exec(`UPDATE heartbeat_attachments SET state=?,pause_reason=?,pause_actor_kind=?,pause_actor_name=?,pre_ping_failure_script_id=?,pre_ping_failure_revision=?,pre_ping_failure_at=?,revision=?,updated_at=? WHERE id=? AND revision=? AND pre_ping_script_id=? AND pre_ping_script_revision=?`,
		rec.State, rec.PauseReason, rec.PauseActor.Kind, rec.PauseActor.Name, scriptID, scriptRevision, nowText, rec.Revision, nowText, id, expectedAttachmentRevision, scriptID, scriptRevision)
	if err != nil {
		return Attachment{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Attachment{}, false, err
	}
	if changed == 0 {
		return rec, false, nil
	}
	if err := appendHeartbeatAttachmentEvent(tx, id, "pre_ping_failed", rec.PauseActor, rec.Authority, strings.TrimSpace(reason), nowText); err != nil {
		return Attachment{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Attachment{}, false, err
	}
	return rec, true, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// claimWakeDecision durably claims the single decision row for this
// attachment's due minute. ok=false means another evaluation (a duplicate
// tick, or a re-evaluation after restart) already claimed and the caller must
// not evaluate gates or delivery again.
func (s *Store) ClaimWakeDecision(attachmentID string, dueEpochMinute int64, at time.Time) (int64, bool, error) {
	res, err := s.DB.Exec(`INSERT OR IGNORE INTO heartbeat_wake_log(attachment_id,due_epoch_minute,gate,availability_state,availability_reasons,outcome,operation_id,error_class,occurred_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		attachmentID, dueEpochMinute, "", "", "", "claimed", "", "", at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, false, err
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		return 0, false, nil
	}
	var id int64
	if err := s.DB.QueryRow(`SELECT id FROM heartbeat_wake_log WHERE attachment_id=? AND due_epoch_minute=?`, attachmentID, dueEpochMinute).Scan(&id); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (s *Store) SettleWakeDecision(id int64, gate, availabilityState string, reasons []string, outcome, operationID, errorClass string, at time.Time, audits ...PrePingAudit) error {
	usageState := ""
	if operationID != "" {
		usageState = UsagePending
	}
	var audit PrePingAudit
	if len(audits) > 0 {
		audit = audits[0]
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE heartbeat_wake_log SET gate=?,availability_state=?,availability_reasons=?,outcome=?,operation_id=?,error_class=?,pre_ping_script_id=?,pre_ping_revision=?,pre_ping_result=?,pre_ping_reason=?,pre_ping_duration_ms=?,pre_ping_prompt_changed=?,pre_ping_failure_latched=?,pre_ping_dedupe_key=?,usage_state=?,occurred_at=? WHERE id=? AND outcome='claimed'`,
		gate, availabilityState, strings.Join(reasons, ","), outcome, operationID, errorClass,
		audit.ScriptID, audit.Revision, audit.Result, audit.Reason, audit.DurationMS, boolInt(audit.PromptChanged), boolInt(audit.FailureLatched), audit.DedupeKey, usageState, at.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed > 0 && operationID != "" {
		if _, err := tx.Exec(`UPDATE heartbeat_attachments SET consecutive_wakes=consecutive_wakes+1 WHERE id=(SELECT attachment_id FROM heartbeat_wake_log WHERE id=?)`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ObserveHumanMessage(attachmentID, observed string, at time.Time) error {
	if strings.TrimSpace(observed) == "" {
		return nil
	}
	when, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil {
		return fmt.Errorf("invalid lifecycle human-message time: %w", err)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stored string
	if err := tx.QueryRow(`SELECT last_human_message_at FROM heartbeat_attachments WHERE id=?`, attachmentID).Scan(&stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	// Compared as times: RFC3339Nano trims trailing zeros, so as text ".2374Z"
	// sorts after the later ".237401Z".
	if stored != "" {
		previous, err := time.Parse(time.RFC3339Nano, stored)
		if err != nil {
			return fmt.Errorf("invalid stored human-message time %q: %w", stored, err)
		}
		if !previous.Before(when) {
			return nil
		}
	}
	if _, err := tx.Exec(`UPDATE heartbeat_attachments SET last_human_message_at=?,consecutive_wakes=0,updated_at=? WHERE id=?`,
		when.UTC().Format(time.RFC3339Nano), at.UTC().Format(time.RFC3339Nano), attachmentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) HasWakeCapacity(since time.Time, maximum int) (bool, error) {
	if maximum < 1 {
		return false, errors.New("heartbeat wake cap must be positive")
	}
	var count int
	err := s.DB.QueryRow(`SELECT count(*) FROM heartbeat_wake_log WHERE operation_id!='' AND occurred_at>=?`, since.UTC().Format(time.RFC3339Nano)).Scan(&count)
	return count < maximum, err
}

func (s *Store) PruneDecisionLog(before time.Time) (int64, error) {
	result, err := s.DB.Exec(`DELETE FROM heartbeat_wake_log WHERE occurred_at<? AND outcome NOT IN (?,?)`,
		before.UTC().Format(time.RFC3339Nano), OutcomeDelivered, OutcomeFailed)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) PendingUsageTargets() ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT a.target_hosted_thread_id FROM heartbeat_wake_log w JOIN heartbeat_attachments a ON a.id=w.attachment_id WHERE w.operation_id!='' AND w.usage_state=? ORDER BY a.target_hosted_thread_id`, UsagePending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (s *Store) RecordWakeUsage(operationID string, usage *interact.TokenUsage) error {
	state := UsageUnavailable
	var input, cached, output any
	if usage != nil {
		state = UsageReported
		if usage.InputTokens != nil {
			input = *usage.InputTokens
		}
		if usage.CachedInputTokens != nil {
			cached = *usage.CachedInputTokens
		}
		if usage.OutputTokens != nil {
			output = *usage.OutputTokens
		}
	}
	_, err := s.DB.Exec(`UPDATE heartbeat_wake_log SET usage_state=?,usage_input_tokens=?,usage_cached_input_tokens=?,usage_output_tokens=? WHERE operation_id=? AND usage_state=?`,
		state, input, cached, output, operationID, UsagePending)
	return err
}

func (s *Store) LastDeliveredDedupeKey(attachmentID string) (string, error) {
	var key string
	err := s.DB.QueryRow(`SELECT pre_ping_dedupe_key FROM heartbeat_wake_log WHERE attachment_id=? AND outcome=? AND pre_ping_dedupe_key!='' ORDER BY id DESC LIMIT 1`,
		attachmentID, OutcomeDelivered).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return key, err
}

func (s *Store) ListWakeLog(attachmentID string, limit int) ([]WakeDecision, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	rows, err := s.DB.Query(`SELECT id,attachment_id,due_epoch_minute,gate,availability_state,availability_reasons,outcome,operation_id,error_class,pre_ping_script_id,pre_ping_revision,pre_ping_result,pre_ping_reason,pre_ping_duration_ms,pre_ping_prompt_changed,pre_ping_failure_latched,pre_ping_dedupe_key,occurred_at,legacy_tick_sequence,usage_state,usage_input_tokens,usage_cached_input_tokens,usage_output_tokens FROM heartbeat_wake_log WHERE (?='' OR attachment_id=?) ORDER BY id DESC LIMIT ?`, attachmentID, attachmentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []WakeDecision{}
	for rows.Next() {
		var item WakeDecision
		var dueEpochMinute, legacyTick sql.NullInt64
		var input, cached, output sql.NullInt64
		var reasons string
		var promptChanged, failureLatched int
		if err := rows.Scan(&item.ID, &item.AttachmentID, &dueEpochMinute, &item.Gate, &item.AvailabilityState, &reasons, &item.Outcome, &item.OperationID, &item.ErrorClass,
			&item.PrePingScriptID, &item.PrePingRevision, &item.PrePingResult, &item.PrePingReason, &item.PrePingDurationMS, &promptChanged, &failureLatched, &item.PrePingDedupeKey, &item.OccurredAt, &legacyTick,
			&item.UsageState, &input, &cached, &output); err != nil {
			return nil, err
		}
		item.PrePingPromptChanged, item.PrePingFailureLatched = promptChanged != 0, failureLatched != 0
		if dueEpochMinute.Valid {
			item.DueEpochMinute = &dueEpochMinute.Int64
		}
		if legacyTick.Valid {
			item.LegacyTickSequence = &legacyTick.Int64
		}
		if item.UsageState == UsageReported {
			item.Usage = &interact.TokenUsage{}
			if input.Valid {
				value := input.Int64
				item.Usage.InputTokens = &value
			}
			if cached.Valid {
				value := cached.Int64
				item.Usage.CachedInputTokens = &value
			}
			if output.Valid {
				value := output.Int64
				item.Usage.OutputTokens = &value
			}
		}
		if reasons != "" {
			item.AvailabilityReasons = strings.FieldsFunc(reasons, func(r rune) bool { return r == ',' })
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func normalizeHeartbeatScriptInput(input ScriptRevisionInput) (ScriptRevisionInput, error) {
	input.Name, input.Source = strings.TrimSpace(input.Name), strings.TrimSpace(input.Source)
	input.TemplateID, input.TemplateRevision = strings.TrimSpace(input.TemplateID), strings.TrimSpace(input.TemplateRevision)
	input.Authority = strings.TrimSpace(input.Authority)
	if input.Name == "" || len(input.Name) > 120 {
		return input, errors.New("script name must be 1-120 characters")
	}
	if input.Source == "" || len(input.Source) > 64<<10 {
		return input, errors.New("script source must be 1-65536 bytes")
	}
	if len(input.DefaultPrompt) > 16<<10 {
		return input, errors.New("defaultPrompt exceeds 16384 bytes")
	}
	settings, err := NormalizeJSONObject(input.DefaultSettings, 16<<10)
	if err != nil {
		return input, fmt.Errorf("invalid defaultSettings: %w", err)
	}
	input.DefaultSettings = settings
	return input, nil
}

func SourceSHA256(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

func newHeartbeatID() (string, error) {
	var value [4]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Store) CreateScript(input ScriptRevisionInput, now time.Time) (Script, ScriptRevision, error) {
	input, err := normalizeHeartbeatScriptInput(input)
	if err != nil {
		return Script{}, ScriptRevision{}, err
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	suffix, err := newHeartbeatID()
	if err != nil {
		return Script{}, ScriptRevision{}, err
	}
	script := Script{ID: "hbs_" + suffix, Name: input.Name, Owner: input.Actor, CurrentRevision: 1, CreatedAt: stamp, UpdatedAt: stamp}
	revision := ScriptRevision{ScriptID: script.ID, Revision: 1, Language: "javascript", Source: input.Source,
		SourceSHA256: SourceSHA256(input.Source), DefaultSettings: input.DefaultSettings, DefaultPrompt: input.DefaultPrompt,
		TemplateID: input.TemplateID, TemplateRevision: input.TemplateRevision, Authority: input.Authority, Actor: input.Actor, CreatedAt: stamp}
	tx, err := s.DB.Begin()
	if err != nil {
		return Script{}, ScriptRevision{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO heartbeat_scripts(id,name,owner_kind,owner_hosted_thread_id,owner_name,current_revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		script.ID, script.Name, script.Owner.Kind, script.Owner.HostedThreadID, script.Owner.Name, 1, stamp, stamp); err != nil {
		return Script{}, ScriptRevision{}, err
	}
	if err := insertHeartbeatScriptRevisionTx(tx, revision); err != nil {
		return Script{}, ScriptRevision{}, err
	}
	return script, revision, tx.Commit()
}

func insertHeartbeatScriptRevisionTx(tx *sql.Tx, revision ScriptRevision) error {
	_, err := tx.Exec(`INSERT INTO heartbeat_script_revisions(script_id,revision,language,source,source_sha256,default_settings,default_prompt,template_id,template_revision,authority,actor_kind,actor_hosted_thread_id,actor_name,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		revision.ScriptID, revision.Revision, revision.Language, revision.Source, revision.SourceSHA256, string(revision.DefaultSettings), revision.DefaultPrompt,
		revision.TemplateID, revision.TemplateRevision, revision.Authority, revision.Actor.Kind, revision.Actor.HostedThreadID, revision.Actor.Name, revision.CreatedAt)
	return err
}

func scanHeartbeatScript(scanner sqlScanner) (Script, error) {
	var value Script
	err := scanner.Scan(&value.ID, &value.Name, &value.Owner.Kind, &value.Owner.HostedThreadID, &value.Owner.Name, &value.CurrentRevision, &value.CreatedAt, &value.UpdatedAt)
	return value, err
}

func scriptRevisionTx(tx *sql.Tx, scriptID string, revision int64) (ScriptRevision, error) {
	var value ScriptRevision
	var settings string
	err := tx.QueryRow(`SELECT script_id,revision,language,source,source_sha256,default_settings,default_prompt,template_id,template_revision,authority,actor_kind,actor_hosted_thread_id,actor_name,created_at FROM heartbeat_script_revisions WHERE script_id=? AND revision=?`, scriptID, revision).
		Scan(&value.ScriptID, &value.Revision, &value.Language, &value.Source, &value.SourceSHA256, &settings, &value.DefaultPrompt, &value.TemplateID, &value.TemplateRevision, &value.Authority, &value.Actor.Kind, &value.Actor.HostedThreadID, &value.Actor.Name, &value.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrScriptNotFound
	}
	value.DefaultSettings = json.RawMessage(settings)
	return value, err
}

func (s *Store) ScriptRevision(scriptID string, revision int64) (ScriptRevision, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return ScriptRevision{}, err
	}
	defer tx.Rollback()
	value, err := scriptRevisionTx(tx, scriptID, revision)
	return value, err
}

func (s *Store) Script(scriptID string) (Script, error) {
	value, err := scanHeartbeatScript(s.DB.QueryRow(`SELECT id,name,owner_kind,owner_hosted_thread_id,owner_name,current_revision,created_at,updated_at FROM heartbeat_scripts WHERE id=?`, scriptID))
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrScriptNotFound
	}
	return value, err
}

func (s *Store) ListScripts() ([]Script, error) {
	rows, err := s.DB.Query(`SELECT id,name,owner_kind,owner_hosted_thread_id,owner_name,current_revision,created_at,updated_at FROM heartbeat_scripts ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Script{}
	for rows.Next() {
		value, err := scanHeartbeatScript(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, value)
	}
	return items, rows.Err()
}
