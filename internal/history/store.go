package history

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	statedb "github.com/dkroshow/nano/internal/state/db"
	_ "modernc.org/sqlite"
)

// Store owns Nano's durable read model. It never opens provider-owned files;
// callers pass discovery/projection evidence into its transaction methods.
type Store struct {
	db                  *sql.DB
	writeMu             timedWriteMutex
	backgroundOnce      sync.Once
	backgroundLane      chan struct{}
	backgroundIdleUntil time.Time

	// counts caches IndexStatus's interaction tally. Every write that adds,
	// removes or reclassifies a thread bumps catalog_version in the same
	// transaction, so the tally is recounted only when the version moves.
	countsMu      sync.Mutex
	counts        InteractionCounts
	countsVersion uint64
	countsValid   bool

	resident residentContinuations
}

// Provisional; set from the active-thread copy test.
const backgroundWriteTarget = 250 * time.Millisecond

// All write paths use this mutex, including the rare store-wide search resets.
// Caller capture happens after acquisition, so the diagnostic measures holds only.
type timedWriteMutex struct {
	sync.Mutex
	started time.Time
	caller  uintptr

	// A slow hold logs at most once a minute; the line carries how many more
	// came since the previous one. Guarded by the mutex itself.
	slowLogged  time.Time
	slowSkipped int
	slowLongest time.Duration
}

const slowWriteHold, slowWriteLogInterval = 250 * time.Millisecond, time.Minute

func (m *timedWriteMutex) Lock() {
	m.Mutex.Lock()
	m.started = time.Now()
	m.caller, _, _, _ = runtime.Caller(1)
	if fn := runtime.FuncForPC(m.caller); fn != nil && strings.HasSuffix(fn.Name(), ".backgroundWrite") {
		m.caller, _, _, _ = runtime.Caller(2)
	}
}

func (m *timedWriteMutex) Unlock() {
	now := time.Now()
	held, caller := now.Sub(m.started), m.caller
	logIt, skipped, longest := false, 0, time.Duration(0)
	if held > slowWriteHold {
		if now.Sub(m.slowLogged) >= slowWriteLogInterval {
			logIt, skipped, longest = true, m.slowSkipped, m.slowLongest
			m.slowLogged, m.slowSkipped, m.slowLongest = now, 0, 0
		} else {
			m.slowSkipped++
			m.slowLongest = max(m.slowLongest, held)
		}
	}
	m.Mutex.Unlock()
	if !logIt {
		return
	}
	name := "unknown"
	if fn := runtime.FuncForPC(caller); fn != nil {
		name = fn.Name()
	}
	if skipped == 0 {
		log.Printf("history slow write hold caller=%s duration=%s", name, held)
		return
	}
	log.Printf("history slow write hold caller=%s duration=%s; %d more since the last line, longest %s", name, held, skipped, longest)
}

// backgroundWrite admits only one background waiter and idles the lane for
// at least the preceding hold. Foreground writers do not take this token.
func (s *Store) backgroundWrite(ctx context.Context) (func(), error) {
	s.backgroundOnce.Do(func() { s.backgroundLane = make(chan struct{}, 1) })
	select {
	case s.backgroundLane <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if wait := time.Until(s.backgroundIdleUntil); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			<-s.backgroundLane
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	s.writeMu.Lock()
	started := time.Now()
	return func() {
		held := time.Since(started)
		s.writeMu.Unlock()
		s.backgroundIdleUntil = time.Now().Add(held)
		<-s.backgroundLane
	}, nil
}

const historyJournalSizeLimit = 256 << 20

func Open(path string) (*Store, error) {
	db, err := statedb.Open(context.Background(), path, fmt.Sprintf("journal_size_limit(%d)", historyJournalSizeLimit))
	if err != nil {
		return nil, fmt.Errorf("open history database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// OpenReadOnly opens an existing history store without migrations, journal
// changes, permission changes, or directory creation. It is the inspection
// boundary for corpus checks against the live database.
func OpenReadOnly(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("history database path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve history database path: %w", err)
	}
	u := url.URL{Scheme: "file", Path: abs}
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", fmt.Sprintf("journal_size_limit(%d)", historyJournalSizeLimit))
	query.Add("_pragma", "query_only(1)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open read-only history database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open read-only history database: %w", err)
	}
	return &Store{db: db}, nil
}

// OpenMaintenance opens an existing current-schema store for bounded repair.
// It never creates a file, migrates schema, or changes journal mode.
func OpenMaintenance(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("history database path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve history database path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("inspect history database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("history database is not a regular file")
	}
	u := url.URL{Scheme: "file", Path: abs}
	query := u.Query()
	query.Set("mode", "rw")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", fmt.Sprintf("journal_size_limit(%d)", historyJournalSizeLimit))
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open maintenance history database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open maintenance history database: %w", err)
	}
	var schemaVersion int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&schemaVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("inspect maintenance history schema: %w", err)
	}
	if schemaVersion != 28 {
		db.Close()
		return nil, fmt.Errorf("history database schema is %d, want 28", schemaVersion)
	}
	return &Store{db: db}, nil
}

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("enable history WAL: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin history migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
		return fmt.Errorf("apply history schema v1: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (1, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v1: %w", err)
	}
	if err := migrateFixedWidthTimes(ctx, tx); err != nil {
		return err
	}
	if err := migrateTranscriptV3(ctx, tx); err != nil {
		return err
	}
	if err := migrateTranscriptEvidenceHashesV4(ctx, tx); err != nil {
		return err
	}
	if err := migrateInteractionColumnV5(ctx, tx); err != nil {
		return err
	}
	if err := migrateThreadNamesV6(ctx, tx); err != nil {
		return err
	}
	if err := migrateTurnUsageV7(ctx, tx); err != nil {
		return err
	}
	if err := migrateCodexWorkspaceResolutionsV8(ctx, tx); err != nil {
		return err
	}
	if err := migrateClaudeWorkspaceResolutionsV9(ctx, tx); err != nil {
		return err
	}
	if err := migrateConversationSearchV10(ctx, tx); err != nil {
		return err
	}
	if err := migrateTurnUsageContractV11(ctx, tx); err != nil {
		return err
	}
	if err := migrateTurnUsageModelEffortV12(ctx, tx); err != nil {
		return err
	}
	if err := migrateTurnUsageServiceTierV13(ctx, tx); err != nil {
		return err
	}
	if err := migrateCanonicalItemAliasesV14(ctx, tx); err != nil {
		return err
	}
	if err := migrateCanonicalBodyRefsV15(ctx, tx); err != nil {
		return err
	}
	if err := migrateCanonicalAliasConflictsV16(ctx, tx); err != nil {
		return err
	}
	if err := migrateParserIdentityReceiptsV17(ctx, tx); err != nil {
		return err
	}
	if err := migrateTranscriptObservationsV18(ctx, tx); err != nil {
		return err
	}
	if err := migrateParsedItemModelV19(ctx, tx); err != nil {
		return err
	}
	if err := migrateParsedTurnModelV20(ctx, tx); err != nil {
		return err
	}
	if err := migrateParserDiagnosticsV21(ctx, tx); err != nil {
		return err
	}
	if err := migrateTranscriptReadOffsetV22(ctx, tx); err != nil {
		return err
	}
	if err := migrateSupersededProjectionIndexV23(ctx, tx); err != nil {
		return err
	}
	if err := migrateParserStateEntriesV24(ctx, tx); err != nil {
		return err
	}
	if err := migrateParserChecksV25(ctx, tx); err != nil {
		return err
	}
	if err := migrateTurnItemIndexV26(ctx, tx); err != nil {
		return err
	}
	if err := migrateMessagePageIndexV27(ctx, tx); err != nil {
		return err
	}
	if err := migrateParserCompactionV28(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit history schema v1: %w", err)
	}
	return nil
}

// migrateParserCompactionV28 marks which active checkpoints the background
// compaction pass has checked under the reader's retention rule.
func migrateParserCompactionV28(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=28`).Scan(&applied); err != nil {
		return err
	}
	if applied != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE conversation_projections ADD COLUMN parser_retention_checked_at TEXT`); err != nil {
		return fmt.Errorf("apply parser compaction v28: %w", err)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES (28,?)`, formatTime(time.Now().UTC()))
	return err
}

func migrateMessagePageIndexV27(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV27); err != nil {
		return fmt.Errorf("apply message page index v27: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES (27,?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record message page index v27: %w", err)
	}
	return nil
}

func migrateTurnItemIndexV26(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV26); err != nil {
		return fmt.Errorf("apply turn item index v26: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES (26,?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record turn item index v26: %w", err)
	}
	return nil
}

func migrateParserChecksV25(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV25); err != nil {
		return fmt.Errorf("apply parser checks v25: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES (25,?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record parser checks v25: %w", err)
	}
	return nil
}

func migrateParserStateEntriesV24(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV24); err != nil {
		return fmt.Errorf("apply parser state entries v24: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES (24,?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record parser state entries v24: %w", err)
	}
	return nil
}

func migrateSupersededProjectionIndexV23(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV23); err != nil {
		return fmt.Errorf("apply superseded projection index v23: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES (23,?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record superseded projection index v23: %w", err)
	}
	return nil
}

func migrateParserIdentityReceiptsV17(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV17); err != nil {
		return fmt.Errorf("apply parser identity receipts v17: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (17, ?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record parser identity receipts v17: %w", err)
	}
	return nil
}

func migrateTranscriptObservationsV18(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=18`).Scan(&applied); err != nil {
		return fmt.Errorf("read transcript observations migration: %w", err)
	}
	if applied != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, schemaV18); err != nil {
		return fmt.Errorf("apply transcript observations v18: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (18, ?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record transcript observations v18: %w", err)
	}
	return nil
}

func migrateParsedItemModelV19(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=19`).Scan(&applied); err != nil {
		return fmt.Errorf("read parsed item migration: %w", err)
	}
	if applied != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE conversation_items ADD COLUMN parsed_json BLOB NOT NULL DEFAULT '{}'`); err != nil {
		return fmt.Errorf("apply parsed item model v19: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (19, ?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record parsed item model v19: %w", err)
	}
	return nil
}

func migrateParsedTurnModelV20(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=20`).Scan(&applied); err != nil {
		return err
	}
	if applied != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, schemaV20); err != nil {
		return fmt.Errorf("apply parsed turn model v20: %w", err)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (20, ?)`, formatTime(time.Now().UTC()))
	return err
}

func migrateParserDiagnosticsV21(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS conversation_parse_diagnostics (
id INTEGER PRIMARY KEY,
projection_id INTEGER NOT NULL REFERENCES conversation_projections(id) ON DELETE CASCADE,
diagnostic_key TEXT NOT NULL,
diagnostic_json BLOB NOT NULL,
UNIQUE(projection_id,diagnostic_key))`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES (21,?)`, formatTime(time.Now().UTC()))
	return err
}

func migrateTranscriptReadOffsetV22(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=22`).Scan(&applied); err != nil {
		return err
	}
	if applied != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE conversation_projections ADD COLUMN read_offset INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("apply transcript read offset v22: %w", err)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES (22,?)`, formatTime(time.Now().UTC()))
	return err
}

func migrateCanonicalItemAliasesV14(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 14`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v14: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, schemaV14); err != nil {
		return fmt.Errorf("apply history schema v14: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (14, ?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v14: %w", err)
	}
	return nil
}

// migrateCanonicalBodyRefsV15 makes content references projection-local. Item
// ids are now canonical across rebuilds, so a content ref derived from one may
// legitimately recur in the active projection while its predecessor remains
// retained for alias compatibility. The old global UNIQUE(content_ref) turned
// that normal rebuild into a degraded transcript.
func migrateCanonicalBodyRefsV15(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 15`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v15: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, schemaV15); err != nil {
		return fmt.Errorf("apply history schema v15: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (15, ?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history migration v15: %w", err)
	}
	return nil
}

func migrateCanonicalAliasConflictsV16(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 16`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v16: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, schemaV16); err != nil {
		return fmt.Errorf("apply history schema v16: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (16, ?)`, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history migration v16: %w", err)
	}
	return nil
}

func migrateTurnUsageServiceTierV13(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 13`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v13: %w", err)
	}
	if applied > 0 {
		return nil
	}
	hasColumn, err := sqliteTableHasColumn(ctx, tx, "conversation_turns", "usage_service_tier")
	if err != nil {
		return fmt.Errorf("inspect history schema v13: %w", err)
	}
	if !hasColumn {
		if _, err := tx.ExecContext(ctx, schemaV13); err != nil {
			return fmt.Errorf("apply history schema v13: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (13, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v13: %w", err)
	}
	return nil
}

func migrateTurnUsageModelEffortV12(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 12`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v12: %w", err)
	}
	if applied > 0 {
		return nil
	}
	hasColumn, err := sqliteTableHasColumn(ctx, tx, "conversation_turns", "usage_model")
	if err != nil {
		return fmt.Errorf("inspect history schema v12: %w", err)
	}
	if !hasColumn {
		if _, err := tx.ExecContext(ctx, schemaV12); err != nil {
			return fmt.Errorf("apply history schema v12: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (12, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v12: %w", err)
	}
	return nil
}

func migrateTurnUsageContractV11(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 11`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v11: %w", err)
	}
	if applied > 0 {
		return nil
	}
	hasColumn, err := sqliteTableHasColumn(ctx, tx, "conversation_turns", "usage_contract")
	if err != nil {
		return fmt.Errorf("inspect history schema v11: %w", err)
	}
	if !hasColumn {
		if _, err := tx.ExecContext(ctx, schemaV11); err != nil {
			return fmt.Errorf("apply history schema v11: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (11, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v11: %w", err)
	}
	return nil
}

func migrateConversationSearchV10(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 10`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v10: %w", err)
	}
	if applied == 0 {
		if _, err := tx.ExecContext(ctx, schemaV10); err != nil {
			return fmt.Errorf("apply history schema v10: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (10, ?)`,
			formatTime(time.Now().UTC())); err != nil {
			return fmt.Errorf("record history schema v10: %w", err)
		}
	}
	// This comparison runs on every Store.Open after the empty schema exists.
	// It is limited to the two small state rows; it never scans or backfills
	// conversation items during startup.
	if err := initializeConversationSearchState(ctx, tx, defaultConversationSearchVersions()); err != nil {
		return fmt.Errorf("initialize history search state v10: %w", err)
	}
	return nil
}

func migrateClaudeWorkspaceResolutionsV9(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 9`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v9: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, schemaV9); err != nil {
		return fmt.Errorf("apply history schema v9: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (9, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v9: %w", err)
	}
	return nil
}

func migrateCodexWorkspaceResolutionsV8(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 8`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v8: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, schemaV8); err != nil {
		return fmt.Errorf("apply history schema v8: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (8, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v8: %w", err)
	}
	return nil
}

func migrateTurnUsageV7(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 7`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v7: %w", err)
	}
	if applied > 0 {
		return nil
	}
	// The ALTERs are guarded on one column so a partially applied migration (or
	// a database created before the version row existed) stays idempotent.
	hasColumn, err := sqliteTableHasColumn(ctx, tx, "conversation_turns", "usage_input")
	if err != nil {
		return fmt.Errorf("inspect history schema v7: %w", err)
	}
	if !hasColumn {
		if _, err := tx.ExecContext(ctx, schemaV7); err != nil {
			return fmt.Errorf("apply history schema v7: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (7, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v7: %w", err)
	}
	return nil
}

func migrateThreadNamesV6(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 6`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v6: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, schemaV6); err != nil {
		return fmt.Errorf("apply history schema v6: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (6, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v6: %w", err)
	}
	return nil
}

func migrateInteractionColumnV5(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 5`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v5: %w", err)
	}
	if applied > 0 {
		return nil
	}
	hasColumn, err := sqliteTableHasColumn(ctx, tx, "threads", "interaction")
	if err != nil {
		return fmt.Errorf("inspect history schema v5: %w", err)
	}
	if !hasColumn {
		if _, err := tx.ExecContext(ctx, schemaV5); err != nil {
			return fmt.Errorf("apply history schema v5: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (5, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v5: %w", err)
	}
	return nil
}

func migrateTranscriptEvidenceHashesV4(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 4`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v4: %w", err)
	}
	if applied > 0 {
		return nil
	}
	hasColumn, err := sqliteTableHasColumn(ctx, tx, "conversation_items", "source_line_sha256")
	if err != nil {
		return fmt.Errorf("inspect history schema v4: %w", err)
	}
	if !hasColumn {
		if _, err := tx.ExecContext(ctx, schemaV4); err != nil {
			return fmt.Errorf("apply history schema v4: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (4, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v4: %w", err)
	}
	return nil
}

func sqliteTableHasColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func migrateTranscriptV3(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 3`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v3: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, schemaV3); err != nil {
		return fmt.Errorf("apply history schema v3: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (3, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v3: %w", err)
	}
	return nil
}

func migrateFixedWidthTimes(ctx context.Context, tx *sql.Tx) error {
	var applied int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 2`).Scan(&applied); err != nil {
		return fmt.Errorf("read history migration v2: %w", err)
	}
	if applied > 0 {
		return nil
	}
	columns := []struct {
		table, key, column string
	}{
		{"source_files", "id", "mtime"},
		{"source_files", "id", "last_indexed_at"},
		{"threads", "id", "created_at"},
		{"threads", "id", "updated_at"},
		{"threads", "id", "last_indexed_at"},
		{"index_state", "source_root_id", "last_completed_at"},
	}
	for _, column := range columns {
		if err := normalizeTimeColumn(ctx, tx, column.table, column.key, column.column); err != nil {
			return fmt.Errorf("normalize %s.%s for history migration v2: %w", column.table, column.column, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (2, ?)`,
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record history schema v2: %w", err)
	}
	return nil
}

func normalizeTimeColumn(ctx context.Context, tx *sql.Tx, table, keyColumn, timeColumn string) error {
	query := fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s IS NOT NULL", keyColumn, timeColumn, table, timeColumn)
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	type update struct {
		key   any
		value string
	}
	var updates []update
	for rows.Next() {
		var key any
		var raw string
		if err := rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return err
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			rows.Close()
			return err
		}
		formatted := formatTime(parsed)
		if formatted != raw {
			updates = append(updates, update{key: key, value: formatted})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range updates {
		statement := fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ?", table, timeColumn, keyColumn)
		if _, err := tx.ExecContext(ctx, statement, item.value, item.key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) EnsureRoot(ctx context.Context, root SourceRoot) (PersistedRoot, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := validateRoot(root); err != nil {
		return PersistedRoot{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PersistedRoot{}, err
	}
	defer tx.Rollback()
	persisted, _, err := ensureRootTx(ctx, tx, root)
	if err != nil {
		return PersistedRoot{}, err
	}
	if err := tx.Commit(); err != nil {
		return PersistedRoot{}, err
	}
	return persisted, nil
}

func ensureRootTx(ctx context.Context, tx *sql.Tx, root SourceRoot) (PersistedRoot, bool, error) {
	result, err := tx.ExecContext(ctx, `
INSERT INTO source_roots(provider, root_kind, canonical_root, archived, enabled)
VALUES (?, ?, ?, ?, 1)
ON CONFLICT(provider, root_kind, canonical_root) DO UPDATE SET
  archived = excluded.archived,
  enabled = 1
WHERE archived IS NOT excluded.archived OR enabled IS NOT 1`,
		root.Provider, root.Kind, root.Path, root.Archived)
	if err != nil {
		return PersistedRoot{}, false, fmt.Errorf("upsert history root: %w", err)
	}
	changed, err := resultChanged(result)
	if err != nil {
		return PersistedRoot{}, false, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `
SELECT id FROM source_roots WHERE provider = ? AND root_kind = ? AND canonical_root = ?`,
		root.Provider, root.Kind, root.Path).Scan(&id); err != nil {
		return PersistedRoot{}, false, fmt.Errorf("read history root: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO index_state(source_root_id, state, source_count)
VALUES (?, ?, 0)`, id, IndexStarting); err != nil {
		return PersistedRoot{}, false, fmt.Errorf("initialize root index state: %w", err)
	}
	return PersistedRoot{ID: id, Root: root, Enabled: true}, changed, nil
}

func validateRoot(root SourceRoot) error {
	if root.Provider != ProviderCodex && root.Provider != ProviderClaude && root.Provider != ProviderGrok {
		return fmt.Errorf("invalid history provider %q", root.Provider)
	}
	if root.Kind == "" || !filepath.IsAbs(root.Path) {
		return errors.New("history root requires a kind and absolute path")
	}
	if (root.Provider == ProviderCodex && root.Kind != RootCodexSessions && root.Kind != RootCodexArchived) ||
		(root.Provider == ProviderClaude && root.Kind != RootClaudeProjects) ||
		(root.Provider == ProviderGrok && root.Kind != RootGrokSessions) {
		return fmt.Errorf("history root kind %q does not belong to provider %q", root.Kind, root.Provider)
	}
	return nil
}

func mintThreadID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("mint Nano thread ID: %w", err)
	}
	return "nth_" + hex.EncodeToString(bytes[:]), nil
}

func bumpCatalogVersion(ctx context.Context, tx *sql.Tx) (uint64, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE catalog_state SET catalog_version = catalog_version + 1 WHERE singleton = 1`); err != nil {
		return 0, fmt.Errorf("increment catalog version: %w", err)
	}
	return catalogVersionTx(ctx, tx)
}

func catalogVersionTx(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var version uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT catalog_version FROM catalog_state WHERE singleton = 1`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read catalog version: %w", err)
	}
	return version, nil
}

func resultChanged(result sql.Result) (bool, error) {
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

const sqliteTimeFormat = "2006-01-02T15:04:05.000000000Z"

func formatTime(value time.Time) string { return value.UTC().Format(sqliteTimeFormat) }

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
