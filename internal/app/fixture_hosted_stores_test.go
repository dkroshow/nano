package app

import (
	"context"
	"database/sql"
	"github.com/dkroshow/nano/internal/edge/stateopen"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	statedb "github.com/dkroshow/nano/internal/state/db"
	state "github.com/dkroshow/nano/internal/state/store"
	"github.com/dkroshow/nano/internal/threads"
)

const controlSchemaVersion = state.LegacySchemaVersion

func newTestAgentExecutionStore(t *testing.T, runID string) *hostedstore.ExecutionStore {
	t.Helper()
	store, err := newAgentExecutionStore(filepath.Join(t.TempDir(), "control.sqlite"), runID)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newAgentExecutionStore(path, runID string) (*hostedstore.ExecutionStore, error) {
	control, err := fixtureOpenMetadata(path)
	if err != nil {
		return nil, err
	}
	return newControlAgentExecutionStore(control, runID)
}

func newTestControlMetadata(t *testing.T) *hostedstore.MetadataStore {
	t.Helper()
	store, err := fixtureOpenMetadata(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

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
	if err := control.ClaimNativeIDResolving(hostedID, provider, native, false, &nativeResolution{Handle: handle, Native: native}); err != nil {
		t.Fatal(err)
	}
}

// newTestHostedControl opens the real hosted and prompt store boundaries on a temporary file.
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

func fixtureBeginSession(store *hostedstore.ExecutionStore, clientID, hash, provider, mode string) (threads.SessionRecord, bool, error) {
	return fixtureBeginSessionWithSubscriptions(store, clientID, hash, provider, mode, threads.SessionRecord{}, subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None})
}
func fixtureBeginSessionWithInvoker(store *hostedstore.ExecutionStore, clientID, hash, provider, mode string, invoker threads.SessionRecord, assignment ...*hostedstore.Assignment) (threads.SessionRecord, bool, error) {
	return fixtureBeginSessionWithSubscriptions(store, clientID, hash, provider, mode, invoker, subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None}, assignment...)
}
func fixtureBeginSessionWithSubscriptions(store *hostedstore.ExecutionStore, clientID, hash, provider, mode string, invoker threads.SessionRecord, choices subscriptions.Choices, assignment ...*hostedstore.Assignment) (threads.SessionRecord, bool, error) {
	return fixtureBeginSessionWithSubscriptionsAndPlacement(store, clientID, hash, provider, mode, sidebarPlacementInvoker, invoker, choices, assignment...)
}
func fixtureBeginSessionWithSubscriptionsAndPlacement(store *hostedstore.ExecutionStore, clientID, hash, provider, mode, placement string, invoker threads.SessionRecord, choices subscriptions.Choices, assignment ...*hostedstore.Assignment) (threads.SessionRecord, bool, error) {
	return fixtureCreateHosted(store, clientID, hash, provider, mode, placement, invoker, choices, assignment...)
}
func fixtureEnsureSession(store *hostedstore.ExecutionStore, id, provider, mode string) (threads.SessionRecord, error) {
	return fixtureEnsureSessionWithIdentity(store, id, provider, mode, "", "", "")
}
func fixtureEnsureSessionWithIdentity(store *hostedstore.ExecutionStore, id, provider, mode, invoker, native, invokerProvider string) (threads.SessionRecord, error) {
	return store.EnsureNamedSessionWithIdentity(id, provider, mode, invoker, native, invokerProvider, "")
}
func fixtureBeginTurn(store *hostedstore.ExecutionStore, hostedID, actionID, hash string) (threads.TurnRecord, bool, error) {
	return fixtureAdmitTurn(store, hostedID, actionID, hash)
}

func recordTestCompletedTurn(store *hostedmodel.CompletedTurnStore, finish coreturns.Finish) (completedTurn, bool, error) {
	core, err := store.CoreStore(context.Background())
	if err != nil {
		return completedTurn{}, false, err
	}
	return core.Finished(context.Background(), finish)
}
func mustCompletedStore(t *testing.T) *hostedmodel.CompletedTurnStore {
	t.Helper()
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "legacy-completed.sqlite"), "completed-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Control.Close() })
	return stores.Completed
}
func fixtureLatestCompleted(store *hostedmodel.CompletedTurnStore, target string) (completedTurn, error) {
	core, err := store.CoreStore(context.Background())
	if err != nil {
		return completedTurn{}, err
	}
	return core.Latest(context.Background(), target)
}

// fixtureWriteTurn seeds the committed state a stopped process left for the next opener.
func fixtureWriteTurn(store *hostedstore.ExecutionStore, rec threads.TurnRecord) error {
	var source, code, message string
	if rec.Error != nil {
		source, code, message = rec.Error.Source, rec.Error.Code, rec.Error.Message
	}
	_, err := store.Control().CanonicalDatabase().Exec(`UPDATE hosted_turn_operations SET status=?, provider_turn_id=?, final_item_id=?, error_source=?, error_code=?, error_message=?, updated_at=?, revision=?, transport_action_id=? WHERE operation_id=?`, rec.Status, rec.ProviderTurnID, rec.FinalItemID, source, code, message, rec.UpdatedAt, rec.Revision, rec.TransportActionID, rec.ID)
	return err
}

func fixtureRecordCreationName(store *hostedstore.MetadataStore, hostedID, name string) error {
	_, _, err := threads.RecordCreationName(context.Background(), store.CanonicalDatabase(), hostedID, name, "creation_request")
	return err
}

// The delivery transaction callback holds the real execution mutation lock.
// A missing operation changes no row while concurrency tests pause the transaction.
func fixtureExecutionLocked(t *testing.T, store *hostedstore.ExecutionStore, whileLocked func()) {
	t.Helper()
	err := store.MarkTurnDeliveryWith("", "", "", nil, func(*sql.Tx) error { whileLocked(); return nil })
	if err != nil {
		t.Error(err)
	}
}

// openThreadRuntimeStores gives tests a disposable set of the default store paths.
func openThreadRuntimeStores(ctx context.Context, directory, legacyCompleted, runID string) (*stateopen.RuntimeStores, error) {
	return stateopen.OpenRuntimeStoresAt(ctx, state.StorePaths{
		Destination: filepath.Join(directory, "nano_threads.sqlite"), Control: filepath.Join(directory, "control.sqlite"),
		BackupRoot: filepath.Join(directory, "threads-backups"),
		Turns:      prompts.LegacyTurnPaths{Queue: filepath.Join(directory, "queued-turns.json"), Attempts: filepath.Join(directory, "send-attempts.json"), Completed: legacyCompleted},
	}, runID)
}

// newControlAgentExecutionStore composes feature admission with durable execution loading.
func newControlAgentExecutionStore(control *hostedstore.MetadataStore, runID string) (*hostedstore.ExecutionStore, error) {
	return hostedstore.NewExecutionStore(control, runID, subscriptions.IsFeatureAction)
}

func fixtureOpenMetadata(path string) (*hostedstore.MetadataStore, error) {
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		return nil, err
	}
	if err = state.MigrateLegacySchema(db, statewiring.Migrations()); err != nil {
		db.Close()
		return nil, err
	}
	reads, err := statedb.Open(context.Background(), path)
	if err != nil {
		db.Close()
		return nil, err
	}
	store, err := hostedstore.NewMetadataStore(db, reads)
	if err != nil {
		db.Close()
		reads.Close()
	}
	return store, err
}
