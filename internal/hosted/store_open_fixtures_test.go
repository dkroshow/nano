package hosted

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
)

func openTestMetadataStore(path string) (*hostedstore.MetadataStore, error) {
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
	if err := state.MigrateLegacySchema(db, statewiring.Migrations()); err != nil {
		db.Close()
		return nil, err
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
	store, err := hostedstore.NewMetadataStore(db, readDB)
	if err != nil {
		readDB.Close()
		db.Close()
		return nil, err
	}
	return store, nil
}

func newTestAgentExecutionStore(t *testing.T, runID string) *hostedstore.ExecutionStore {
	t.Helper()
	store, err := newAgentExecutionStore(filepath.Join(t.TempDir(), "control.sqlite"), runID)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newAgentExecutionStore(path, runID string) (*hostedstore.ExecutionStore, error) {
	control, err := openTestMetadataStore(path)
	if err != nil {
		return nil, err
	}
	return newControlAgentExecutionStore(control, runID)
}

func newTestControlMetadata(t *testing.T) *hostedstore.MetadataStore {
	t.Helper()
	store, err := openTestMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestHostedControl provisions the production fail-closed dependencies for
// tests that exercise a hosted HTTP/session path.  Fixtures must opt in to a
// real temporary control.sqlite rather than relying on a legacy JSON fallback.
// migrateTestControl brings a fresh control store to the current schema.
func migrateTestControl(t *testing.T, metadata *hostedstore.MetadataStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := prompts.MigrateTurnRecords(ctx, metadata.CanonicalDatabase(), prompts.LegacyTurnPaths{}, filepath.Join(t.TempDir(), "schema-20")); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.MigrateTurnTransactions(ctx, metadata.CanonicalDatabase(), filepath.Join(t.TempDir(), "schema-21.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := coreturns.MigrateCompletedTurnUsage(ctx, metadata.CanonicalDatabase(), filepath.Join(t.TempDir(), "schema-22.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.MigrateMemberRecovery(ctx, metadata.CanonicalDatabase(), filepath.Join(t.TempDir(), "schema-23.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.MigrateParkedDiscard(ctx, metadata.CanonicalDatabase(), filepath.Join(t.TempDir(), "schema-24.sqlite")); err != nil {
		t.Fatal(err)
	}
}

// resolveTestProvisional records, as a child's init does, that the hosted
// thread's provisional handle is native, which makes native current.
func resolveTestProvisional(t *testing.T, control *hostedstore.MetadataStore, hostedID, provider, handle, native string) {
	t.Helper()
	if err := control.ClaimNativeIDResolving(hostedID, provider, native, false, &hostedstore.NativeResolution{Handle: handle, Native: native}); err != nil {
		t.Fatal(err)
	}
}

func newTestHostedControl(t *testing.T) (*hostedstore.MetadataStore, *hostedstore.ExecutionStore) {
	t.Helper()
	metadata := newTestControlMetadata(t)
	migrateTestControl(t, metadata)
	executions, err := newControlAgentExecutionStore(metadata, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	return metadata, executions
}
