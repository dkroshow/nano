package app

// control_metadata.go owns the durable hosted-thread metadata read model. It
// deliberately lives outside history.sqlite: provider history is rebuildable,
// while a hosted identity and an operator decision are not.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dkroshow/nano/internal/hosted"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"

	_ "modernc.org/sqlite"
)

const controlSchemaVersion = state.LegacySchemaVersion
const metadataEventWarningThreshold = 256
const metadataEventInventoryLimit = 8

const (
	metadataDispositionRequested = "requested"
	metadataDispositionConfirmed = "confirmed"
	metadataDispositionRejected  = "rejected"
	metadataDispositionCleared   = "cleared"
	metadataDispositionObserved  = "observed"
	metadataDispositionUnknown   = "unknown"

	metadataFreshnessLive       = "live"
	metadataFreshnessHistorical = "historical"
	metadataFreshnessReplayed   = "replayed"

	metadataSourceMigration = "migration"
)

type metadataEvent = hosted.MetadataEvent

func metadataConfirmedFact(field string) bool {
	return field == orchestrationAssignmentField || field == threads.CreationNameField
}

type metadataEventPage = hosted.MetadataEventPage

// metadataEventView is the deliberately small public evidence contract.  The
// internal retry key is not exported: it is repository implementation detail,
// not evidence a client should retain or treat as user-visible identity.
type metadataEventView = hosted.MetadataEventView

type metadataEventFilter = hosted.MetadataEventFilter

// eventsAfter exposes immutable evidence with a bounded event-id cursor. It
// intentionally has no JSON posture fallback: callers refetch current state
// when their revision/change cursor is behind.
func (s *controlMetadataStore) eventsAfter(hostedID string, after int64, limit int, filter metadataEventFilter) (metadataEventPage, error) {
	if s == nil {
		return metadataEventPage{}, fmt.Errorf("canonical metadata unavailable")
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	where := []string{"hosted_thread_id=?", "event_id>?"}
	args := []any{hostedID, after}
	for _, item := range []struct{ column, value string }{{"field", filter.Field}, {"source", filter.Source}, {"disposition", filter.Disposition}, {"evidence_freshness", filter.Freshness}} {
		if item.value != "" {
			where, args = append(where, item.column+"=?"), append(args, item.value)
		}
	}
	args = append(args, limit)
	query := `SELECT event_id,hosted_thread_id,field,value_json,disposition,source,COALESCE(evidence,''),COALESCE(native_thread_id,''),COALESCE(nano_session_id,''),attachment_generation,evidence_freshness,causation_event_id,observed_at,committed_at FROM thread_metadata_events WHERE ` + strings.Join(where, " AND ") + ` ORDER BY event_id LIMIT ?`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return metadataEventPage{}, err
	}
	defer rows.Close()
	page := metadataEventPage{Events: []metadataEventView{}}
	for rows.Next() {
		var raw string
		var causation sql.NullInt64
		var event metadataEventView
		if err := rows.Scan(&event.ID, &event.HostedThreadID, &event.Field, &raw, &event.Disposition, &event.Source, &event.Evidence, &event.NativeThreadID, &event.NanoSessionID, &event.AttachmentGeneration, &event.Freshness, &causation, &event.ObservedAt, &event.CommittedAt); err != nil {
			return page, err
		}
		event.Value = json.RawMessage(raw)
		if causation.Valid {
			id := causation.Int64
			event.CausationEventID = &id
		}
		page.Events = append(page.Events, event)
		page.NextCursor = event.ID
	}
	return page, rows.Err()
}

// controlMetadataStore is intentionally a small repository rather than a
// general event framework. It owns only hosted-thread metadata and folds an
// event into desired/effective current rows in the same transaction.
type metadataChange struct {
	HostedThreadID string   `json:"hostedThreadId"`
	Revision       int64    `json:"revision"`
	Fields         []string `json:"fields"`
}
type controlMetadataStore struct {
	db                    *sql.DB
	readDB                *sql.DB
	changeMu              sync.Mutex
	changes               []metadataChange
	latestRevisions       map[string]int64
	volumeMu              sync.Mutex
	eventVolume           map[string]int
	warnedVolume          map[string]bool
	duplicateSuppressions int
}

// metadataEventInventory is a compact process-start diagnostic. The full
// immutable event log remains in SQLite; this reports only bounded leaders so
// opening a control database never emits one line per hosted thread.
type metadataEventInventory struct {
	TotalEvents           int
	PerThread             []metadataInventoryCount
	PerField              []metadataInventoryCount
	DuplicateSuppressions int
	LargestHostedThreadID string
	LargestField          string
	LargestEventID        int64
	LargestValueBytes     int
}

type metadataInventoryCount struct {
	Key   string
	Count int
}

func (s *controlMetadataStore) noteEventVolume(hostedID, field string) {
	if s == nil {
		return
	}
	key := hostedID + "\x00" + field
	s.volumeMu.Lock()
	defer s.volumeMu.Unlock()
	if s.eventVolume == nil {
		s.eventVolume, s.warnedVolume = map[string]int{}, map[string]bool{}
	}
	s.eventVolume[key]++
	if s.eventVolume[key] >= metadataEventWarningThreshold && !s.warnedVolume[key] {
		s.warnedVolume[key] = true
		log.Printf("canonical metadata event volume warning: hosted thread %s field %s has %d events (events retained)", hostedID, field, s.eventVolume[key])
	}
}

func (s *controlMetadataStore) noteDuplicateSuppression() {
	if s == nil {
		return
	}
	s.volumeMu.Lock()
	defer s.volumeMu.Unlock()
	s.duplicateSuppressions++
}

func (s *controlMetadataStore) initializeEventVolume() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("canonical metadata store unavailable")
	}
	rows, err := s.db.Query(`SELECT hosted_thread_id,field,count(*) FROM thread_metadata_events GROUP BY hosted_thread_id,field`)
	if err != nil {
		return err
	}
	volumes := map[string]int{}
	warned := map[string]bool{}
	for rows.Next() {
		var hostedID, field string
		var count int
		if err := rows.Scan(&hostedID, &field, &count); err != nil {
			_ = rows.Close()
			return err
		}
		key := hostedID + "\x00" + field
		volumes[key] = count
		// A prior process has already crossed this threshold. Do not emit a
		// second warning merely because Nano restarted; a count of 255 remains
		// un-warned, so its next real commit still emits the warning.
		warned[key] = count >= metadataEventWarningThreshold
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	// Startup intentionally keeps one connection until migration and
	// instrumentation finish. Close this cursor before inventory issues its own
	// queries, otherwise startup deadlocks waiting for the connection held by
	// rows. The bounded runtime pool is enabled only after this function returns.
	if err := rows.Close(); err != nil {
		return err
	}
	s.volumeMu.Lock()
	s.eventVolume = volumes
	s.warnedVolume = warned
	s.duplicateSuppressions = 0
	s.volumeMu.Unlock()
	inventory, err := s.eventInventory()
	if err != nil {
		return err
	}
	if inventory.TotalEvents > 0 {
		log.Printf("canonical metadata inventory: events=%d perThread=%s perField=%s duplicateSuppressions=%d largest=%s/%s#%d(%dB)", inventory.TotalEvents, formatMetadataInventoryCounts(inventory.PerThread), formatMetadataInventoryCounts(inventory.PerField), inventory.DuplicateSuppressions, inventory.LargestHostedThreadID, inventory.LargestField, inventory.LargestEventID, inventory.LargestValueBytes)
	}
	return nil
}

func (s *controlMetadataStore) eventInventory() (metadataEventInventory, error) {
	if s == nil || s.db == nil {
		return metadataEventInventory{}, fmt.Errorf("canonical metadata store unavailable")
	}
	inventory := metadataEventInventory{}
	if err := s.db.QueryRow(`SELECT count(*) FROM thread_metadata_events`).Scan(&inventory.TotalEvents); err != nil {
		return inventory, err
	}
	threadCounts, err := metadataInventoryCounts(s.db, `SELECT hosted_thread_id,count(*) FROM thread_metadata_events GROUP BY hosted_thread_id ORDER BY count(*) DESC,hosted_thread_id LIMIT ?`)
	if err != nil {
		return inventory, err
	}
	inventory.PerThread = threadCounts
	fieldCounts, err := metadataInventoryCounts(s.db, `SELECT field,count(*) FROM thread_metadata_events GROUP BY field ORDER BY count(*) DESC,field LIMIT ?`)
	if err != nil {
		return inventory, err
	}
	inventory.PerField = fieldCounts
	_ = s.db.QueryRow(`SELECT hosted_thread_id,field,event_id,LENGTH(value_json) FROM thread_metadata_events ORDER BY LENGTH(value_json) DESC,event_id ASC LIMIT 1`).Scan(&inventory.LargestHostedThreadID, &inventory.LargestField, &inventory.LargestEventID, &inventory.LargestValueBytes)
	s.volumeMu.Lock()
	inventory.DuplicateSuppressions = s.duplicateSuppressions
	s.volumeMu.Unlock()
	return inventory, nil
}

func metadataInventoryCounts(db *sql.DB, query string) ([]metadataInventoryCount, error) {
	rows, err := db.Query(query, metadataEventInventoryLimit)
	if err != nil {
		return nil, err
	}
	counts := []metadataInventoryCount{}
	for rows.Next() {
		var count metadataInventoryCount
		if err := rows.Scan(&count.Key, &count.Count); err != nil {
			_ = rows.Close()
			return nil, err
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return counts, nil
}

func formatMetadataInventoryCounts(counts []metadataInventoryCount) string {
	if len(counts) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, fmt.Sprintf("%s:%d", count.Key, count.Count))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (s *controlMetadataStore) publishChange(hostedID string, revision int64, fields ...string) {
	if s == nil {
		return
	}
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	// Publications can arrive out of order; the latest revision never moves back.
	if s.latestRevisions == nil {
		s.latestRevisions = make(map[string]int64)
	}
	s.latestRevisions[hostedID] = max(s.latestRevisions[hostedID], revision)
	s.changes = append(s.changes, metadataChange{HostedThreadID: hostedID, Revision: revision, Fields: fields})
	if len(s.changes) > 128 {
		s.changes = append([]metadataChange(nil), s.changes[len(s.changes)-128:]...)
	}
}

// latestRevision is the newest canonical revision published for hostedID since
// boot, or 0 when none was. Runtime events carry it so a browser re-reads one
// thread's metadata instead of the whole session list.
func (s *controlMetadataStore) latestRevision(hostedID string) int64 {
	if s == nil {
		return 0
	}
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	return s.latestRevisions[hostedID]
}

func (s *controlMetadataStore) changesAfter(hostedID string, revision int64) []metadataChange {
	if s == nil {
		return nil
	}
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	out := []metadataChange{}
	for _, c := range s.changes {
		if c.HostedThreadID == hostedID && c.Revision > revision {
			out = append(out, c)
		}
	}
	return out
}

type metadataCurrentView = hosted.MetadataCurrent

type hostedMetadataView = hosted.MetadataView
type hostedAttachmentView = hosted.AttachmentView
type hostedMetadataName = hosted.MetadataName
type hostedNativeIDView = hosted.NativeIDView

func validateMetadataEvent(event metadataEvent) error {
	if event.Field != postureFieldModel && event.Field != postureFieldEffort && event.Field != postureFieldPermissionMode && event.Field != postureFieldServiceTier && event.Field != "cwd" && event.Field != orchestrationAssignmentField && event.Field != threads.CreationNameField {
		return fmt.Errorf("unsupported metadata field %q", event.Field)
	}
	if event.Disposition != metadataDispositionRequested && event.Disposition != metadataDispositionConfirmed && event.Disposition != metadataDispositionRejected && event.Disposition != metadataDispositionCleared && event.Disposition != metadataDispositionObserved {
		return fmt.Errorf("invalid metadata disposition %q", event.Disposition)
	}
	if event.Source != postureSourceCreation && event.Source != postureSourceOperator && event.Source != postureSourceProvider && event.Source != metadataSourceMigration {
		return fmt.Errorf("invalid metadata source %q", event.Source)
	}
	if event.Freshness != metadataFreshnessLive && event.Freshness != metadataFreshnessHistorical && event.Freshness != metadataFreshnessReplayed {
		return fmt.Errorf("invalid metadata freshness %q", event.Freshness)
	}
	if event.Field == threads.CreationNameField {
		if event.Source != postureSourceCreation || event.Disposition != metadataDispositionConfirmed || event.Freshness != metadataFreshnessLive {
			return fmt.Errorf("creation name must be a live confirmed creation fact")
		}
		name, ok := event.Value.(string)
		if !ok {
			return fmt.Errorf("creation name value is invalid")
		}
		if _, err := validateCreationName(name); err != nil {
			return err
		}
		return nil
	}
	if event.Field == orchestrationAssignmentField {
		if event.Source != postureSourceCreation && event.Source != postureSourceOperator {
			return fmt.Errorf("orchestration assignment source must be creation or operator")
		}
		if event.Disposition == metadataDispositionCleared {
			if event.Value != nil {
				return fmt.Errorf("cleared orchestration assignment value must be null")
			}
			return nil
		}
		if event.Disposition != metadataDispositionConfirmed {
			return fmt.Errorf("orchestration assignment disposition must be confirmed or cleared")
		}
		assignment, ok := event.Value.(orchestrationAssignment)
		if !ok {
			return fmt.Errorf("orchestration assignment value is invalid")
		}
		if (assignment.AssignmentRole != "worker" && assignment.AssignmentRole != "auditor") || strings.TrimSpace(assignment.Focus) != assignment.Focus || assignment.Focus == "" || len([]byte(assignment.Focus)) > 500 {
			return fmt.Errorf("orchestration assignment is invalid")
		}
		if strings.TrimSpace(assignment.RelatedHostedThreadID) != assignment.RelatedHostedThreadID {
			return fmt.Errorf("orchestration assignment relationship is invalid")
		}
		if (assignment.AssignmentRole == "worker" && len(assignment.AuditTargets) != 0) || len(assignment.AuditTargets) > 16 {
			return fmt.Errorf("orchestration assignment audit targets are invalid")
		}
		return nil
	}
	value, ok := event.Value.(string)
	if !ok {
		return fmt.Errorf("metadata value for %s must be a string", event.Field)
	}
	if event.Disposition == metadataDispositionCleared && value != "" {
		return fmt.Errorf("cleared metadata value must be empty")
	}
	if event.Field == postureFieldServiceTier && (len(value) > 64 || strings.TrimSpace(value) != value) {
		return fmt.Errorf("invalid service tier %q", value)
	}
	// A bounded legacy fold may deliberately establish an empty observed
	// baseline. In particular, an old clear must not revive an earlier effort
	// value simply because the legacy event has no provider source.
	legacyEmptyFold := event.Source == metadataSourceMigration && event.Evidence == "legacy_fold_baseline" && event.Disposition == metadataDispositionObserved
	if event.Field == postureFieldEffort && value == "" && event.Disposition != metadataDispositionCleared && !legacyEmptyFold {
		return fmt.Errorf("empty effort requires cleared disposition")
	}
	return nil
}

func openControlMetadataStore(path string) (*controlMetadataStore, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve control metadata path: %w", err)
	}
	db, err := statedb.Open(context.Background(), abs)
	if err != nil {
		return nil, err
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 0 && version < state.LegacySchemaVersion {
		backupRoot := filepath.Join(filepath.Dir(abs), "threads-schema-backups")
		if err := os.MkdirAll(backupRoot, 0700); err != nil {
			db.Close()
			return nil, err
		}
		backupDir, err := os.MkdirTemp(backupRoot, "before-schema-")
		if err != nil {
			db.Close()
			return nil, err
		}
		if err := threads.Backup(context.Background(), db, filepath.Join(backupDir, "threads.sqlite")); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &controlMetadataStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.initializeEventVolume(); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize canonical metadata instrumentation: %w", err)
	}

	// Browser registry reads can traverse every hosted session. Give those
	// read-only projections a bounded WAL pool so they never monopolize the one
	// serialized control-plane connection used by durable writes such as Codex
	// /fast.
	u := url.URL{Scheme: "file", Path: abs}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = query.Encode()
	readDB, err := sql.Open("sqlite", u.String())
	if err != nil {
		db.Close()
		return nil, err
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(4)
	if err := readDB.Ping(); err != nil {
		readDB.Close()
		db.Close()
		return nil, fmt.Errorf("open canonical metadata read pool: %w", err)
	}
	s.readDB = readDB
	return s, nil
}

func (s *controlMetadataStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	var readErr error
	if s.readDB != nil {
		readErr = s.readDB.Close()
	}
	return errors.Join(readErr, s.db.Close())
}

func (s *controlMetadataStore) readPool() *sql.DB {
	if s != nil && s.readDB != nil {
		return s.readDB
	}
	if s == nil {
		return nil
	}
	return s.db
}

func (s *controlMetadataStore) migrate() error {
	return state.MigrateLegacySchema(s.db, statewiring.Migrations())
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *controlMetadataStore) integrityCheck() error {
	return state.CheckMetadataIntegrity(s.db, statewiring.Migrations())
}

func (s *controlMetadataStore) ensureHostedThread(id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider string) error {
	if s == nil {
		return fmt.Errorf("canonical metadata store unavailable")
	}
	if id == "" || provider == "" || provider == "unknown" {
		return fmt.Errorf("hosted thread id and provider are required")
	}
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	var existingProvider, existingMode, existingInvoker, existingNative, existingInvokerProvider string
	err := s.db.QueryRow(`SELECT provider,presentation_mode,COALESCE(invoker_hosted_id,''),COALESCE(invoker_native_id,''),COALESCE(invoker_provider,'') FROM hosted_threads WHERE hosted_thread_id=?`, id).Scan(&existingProvider, &existingMode, &existingInvoker, &existingNative, &existingInvokerProvider)
	if err == nil {
		if existingProvider != provider || existingInvoker != invokerHosted || existingNative != invokerNative || existingInvokerProvider != invokerProvider {
			return fmt.Errorf("hosted identity %s conflicts with its immutable provider or invocation lineage", id)
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,presentation_mode,invoker_hosted_id,invoker_native_id,invoker_provider,notify_invoker_on_terminal,revision,created_at,updated_at)
		VALUES(?,?,?,?,?,?,0,1,?,?) ON CONFLICT(hosted_thread_id) DO NOTHING`, id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider, now, now)
	if err == nil {
		if changed, _ := result.RowsAffected(); changed == 1 {
			s.publishChange(id, 1, "identity")
		}
	}
	return err
}

// setPresentationMode commits the durable presentation fact. control.sqlite is
// the single authority for a thread's mode; every other copy (execution cache,
// live session, attachment JSON) is a projection synchronized after this
// commit. Same-value writes are revision-neutral so an idempotent PUT cannot
// trigger needless client refetches, and the change is published only when a
// row actually flipped. The caller owns runtime notification ordering.
func (s *controlMetadataStore) setPresentationMode(hostedID, mode string) (revision int64, changed bool, err error) {
	if s == nil || hostedID == "" {
		return 0, false, fmt.Errorf("canonical metadata store unavailable or hosted id missing")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.Exec(`UPDATE hosted_threads SET presentation_mode=?,revision=revision+1,updated_at=? WHERE hosted_thread_id=? AND presentation_mode<>?`, mode, now, hostedID, mode)
	if err != nil {
		return 0, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if err := s.db.QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, hostedID).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("unknown hosted thread %s", hostedID)
		}
		return 0, false, err
	}
	if rows > 0 {
		s.publishChange(hostedID, revision, "presentationMode")
	}
	return revision, rows > 0, nil
}

// The columns remain the authority. The metadata event records history only,
// in the same transaction, without creating another current-state projection.
func (s *controlMetadataStore) setSubscriptions(hostedID string, choices subscriptions.Choices) (revision int64, changed bool, err error) {
	if s == nil || hostedID == "" {
		return 0, false, fmt.Errorf("canonical metadata store unavailable or hosted id missing")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var policy string
	var legacyPrompt bool
	if err := tx.QueryRow(`SELECT terminal_notification_policy,notify_invoker_on_terminal,revision FROM hosted_threads WHERE hosted_thread_id=?`, hostedID).Scan(&policy, &legacyPrompt, &revision); err != nil {
		return 0, false, err
	}
	old := subscriptions.FromStored(policy, legacyPrompt)
	if old == choices {
		return revision, false, nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := tx.QueryRow(`UPDATE hosted_threads SET terminal_notification_policy=?,notify_invoker_on_terminal=?,revision=revision+1,updated_at=? WHERE hosted_thread_id=? RETURNING revision`, subscriptions.StoredPolicy(choices), choices.Prompt == subscriptions.Send, now, hostedID).Scan(&revision); err != nil {
		return 0, false, err
	}
	value, err := json.Marshal(struct {
		Old subscriptions.Choices `json:"old"`
		New subscriptions.Choices `json:"new"`
	}{old, choices})
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(`INSERT INTO thread_metadata_events(hosted_thread_id,field,value_json,disposition,source,evidence,attachment_generation,evidence_freshness,observed_at,committed_at) VALUES(?,?,?,?,?,?,0,?,?,?)`, hostedID, "subscriptions", string(value), metadataDispositionConfirmed, postureSourceOperator, "hosted_thread_subscriptions_put", metadataFreshnessLive, now, now); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	s.publishChange(hostedID, revision, "subscriptions")
	return revision, true, nil
}

// setSidebarPlacement changes only the operational tree shown in Nano. The
// immutable invocation edge remains untouched for family/history inspection.
func (s *controlMetadataStore) setSidebarPlacement(hostedID, placement string) (revision int64, changed bool, err error) {
	if s == nil || hostedID == "" {
		return 0, false, fmt.Errorf("canonical metadata store unavailable or hosted id missing")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.Exec(`UPDATE hosted_threads SET sidebar_placement=?,revision=revision+1,updated_at=? WHERE hosted_thread_id=? AND sidebar_placement<>?`, placement, now, hostedID, placement)
	if err != nil {
		return 0, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if err := s.db.QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, hostedID).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("unknown hosted thread %s", hostedID)
		}
		return 0, false, err
	}
	if rows > 0 {
		s.publishChange(hostedID, revision, "sidebarPlacement")
	}
	return revision, rows > 0, nil
}

// beginTransportBinding advances the attachment generation before a provider
// transport is attached. Passive catalog/import identity observations use
// claimNativeID instead and therefore cannot manufacture fresh evidence.
func (s *controlMetadataStore) beginTransportBinding(hostedID string) (int64, error) {
	if s == nil || hostedID == "" {
		return 0, fmt.Errorf("canonical metadata store unavailable or hosted id missing")
	}
	binding, err := threads.BeginTransportBinding(context.Background(), s.db, hostedID)
	if err != nil {
		return 0, err
	}
	s.publishChange(hostedID, binding.Revision, "attachmentGeneration")
	return binding.Generation, nil
}

// claimNativeID records the currently active provider identity while retaining
// every predecessor for audit. The partial unique index rejects two active
// hosted records claiming the same provider/native identity.
// nativeResolution is one provisional handle's exact native: the session its
// own child announced (its root init, live or in the spool).
type nativeResolution = hosted.NativeResolution

// errUnresolvedProvisional refuses a claim that would retire a provisional
// handle without its exact resolution: prompts accepted under that handle are
// reachable only through it.
var errUnresolvedProvisional = errors.New("provisional native would be retired without its exact resolution")

// resolvedNative is the native a provisional handle of the hosted thread was
// recorded to resolve to, or "" while it has none.
func (s *controlMetadataStore) resolvedNative(ctx context.Context, hostedID, provider, handle string) (string, error) {
	if s == nil || hostedID == "" || handle == "" {
		return "", nil
	}
	var resolved sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT resolved_native_id FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND provider=? AND native_thread_id=?`, hostedID, provider, handle).Scan(&resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return resolved.String, err
}

func (s *controlMetadataStore) claimNativeID(hostedID, provider, nativeID string, provisional bool) error {
	return s.claimNativeIDResolving(hostedID, provider, nativeID, provisional, nil)
}

// claimNativeIDResolving claims nativeID for the hosted thread. Whatever the
// caller, a claim that would retire the current provisional handle must carry
// that handle's exact resolution to nativeID, which it records once in the
// same transaction; a resolution never changes. A claim of the native already
// current with a resolution succeeds only when that resolution is already
// recorded, so a retry after a commit whose report failed is idempotent.
func (s *controlMetadataStore) claimNativeIDResolving(hostedID, provider, nativeID string, provisional bool, pair *nativeResolution) error {
	if s == nil || hostedID == "" || provider == "" || nativeID == "" {
		return fmt.Errorf("hosted id, provider, and native id are required")
	}
	if pair != nil && (pair.Handle == "" || pair.Native != nativeID || provisional) {
		return fmt.Errorf("native resolution %+v does not name the claimed native %s", *pair, nativeID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var hostedProvider, currentNative string
	if err := tx.QueryRow(`SELECT provider,COALESCE(current_native_id,'') FROM hosted_threads WHERE hosted_thread_id=?`, hostedID).Scan(&hostedProvider, &currentNative); err != nil {
		return err
	}
	if hostedProvider != provider {
		return fmt.Errorf("native claim provider %s conflicts with hosted provider %s", provider, hostedProvider)
	}
	if currentNative == nativeID && pair != nil {
		var resolved sql.NullString
		if err := tx.QueryRow(`SELECT resolved_native_id FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND provider=? AND native_thread_id=?`, hostedID, provider, pair.Handle).Scan(&resolved); err != nil && err != sql.ErrNoRows {
			return err
		}
		if !resolved.Valid || resolved.String != nativeID {
			return fmt.Errorf("%w: %s is current but %s has no resolution to it", errUnresolvedProvisional, nativeID, pair.Handle)
		}
		return tx.Commit()
	}
	var currentProvisional bool
	if currentNative != "" && currentNative != nativeID {
		err := tx.QueryRow(`SELECT provisional FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND provider=? AND native_thread_id=? AND retired_at IS NULL`, hostedID, provider, currentNative).Scan(&currentProvisional)
		switch {
		case err == sql.ErrNoRows && nativeThreadIDProvisional(provider, currentNative):
			// No active claim says what this handle is; it is not authority
			// to retire it.
			return fmt.Errorf("%w: current %s has no active claim", errUnresolvedProvisional, currentNative)
		case err != nil && err != sql.ErrNoRows:
			return err
		}
	}
	switch {
	case currentProvisional:
		if pair == nil || pair.Handle != currentNative {
			return fmt.Errorf("%w: claiming %s would retire %s", errUnresolvedProvisional, nativeID, currentNative)
		}
		result, err := tx.Exec(`UPDATE hosted_thread_native_ids SET resolved_native_id=? WHERE hosted_thread_id=? AND provider=? AND native_thread_id=? AND (resolved_native_id IS NULL OR resolved_native_id=?)`,
			nativeID, hostedID, provider, currentNative, nativeID)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return fmt.Errorf("%w: %s already resolves to another native", errUnresolvedProvisional, currentNative)
		}
	case pair != nil:
		return fmt.Errorf("%w: %s is not the current provisional handle", errUnresolvedProvisional, pair.Handle)
	}
	if currentNative == nativeID {
		var existingProvisional bool
		if err := tx.QueryRow(`SELECT provisional FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND provider=? AND native_thread_id=? AND retired_at IS NULL`, hostedID, provider, nativeID).Scan(&existingProvisional); err == nil && existingProvisional == provisional {
			return tx.Commit()
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE hosted_thread_native_ids SET retired_at=? WHERE hosted_thread_id=? AND provider=? AND native_thread_id<>? AND retired_at IS NULL`, now, hostedID, provider, nativeID); err != nil {
		return err
	}
	var activeOwner string
	err = tx.QueryRow(`SELECT hosted_thread_id FROM hosted_thread_native_ids WHERE provider=? AND native_thread_id=? AND retired_at IS NULL`, provider, nativeID).Scan(&activeOwner)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && activeOwner != hostedID {
		return errNativeClaimOwned
	}
	if _, err := tx.Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,provisional,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(hosted_thread_id,provider,native_thread_id) DO UPDATE SET provisional=excluded.provisional,retired_at=NULL,last_seen_at=excluded.last_seen_at`, hostedID, provider, nativeID, provisional, now, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE hosted_threads SET current_native_id=?,revision=revision+1,updated_at=? WHERE hosted_thread_id=?`, nativeID, now, hostedID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var revision int64
	if err := s.db.QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, hostedID).Scan(&revision); err != nil {
		return err
	}
	s.publishChange(hostedID, revision, "nativeThreadId")
	return nil
}

// appendEvent atomically records evidence and reconciles it into one current
// row. Requested/cleared update desired; fresh confirmed/rejected evidence
// updates effective. Historical/replayed provider evidence only establishes an
// observed baseline when effective is still unknown.
func (s *controlMetadataStore) appendEvent(event metadataEvent) (metadataEvent, error) {
	// Callers may reuse a prior return value for a retry. Commit status belongs
	// to this invocation, never to the immutable event row it happened to name.
	event.Committed = false
	if s == nil || event.HostedThreadID == "" || event.Field == "" || event.Source == "" {
		return event, fmt.Errorf("hosted id, field, and source are required")
	}
	if event.Freshness == "" {
		event.Freshness = metadataFreshnessLive
	}
	if err := validateMetadataEvent(event); err != nil {
		return event, err
	}
	if event.ObservedAt == "" {
		event.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	value, err := json.Marshal(event.Value)
	if err != nil {
		return event, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return event, err
	}
	defer tx.Rollback()
	var provider string
	var currentGeneration int64
	if err := tx.QueryRow(`SELECT provider,attachment_generation FROM hosted_threads WHERE hosted_thread_id=?`, event.HostedThreadID).Scan(&provider, &currentGeneration); err != nil {
		if err == sql.ErrNoRows {
			return event, fmt.Errorf("unknown hosted thread %s", event.HostedThreadID)
		}
		return event, err
	}
	if provider == "" || provider == "unknown" {
		return event, fmt.Errorf("hosted thread %s has invalid provider", event.HostedThreadID)
	}
	if event.SourceEventKey != "" {
		var prior metadataEvent
		var priorValue string
		var priorCausation sql.NullInt64
		err := tx.QueryRow(`SELECT event_id,hosted_thread_id,field,value_json,disposition,source,COALESCE(evidence,''),COALESCE(native_thread_id,''),COALESCE(nano_session_id,''),attachment_generation,evidence_freshness,causation_event_id FROM thread_metadata_events WHERE source_event_key=?`, event.SourceEventKey).Scan(&prior.ID, &prior.HostedThreadID, &prior.Field, &priorValue, &prior.Disposition, &prior.Source, &prior.Evidence, &prior.NativeThreadID, &prior.NanoSessionID, &prior.AttachmentGeneration, &prior.Freshness, &priorCausation)
		if err == nil {
			prior.Value = json.RawMessage(priorValue)
			if priorCausation.Valid {
				id := priorCausation.Int64
				prior.CausationEventID = &id
			}
			if prior.HostedThreadID != event.HostedThreadID || prior.Field != event.Field || priorValue != string(value) || prior.Disposition != event.Disposition || prior.Source != event.Source || prior.Evidence != event.Evidence || prior.NativeThreadID != event.NativeThreadID || prior.NanoSessionID != event.NanoSessionID || prior.AttachmentGeneration != event.AttachmentGeneration || prior.Freshness != event.Freshness || !sameCausation(prior.CausationEventID, event.CausationEventID) {
				if event.Field == orchestrationAssignmentField {
					return event, fmt.Errorf("%w: source_event_key %q has different assignment content", errAssignmentIdempotencyConflict, event.SourceEventKey)
				}
				return event, fmt.Errorf("source_event_key %q conflicts with different metadata evidence", event.SourceEventKey)
			}
			event.ID = prior.ID
			if err := tx.Commit(); err != nil {
				return event, err
			}
			s.noteDuplicateSuppression()
			return event, nil
		} else if err != sql.ErrNoRows {
			return event, err
		}
	}
	// Replayed runtime observations must not grow the log. A caller/operator
	// action is intentionally exempt: matching choices are still audit events.
	// Every re-attach re-reports the provider's settings, so the same fact at a
	// later attachment generation or session is the current row seen again,
	// not a change: recording it bumped the revision that makes every open tab
	// re-fetch the thread. A causal answer to a request is never a repeat.
	if event.Source != postureSourceOperator {
		var duplicate int64
		column := "desired_event_id"
		if event.Source == postureSourceProvider {
			column = "effective_event_id"
		}
		query := fmt.Sprintf(`SELECT e.event_id FROM thread_metadata_current c JOIN thread_metadata_events e ON e.event_id=c.%s WHERE c.hosted_thread_id=? AND c.field=? AND e.value_json=? AND e.disposition=? AND e.source=? AND e.evidence_freshness=? AND (? IS NULL OR e.causation_event_id=?)`, column)
		err := tx.QueryRow(query, event.HostedThreadID, event.Field, string(value), event.Disposition, event.Source, event.Freshness, event.CausationEventID, event.CausationEventID).Scan(&duplicate)
		if err == nil {
			event.ID = duplicate
			if err := tx.Commit(); err != nil {
				return event, err
			}
			s.noteDuplicateSuppression()
			return event, nil
		}
		if err != sql.ErrNoRows {
			return event, err
		}
	}
	var sourceEventKey any
	if event.SourceEventKey != "" {
		sourceEventKey = event.SourceEventKey
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.Exec(`INSERT INTO thread_metadata_events(hosted_thread_id,field,value_json,disposition,source,evidence,native_thread_id,nano_session_id,attachment_generation,evidence_freshness,causation_event_id,source_event_key,observed_at,committed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.HostedThreadID, event.Field, string(value), event.Disposition, event.Source, event.Evidence, event.NativeThreadID, event.NanoSessionID, event.AttachmentGeneration, event.Freshness, event.CausationEventID, sourceEventKey, event.ObservedAt, now)
	if err != nil {
		return event, err
	}
	event.ID, _ = result.LastInsertId()
	event.CommittedAt = now
	var desiredValue, desiredDisposition, desiredSource sql.NullString
	var desiredID, desiredSettlementID sql.NullInt64
	var effectiveValue, effectiveDisposition, effectiveSource sql.NullString
	var effectiveID sql.NullInt64
	err = tx.QueryRow(`SELECT desired_value_json,desired_disposition,desired_source,desired_event_id,desired_settlement_event_id,effective_value_json,effective_disposition,effective_source,effective_event_id FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, event.HostedThreadID, event.Field).Scan(&desiredValue, &desiredDisposition, &desiredSource, &desiredID, &desiredSettlementID, &effectiveValue, &effectiveDisposition, &effectiveSource, &effectiveID)
	if err != nil && err != sql.ErrNoRows {
		return event, err
	}
	desiredIsHistorical := false
	if desiredID.Valid {
		var desiredFreshness string
		if err := tx.QueryRow(`SELECT evidence_freshness FROM thread_metadata_events WHERE event_id=?`, desiredID.Int64).Scan(&desiredFreshness); err != nil {
			return event, err
		}
		desiredIsHistorical = desiredFreshness == metadataFreshnessHistorical || desiredFreshness == metadataFreshnessReplayed
	}
	// Historical JSON intent can establish an old baseline, but it is never
	// allowed to displace a live desired decision or a provider-confirmed value
	// that arrived before a restarted migration completed.
	strongEffective := effectiveID.Valid && effectiveSource.Valid && effectiveSource.String == postureSourceProvider && effectiveDisposition.Valid && effectiveDisposition.String == metadataDispositionConfirmed
	allowDesired := event.Freshness == metadataFreshnessLive || (!desiredID.Valid && !strongEffective) || desiredIsHistorical
	isRelationship := metadataConfirmedFact(event.Field)
	if (event.Disposition == metadataDispositionRequested || event.Disposition == metadataDispositionCleared || (isRelationship && event.Disposition == metadataDispositionConfirmed)) && allowDesired {
		desiredValue, desiredDisposition, desiredSource, desiredID = sql.NullString{String: string(value), Valid: true}, sql.NullString{String: event.Disposition, Valid: true}, sql.NullString{String: event.Source, Valid: true}, sql.NullInt64{Int64: event.ID, Valid: true}
		desiredSettlementID = sql.NullInt64{}
	}
	if isRelationship && event.Disposition == metadataDispositionConfirmed {
		effectiveValue, effectiveDisposition, effectiveSource, effectiveID = sql.NullString{String: string(value), Valid: true}, sql.NullString{String: metadataDispositionConfirmed, Valid: true}, sql.NullString{String: event.Source, Valid: true}, sql.NullInt64{Int64: event.ID, Valid: true}
	}
	causallyLinked := event.CausationEventID != nil && desiredID.Valid && *event.CausationEventID == desiredID.Int64
	if causallyLinked && event.Disposition == metadataDispositionConfirmed {
		desiredDisposition = sql.NullString{String: metadataDispositionConfirmed, Valid: true}
		desiredSettlementID = sql.NullInt64{Int64: event.ID, Valid: true}
	}
	if causallyLinked && event.Disposition == metadataDispositionRejected {
		desiredDisposition = sql.NullString{String: metadataDispositionRejected, Valid: true}
		desiredSettlementID = sql.NullInt64{Int64: event.ID, Valid: true}
	}
	freshProvider := event.Source == postureSourceProvider && event.Freshness == metadataFreshnessLive && (event.AttachmentGeneration == currentGeneration || causallyLinked) && event.Disposition == metadataDispositionConfirmed
	historicalObservation := event.Source == postureSourceProvider || event.Disposition == metadataDispositionObserved
	// The migration fold is a bounded snapshot of old JSON. It may be replayed
	// with a later suffix after an interruption, so it may replace only its own
	// earlier observed baseline. It can never displace provider-confirmed state.
	priorMigrationBaseline := effectiveID.Valid && effectiveSource.Valid && (effectiveSource.String == metadataSourceMigration || effectiveSource.String == postureSourceProvider) && effectiveDisposition.Valid && effectiveDisposition.String == metadataDispositionObserved
	legacyFoldBaseline := event.Source == metadataSourceMigration && event.Evidence == "legacy_fold_baseline"
	baseline := historicalObservation && (event.Freshness == metadataFreshnessHistorical || event.Freshness == metadataFreshnessReplayed) && (!effectiveID.Valid || (legacyFoldBaseline && priorMigrationBaseline)) && (!desiredID.Valid || desiredIsHistorical)
	if freshProvider || baseline {
		disposition := event.Disposition
		if baseline {
			disposition = metadataDispositionObserved
		}
		effectiveValue, effectiveDisposition, effectiveSource, effectiveID = sql.NullString{String: string(value), Valid: true}, sql.NullString{String: disposition, Valid: true}, sql.NullString{String: event.Source, Valid: true}, sql.NullInt64{Int64: event.ID, Valid: true}
	}
	var revision int64
	if err := tx.QueryRow(`UPDATE hosted_threads SET revision=revision+1,updated_at=? WHERE hosted_thread_id=? RETURNING revision`, now, event.HostedThreadID).Scan(&revision); err != nil {
		return event, err
	}
	_, err = tx.Exec(`INSERT INTO thread_metadata_current(hosted_thread_id,field,desired_value_json,desired_disposition,desired_source,desired_event_id,desired_settlement_event_id,effective_value_json,effective_disposition,effective_source,effective_event_id,thread_revision,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hosted_thread_id,field) DO UPDATE SET desired_value_json=excluded.desired_value_json,desired_disposition=excluded.desired_disposition,desired_source=excluded.desired_source,desired_event_id=excluded.desired_event_id,desired_settlement_event_id=excluded.desired_settlement_event_id,effective_value_json=excluded.effective_value_json,effective_disposition=excluded.effective_disposition,effective_source=excluded.effective_source,effective_event_id=excluded.effective_event_id,thread_revision=excluded.thread_revision,updated_at=excluded.updated_at`, event.HostedThreadID, event.Field, nullableString(desiredValue), nonnullDisposition(desiredDisposition), nullableString(desiredSource), nullableInt(desiredID), nullableInt(desiredSettlementID), nullableString(effectiveValue), nonnullDisposition(effectiveDisposition), nullableString(effectiveSource), nullableInt(effectiveID), revision, now)
	if err != nil {
		return event, err
	}
	if err := tx.Commit(); err != nil {
		return event, err
	}
	event.Committed = true
	s.publishChange(event.HostedThreadID, revision, event.Field)
	s.noteEventVolume(event.HostedThreadID, event.Field)
	return event, nil
}

func sameCausation(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func nullableString(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}
func nullableInt(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}
func nonnullDisposition(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return metadataDispositionUnknown
}

// projectMetadataDisplay centralizes the server-authored display projection.
// A live requested or cleared decision is pending and wins. Historical and
// replayed intent remains audit/resume state, but yields to an effective
// observation when one exists. With no effective observation it remains the
// best available historical baseline. Empty values are meaningful clears.
func projectMetadataDisplay(entry *metadataCurrentView, desiredFreshness, effectiveFreshness string) {
	entry.DisplayDisposition = metadataDispositionUnknown
	entry.DisplayValue = nil
	entry.DisplaySource = ""
	entry.DisplayFreshness = ""

	useDesired := entry.DesiredEventID != nil &&
		(entry.DesiredDisposition == metadataDispositionRequested || entry.DesiredDisposition == metadataDispositionCleared) &&
		(desiredFreshness == metadataFreshnessLive || len(entry.EffectiveValue) == 0)
	if useDesired {
		entry.DisplayValue = append(json.RawMessage(nil), entry.DesiredValue...)
		entry.DisplayDisposition = entry.DesiredDisposition
		entry.DisplaySource = entry.DesiredSource
		entry.DisplayFreshness = desiredFreshness
		return
	}
	if len(entry.EffectiveValue) != 0 {
		entry.DisplayValue = append(json.RawMessage(nil), entry.EffectiveValue...)
		entry.DisplayDisposition = entry.EffectiveDisposition
		entry.DisplaySource = entry.EffectiveSource
		entry.DisplayFreshness = effectiveFreshness
	}
}

// posture is the canonical display projection from the thread's view.
// Empty/unknown stays empty, never a product default.
func (s *controlMetadataStore) posture(hostedID string) sessionPosture {
	if hostedID == "" {
		return sessionPosture{}
	}
	view, err := s.view(hostedID)
	if err != nil {
		return sessionPosture{}
	}
	return postureFromFields(view.Fields)
}

func postureFromFields(fields map[string]metadataCurrentView) sessionPosture {
	p := sessionPosture{}
	for field, entry := range fields {
		var decoded string
		if len(entry.DisplayValue) == 0 || json.Unmarshal(entry.DisplayValue, &decoded) != nil {
			continue
		}
		switch field {
		case postureFieldModel:
			p.Model = decoded
		case postureFieldEffort:
			p.Effort = decoded
		case postureFieldPermissionMode:
			p.PermissionMode = decoded
		case postureFieldServiceTier:
			p.ServiceTier = decoded
		}
	}
	return p
}

// view reads one thread's committed metadata from the read pool. It is a
// standalone read, never part of a write transaction, so it need not queue for
// the one writer connection behind durable writes and other reads.
func (s *controlMetadataStore) view(hostedID string) (hostedMetadataView, error) {
	return s.viewFrom(s.readPool(), hostedID)
}

func (s *controlMetadataStore) viewFrom(db *sql.DB, hostedID string) (hostedMetadataView, error) {
	views, err := s.viewsFrom(db, []string{hostedID})
	if err != nil {
		return hostedMetadataView{}, err
	}
	view, ok := views[hostedID]
	if !ok {
		return hostedMetadataView{}, sql.ErrNoRows
	}
	return view, nil
}

// viewsFrom reads the canonical views of many hosted threads: three reads per
// metadataReadChunk ids, never one set per thread. An id with no hosted
// record is absent.
func (s *controlMetadataStore) viewsFrom(db *sql.DB, hostedIDs []string) (map[string]hostedMetadataView, error) {
	if s == nil {
		return nil, fmt.Errorf("canonical metadata unavailable")
	}
	views := make(map[string]hostedMetadataView, len(hostedIDs))
	for start := 0; start < len(hostedIDs); start += metadataReadChunk {
		chunk := hostedIDs[start:min(start+metadataReadChunk, len(hostedIDs))]
		in, args := "(?"+strings.Repeat(",?", len(chunk)-1)+")", make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		threads, err := db.Query(`SELECT hosted_thread_id,revision,provider,COALESCE(current_native_id,''),attachment_generation,presentation_mode,sidebar_placement,COALESCE(invoker_hosted_id,''),COALESCE(invoker_native_id,''),COALESCE(invoker_provider,''),notify_invoker_on_terminal,terminal_notification_policy,created_at,updated_at FROM hosted_threads WHERE hosted_thread_id IN `+in, args...)
		if err != nil {
			return nil, err
		}
		for threads.Next() {
			v := hostedMetadataView{Fields: map[string]metadataCurrentView{}}
			var legacyPrompt bool
			var storedPolicy string
			if err := threads.Scan(&v.HostedThreadID, &v.Revision, &v.Provider, &v.NativeThreadID, &v.AttachmentGeneration, &v.PresentationMode, &v.SidebarPlacement, &v.InvokerHostedThreadID, &v.InvokerNativeThreadID, &v.InvokerProvider, &legacyPrompt, &storedPolicy, &v.CreatedAt, &v.UpdatedAt); err != nil {
				threads.Close()
				return nil, err
			}
			v.Subscriptions = subscriptions.FromStored(storedPolicy, legacyPrompt)
			views[v.HostedThreadID] = v
		}
		if err := errors.Join(threads.Err(), threads.Close()); err != nil {
			return nil, err
		}
		history, err := db.Query(`SELECT hosted_thread_id,provider,native_thread_id,provisional,COALESCE(retired_at,''),first_seen_at,last_seen_at FROM hosted_thread_native_ids WHERE hosted_thread_id IN `+in+` ORDER BY hosted_thread_id,first_seen_at,native_thread_id`, args...)
		if err != nil {
			return nil, err
		}
		for history.Next() {
			var hostedID string
			var item hostedNativeIDView
			if err := history.Scan(&hostedID, &item.Provider, &item.NativeThreadID, &item.Provisional, &item.RetiredAt, &item.FirstSeenAt, &item.LastSeenAt); err != nil {
				history.Close()
				return nil, err
			}
			if v, ok := views[hostedID]; ok {
				v.NativeIDHistory = append(v.NativeIDHistory, item)
				views[hostedID] = v
			}
		}
		if err := errors.Join(history.Err(), history.Close()); err != nil {
			return nil, err
		}
		rows, err := db.Query(`SELECT c.hosted_thread_id,c.field,c.desired_value_json,c.desired_disposition,c.desired_source,c.desired_event_id,c.desired_settlement_event_id,c.effective_value_json,c.effective_disposition,c.effective_source,c.effective_event_id,c.updated_at,
		COALESCE(d.evidence_freshness,''),COALESCE(e.evidence_freshness,'')
		FROM thread_metadata_current c
		LEFT JOIN thread_metadata_events d ON d.event_id=c.desired_event_id
		LEFT JOIN thread_metadata_events e ON e.event_id=c.effective_event_id
		WHERE c.hosted_thread_id IN `+in, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var hostedID, field string
			var dVal, dDisp, dSource, eVal, eDisp, eSource, updated sql.NullString
			var desiredFreshness, effectiveFreshness string
			var dID, settlementID, eID sql.NullInt64
			if err := rows.Scan(&hostedID, &field, &dVal, &dDisp, &dSource, &dID, &settlementID, &eVal, &eDisp, &eSource, &eID, &updated, &desiredFreshness, &effectiveFreshness); err != nil {
				rows.Close()
				return nil, err
			}
			entry := metadataCurrentView{DesiredDisposition: nonnullDisposition(dDisp), DesiredSource: dSource.String, EffectiveDisposition: nonnullDisposition(eDisp), EffectiveSource: eSource.String, UpdatedAt: updated.String}
			if dVal.Valid {
				entry.DesiredValue = json.RawMessage(dVal.String)
			}
			if dID.Valid {
				id := dID.Int64
				entry.DesiredEventID = &id
			}
			if settlementID.Valid {
				id := settlementID.Int64
				entry.DesiredSettlementEventID = &id
			}
			if eVal.Valid {
				entry.EffectiveValue = json.RawMessage(eVal.String)
			}
			if eID.Valid {
				id := eID.Int64
				entry.EffectiveEventID = &id
			}
			projectMetadataDisplay(&entry, desiredFreshness, effectiveFreshness)
			if v, ok := views[hostedID]; ok {
				v.Fields[field] = entry
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return views, nil
}

// metadataReadChunk bounds one many-ids metadata read, well under SQLite's
// parameter limit.
const metadataReadChunk = 500

func (s *controlMetadataStore) destroy(hostedID string) error {
	if s == nil {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM hosted_threads WHERE hosted_thread_id=?`, hostedID)
	return err
}

// importLegacyAttachments imports attachment-only facts as historical
// observations. They establish a baseline only where stronger current
// provider/desired evidence does not already exist.
func (s *controlMetadataStore) importLegacyAttachments(records []attachmentRecord) error {
	return s.importLegacyAttachmentsUntil(records, -1)
}

// importAndScrubLegacyAttachments completes the read-only attachment bridge
// before removing retired posture fields from its JSON copy. The two stores
// cannot share a transaction, so a failed canonical import deliberately
// leaves attachments.json untouched for diagnosis and retry.
func importAndScrubLegacyAttachments(control *controlMetadataStore, attachments *attachmentStore) error {
	if control == nil {
		return fmt.Errorf("canonical metadata store unavailable")
	}
	if err := control.importLegacyAttachments(attachments.records()); err != nil {
		return err
	}
	return attachments.scrubRetiredMetadata()
}

// reconcileLegacyAttachmentClaims makes the attachment store's current
// transport owner authoritative during the one-way legacy import. A normal
// live claim must never steal another hosted thread's active native ID, but a
// legacy attachment can legitimately be newer than the canonical baseline.
// Retiring the old claim and activating the attachment owner in one
// transaction preserves the same single-owner invariant throughout startup.
func (s *controlMetadataStore) reconcileLegacyAttachmentClaims(records []attachmentRecord) error {
	if s == nil {
		return fmt.Errorf("canonical metadata store unavailable")
	}
	type claimGroup struct {
		provider string
		nativeID string
		records  []attachmentRecord
	}
	groups := make(map[string]*claimGroup)
	for _, record := range records {
		if record.HostedThreadID == "" || record.Provider == "" || record.NativeThreadID == "" {
			continue
		}
		key := record.Provider + "\x00" + record.NativeThreadID
		group := groups[key]
		if group == nil {
			group = &claimGroup{provider: record.Provider, nativeID: record.NativeThreadID}
			groups[key] = group
		}
		group.records = append(group.records, record)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range keys {
		group := groups[key]
		winner := chooseLegacyAttachmentClaim(group.records)
		if _, err := tx.Exec(`UPDATE hosted_thread_native_ids SET retired_at=COALESCE(retired_at,last_seen_at) WHERE provider=? AND native_thread_id=? AND retired_at IS NULL`, group.provider, group.nativeID); err != nil {
			return err
		}
		for _, record := range group.records {
			observedAt := legacyAttachmentClaimTime(record)
			retiredAt := any(observedAt)
			if record.HostedThreadID == winner.HostedThreadID {
				retiredAt = nil
			}
			if _, err := tx.Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,provisional,retired_at,first_seen_at,last_seen_at)
				VALUES(?,?,?,?,?,?,?) ON CONFLICT(hosted_thread_id,provider,native_thread_id) DO UPDATE SET provisional=excluded.provisional,retired_at=excluded.retired_at,last_seen_at=excluded.last_seen_at`, record.HostedThreadID, group.provider, group.nativeID, record.provisional(), retiredAt, observedAt, observedAt); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE hosted_threads SET current_native_id=? WHERE hosted_thread_id=?`, group.nativeID, record.HostedThreadID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func chooseLegacyAttachmentClaim(records []attachmentRecord) attachmentRecord {
	ordered := append([]attachmentRecord(nil), records...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := legacyAttachmentClaimTime(ordered[i]), legacyAttachmentClaimTime(ordered[j])
		if a != b {
			return a > b
		}
		return ordered[i].HostedThreadID < ordered[j].HostedThreadID
	})
	return ordered[0]
}

func legacyAttachmentClaimTime(record attachmentRecord) string {
	if observedAt, err := time.Parse(time.RFC3339Nano, record.AttachedAt); err == nil {
		return observedAt.UTC().Format(time.RFC3339Nano)
	}
	return "1970-01-01T00:00:00Z"
}

// importLegacyAttachmentsUntil is the bounded/restartable attachment bridge.
// limit counts imported facts, not input rows, so an interruption never marks
// completion before every independently idempotent fact has committed.
func (s *controlMetadataStore) importLegacyAttachmentsUntil(records []attachmentRecord, limit int) error {
	if s == nil {
		return fmt.Errorf("canonical metadata store unavailable")
	}
	keys := make([]string, 0, len(records))
	processed := 0
	for _, r := range records {
		keys = append(keys, r.HostedThreadID+"\x00"+r.Provider+"\x00"+r.NativeThreadID+"\x00"+r.Cwd+"\x00"+r.Model+"\x00"+r.TurnServiceTier)
	}
	sort.Strings(keys)
	fingerprint := agentHash([]byte(fmt.Sprintf("%q", keys)))
	var done string
	err := s.db.QueryRow(`SELECT source_fingerprint FROM control_migrations WHERE name='legacy-attachments-v1'`).Scan(&done)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if done == fingerprint {
		return s.integrityCheck()
	}
	for _, r := range records {
		if r.HostedThreadID == "" || r.Provider == "" {
			continue
		}
		if err := s.ensureHostedThread(r.HostedThreadID, r.Provider, r.PresentationMode, r.InvokerHostedThreadID, r.InvokerNativeThreadID, r.InvokerProvider); err != nil {
			return err
		}
	}
	if err := s.reconcileLegacyAttachmentClaims(records); err != nil {
		return err
	}
	for _, r := range records {
		if r.HostedThreadID == "" || r.Provider == "" {
			continue
		}
		for _, fact := range []postureFieldValue{{"cwd", r.Cwd}, {postureFieldModel, r.Model}, {postureFieldServiceTier, r.TurnServiceTier}} {
			if fact.value == "" {
				continue
			}
			keyMaterial := r.HostedThreadID + "\x00" + r.Provider + "\x00" + r.NativeThreadID + "\x00" + fact.field + "\x00" + fact.value
			entry := metadataEvent{HostedThreadID: r.HostedThreadID, Field: fact.field, Value: fact.value, Disposition: metadataDispositionObserved, Source: metadataSourceMigration, Evidence: "legacy_attachment_baseline", NativeThreadID: r.NativeThreadID, Freshness: metadataFreshnessHistorical, SourceEventKey: "legacy-attachment:" + agentHash([]byte(keyMaterial))}
			if _, err := s.appendEvent(entry); err != nil {
				return err
			}
			processed++
			if limit >= 0 && processed >= limit {
				return fmt.Errorf("legacy attachment import interrupted")
			}
		}
	}
	return s.markMigrationComplete("legacy-attachments-v1", fingerprint)
}

func (s *controlMetadataStore) markMigrationComplete(name, fingerprint string) error {
	_, err := s.db.Exec(`INSERT INTO control_migrations(name,source_fingerprint,completed_at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET source_fingerprint=excluded.source_fingerprint,completed_at=excluded.completed_at`, name, fingerprint, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return s.integrityCheck()
}

// canonicalDatabase supplies the shared transaction owner when composing the
// prompt and lifecycle stores; it never opens another database.
func (s *controlMetadataStore) canonicalDatabase() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}
