package app

import (
	"context"
	"database/sql"
	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	"path/filepath"
	"testing"
)

const controlSchemaVersion = state.LegacySchemaVersion

func newTestAgentExecutionStore(t *testing.T, runID string) *agentExecutionStore {
	t.Helper()
	store, err := newAgentExecutionStore(filepath.Join(t.TempDir(), "control.sqlite"), runID)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newAgentExecutionStore(path, runID string) (*agentExecutionStore, error) {
	control, err := openControlMetadataStore(path)
	if err != nil {
		return nil, err
	}
	return newControlAgentExecutionStore(control, runID)
}

func newTestControlMetadata(t *testing.T) *controlMetadataStore {
	t.Helper()
	store, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// migrateTestControl brings a fresh control store to the current schema.
func migrateTestControl(t *testing.T, metadata *controlMetadataStore) {
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
func resolveTestProvisional(t *testing.T, control *controlMetadataStore, hostedID, provider, handle, native string) {
	t.Helper()
	if err := control.ClaimNativeIDResolving(hostedID, provider, native, false, &nativeResolution{Handle: handle, Native: native}); err != nil {
		t.Fatal(err)
	}
}

// newTestHostedControl opens the real hosted and prompt store boundaries on a temporary file.
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

func fixtureBeginSession(store *agentExecutionStore, clientID, hash, provider, mode string) (agentSessionRecord, bool, error) {
	return fixtureBeginSessionWithSubscriptions(store, clientID, hash, provider, mode, agentSessionRecord{}, subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None})
}
func fixtureBeginSessionWithInvoker(store *agentExecutionStore, clientID, hash, provider, mode string, invoker agentSessionRecord, assignment ...*orchestrationAssignment) (agentSessionRecord, bool, error) {
	return fixtureBeginSessionWithSubscriptions(store, clientID, hash, provider, mode, invoker, subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None}, assignment...)
}
func fixtureBeginSessionWithSubscriptions(store *agentExecutionStore, clientID, hash, provider, mode string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*orchestrationAssignment) (agentSessionRecord, bool, error) {
	return fixtureBeginSessionWithSubscriptionsAndPlacement(store, clientID, hash, provider, mode, sidebarPlacementInvoker, invoker, choices, assignment...)
}
func fixtureBeginSessionWithSubscriptionsAndPlacement(store *agentExecutionStore, clientID, hash, provider, mode, placement string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*orchestrationAssignment) (agentSessionRecord, bool, error) {
	return fixtureCreateHosted(store, clientID, hash, provider, mode, placement, invoker, choices, assignment...)
}
func fixtureEnsureSession(store *agentExecutionStore, id, provider, mode string) (agentSessionRecord, error) {
	return fixtureEnsureSessionWithIdentity(store, id, provider, mode, "", "", "")
}
func fixtureEnsureSessionWithIdentity(store *agentExecutionStore, id, provider, mode, invoker, native, invokerProvider string) (agentSessionRecord, error) {
	return store.EnsureNamedSessionWithIdentity(id, provider, mode, invoker, native, invokerProvider, "")
}
func fixtureBeginTurn(store *agentExecutionStore, hostedID, actionID, hash string) (agentTurnRecord, bool, error) {
	return fixtureAdmitTurn(store, hostedID, actionID, hash)
}

func recordTestCompletedTurn(store *completedTurnStore, finish coreturns.Finish) (completedTurn, bool, error) {
	core, err := store.CoreStore(context.Background())
	if err != nil {
		return completedTurn{}, false, err
	}
	return core.Finished(context.Background(), finish)
}
func mustCompletedStore(t *testing.T) *completedTurnStore {
	t.Helper()
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "legacy-completed.sqlite"), "completed-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.control.Close() })
	return stores.completed
}
func fixtureLatestCompleted(store *completedTurnStore, target string) (completedTurn, error) {
	core, err := store.CoreStore(context.Background())
	if err != nil {
		return completedTurn{}, err
	}
	return core.Latest(context.Background(), target)
}

// fixtureWriteTurn seeds the committed state a stopped process left for the next opener.
func fixtureWriteTurn(store *agentExecutionStore, rec agentTurnRecord) error {
	var source, code, message string
	if rec.Error != nil {
		source, code, message = rec.Error.Source, rec.Error.Code, rec.Error.Message
	}
	_, err := store.Control().CanonicalDatabase().Exec(`UPDATE hosted_turn_operations SET status=?, provider_turn_id=?, final_item_id=?, error_source=?, error_code=?, error_message=?, updated_at=?, revision=?, transport_action_id=? WHERE operation_id=?`, rec.Status, rec.ProviderTurnID, rec.FinalItemID, source, code, message, rec.UpdatedAt, rec.Revision, rec.TransportActionID, rec.ID)
	return err
}

func fixtureRecordCreationName(store *controlMetadataStore, hostedID, name string) error {
	_, _, err := threadvalues.RecordCreationName(context.Background(), store.CanonicalDatabase(), hostedID, name, "creation_request")
	return err
}

// The delivery transaction callback holds the real execution mutation lock.
// A missing operation changes no row while concurrency tests pause the transaction.
func fixtureExecutionLocked(t *testing.T, store *agentExecutionStore, whileLocked func()) {
	t.Helper()
	err := store.MarkTurnDeliveryWith("", "", "", nil, func(*sql.Tx) error { whileLocked(); return nil })
	if err != nil {
		t.Error(err)
	}
}
