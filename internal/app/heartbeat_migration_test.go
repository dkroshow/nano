package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/interact"
)

// buildLegacyHeartbeatDB hand-builds a legacy heartbeat.sqlite:
// phase_tick/enabled/held attachments, a tick_sequence wake log, and the
// retired status-agent tables (heartbeat_state, status_agents,
// heartbeat_status_events, status_judgments).
func buildLegacyHeartbeatDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/heartbeat.sqlite"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE heartbeat_state (id INTEGER PRIMARY KEY CHECK (id=1), tick_sequence INTEGER NOT NULL DEFAULT 0, last_tick_at TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO heartbeat_state(id,tick_sequence) VALUES(1,900)`,
		`CREATE TABLE heartbeat_attachments (
			id TEXT PRIMARY KEY, target_hosted_thread_id TEXT NOT NULL,
			cadence_minutes INTEGER NOT NULL, phase_tick INTEGER NOT NULL,
			prompt TEXT NOT NULL, override_prompt TEXT NOT NULL DEFAULT '', tag_policy TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1, held INTEGER NOT NULL DEFAULT 0, hold_reason TEXT NOT NULL DEFAULT '',
			status_provider TEXT NOT NULL, status_model TEXT NOT NULL, status_effort TEXT NOT NULL,
			context_threshold INTEGER NOT NULL DEFAULT 0, authority TEXT NOT NULL DEFAULT '',
			creator_kind TEXT NOT NULL, creator_hosted_thread_id TEXT NOT NULL DEFAULT '', creator_name TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, removed_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE heartbeat_attachment_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, attachment_id TEXT NOT NULL, event_type TEXT NOT NULL,
			actor_kind TEXT NOT NULL, actor_hosted_thread_id TEXT NOT NULL DEFAULT '', actor_name TEXT NOT NULL DEFAULT '',
			authority TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE status_agents (target_hosted_thread_id TEXT PRIMARY KEY, child_hosted_thread_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL, model TEXT NOT NULL, effort TEXT NOT NULL, lifecycle TEXT NOT NULL, generation INTEGER NOT NULL DEFAULT 1, retry_count INTEGER NOT NULL DEFAULT 0, next_retry_at TEXT NOT NULL DEFAULT '', carry_summary_hash TEXT NOT NULL DEFAULT '', input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, retired_at TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE heartbeat_status_events (id INTEGER PRIMARY KEY AUTOINCREMENT, target_hosted_thread_id TEXT NOT NULL, provider TEXT NOT NULL, turn_id TEXT NOT NULL, final_message_id TEXT NOT NULL, completed_at TEXT NOT NULL, source TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE status_judgments (id INTEGER PRIMARY KEY AUTOINCREMENT, target_hosted_thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, final_message_id TEXT NOT NULL, tag TEXT NOT NULL, reason TEXT NOT NULL, classifier_provider TEXT NOT NULL, classifier_model TEXT NOT NULL, classifier_effort TEXT NOT NULL, prompt_version TEXT NOT NULL, failure_state TEXT NOT NULL DEFAULT '', input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL)`,
		`CREATE TABLE heartbeat_wake_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT, attachment_id TEXT NOT NULL, tick_sequence INTEGER NOT NULL,
			availability_state TEXT NOT NULL, availability_reasons TEXT NOT NULL DEFAULT '', judgment_id INTEGER,
			tag TEXT NOT NULL DEFAULT '', override_used INTEGER NOT NULL DEFAULT 0, outcome TEXT NOT NULL,
			operation_id TEXT NOT NULL DEFAULT '', error_class TEXT NOT NULL DEFAULT '', occurred_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("legacy schema: %v (%s)", err, stmt)
		}
	}
	inserts := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO heartbeat_attachments(id,target_hosted_thread_id,cadence_minutes,phase_tick,prompt,override_prompt,tag_policy,enabled,held,hold_reason,status_provider,status_model,status_effort,context_threshold,authority,creator_kind,creator_hosted_thread_id,creator_name,revision,created_at,updated_at,removed_at)
			VALUES('hba_active','nht_active',5,902,'Keep going.','','safe-only',1,0,'','codex','gpt-5.6-terra','low',100000,'Operator request','operator','','Operator',2,'2026-08-20T00:00:00Z','2026-08-21T00:00:00Z','')`, nil},
		{`INSERT INTO heartbeat_attachments(id,target_hosted_thread_id,cadence_minutes,phase_tick,prompt,override_prompt,tag_policy,enabled,held,hold_reason,status_provider,status_model,status_effort,context_threshold,authority,creator_kind,creator_hosted_thread_id,creator_name,revision,created_at,updated_at,removed_at)
			VALUES('hba_held','nht_held',10,0,'Nudge please.','','safe-only',1,1,'Operator is reviewing.','codex','gpt-5.6-terra','low',100000,'','operator','','Operator',3,'2026-08-19T00:00:00Z','2026-08-22T00:00:00Z','')`, nil},
		{`INSERT INTO heartbeat_attachments(id,target_hosted_thread_id,cadence_minutes,phase_tick,prompt,override_prompt,tag_policy,enabled,held,hold_reason,status_provider,status_model,status_effort,context_threshold,authority,creator_kind,creator_hosted_thread_id,creator_name,revision,created_at,updated_at,removed_at)
			VALUES('hba_disabled','nht_disabled',3,0,'Nudge please.','','safe-only',0,0,'','codex','gpt-5.6-terra','low',100000,'','operator','','Operator',2,'2026-08-19T00:00:00Z','2026-08-22T00:00:00Z','')`, nil},
		{`INSERT INTO heartbeat_attachments(id,target_hosted_thread_id,cadence_minutes,phase_tick,prompt,override_prompt,tag_policy,enabled,held,hold_reason,status_provider,status_model,status_effort,context_threshold,authority,creator_kind,creator_hosted_thread_id,creator_name,revision,created_at,updated_at,removed_at)
			VALUES('hba_removed','nht_removed',5,0,'Done now.','','safe-only',0,0,'','codex','gpt-5.6-terra','low',100000,'','operator','','Operator',4,'2026-08-01T00:00:00Z','2026-08-02T00:00:00Z','2026-08-02T00:00:00Z')`, nil},
		{`INSERT INTO heartbeat_attachment_events(attachment_id,event_type,actor_kind,actor_hosted_thread_id,actor_name,authority,detail,occurred_at) VALUES('hba_active','created','operator','','Operator','Operator request','','2026-08-20T00:00:00Z')`, nil},
		{`INSERT INTO heartbeat_attachment_events(attachment_id,event_type,actor_kind,actor_hosted_thread_id,actor_name,authority,detail,occurred_at) VALUES('hba_held','held','operator','','Operator','','Operator is reviewing.','2026-08-22T00:00:00Z')`, nil},
		{`INSERT INTO heartbeat_wake_log(attachment_id,tick_sequence,availability_state,availability_reasons,judgment_id,tag,override_used,outcome,operation_id,error_class,occurred_at) VALUES('hba_active',900,'available','',NULL,'',0,'submitted','aop_legacy_1','','2026-08-21T00:00:00Z')`, nil},
		{`INSERT INTO heartbeat_wake_log(attachment_id,tick_sequence,availability_state,availability_reasons,judgment_id,tag,override_used,outcome,operation_id,error_class,occurred_at) VALUES('hba_held',905,'','',NULL,'',0,'held','','','2026-08-22T00:05:00Z')`, nil},
		{`INSERT INTO heartbeat_wake_log(attachment_id,tick_sequence,availability_state,availability_reasons,judgment_id,tag,override_used,outcome,operation_id,error_class,occurred_at) VALUES('hba_active',895,'busy','turn_active',NULL,'',0,'skipped','','hard_facts','2026-08-20T12:00:00Z')`, nil},
	}
	for _, ins := range inserts {
		if _, err := db.Exec(ins.query, ins.args...); err != nil {
			t.Fatalf("legacy data: %v", err)
		}
	}
	return path
}

func TestMigrationRebuildsAttachmentsAndDropsStatusAgentTables(t *testing.T) {
	path := buildLegacyHeartbeatDB(t)
	store, err := openHeartbeatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backups, err := filepath.Glob(path + ".before-stage6-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("pre-migration backups = %v, %v", backups, err)
	}
	backup, err := sql.Open("sqlite", "file:"+backups[0]+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var legacyColumns int
	if err := backup.QueryRow(`SELECT count(*) FROM pragma_table_info('heartbeat_attachments') WHERE name='phase_tick'`).Scan(&legacyColumns); err != nil || legacyColumns != 1 {
		t.Fatalf("backup lost legacy schema: columns=%d err=%v", legacyColumns, err)
	}

	active, err := store.Attachment("hba_active")
	if err != nil {
		t.Fatal(err)
	}
	// phase_tick=902, cadence=5 -> offset = 902 % 5 = 2.
	if active.State != heartbeatStateActive || active.OffsetMinutes != 2 || active.CadenceMinutes != 5 ||
		active.Prompt != "Keep going." || active.Authority != "Operator request" || active.Creator.Name != "Operator" || active.Revision != 2 {
		t.Fatalf("migrated active attachment = %#v", active)
	}

	held, err := store.Attachment("hba_held")
	if err != nil {
		t.Fatal(err)
	}
	if held.State != heartbeatStatePaused || held.PauseReason != "Operator is reviewing." || held.CadenceMinutes != 10 {
		t.Fatalf("migrated held attachment = %#v", held)
	}

	disabled, err := store.Attachment("hba_disabled")
	if err != nil {
		t.Fatal(err)
	}
	if disabled.State != heartbeatStatePaused || disabled.PauseReason == "" {
		t.Fatalf("migrated disabled attachment = %#v", disabled)
	}

	all, err := store.ListAttachments(true)
	if err != nil || len(all) != 4 {
		t.Fatalf("historical attachment count = %#v err=%v", all, err)
	}
	var removedFound bool
	for _, rec := range all {
		if rec.ID == "hba_removed" {
			removedFound = true
			if rec.State != heartbeatStateRemoved || rec.RemovedAt == "" {
				t.Fatalf("migrated removed attachment = %#v", rec)
			}
		}
	}
	if !removedFound {
		t.Fatal("removed legacy attachment lost on migration")
	}

	var attachmentEvents int
	if err := store.DB.QueryRow(`SELECT count(*) FROM heartbeat_attachment_events`).Scan(&attachmentEvents); err != nil || attachmentEvents != 2 {
		t.Fatalf("attachment audit events survived migration = %d err=%v", attachmentEvents, err)
	}

	wakeRows, err := store.ListWakeLog("", 10)
	if err != nil || len(wakeRows) != 3 {
		t.Fatalf("migrated wake log = %#v err=%v", wakeRows, err)
	}
	byTick := map[int64]heartbeatWakeDecision{}
	for _, row := range wakeRows {
		if row.DueEpochMinute != nil {
			t.Fatalf("migrated legacy wake row has a due_epoch_minute: %#v", row)
		}
		if row.LegacyTickSequence == nil {
			t.Fatalf("migrated legacy wake row lost its tick sequence: %#v", row)
		}
		byTick[*row.LegacyTickSequence] = row
	}
	if row := byTick[900]; row.Gate != heartbeatGateDelivery || row.Outcome != "submitted" || row.OperationID != "aop_legacy_1" {
		t.Fatalf("migrated delivered wake row = %#v", row)
	}
	if row := byTick[905]; row.Gate != heartbeatGateManualPause || row.Outcome != "held" {
		t.Fatalf("migrated held wake row = %#v", row)
	}
	if row := byTick[895]; row.Gate != heartbeatGateHardFacts || row.Outcome != "skipped" {
		t.Fatalf("migrated hard-facts wake row = %#v", row)
	}

	for _, table := range []string{"heartbeat_state", "status_agents", "heartbeat_status_events", "status_judgments"} {
		exists, err := tableExists(store.DB, table)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("retired table %q survived migration", table)
		}
	}

	// Migration is idempotent: reopening an already-migrated database must
	// not error or re-migrate.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openHeartbeatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reloaded, err := store.Attachment("hba_active")
	if err != nil || reloaded.OffsetMinutes != 2 {
		t.Fatalf("re-migration changed attachment = %#v err=%v", reloaded, err)
	}
	if backups, _ := filepath.Glob(path + ".before-stage6-*"); len(backups) != 1 {
		t.Fatalf("reopen created another migration backup: %v", backups)
	}
}

func TestMigrationOnFreshDatabaseCreatesV2SchemaDirectly(t *testing.T) {
	store := openTestHeartbeatStore(t)
	rec, err := store.CreateAttachment(testHeartbeatInput("nht_fresh"), time.Now())
	if err != nil || rec.State != heartbeatStateActive || rec.OffsetMinutes != 0 {
		t.Fatalf("fresh attachment = %#v err=%v", rec, err)
	}
	for _, table := range []string{"heartbeat_state", "status_agents", "heartbeat_status_events", "status_judgments"} {
		exists, err := tableExists(store.DB, table)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("retired table %q created on a fresh database", table)
		}
	}
}

func TestHeartbeatMigrationRetiresClosedConditionAndKeepsUnusedColumns(t *testing.T) {
	path := t.TempDir() + "/heartbeat.sqlite"
	store, err := openHeartbeatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := store.CreateAttachment(testHeartbeatInput("nht_condition"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(`UPDATE heartbeat_attachments SET condition_type='retired_custom_template' WHERE id=?`, rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openHeartbeatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if backups, _ := filepath.Glob(path + ".before-stage6-*"); len(backups) != 1 {
		t.Fatalf("pre-migration backups = %v", backups)
	}
	loaded, err := store.Attachment(rec.ID)
	if err != nil || loaded.State != heartbeatStatePaused || loaded.PauseReason == "" {
		t.Fatalf("retired condition attachment = %#v, %v", loaded, err)
	}
	var condition, event string
	if err := store.DB.QueryRow(`SELECT condition_type FROM heartbeat_attachments WHERE id=?`, rec.ID).Scan(&condition); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`SELECT event_type FROM heartbeat_attachment_events WHERE attachment_id=? ORDER BY id DESC LIMIT 1`, rec.ID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	if condition != "" || event != "condition_retired" {
		t.Fatalf("condition=%q event=%q", condition, event)
	}
	for _, column := range []struct{ table, name string }{
		{"heartbeat_attachments", "condition_type"},
		{"heartbeat_wake_log", "condition_type"},
		{"heartbeat_wake_log", "condition_result"},
	} {
		var count int
		err := store.DB.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, column.table, column.name).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("unused column %s.%s count=%d err=%v", column.table, column.name, count, err)
		}
	}
}

func TestHeartbeatMigrationReplacesLegacyScriptsWithSuspendedPrivateTemplates(t *testing.T) {
	path := t.TempDir() + "/heartbeat.sqlite"
	store, err := openHeartbeatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, legacy, err := store.CreateScript(heartbeatScriptRevisionInput{
		Name: "legacy custom", Source: `process.stdout.write('{"decision":"wake"}')`, Actor: heartbeatActor{Kind: "operator"},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	makeAttachment := func(target string) heartbeatAttachment {
		input := testHeartbeatInput(target)
		input.PrePing = &heartbeatPrePingReference{ScriptID: legacy.ScriptID, Revision: legacy.Revision, Settings: json.RawMessage(`{}`)}
		rec, err := store.CreateAttachment(input, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	first, second := makeAttachment("nht_legacy_first"), makeAttachment("nht_legacy_second")
	if _, err := store.PauseAttachment(first.ID, "controlled freeze", heartbeatActor{Kind: "operator", Name: "Operator"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openHeartbeatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err = store.Attachment(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err = store.Attachment(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != heartbeatStatePaused || first.PauseReason != "controlled freeze" || !first.PrePingSuspended ||
		second.State != heartbeatStatePaused || second.PauseReason == "" || !second.PrePingSuspended {
		t.Fatalf("migrated attachments = %#v %#v", first, second)
	}
	if first.PrePing == nil || second.PrePing == nil || first.PrePing.ScriptID == legacy.ScriptID || second.PrePing.ScriptID == legacy.ScriptID || first.PrePing.ScriptID == second.PrePing.ScriptID {
		t.Fatalf("private copies = %#v %#v; legacy=%#v", first.PrePing, second.PrePing, legacy)
	}
	template, err := heartbeatScriptTemplateByID(heartbeatInboxTemplateID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []heartbeatAttachment{first, second} {
		revision, err := store.ScriptRevision(rec.PrePing.ScriptID, rec.PrePing.Revision)
		if err != nil || revision.TemplateID != heartbeatInboxTemplateID || revision.Source != template.Source {
			t.Fatalf("template copy = %#v, %v", revision, err)
		}
	}
	retained, err := store.ScriptRevision(legacy.ScriptID, legacy.Revision)
	if err != nil || retained.Source != legacy.Source {
		t.Fatalf("legacy source = %#v, %v", retained, err)
	}
	var detail string
	if err := store.DB.QueryRow(`SELECT detail FROM heartbeat_attachment_events WHERE attachment_id=? AND event_type='legacy_pre_ping_retired'`, first.ID).Scan(&detail); err != nil || !strings.Contains(detail, legacy.ScriptID) || !strings.Contains(detail, "revision 1") {
		t.Fatalf("migration audit detail = %q, %v", detail, err)
	}
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h := newHeartbeatService(&server{}, store, true, nil)
	h.Client = client
	h.EvaluateDue(context.Background(), first, 500, time.Now())
	if client.lifecycleReads != 0 || len(client.prompts) != 0 {
		t.Fatalf("suspended legacy attachment ran: reads=%d prompts=%d", client.lifecycleReads, len(client.prompts))
	}
	resumed, err := store.ResumeAttachment(first.ID, "Operator resumed after migration", heartbeatActor{Kind: "operator", Name: "Operator"}, time.Now())
	if err != nil || resumed.PrePingSuspended {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
}
