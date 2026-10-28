package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/history"
)

func newTestControlMetadata(t *testing.T) *controlMetadataStore {
	t.Helper()
	store, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestAgentMessageStore(t *testing.T) *agentMessageStore {
	t.Helper()
	mail, _, err := openAgentMessageStore(filepath.Join(t.TempDir(), "mail.sqlite"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mail.Close() })
	return mail
}

func TestControlMetadataReadPoolIsBoundedAndReadOnly(t *testing.T) {
	store := newTestControlMetadata(t)
	if store.readDB == nil {
		t.Fatal("canonical metadata read pool is unavailable")
	}
	if got := store.readDB.Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("canonical metadata read connections = %d, want 4", got)
	}
	var foreignKeys, busyTimeout int
	if err := store.readDB.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := store.readDB.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 {
		t.Fatalf("read pool pragmas = foreign_keys:%d busy_timeout:%d", foreignKeys, busyTimeout)
	}
	if _, err := store.readDB.Exec(`CREATE TABLE must_stay_read_only(id INTEGER)`); err == nil {
		t.Fatal("canonical metadata read pool accepted a write")
	}
}

// A single-thread metadata view used to wait for the one writer connection,
// so at a restart each registration's view queued behind browser reads and
// durable writes there (66 s of waits in the first 30 s of a boot). It reads
// committed state from the read pool instead. Boundary: the control store on
// its real SQLite file, with a write transaction holding the writer.
func TestControlMetadataViewDoesNotWaitForTheWriter(t *testing.T) {
	control, executions := newTestHostedControl(t)
	record, _, err := executions.beginSession("viewed", "viewed-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := control.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE hosted_threads SET revision=revision WHERE hosted_thread_id=?`, record.ID); err != nil {
		t.Fatal(err)
	}
	viewed := make(chan error, 1)
	go func() {
		_, err := control.view(record.ID)
		viewed <- err
	}()
	select {
	case err := <-viewed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the metadata view waited for the writer connection")
	}
}

// newTestHostedControl provisions the production fail-closed dependencies for
// tests that exercise a hosted HTTP/session path.  Fixtures must opt in to a
// real temporary control.sqlite rather than relying on a legacy JSON fallback.
// migrateTestControl brings a fresh control store to the current schema.
func migrateTestControl(t *testing.T, metadata *controlMetadataStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := prompts.MigrateTurnRecords(ctx, metadata.db, prompts.LegacyTurnPaths{}, filepath.Join(t.TempDir(), "schema-20")); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.MigrateTurnTransactions(ctx, metadata.db, filepath.Join(t.TempDir(), "schema-21.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := coreturns.MigrateCompletedTurnUsage(ctx, metadata.db, filepath.Join(t.TempDir(), "schema-22.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.MigrateMemberRecovery(ctx, metadata.db, filepath.Join(t.TempDir(), "schema-23.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.MigrateParkedDiscard(ctx, metadata.db, filepath.Join(t.TempDir(), "schema-24.sqlite")); err != nil {
		t.Fatal(err)
	}
}

// resolveTestProvisional records, as a child's init does, that the hosted
// thread's provisional handle is native, which makes native current.
func resolveTestProvisional(t *testing.T, control *controlMetadataStore, hostedID, provider, handle, native string) {
	t.Helper()
	if err := control.claimNativeIDResolving(hostedID, provider, native, false, &nativeResolution{Handle: handle, Native: native}); err != nil {
		t.Fatal(err)
	}
}

func newTestHostedControl(t *testing.T) (*controlMetadataStore, *agentExecutionStore) {
	t.Helper()
	metadata := newTestControlMetadata(t)
	migrateTestControl(t, metadata)
	executions, err := newControlAgentExecutionStore(metadata, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	return metadata, executions
}

func TestControlMetadataMigratesInvokerNotificationFlagFromVersionSix(t *testing.T) {
	store := newTestControlMetadata(t)
	if _, err := store.db.Exec(`DROP INDEX hosted_threads_notify_invoker`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX hosted_turn_operations_notification_lookup`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE hosted_threads RENAME TO hosted_threads_v6`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE hosted_threads (
		hosted_thread_id TEXT PRIMARY KEY, provider TEXT NOT NULL,
		current_native_id TEXT, presentation_mode TEXT NOT NULL,
		invoker_hosted_id TEXT, invoker_native_id TEXT, invoker_provider TEXT,
		attachment_generation INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE hosted_threads_v6`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.Query(`PRAGMA table_info(hosted_threads)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "notify_invoker_on_terminal" {
			found = notNull == 1 && defaultValue.Valid && defaultValue.String == "0"
		}
	}
	if !found {
		t.Fatal("migration did not add NOT NULL DEFAULT 0 notify_invoker_on_terminal")
	}
}

func TestControlMetadataMigrationAddsBrowserDuplicateIncidentsFromVersion17(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE browser_duplicate_incidents`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version = 17`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version, tables int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='browser_duplicate_incidents'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != controlSchemaVersion || tables != 1 {
		t.Fatalf("migration version=%d tables=%d, want %d and 1", version, tables, controlSchemaVersion)
	}
}

func TestControlMetadataRefusesVersionThreeWithoutDroppingExecutionState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE agent_execution_state (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO agent_execution_state(value) VALUES ('must survive')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if reopened, err := openControlMetadataStore(path); err == nil {
		reopened.Close()
		t.Fatal("version 3 migration succeeded without a lossless execution-state importer")
	} else if !strings.Contains(err.Error(), "schema 3") {
		t.Fatalf("version 3 migration error = %v, want schema 3 refusal", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM agent_execution_state`).Scan(&value); err != nil {
		t.Fatalf("legacy execution state was dropped: %v", err)
	}
	if value != "must survive" {
		t.Fatalf("legacy execution state = %q", value)
	}
}

func TestControlMetadataRequestedThenConfirmedIsOneCurrentRecord(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_priority", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	requested, err := store.appendEvent(metadataEvent{
		HostedThreadID: "nht_priority", Field: postureFieldServiceTier, Value: "priority",
		Disposition: metadataDispositionRequested, Source: postureSourceCreation,
		Freshness: metadataFreshnessLive, SourceEventKey: "creation-priority",
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.appendEvent(metadataEvent{
		HostedThreadID: "nht_priority", Field: postureFieldServiceTier, Value: "priority",
		Disposition: metadataDispositionConfirmed, Source: postureSourceProvider,
		Freshness: metadataFreshnessLive, SourceEventKey: "provider-priority",
	})
	if err != nil {
		t.Fatal(err)
	}
	var desiredID, effectiveID, revision int64
	var desiredDisposition, effectiveDisposition string
	if err := store.db.QueryRow(`SELECT desired_event_id,effective_event_id,desired_disposition,effective_disposition,thread_revision FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, "nht_priority", postureFieldServiceTier).Scan(&desiredID, &effectiveID, &desiredDisposition, &effectiveDisposition, &revision); err != nil {
		t.Fatal(err)
	}
	if desiredID != requested.ID || effectiveID != confirmed.ID || desiredDisposition != metadataDispositionRequested || effectiveDisposition != metadataDispositionConfirmed || revision != 3 {
		t.Fatalf("current = desired %d/%s effective %d/%s revision %d", desiredID, desiredDisposition, effectiveID, effectiveDisposition, revision)
	}
	if err := store.integrityCheck(); err != nil {
		t.Fatal(err)
	}
}

func TestControlMetadataHistoricalProviderEvidenceCannotConfirmNewRequest(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_replay", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_replay", Field: postureFieldModel, Value: "new", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_replay", Field: postureFieldModel, Value: "old", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessReplayed}); err != nil {
		t.Fatal(err)
	}
	var effective sql.NullInt64
	if err := store.db.QueryRow(`SELECT effective_event_id FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, "nht_replay", postureFieldModel).Scan(&effective); err != nil {
		t.Fatal(err)
	}
	if effective.Valid {
		t.Fatalf("replayed evidence became effective event %d", effective.Int64)
	}
}

func TestControlMetadataEventAndCurrentRollbackTogether(t *testing.T) {
	store := newTestControlMetadata(t)
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "missing", Field: postureFieldModel, Value: "x", Disposition: metadataDispositionRequested, Source: postureSourceCreation}); err == nil {
		t.Fatal("append for unknown hosted thread succeeded")
	}
	var events int
	if err := store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("failed transaction retained %d events", events)
	}
}

// A provisional handle retires only with its own exact resolution, recorded
// once in the same claim: no caller can drop it without the pair, a retry after
// a commit is idempotent, and the resolution never changes.
func TestControlMetadataNativeSuccessionRetiresTheOldClaim(t *testing.T) {
	store, _ := newTestHostedControl(t)
	if err := store.ensureHostedThread("nht_native", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.claimNativeID("nht_native", "claude", "cs_provisional", true); err != nil {
		t.Fatal(err)
	}
	if err := store.claimNativeID("nht_native", "claude", "native-successor", false); !errors.Is(err, errUnresolvedProvisional) {
		t.Fatalf("an unresolved provisional handle was retired: %v", err)
	}
	pair := nativeResolution{Handle: "cs_provisional", Native: "native-successor"}
	if err := store.claimNativeIDResolving("nht_native", "claude", "native-successor", false, &pair); err != nil {
		t.Fatal(err)
	}
	if err := store.claimNativeIDResolving("nht_native", "claude", "native-successor", false, &pair); err != nil {
		t.Fatalf("retrying a recorded resolution = %v", err)
	}
	other := nativeResolution{Handle: "cs_provisional", Native: "native-other"}
	if err := store.claimNativeIDResolving("nht_native", "claude", "native-other", false, &other); !errors.Is(err, errUnresolvedProvisional) {
		t.Fatalf("a recorded resolution was replaced: %v", err)
	}
	var resolved sql.NullString
	if err := store.db.QueryRow(`SELECT resolved_native_id FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND native_thread_id=?`, "nht_native", "cs_provisional").Scan(&resolved); err != nil || resolved.String != "native-successor" {
		t.Fatalf("resolution = %v, %v", resolved, err)
	}
	var retired sql.NullString
	if err := store.db.QueryRow(`SELECT retired_at FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND native_thread_id=?`, "nht_native", "cs_provisional").Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if !retired.Valid {
		t.Fatal("provisional native claim remained active")
	}
	var current string
	if err := store.db.QueryRow(`SELECT current_native_id FROM hosted_threads WHERE hosted_thread_id=?`, "nht_native").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != "native-successor" {
		t.Fatalf("current native id = %q", current)
	}
}

func TestControlMetadataFreshnessRequiresGenerationOrExactCausation(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_generation", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.beginTransportBinding("nht_generation")
	if err != nil || generation != 1 {
		t.Fatalf("binding generation = %d, %v", generation, err)
	}
	requested, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "new", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "old", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation - 1}); err != nil {
		t.Fatal(err)
	}
	assertNoEffective(t, store, "nht_generation", postureFieldModel)
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "new", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation}); err != nil {
		t.Fatal(err)
	}
	assertCurrentValue(t, store, "nht_generation", postureFieldModel, `"new"`, metadataDispositionConfirmed)
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "linked", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation - 1, CausationEventID: &requested.ID}); err != nil {
		t.Fatal(err)
	}
	assertCurrentValue(t, store, "nht_generation", postureFieldModel, `"linked"`, metadataDispositionConfirmed)
	other, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldEffort, Value: "high", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	wrong := other.ID
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "wrong", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation - 1, CausationEventID: &wrong}); err != nil {
		t.Fatal(err)
	}
	assertCurrentValue(t, store, "nht_generation", postureFieldModel, `"linked"`, metadataDispositionConfirmed)
}

func TestControlMetadataCoalescesObservationsButRetainsOperatorAuditEvents(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_coalesce", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.beginTransportBinding("nht_coalesce")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_coalesce", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_coalesce", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate observation ids = %d, %d", first.ID, second.ID)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_coalesce", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: generation}); err != nil {
			t.Fatal(err)
		}
	}
	var events, revision int
	if err := store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, "nht_coalesce").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if events != 3 || revision != 5 {
		t.Fatalf("events/revision = %d/%d, want 3/5", events, revision)
	}
}

func TestControlMetadataRejectsInvalidIdentity(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_invalid", "", presentationShared, "", "", ""); err == nil {
		t.Fatal("empty provider accepted")
	}
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_missing", Field: postureFieldModel, Value: "x", Disposition: metadataDispositionRequested, Source: postureSourceCreation}); err == nil {
		t.Fatal("unknown hosted identity accepted")
	}
}

func TestHostedMetadataAPIReadsCanonicalCurrentState(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_api", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.beginTransportBinding("nht_api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_api", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, AttachmentGeneration: generation}); err != nil {
		t.Fatal(err)
	}
	executions := newTestAgentExecutionStore(t, "test")
	if _, err := executions.ensureSession("nht_api", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, agentExecutions: executions, controlMetadata: store}
	rec := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_api/metadata", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !containsAll(body, `"serviceTier"`, `"priority"`, `"requested"`) {
		t.Fatalf("metadata body = %s", body)
	}
	changes := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(changes, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_api/metadata/changes?afterRevision=0", nil))
	if changes.Code != http.StatusOK || !containsAll(changes.Body.String(), `"hostedThreadId":"nht_api"`, `"serviceTier"`) {
		t.Fatalf("metadata changes = %d %s", changes.Code, changes.Body.String())
	}
	events := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(events, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_api/metadata/events?after=0&limit=1", nil))
	if events.Code != http.StatusOK || !containsAll(events.Body.String(), `"events"`, `"nextCursor"`, `"serviceTier"`) {
		t.Fatalf("metadata events = %d %s", events.Code, events.Body.String())
	}
}

func TestHostedMetadataAPIBaseMissingCanonicalRecordReturnsNotFound(t *testing.T) {
	metadata := newTestControlMetadata(t)
	// This deliberately uses the legacy in-memory execution fixture only to
	// exercise a durable execution record whose canonical metadata row was
	// removed. Production control-mode records cannot get into this state
	// because their foreign key owns the hosted identity.
	executions := newTestAgentExecutionStore(t, "test")
	if _, err := executions.ensureSession("nht_missing_metadata", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, agentExecutions: executions, controlMetadata: metadata}
	rec := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_missing_metadata/metadata", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("base metadata missing canonical row = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHostedMetadataEventsAPIUsesPublicFilteredCursorContract(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_event_api", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.beginTransportBinding("nht_event_api")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []metadataEvent{
		{HostedThreadID: "nht_event_api", Field: postureFieldModel, Value: "old", Disposition: metadataDispositionObserved, Source: metadataSourceMigration, Freshness: metadataFreshnessHistorical, SourceEventKey: "old-model"},
		{HostedThreadID: "nht_event_api", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: generation, SourceEventKey: "priority"},
		{HostedThreadID: "nht_event_api", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation, SourceEventKey: "priority-confirmed"},
	} {
		if _, err := store.appendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	executions, err := newControlAgentExecutionStore(store, "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executions.ensureSession("nht_event_api", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, agentExecutions: executions, controlMetadata: store}
	rec := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_event_api/metadata/events?after=0&limit=1&field=serviceTier&freshness=live", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Events []struct {
			ID             int64           `json:"id"`
			HostedThreadID string          `json:"hostedThreadId"`
			Field          string          `json:"field"`
			Value          json.RawMessage `json:"value"`
			SourceEventKey json.RawMessage `json:"sourceEventKey"`
		} `json:"events"`
		NextCursor int64 `json:"nextCursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].HostedThreadID != "nht_event_api" || page.Events[0].Field != postureFieldServiceTier || len(page.Events[0].SourceEventKey) != 0 || page.NextCursor != page.Events[0].ID {
		t.Fatalf("public event page = %+v", page)
	}
	bad := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_event_api/metadata/events?field=not-a-field", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid filter = %d %s", bad.Code, bad.Body.String())
	}
	missing := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_missing/metadata/events", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown events = %d %s", missing.Code, missing.Body.String())
	}
}

func TestHostedMetadataAPIDegradedModeDoesNotServePartialCurrent(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_metadata_degraded", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	executions := newTestAgentExecutionStore(t, "test")
	if _, err := executions.ensureSession("nht_metadata_degraded", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, agentExecutions: executions, controlMetadata: store, controlMetadataErr: "legacy import checksum failed"}
	rec := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_metadata_degraded/metadata", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "legacy import checksum failed") {
		t.Fatalf("degraded metadata response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestExecutionControlDegradedModeRejectsHostedMutations(t *testing.T) {
	server := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, executionControlErr: "control.sqlite is corrupt"}
	rec := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_unavailable", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "control.sqlite is corrupt") {
		t.Fatalf("execution degraded response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSessionsRouteDegradedModeKeepsReadsButRejectsNewHostedSession(t *testing.T) {
	server := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, executionControlErr: "control.sqlite is corrupt"}
	get := httptest.NewRecorder()
	server.handleSessionsList(get, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("degraded GET status = %d", get.Code)
	}
	post := httptest.NewRecorder()
	server.handleSessionCreate(post, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"provider":"codex","mode":"start"}`)))
	if post.Code != http.StatusServiceUnavailable || !strings.Contains(post.Body.String(), "control.sqlite is corrupt") {
		t.Fatalf("degraded POST = %d %s", post.Code, post.Body.String())
	}
}

func TestRecordPostureFailureSeparatesDegradedIngestionFromReadAvailability(t *testing.T) {
	store := newTestControlMetadata(t)
	notified := 0
	sess := &session{hostedThreadID: "nht_missing",
		id:             "nano-1",
		metadata:       store,
		state:          map[string]json.RawMessage{},
		runtimeNotify:  func() { notified++ },
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.recordPosture([]postureEntry{{Field: postureFieldServiceTier, Value: "priority", Source: postureSourceCreation}}, metadataFreshnessLive)
	if notified != 1 {
		t.Fatalf("runtime notifications = %d", notified)
	}
	if !sess.summary().MetadataUnavailable {
		t.Fatal("missing canonical record was not unavailable in summary")
	}
	if !sess.summary().MetadataDegraded {
		t.Fatal("failed metadata ingestion was not degraded in summary")
	}
}

func TestIngestionFailureKeepsLastCommittedCanonicalStateVisible(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_degraded_readable", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.beginTransportBinding("nht_degraded_readable")
	if err != nil {
		t.Fatal(err)
	}
	sess := &session{hostedThreadID: "nht_degraded_readable",
		provider:             "codex",
		id:                   "nano-1",
		metadata:             store,
		attachmentGeneration: generation,
		state:                map[string]json.RawMessage{},
		lifecycleState:       testLifecycleSnapshot("ready", false)}
	sess.recordPosture([]postureEntry{{Field: postureFieldServiceTier, Value: "priority", Source: postureSourceCreation}}, metadataFreshnessLive)
	// An unsupported newer observation fails before commit. It must degrade
	// ingestion without allowing the ephemeral session to veto the healthy
	// canonical record already in SQLite.
	sess.recordPosture([]postureEntry{{Field: "unsupported", Value: "bad", Source: postureSourceProvider}}, metadataFreshnessLive)

	summary := sess.summary()
	if summary.MetadataUnavailable || !summary.MetadataDegraded || summary.Metadata == nil {
		t.Fatalf("degraded readable summary = %+v", summary)
	}
	if summary.Posture.ServiceTier != "priority" {
		t.Fatalf("last committed posture hidden: %+v", summary.Posture)
	}
}

func TestRecordPosturePublishesRuntimeOnlyAfterCommittedMutation(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_runtime_commit", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.beginTransportBinding("nht_runtime_commit")
	if err != nil {
		t.Fatal(err)
	}
	notified := 0
	sess := &session{hostedThreadID: "nht_runtime_commit",
		id:                   "nano-1",
		metadata:             store,
		attachmentGeneration: generation,
		state:                map[string]json.RawMessage{},
		runtimeNotify:        func() { notified++ },
		lifecycleState:       testLifecycleSnapshot("ready", false)}
	sess.recordPosture([]postureEntry{{Field: postureFieldServiceTier, Value: "priority", Source: postureSourceOperator}}, metadataFreshnessLive)
	if notified != 1 {
		t.Fatalf("runtime notifications = %d, want one committed mutation", notified)
	}
	view, err := store.view("nht_runtime_commit")
	if err != nil || view.Fields[postureFieldServiceTier].DesiredDisposition != metadataDispositionRequested {
		t.Fatalf("runtime notification preceded committed current state: view=%+v err=%v", view, err)
	}
	snapshot := sess.snapshotProviderCausation(map[string]string{postureFieldServiceTier: "priority"})
	sess.recordProviderEvidence("test_provider", map[string]string{postureFieldServiceTier: "priority"}, metadataFreshnessLive, snapshot)
	if notified != 2 {
		t.Fatalf("provider runtime notifications = %d, want post-commit notification", notified)
	}
	view, err = store.view("nht_runtime_commit")
	if err != nil || view.Fields[postureFieldServiceTier].EffectiveDisposition != metadataDispositionConfirmed {
		t.Fatalf("provider notification preceded committed effective state: view=%+v err=%v", view, err)
	}
	// A retry is a semantic no-op: it must not make every other browser tab
	// refetch an unchanged hosted revision.
	sess.recordProviderEvidence("test_provider", map[string]string{postureFieldServiceTier: "priority"}, metadataFreshnessLive, snapshot)
	if notified != 2 {
		t.Fatalf("duplicate provider evidence notified %d times, want 2", notified)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	sess.recordProviderRejection("test_rejection", providerCausationSnapshot{postureFieldServiceTier: snapshot[postureFieldServiceTier]})
	if notified != 3 || !sess.metadataDegraded {
		t.Fatalf("failed provider rejection did not publish degraded state: notifications=%d degraded=%v", notified, sess.metadataDegraded)
	}
}

func TestMetadataChangesAreMonotonicAndRecoverableByCurrentRefetch(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_changes", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := store.beginTransportBinding("nht_changes")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.appendEvent(metadataEvent{HostedThreadID: "nht_changes", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: g})
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.view("nht_changes")
	if err != nil {
		t.Fatal(err)
	}
	changes := store.changesAfter("nht_changes", 0)
	if len(changes) != 3 || changes[2].Revision != view.Revision || !reflect.DeepEqual(changes[0].Fields, []string{"identity"}) || !reflect.DeepEqual(changes[1].Fields, []string{"attachmentGeneration"}) || !reflect.DeepEqual(changes[2].Fields, []string{postureFieldServiceTier}) {
		t.Fatalf("changes=%+v view=%+v", changes, view)
	}
	// An exact source-key retry is a no-op and emits no revision hint.
	keyed := metadataEvent{HostedThreadID: "nht_changes", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, SourceEventKey: "change-retry"}
	if _, err := store.appendEvent(keyed); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.view("nht_changes")
	if _, err := store.appendEvent(keyed); err != nil {
		t.Fatal(err)
	}
	if got := store.changesAfter("nht_changes", latest.Revision); len(got) != 0 {
		t.Fatalf("duplicate emitted change %#v", got)
	}
}

func TestMetadataChangesAPIReportsGapsAndDegradedState(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_hint", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, _ := store.beginTransportBinding("nht_hint")
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_hint", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err != nil {
		t.Fatal(err)
	}
	exec := newTestAgentExecutionStore(t, "test")
	if _, err := exec.ensureSession("nht_hint", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, agentExecutions: exec, controlMetadata: store}
	good := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(good, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_hint/metadata/changes?afterRevision=2", nil))
	if good.Code != http.StatusOK || !containsAll(good.Body.String(), `"currentRevision":3`, `"refetchRequired":false`) {
		t.Fatalf("incremental hints %d %s", good.Code, good.Body.String())
	}
	bad := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_hint/metadata/changes?afterRevision=nope", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("malformed cursor=%d", bad.Code)
	}
	unknown := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_unknown/metadata/changes?afterRevision=0", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown hint thread=%d", unknown.Code)
	}
	store.changeMu.Lock()
	store.changes = nil
	store.changeMu.Unlock()
	gap := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(gap, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_hint/metadata/changes?afterRevision=0", nil))
	if gap.Code != http.StatusOK || !strings.Contains(gap.Body.String(), `"refetchRequired":true`) {
		t.Fatalf("restart gap=%d %s", gap.Code, gap.Body.String())
	}
	server.controlMetadataErr = "degraded"
	degraded := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(degraded, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_hint/metadata/changes?afterRevision=0", nil))
	if degraded.Code != http.StatusServiceUnavailable {
		t.Fatalf("degraded hints=%d", degraded.Code)
	}
}

func TestControlMetadataCurrentOnlyDedupAndIdentityValidation(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_dedup", "codex", presentationShared, "parent", "native-parent", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := store.ensureHostedThread("nht_dedup", "claude", presentationShared, "parent", "native-parent", "claude"); err == nil {
		t.Fatal("provider conflict accepted")
	}
	if err := store.ensureHostedThread("nht_dedup", "codex", presentationShared, "other", "native-parent", "claude"); err == nil {
		t.Fatal("lineage conflict accepted")
	}
	g, _ := store.beginTransportBinding("nht_dedup")
	for _, value := range []string{"a", "b", "a"} {
		if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_dedup", Field: postureFieldModel, Value: value, Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id='nht_dedup'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("A-B-A events = %d, want 3", count)
	}
	if err := store.claimNativeID("nht_dedup", "claude", "native", false); err == nil {
		t.Fatal("native provider conflict accepted")
	}
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_dedup", Field: "bogus", Value: "x", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestControlMetadataSourceEventKeyIsIdempotentOnlyForSameEvidence(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_key", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, _ := store.beginTransportBinding("nht_key")
	e := metadataEvent{HostedThreadID: "nht_key", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, SourceEventKey: "provider-event-1"}
	first, err := store.appendEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Committed {
		t.Fatal("first source-key evidence did not report a committed revision")
	}
	second, err := store.appendEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.Committed {
		t.Fatalf("same retry result = ids %d/%d committed=%v", first.ID, second.ID, second.Committed)
	}
	reused, err := store.appendEvent(first)
	if err != nil || reused.Committed || reused.ID != first.ID {
		t.Fatalf("returned event reused as retry = %#v, err=%v", reused, err)
	}
	e.Value = "luna"
	if _, err := store.appendEvent(e); err == nil {
		t.Fatal("same key with changed payload succeeded")
	}
	var count, revision int
	_ = store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events`).Scan(&count)
	_ = store.db.QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id='nht_key'`).Scan(&revision)
	if count != 1 || revision != 3 {
		t.Fatalf("collision mutated count/revision %d/%d", count, revision)
	}
}

func TestControlMetadataRejectionKeepsPriorEffectiveAndSettlesDesired(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_reject", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, _ := store.beginTransportBinding("nht_reject")
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_reject", Field: postureFieldServiceTier, Value: "default", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err != nil {
		t.Fatal(err)
	}
	req, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_reject", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: g})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_reject", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRejected, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, CausationEventID: &req.ID})
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentValue(t, store, "nht_reject", postureFieldServiceTier, `"default"`, metadataDispositionConfirmed)
	var disposition string
	var desiredID, settlementID int64
	var desiredValue string
	if err := store.db.QueryRow(`SELECT desired_disposition,desired_event_id,desired_settlement_event_id,desired_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, "nht_reject", postureFieldServiceTier).Scan(&disposition, &desiredID, &settlementID, &desiredValue); err != nil {
		t.Fatal(err)
	}
	if disposition != metadataDispositionRejected || desiredID != req.ID || settlementID != rejected.ID || desiredValue != `"priority"` {
		t.Fatalf("settled desired = %s id=%d settlement=%d value=%s", disposition, desiredID, settlementID, desiredValue)
	}
}

func TestControlMetadataCausalConfirmationSettlesOriginalDesiredEvent(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_confirm", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := store.beginTransportBinding("nht_confirm")
	if err != nil {
		t.Fatal(err)
	}
	requested, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_confirm", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: g})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_confirm", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, CausationEventID: &requested.ID})
	if err != nil {
		t.Fatal(err)
	}
	var desiredID, settlementID, effectiveID int64
	var disposition, value string
	if err := store.db.QueryRow(`SELECT desired_event_id,desired_settlement_event_id,effective_event_id,desired_disposition,desired_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, "nht_confirm", postureFieldModel).Scan(&desiredID, &settlementID, &effectiveID, &disposition, &value); err != nil {
		t.Fatal(err)
	}
	if desiredID != requested.ID || settlementID != confirmed.ID || effectiveID != confirmed.ID || disposition != metadataDispositionConfirmed || value != `"sol"` {
		t.Fatalf("confirmation rewrote desired state: desired=%d settlement=%d effective=%d %s %s", desiredID, settlementID, effectiveID, disposition, value)
	}
}

func TestControlMetadataRejectionWithoutEffectiveRetainsFailedRequest(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_reject_empty", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := store.beginTransportBinding("nht_reject_empty")
	if err != nil {
		t.Fatal(err)
	}
	requested, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_reject_empty", Field: postureFieldModel, Value: "bad-model", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: g})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_reject_empty", Field: postureFieldModel, Value: "bad-model", Disposition: metadataDispositionRejected, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, CausationEventID: &requested.ID})
	if err != nil {
		t.Fatal(err)
	}
	var desiredID, settlementID int64
	var desiredValue string
	var disposition string
	var effective sql.NullInt64
	if err := store.db.QueryRow(`SELECT desired_event_id,desired_settlement_event_id,desired_value_json,desired_disposition,effective_event_id FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, "nht_reject_empty", postureFieldModel).Scan(&desiredID, &settlementID, &desiredValue, &disposition, &effective); err != nil {
		t.Fatal(err)
	}
	if desiredID != requested.ID || settlementID != rejected.ID || desiredValue != `"bad-model"` || disposition != metadataDispositionRejected || effective.Valid {
		t.Fatalf("rejected request state desired=%d settlement=%d value=%s status=%s effective=%v", desiredID, settlementID, desiredValue, disposition, effective)
	}
}

func TestLegacyAttachmentMigrationScrubsRetiredJSONOnlyAfterSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachments.json")
	original := []byte(`[{"provider":"codex","nativeThreadId":"native-scrub","hostedThreadId":"nht_scrub","cwd":"/legacy","model":"legacy-model","turnServiceTier":"priority","bin":"/codex","args":["--resume","native-scrub"],"attachedAt":"2026-08-10T00:00:00Z"}]`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	attachments, err := newAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	failed := newTestControlMetadata(t)
	if err := failed.ensureHostedThread("nht_scrub", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := importAndScrubLegacyAttachments(failed, attachments); err == nil {
		t.Fatal("conflicting attachment migration succeeded")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(original) {
		t.Fatalf("failed migration scrubbed source: %q, %v", data, err)
	}

	success := newTestControlMetadata(t)
	if err := importAndScrubLegacyAttachments(success, attachments); err != nil {
		t.Fatal(err)
	}
	if got := success.posture("nht_scrub"); got.Model != "legacy-model" || got.ServiceTier != "priority" {
		t.Fatalf("attachment import did not retain canonical baseline: %+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"cwd", "model", "turnServiceTier"} {
		if _, ok := raw[0][field]; ok {
			t.Fatalf("successful migration retained retired attachment field %s: %s", field, data)
		}
	}
	attachments.remember(attachmentRecord{Provider: "codex", NativeThreadID: "native-scrub", HostedThreadID: "nht_scrub", Cwd: "/attempted-return", Model: "attempted-return", TurnServiceTier: "default", Bin: "/codex", Args: []string{"--resume", "native-scrub"}, AttachedAt: "2026-08-10T00:01:00Z"})
	reopened, err := newAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.get("codex", "native-scrub")
	if !ok || got.Cwd != "" || got.Model != "" || got.TurnServiceTier != "" || got.Bin != "/codex" || !reflect.DeepEqual(got.Args, []string{"--resume", "native-scrub"}) {
		t.Fatalf("transport refresh restored retired metadata or lost transport: %+v", got)
	}
}

type hostedMetadataNameCatalog struct {
	*fakeThreadCatalog
	ref *history.ThreadNameRef
}

func (c *hostedMetadataNameCatalog) ThreadNameRefFor(context.Context, string, string) (*history.ThreadNameRef, bool) {
	return c.ref, c.ref != nil
}

func TestHostedMetadataNameUsesThreadNameProvenance(t *testing.T) {
	for _, provenance := range []history.ThreadNameProvenance{history.ThreadNameManual, history.ThreadNameHistoricalImport} {
		t.Run(string(provenance), func(t *testing.T) {
			server := &server{catalog: &hostedMetadataNameCatalog{fakeThreadCatalog: &fakeThreadCatalog{}, ref: &history.ThreadNameRef{Value: "durable name", Provenance: provenance}}}
			view := server.decorateHostedMetadata(hostedMetadataView{Provider: "codex", NativeThreadID: "native-name"}, hostedAttachmentView{State: "ready", Provenance: "live_session"})
			if view.Name.Value != "durable name" || view.Name.Provenance != string(provenance) {
				t.Fatalf("hosted name provenance = %+v", view.Name)
			}
		})
	}
}

// The session list carries each field's displayed value and the revision,
// not the full canonical view (85% of its bytes, read by every page). Catches
// the full view, or its name and native-id history, coming back into the list.
func TestSessionListCarriesDisplayedMetadataOnly(t *testing.T) {
	metadata := newTestControlMetadata(t)
	if err := metadata.ensureHostedThread("nht_listed", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	sess := &session{id: "session-listed", provider: "codex", hostedThreadID: "nht_listed", threadID: "native-listed",
		createdAt: time.Now(), state: map[string]json.RawMessage{}, metadata: metadata, runtimeNotify: func() {},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.recordPosture([]postureEntry{{Field: postureFieldServiceTier, Value: "priority", Source: postureSourceCreation}}, metadataFreshnessLive)
	server := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{sess.id: sess}}, controlMetadata: metadata}
	rec := httptest.NewRecorder()
	server.handleSessionsList(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	var body struct {
		Sessions []struct {
			Metadata map[string]json.RawMessage `json:"metadata"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Sessions) != 1 {
		t.Fatalf("sessions = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	listed := body.Sessions[0].Metadata
	if len(listed) != 2 || listed["revision"] == nil || string(listed["fields"]) != `{"serviceTier":{"displayValue":"priority"}}` {
		t.Fatalf("listed metadata = %v", listed)
	}
}

func TestLegacyAttachmentImportResumesWithoutOverwritingStrongerEvidence(t *testing.T) {
	store := newTestControlMetadata(t)
	records := []attachmentRecord{{HostedThreadID: "nht_attachment", Provider: "codex", Cwd: "/work", Model: "attachment-model", TurnServiceTier: "priority", InvokerHostedThreadID: "nht_parent", InvokerNativeThreadID: "parent-native", InvokerProvider: "claude"}, {HostedThreadID: "nht_empty_native", Provider: "claude", Model: "haiku", TurnServiceTier: "priority"}}
	if err := store.importLegacyAttachmentsUntil(records, 1); err == nil {
		t.Fatal("bounded attachment import unexpectedly completed")
	}
	var markers int
	_ = store.db.QueryRow(`SELECT count(*) FROM control_migrations WHERE name='legacy-attachments-v1'`).Scan(&markers)
	if markers != 0 {
		t.Fatalf("interrupted attachment import wrote completion marker %d", markers)
	}
	if err := store.importLegacyAttachments(records); err != nil {
		t.Fatal(err)
	}
	if err := store.importLegacyAttachments(records); err != nil {
		t.Fatal(err)
	}
	view, err := store.view("nht_attachment")
	if err != nil {
		t.Fatal(err)
	}
	if view.InvokerHostedThreadID != "nht_parent" || view.InvokerNativeThreadID != "parent-native" || view.InvokerProvider != "claude" {
		t.Fatalf("attachment lineage = %+v", view)
	}
	if got := store.posture("nht_attachment"); got.Model != "attachment-model" || got.ServiceTier != "priority" {
		t.Fatalf("attachment baseline posture = %+v", got)
	}
	if empty, err := store.view("nht_empty_native"); err != nil || empty.NativeThreadID != "" || empty.Provider != "claude" {
		t.Fatalf("empty native attachment identity = %+v err=%v", empty, err)
	}
	conflict := newTestControlMetadata(t)
	if err := conflict.ensureHostedThread("nht_attachment", "codex", presentationShared, "nht_parent", "parent-native", "claude"); err != nil {
		t.Fatal(err)
	}
	g, err := conflict.beginTransportBinding("nht_attachment")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conflict.appendEvent(metadataEvent{HostedThreadID: "nht_attachment", Field: postureFieldServiceTier, Value: "default", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err != nil {
		t.Fatal(err)
	}
	if err := conflict.importLegacyAttachments(records[:1]); err != nil {
		t.Fatal(err)
	}
	if got := conflict.posture("nht_attachment").ServiceTier; got != "default" {
		t.Fatalf("historical attachment overwrote live provider tier %q", got)
	}
}

func TestLegacyAttachmentImportTransfersActiveNativeOwnership(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_old_owner", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.claimNativeID("nht_old_owner", "codex", "native-shared", false); err != nil {
		t.Fatal(err)
	}
	records := []attachmentRecord{{
		HostedThreadID: "nht_attachment_owner",
		Provider:       "codex",
		NativeThreadID: "native-shared",
		AttachedAt:     "2026-08-12T13:37:00Z",
	}}
	if err := store.importLegacyAttachments(records); err != nil {
		t.Fatal(err)
	}
	if err := store.importLegacyAttachments(records); err != nil {
		t.Fatal(err)
	}

	var activeOwner string
	if err := store.db.QueryRow(`SELECT hosted_thread_id FROM hosted_thread_native_ids WHERE provider='codex' AND native_thread_id='native-shared' AND retired_at IS NULL`).Scan(&activeOwner); err != nil {
		t.Fatal(err)
	}
	if activeOwner != "nht_attachment_owner" {
		t.Fatalf("active owner = %q, want attachment owner", activeOwner)
	}
	var oldRetired string
	if err := store.db.QueryRow(`SELECT COALESCE(retired_at,'') FROM hosted_thread_native_ids WHERE hosted_thread_id='nht_old_owner' AND provider='codex' AND native_thread_id='native-shared'`).Scan(&oldRetired); err != nil {
		t.Fatal(err)
	}
	if oldRetired == "" {
		t.Fatal("old canonical owner remained active")
	}
	view, err := store.view("nht_attachment_owner")
	if err != nil {
		t.Fatal(err)
	}
	if view.NativeThreadID != "native-shared" {
		t.Fatalf("attachment owner native id = %q", view.NativeThreadID)
	}
}

func TestCanonicalNativeSuccessionPublishesOneChangeAndHistory(t *testing.T) {
	store := newTestControlMetadata(t)
	migrateTestControl(t, store)
	if err := store.ensureHostedThread("nht_native_history", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.claimNativeID("nht_native_history", "claude", "cs_provisional", true); err != nil {
		t.Fatal(err)
	}
	before, _ := store.view("nht_native_history")
	resolveTestProvisional(t, store, "nht_native_history", "claude", "cs_provisional", "native-real")
	after, _ := store.view("nht_native_history")
	if after.Revision != before.Revision+1 || after.NativeThreadID != "native-real" || len(after.NativeIDHistory) != 2 || after.NativeIDHistory[0].RetiredAt == "" || after.NativeIDHistory[1].NativeThreadID != "native-real" {
		t.Fatalf("native succession = %+v", after)
	}
	changes := store.changesAfter("nht_native_history", before.Revision)
	if len(changes) != 1 || !containsAll(strings.Join(changes[0].Fields, ","), "nativeThreadId") {
		t.Fatalf("succession changes = %+v", changes)
	}
	if err := store.claimNativeID("nht_native_history", "claude", "native-real", false); err != nil {
		t.Fatal(err)
	}
	repeat, _ := store.view("nht_native_history")
	if repeat.Revision != after.Revision {
		t.Fatalf("identical native claim advanced revision %d→%d", after.Revision, repeat.Revision)
	}
}

func TestMetadataEventVolumeWarningTracksCommittedEventsOnly(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_volume", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	store.eventVolume = map[string]int{"nht_volume\x00" + postureFieldModel: metadataEventWarningThreshold - 1}
	store.warnedVolume = map[string]bool{}
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_volume", Field: postureFieldModel, Value: "x", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatal(err)
	}
	if !store.warnedVolume["nht_volume\x00"+postureFieldModel] {
		t.Fatal("threshold warning was not recorded")
	}
}

func TestMetadataEventVolumeSurvivesRestartAndIgnoresDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ensureHostedThread("nht_volume_restart", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < metadataEventWarningThreshold-2; i++ {
		if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_volume_restart", Field: postureFieldModel, Value: fmt.Sprintf("model-%03d", i), Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive}); err != nil {
			t.Fatal(err)
		}
	}
	provider := metadataEvent{HostedThreadID: "nht_volume_restart", Field: postureFieldModel, Value: "provider-model", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, SourceEventKey: "volume-restart-provider"}
	if event, err := store.appendEvent(provider); err != nil || !event.Committed {
		t.Fatalf("provider seed = %+v, %v", event, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key := "nht_volume_restart\x00" + postureFieldModel
	if got := store.eventVolume[key]; got != metadataEventWarningThreshold-1 {
		t.Fatalf("restart event volume = %d, want %d", got, metadataEventWarningThreshold-1)
	}
	if store.warnedVolume[key] {
		t.Fatal("restart treated the pre-threshold count as warned")
	}
	duplicate, err := store.appendEvent(provider)
	if err != nil || duplicate.Committed {
		t.Fatalf("duplicate provider event = %+v, %v", duplicate, err)
	}
	if got := store.eventVolume[key]; got != metadataEventWarningThreshold-1 {
		t.Fatalf("duplicate changed restart event volume to %d", got)
	}
	inventory, err := store.eventInventory()
	if err != nil {
		t.Fatal(err)
	}
	if inventory.TotalEvents != metadataEventWarningThreshold-1 || inventory.DuplicateSuppressions != 1 || len(inventory.PerThread) == 0 || len(inventory.PerField) == 0 || inventory.LargestEventID == 0 {
		t.Fatalf("restart inventory = %+v", inventory)
	}
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: "nht_volume_restart", Field: postureFieldModel, Value: "operator-after-restart", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatal(err)
	}
	if got := store.eventVolume[key]; got != metadataEventWarningThreshold || !store.warnedVolume[key] {
		t.Fatalf("next committed event did not trigger restart warning: count=%d warned=%v", got, store.warnedVolume[key])
	}
}

func TestHostedMetadataAttachmentStateIsReadTimeJoin(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_attachment_api", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	exec, err := newControlAgentExecutionStore(store, "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.ensureSessionWithIdentity("nht_attachment_api", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := exec.updateSession("nht_attachment_api", "", "", agentSessionDetached, ""); err != nil {
		t.Fatal(err)
	}
	s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, agentExecutions: exec, controlMetadata: store}
	before, _ := store.view("nht_attachment_api")
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_attachment_api/metadata", nil))
	if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"attachment":{"state":"detached","provenance":"execution_record"`) {
		t.Fatalf("detached metadata = %d %s", rec.Code, rec.Body.String())
	}
	live := &session{id: "live",
		hostedThreadID:  "nht_attachment_api",
		state:           map[string]json.RawMessage{},
		agentExecutions: exec,
		metadata:        store,
		lifecycleState:  testLifecycleSnapshot("starting", false)}
	s.sessions[live.id] = live
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_attachment_api/metadata", nil))
	if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"state":"starting"`, `"provenance":"live_session"`) {
		t.Fatalf("starting metadata = %d %s", rec.Code, rec.Body.String())
	}
	live.lifecycleState = testLifecycleSnapshot("ready", live.workActiveLocked())
	if got := s.hostedAttachmentState("nht_attachment_api"); got.State != "ready" || got.Provenance != "live_session" {
		t.Fatalf("ready attachment = %+v", got)
	}
	delete(s.sessions, live.id)
	if err := exec.updateSession("nht_attachment_api", "", "", sessionStatusExited, "provider failed"); err != nil {
		t.Fatal(err)
	}
	if got := s.hostedAttachmentState("nht_attachment_api"); got.State != sessionStatusExited || got.Error != "provider failed" {
		t.Fatalf("exited attachment = %+v", got)
	}
	after, _ := store.view("nht_attachment_api")
	if before.Revision != after.Revision {
		t.Fatalf("attachment read advanced metadata revision %d→%d", before.Revision, after.Revision)
	}
	if got := s.hostedAttachmentState("missing"); got.State != "unavailable" {
		t.Fatalf("unavailable attachment = %+v", got)
	}
}

func assertCurrentValue(t *testing.T, store *controlMetadataStore, hostedID, field, wantValue, wantDisposition string) {
	t.Helper()
	var value, disposition string
	if err := store.db.QueryRow(`SELECT effective_value_json,effective_disposition FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, field).Scan(&value, &disposition); err != nil {
		t.Fatal(err)
	}
	if value != wantValue || disposition != wantDisposition {
		t.Fatalf("effective = %s/%s, want %s/%s", value, disposition, wantValue, wantDisposition)
	}
}

func assertNoEffective(t *testing.T, store *controlMetadataStore, hostedID, field string) {
	t.Helper()
	var value sql.NullString
	if err := store.db.QueryRow(`SELECT effective_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, field).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value.Valid {
		t.Fatalf("unexpected effective value %s", value.String)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

func TestControlSchemaVersionSevenMigratesNotificationState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX hosted_turn_operations_wake_pending`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE hosted_turn_operations DROP COLUMN wake_batch_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE hosted_turn_operations DROP COLUMN invoker_acked_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE hosted_threads DROP COLUMN terminal_notification_policy`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, check := range []struct{ table, column string }{{"hosted_threads", "terminal_notification_policy"}, {"hosted_turn_operations", "wake_batch_id"}, {"hosted_turn_operations", "invoker_acked_at"}} {
		rows, err := store.db.Query("PRAGMA table_info(" + check.table + ")")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var cid int
			var name, kind string
			var notnull, pk int
			var dflt any
			if err := rows.Scan(&cid, &name, &kind, &notnull, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			found = found || name == check.column
		}
		rows.Close()
		if !found {
			t.Fatalf("migration missing %s.%s", check.table, check.column)
		}
	}
}

// Every re-attach re-reports model, effort, cwd, permissionMode and serviceTier.
// 92% of 199,167 stored events repeated the previous value with only the
// attachment generation changed, and each one bumped a revision that made every
// open tab re-fetch /state. The same fact at a new generation is not an event;
// a changed value or a causal settlement of an operator request still is.
func TestMetadataReobservationAtNewGenerationAddsNoEvent(t *testing.T) {
	store := newTestControlMetadata(t)
	const id = "nht_regeneration"
	if err := store.ensureHostedThread(id, "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	observe := func(generation int64, value, key string, cause *int64) metadataEvent {
		t.Helper()
		event, err := store.appendEvent(metadataEvent{HostedThreadID: id, Field: postureFieldModel, Value: value, Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation, CausationEventID: cause, SourceEventKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	first := observe(0, "gpt", "gen0", nil)
	if !first.Committed {
		t.Fatal("first observation was not recorded")
	}
	before, err := store.view(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE hosted_threads SET attachment_generation=1 WHERE hosted_thread_id=?`, id); err != nil {
		t.Fatal(err)
	}
	repeat := observe(1, "gpt", "gen1", nil)
	if repeat.Committed || repeat.ID != first.ID {
		t.Fatalf("re-observation at a new generation recorded event %+v", repeat)
	}
	after, err := store.view(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("re-observation bumped revision %d -> %d", before.Revision, after.Revision)
	}
	if changed := observe(1, "gpt-2", "gen1-changed", nil); !changed.Committed {
		t.Fatal("a changed value was suppressed as a repeat")
	}
	requested, err := store.appendEvent(metadataEvent{HostedThreadID: id, Field: postureFieldModel, Value: "gpt-2", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive})
	if err != nil || !requested.Committed {
		t.Fatalf("operator request = %+v, %v", requested, err)
	}
	cause := requested.ID
	settled := observe(1, "gpt-2", "gen1-causal", &cause)
	if !settled.Committed {
		t.Fatal("a causal confirmation of the value already in effect was suppressed, leaving the request pending")
	}
	view, err := store.view(id)
	if err != nil {
		t.Fatal(err)
	}
	field := view.Fields[postureFieldModel]
	if field.DesiredDisposition != metadataDispositionConfirmed || field.DesiredSettlementEventID == nil || *field.DesiredSettlementEventID != settled.ID {
		t.Fatalf("request not settled by the causal confirmation: %+v", field)
	}
}
