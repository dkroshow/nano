package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func TestControlExecutionCreationRollsBackIdentityAndRuntimeTogether(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.beginSession("caller", "hash", "unknown", presentationShared); err == nil {
		t.Fatal("invalid provider created execution state")
	}
	var hosted, runtime int
	if err := control.db.QueryRow(`SELECT count(*) FROM hosted_threads`).Scan(&hosted); err != nil {
		t.Fatal(err)
	}
	if err := control.db.QueryRow(`SELECT count(*) FROM hosted_execution_sessions`).Scan(&runtime); err != nil {
		t.Fatal(err)
	}
	if hosted != 0 || runtime != 0 {
		t.Fatalf("failed creation left hosted/runtime rows %d/%d", hosted, runtime)
	}
}

func TestControlEnsureSessionPreservesHistoricalIdentityWhenInvokerIsOmitted(t *testing.T) {
	const (
		invokerHosted   = "nht_parent"
		invokerNative   = "native-parent"
		invokerProvider = "codex"
	)
	for _, provider := range []string{"claude", "grok", "codex"} {
		t.Run(provider, func(t *testing.T) {
			for _, reloadCanonical := range []bool{false, true} {
				name := "cached"
				if reloadCanonical {
					name = "canonical_reload"
				}
				t.Run(name, func(t *testing.T) {
					control, store := newTestHostedControl(t)
					nativeID := "historical-" + provider
					historical, err := store.ensureSessionWithIdentity("", provider, presentationShared, invokerHosted, invokerNative, invokerProvider)
					if err != nil {
						t.Fatal(err)
					}
					if err := control.claimNativeID(historical.ID, provider, nativeID, false); err != nil {
						t.Fatal(err)
					}
					if err := store.updateSession(historical.ID, "old-nano-session", nativeID, sessionStatusExited, "closed"); err != nil {
						t.Fatal(err)
					}
					if reloadCanonical {
						store.mu.Lock()
						delete(store.sessions, historical.ID)
						delete(store.sessionBaseline, historical.ID)
						store.mu.Unlock()
					}

					// Explorer resume requests name the provider and native thread, but
					// omit the immutable invoker tuple. The canonical record must fill it
					// back in without changing either durable identity.
					resumed, err := store.ensureSessionWithIdentity(historical.ID, provider, presentationShared, "", "", "")
					if err != nil {
						t.Fatalf("historical resume with omitted invoker identity: %v", err)
					}
					if resumed.ID != historical.ID || resumed.NativeThreadID != nativeID {
						t.Fatalf("resume changed conversation identity: %#v", resumed)
					}
					if resumed.NanoSessionID != "old-nano-session" || resumed.Status != sessionStatusExited {
						t.Fatalf("resume lost the historical execution record: %#v", resumed)
					}
					if resumed.InvokerHostedThreadID != invokerHosted || resumed.InvokerNativeThreadID != invokerNative || resumed.InvokerProvider != invokerProvider {
						t.Fatalf("resume lost invoker identity: %#v", resumed)
					}

					if _, err := store.ensureSessionWithIdentity(historical.ID, provider, presentationShared, invokerHosted, invokerNative, invokerProvider); err != nil {
						t.Fatalf("matching explicit invoker identity: %v", err)
					}
					if _, err := store.ensureSessionWithIdentity(historical.ID, provider, presentationShared, "nht_other_parent", invokerNative, invokerProvider); err == nil || !strings.Contains(err.Error(), "invoker identity") {
						t.Fatalf("explicit invoker mismatch = %v, want invoker identity conflict", err)
					}
				})
			}
		})
	}
}

func TestControlEnsureSessionOrdinaryResumeWithoutInvokerStillWorks(t *testing.T) {
	for _, provider := range []string{"claude", "grok", "codex"} {
		t.Run(provider, func(t *testing.T) {
			_, store := newTestHostedControl(t)
			fresh, err := store.ensureSessionWithIdentity("", provider, presentationShared, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := store.ensureSessionWithIdentity(fresh.ID, provider, presentationShared, "", "", "")
			if err != nil {
				t.Fatalf("ordinary resume: %v", err)
			}
			if resumed.ID != fresh.ID || resumed.InvokerHostedThreadID != "" || resumed.InvokerNativeThreadID != "" || resumed.InvokerProvider != "" {
				t.Fatalf("ordinary resume changed identity: %#v", resumed)
			}
		})
	}
}

func TestControlEnsureSessionDoesNotRetainMemoryOrIdentityAfterSQLFailure(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.db.Exec(`DROP TABLE hosted_execution_sessions`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ensureSession("nht_sql_failure", "codex", presentationShared); err == nil {
		t.Fatal("forced SQL failure succeeded")
	}
	if _, ok := store.session("nht_sql_failure"); ok {
		t.Fatal("failed SQL mutation leaked an in-memory execution record")
	}
	var hosted int
	if err := control.db.QueryRow(`SELECT count(*) FROM hosted_threads WHERE hosted_thread_id='nht_sql_failure'`).Scan(&hosted); err != nil {
		t.Fatal(err)
	}
	if hosted != 0 {
		t.Fatal("failed SQL mutation committed hosted identity")
	}
}

// A failed write resets the operation cache, and a failed reload leaves it
// empty while the committed rows still hold open operations. The persisted
// read by a prompt's own transport action still finds such an operation, says
// "none" only when no row exists, and reports an unreadable store as an error
// rather than as absence.
func TestPersistedTurnForTransportActionReadsCommittedRowsNotTheCache(t *testing.T) {
	ctx := context.Background()
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := store.ensureSession("nht_persisted", "claude", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := store.beginTurn(hosted.ID, "action", "hash")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.resetControlCachesLocked() // the reset of a failed write, with no reload
	store.mu.Unlock()
	if _, cached := store.turnForTransportAction(op.TransportActionID); cached {
		t.Fatal("the reset cache still showed the operation; the case needs it gone")
	}
	persisted, found, err := store.persistedTurnForTransportAction(ctx, hosted.ID, op.TransportActionID)
	if err != nil || !found || persisted.ID != op.ID || agentTurnTerminal(persisted.Status) {
		t.Fatalf("persisted read = %+v found=%v err=%v; want the open operation", persisted, found, err)
	}
	if _, found, err := store.persistedTurnForTransportAction(ctx, hosted.ID, "never-delivered"); err != nil || found {
		t.Fatalf("absent action: found=%v err=%v; want positive absence", found, err)
	}
	if _, err := control.db.Exec(`DROP TABLE hosted_turn_operations`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.persistedTurnForTransportAction(ctx, hosted.ID, op.TransportActionID); err == nil || found {
		t.Fatalf("unreadable store: found=%v err=%v; want an error, not absence", found, err)
	}
}

// On the current schema the per-action operation read seeks its own index:
// proving a prompt never had an operation reads that action's rows, not the
// thread's operation history.
func TestPersistedTurnForTransportActionSeeksItsIndex(t *testing.T) {
	control, _ := newTestHostedControl(t)
	rows, err := control.db.Query(`EXPLAIN QUERY PLAN `+persistedTurnByTransportQuery, "hosted", "action")
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.Contains(plan[0], "hosted_turn_operations_transport") || strings.HasPrefix(plan[0], "SCAN") {
		t.Fatalf("per-action operation read plan = %v; want one search of hosted_turn_operations_transport", plan)
	}
}

func TestControlSQLFailureDoesNotPublishPhantomTurnTransition(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := store.ensureSession("nht_no_phantom", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.db.Exec(`DROP TABLE hosted_turn_operations`); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	before := store.changes.next
	wake := store.changed
	store.mu.Unlock()
	if _, _, err := store.beginTurn(hosted.ID, "phantom", "hash"); err == nil {
		t.Fatal("forced turn SQL failure succeeded")
	}
	store.mu.Lock()
	after := store.changes.next
	store.mu.Unlock()
	if after != before {
		t.Fatalf("phantom transition next=%d -> %d", before, after)
	}
	select {
	case <-wake:
		t.Fatal("failed turn mutation woke committed-state waiters")
	default:
	}
}

func TestControlExecutionConcurrentDistinctOperationsDoNotLoseRows(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := store.ensureSession("nht_concurrent", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, action := range []string{"action-a", "action-b"} {
		wg.Add(1)
		go func(action string) {
			defer wg.Done()
			_, _, err := store.beginTurn(hosted.ID, action, action+"-hash")
			errs <- err
		}(action)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := control.db.QueryRow(`SELECT count(*) FROM hosted_turn_operations WHERE hosted_thread_id=?`, hosted.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("concurrent operation rows = %d", count)
	}
}

func TestControlExecutionTargetedUpdatesLeaveUnrelatedRowsUntouched(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	a, err := store.ensureSession("nht_target_a", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.ensureSession("nht_target_b", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	var bRev int64
	var bUpdated string
	if err := control.db.QueryRow(`SELECT revision,updated_at FROM hosted_execution_sessions WHERE hosted_thread_id=?`, b.ID).Scan(&bRev, &bUpdated); err != nil {
		t.Fatal(err)
	}
	if err := store.updateSession(a.ID, "nano-a", "", "ready", ""); err != nil {
		t.Fatal(err)
	}
	var gotRev int64
	var gotUpdated string
	if err := control.db.QueryRow(`SELECT revision,updated_at FROM hosted_execution_sessions WHERE hosted_thread_id=?`, b.ID).Scan(&gotRev, &gotUpdated); err != nil {
		t.Fatal(err)
	}
	if gotRev != bRev || gotUpdated != bUpdated {
		t.Fatalf("unrelated session changed %d/%s -> %d/%s", bRev, bUpdated, gotRev, gotUpdated)
	}
	first, _, err := store.beginTurn(a.ID, "a1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.beginTurn(a.ID, "a2", "h2")
	if err != nil {
		t.Fatal(err)
	}
	var secondRev int64
	var secondUpdated string
	if err := control.db.QueryRow(`SELECT revision,updated_at FROM hosted_turn_operations WHERE operation_id=?`, second.ID).Scan(&secondRev, &secondUpdated); err != nil {
		t.Fatal(err)
	}
	if err := store.markTurnDelivery(first.ID, "provider-a", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	var gotTurnRev int64
	var gotTurnUpdated string
	if err := control.db.QueryRow(`SELECT revision,updated_at FROM hosted_turn_operations WHERE operation_id=?`, second.ID).Scan(&gotTurnRev, &gotTurnUpdated); err != nil {
		t.Fatal(err)
	}
	if gotTurnRev != secondRev || gotTurnUpdated != secondUpdated {
		t.Fatalf("unrelated turn changed %d/%s -> %d/%s", secondRev, secondUpdated, gotTurnRev, gotTurnUpdated)
	}
}

func TestControlExecutionTwoStoresResolveSameClientActionDeterministically(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	first, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := first.ensureSession("nht_two_stores", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newControlAgentExecutionStore(control, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	a, createdA, err := first.beginTurn(hosted.ID, "same-action", "prompt-hash")
	if err != nil {
		t.Fatal(err)
	}
	b, createdB, err := second.beginTurn(hosted.ID, "same-action", "prompt-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !createdA || createdB || a.ID != b.ID {
		t.Fatalf("two-store idempotency a=%#v/%v b=%#v/%v", a, createdA, b, createdB)
	}
	if _, _, err := second.beginTurn(hosted.ID, "same-action", "other-prompt"); err == nil {
		t.Fatal("same action with a different prompt succeeded")
	}
}

func TestControlExecutionCASRejectsStaleSessionAndTurnWriters(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	a, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := a.ensureSession("nht_cas", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	turn, _, err := a.beginTurn(hosted.ID, "cas-action", "hash")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.updateSession(hosted.ID, "writer-a", "", "ready", "A"); err != nil {
		t.Fatal(err)
	}
	if err := b.updateSession(hosted.ID, "writer-b", "", "ready", "B"); !errors.Is(err, errExecutionConflict) {
		t.Fatalf("stale session err=%v", err)
	}
	var nano string
	if err := control.db.QueryRow(`SELECT COALESCE(nano_session_id,'') FROM hosted_execution_sessions WHERE hosted_thread_id=?`, hosted.ID).Scan(&nano); err != nil {
		t.Fatal(err)
	}
	if nano != "writer-a" {
		t.Fatalf("stale writer overwrote session: %q", nano)
	}
	if err := a.markTurnDelivery(turn.ID, "provider-a", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.markTurnDelivery(turn.ID, "provider-b", agentTurnDeliveryUnknown, nil); !errors.Is(err, errExecutionConflict) {
		t.Fatalf("stale turn err=%v", err)
	}
	if b.sessionNeedsAttention(hosted.ID) {
		t.Fatal("failed write left speculative attention after cache reload")
	}
	var providerTurn string
	var rev int64
	if err := control.db.QueryRow(`SELECT COALESCE(provider_turn_id,''),revision FROM hosted_turn_operations WHERE operation_id=?`, turn.ID).Scan(&providerTurn, &rev); err != nil {
		t.Fatal(err)
	}
	if providerTurn != "provider-a" || rev < 2 {
		t.Fatalf("stale turn overwrote current %q/%d", providerTurn, rev)
	}
}

func TestControlExecutionCASRejectsStalePrune(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	a, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := a.ensureSession("nht_prune_cas", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	turn, _, err := a.beginTurn(hosted.ID, "prune-action", "hash")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.markTurnDelivery(turn.ID, "provider", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.setTurnLocked(turn.ID, nil)
	err = b.persistAndNotifyLocked()
	b.mu.Unlock()
	if !errors.Is(err, errExecutionConflict) {
		t.Fatalf("stale prune err=%v", err)
	}
	var count int
	if err := control.db.QueryRow(`SELECT count(*) FROM hosted_turn_operations WHERE operation_id=?`, turn.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("stale prune deleted advanced row")
	}
}

// Retention removes terminal operations past the newest maxAgentTurns of a
// thread only when the prompt owner says their prompts can never owe a member
// completion. Until that owner is wired, as at the constructor's own persist,
// it keeps everything; it asks only about a thread this persist changed, asks
// about all of that thread's surplus, and keeps whatever the owner protects or
// cannot answer.
func TestControlExecutionRetentionDeletesOnlyExplicitlyPrunedTerminalRows(t *testing.T) {
	retentionAt := func(i int) string {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
	}
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := store.ensureSession("nht_retention", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	// Forty operations past the bound, more than one read asks about: every
	// prompt may still owe except op-retain-001's, which sits in the last read,
	// behind a full read of protected ones.
	const total = maxAgentTurns + 40
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("op-retain-%03d", i)
		rec := agentTurnRecord{ID: id, HostedThreadID: hosted.ID, ClientActionID: id, TransportActionID: id, PromptHash: id, Provider: "codex", Status: agentTurnCompleted, RunID: "run-1", CreatedAt: retentionAt(i), UpdatedAt: retentionAt(i), Revision: 1}
		store.setTurnLocked(rec.ID, &rec)
	}
	persist := func() {
		t.Helper()
		store.mu.Lock()
		err := store.persistLocked(nil)
		store.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	rows := func(id string) int {
		t.Helper()
		var count int
		if err := control.db.QueryRow(`SELECT count(*) FROM hosted_turn_operations WHERE hosted_thread_id=? AND (?='' OR operation_id=?)`, hosted.ID, id, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	touch := func() {
		rec := store.turns["op-retain-101"]
		rec.Revision++
		store.setTurnLocked(rec.ID, &rec)
	}

	persist() // no prompt owner wired yet
	if got := rows(""); got != total {
		t.Fatalf("unwired retention kept %d rows, want all %d", got, total)
	}
	var asked [][]string
	answer := func(_ context.Context, hostedID string, actions []string) (map[string]bool, error) {
		asked = append(asked, append([]string(nil), actions...))
		return map[string]bool{"op-retain-001": true}, nil
	}
	store.pruneEligible = answer
	persist() // nothing changed in this thread
	if len(asked) != 0 || rows("") != total {
		t.Fatalf("an unchanged thread was asked %v or pruned: rows=%d", asked, rows(""))
	}
	store.pruneEligible = func(context.Context, string, []string) (map[string]bool, error) {
		return nil, errors.New("prompt owner unavailable")
	}
	touch()
	persist()
	if rows("") != total {
		t.Fatalf("an unanswered eligibility read pruned rows: %d", rows(""))
	}
	store.pruneEligible = answer
	touch()
	persist()
	seen := map[string]bool{}
	for _, read := range asked {
		if len(read) > maxPruneEligibleBatch {
			t.Fatalf("one read asked %d actions, over %d", len(read), maxPruneEligibleBatch)
		}
		for _, action := range read {
			seen[action] = true
		}
	}
	for i := 0; i < total-maxAgentTurns; i++ {
		if id := fmt.Sprintf("op-retain-%03d", i); !seen[id] {
			t.Fatalf("surplus %s was never asked about; asked %d reads", id, len(asked))
		}
	}
	if len(asked) != 2 || len(seen) != total-maxAgentTurns {
		t.Fatalf("asked %d reads covering %d actions; want the thread's whole surplus of %d in two", len(asked), len(seen), total-maxAgentTurns)
	}
	if rows("op-retain-001") != 0 || rows("op-retain-000") != 1 || rows("op-retain-039") != 1 || rows("") != total-1 {
		t.Fatalf("pruned the wrong surplus: eligible=%d protected=%d,%d total=%d", rows("op-retain-001"), rows("op-retain-000"), rows("op-retain-039"), rows(""))
	}
}

// Regression: persistControlLocked used records[maxAgentTurns:] unguarded,
// which panicked for every ordinary session with fewer retained terminal turns.
func TestControlExecutionRetentionDoesNotSliceSmallTerminalSet(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	store, err := newControlAgentExecutionStore(control, "run")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := store.ensureSession("nht_retention_small", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("op-small-%d", i)
		rec := agentTurnRecord{ID: id, HostedThreadID: hosted.ID, ClientActionID: id, TransportActionID: id, PromptHash: id, Provider: "codex", Status: agentTurnCompleted, RunID: "run", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: fmt.Sprintf("2026-01-01T00:00:0%dZ", i), Revision: 1}
		store.setTurnLocked(rec.ID, &rec)
	}
	store.mu.Lock()
	err = store.persistLocked(nil)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := control.db.QueryRow(`SELECT count(*) FROM hosted_turn_operations WHERE hosted_thread_id=?`, hosted.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("small terminal set rows=%d, want 2", count)
	}
}

func TestAgentExecutionStorePersistsInvocationEdge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "executions.sqlite")
	store, err := newAgentExecutionStore(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := store.beginSession("parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindSession(parent.ID, "nano-parent", "native-parent", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	parent, _ = store.session(parent.ID)
	child, _, err := store.beginSessionWithInvoker("child", "child-hash", "claude", presentationBackground, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindSession(child.ID, "nano-child", "native-child", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}

	reloaded, err := newAgentExecutionStore(path, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.session(child.ID)
	if !ok || got.InvokerHostedThreadID != parent.ID || got.InvokerNativeThreadID != "native-parent" || got.InvokerProvider != "codex" {
		t.Fatalf("reloaded invocation edge = %#v, present=%v", got, ok)
	}
	if resolved, ok := reloaded.resolveSession("native-parent"); !ok || resolved.ID != parent.ID {
		t.Fatalf("detached native invoker resolution = %#v, present=%v", resolved, ok)
	}
}

func TestAgentExecutionStoreIdempotencyAndFinalSelection(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	session, created, err := store.beginSession("caller-session", "request-hash", "codex", presentationBackground)
	if err != nil || !created {
		t.Fatalf("begin session = %#v, %v, %v", session, created, err)
	}
	if session.ID == "caller-session" || session.ClientSessionID != "caller-session" || !strings.HasPrefix(session.ID, "nht_") {
		t.Fatalf("caller alias became resource identity: %#v", session)
	}
	if resolved, ok := store.resolveSession("caller-session"); !ok || resolved.ID != session.ID {
		t.Fatalf("resolve caller alias = %#v, %v", resolved, ok)
	}
	if duplicate, created, err := store.beginSession("caller-session", "request-hash", "codex", presentationBackground); err != nil || created || duplicate.ID != session.ID {
		t.Fatalf("duplicate session = %#v, %v, %v", duplicate, created, err)
	}
	if _, _, err := store.beginSession("caller-session", "different", "codex", presentationBackground); err == nil {
		t.Fatal("session id accepted different parameters")
	}
	if err := store.bindSession(session.ID, "nano-1", "native-1", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}

	turn, created, err := store.beginTurn(session.ID, "action-1", "prompt-hash")
	if err != nil || !created {
		t.Fatalf("begin turn = %#v, %v, %v", turn, created, err)
	}
	if duplicate, created, err := store.beginTurn(session.ID, "action-1", "prompt-hash"); err != nil || created || duplicate.ID != turn.ID {
		t.Fatalf("duplicate turn = %#v, %v, %v", duplicate, created, err)
	}
	if _, _, err := store.beginTurn(session.ID, "action-1", "different"); err == nil {
		t.Fatal("action id accepted a different prompt")
	}
	if err := store.markTurnDelivery(turn.ID, "provider-turn-1", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.observeCandidate("codex", "native-1", "provider-turn-1", "commentary", "commentary", "work in progress"); err != nil {
		t.Fatal(err)
	}
	if err := store.observeCandidate("codex", "native-1", "provider-turn-1", "final", "final_answer", "complete answer"); err != nil {
		t.Fatal(err)
	}
	// Later commentary cannot replace explicit final-answer evidence.
	if err := store.observeCandidate("codex", "native-1", "provider-turn-1", "late", "commentary", "late note"); err != nil {
		t.Fatal(err)
	}
	if err := store.markTurnTerminal("codex", "native-1", "provider-turn-1", agentTurnCompleted, nil, "", ""); err != nil {
		t.Fatal(err)
	}

	got, _ := store.turn(turn.ID)
	if got.Status != agentTurnCompleted || got.FinalItemID != "final" || got.FinalResponse != "complete answer" {
		t.Fatalf("terminal turn = %#v", got)
	}
	if got.CandidateResponse != "" || got.FinalResponseTruncated {
		t.Fatalf("candidate leaked or short response truncated: %#v", got)
	}
	if store.sessionsNeedingAttention()[session.ID] {
		t.Fatal("completed latest turn requested attention")
	}
}

func TestAgentExecutionStoreGuardSerializesCreationAndPreservesReplay(t *testing.T) {
	store := newTestAgentExecutionStore(t, "guarded-run")
	hosted, _, err := store.beginSession("guarded-session", "request-hash", "codex", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindSession(hosted.ID, "nano-guarded", "native-guarded", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}

	guardEntered := make(chan struct{})
	releaseGuard := make(chan struct{})
	continuationDone := make(chan error, 1)
	go func() {
		_, _, err := store.beginTurnAttributedGuarded(hosted.ID, "nano:guarded", "guarded-hash", turnAttribution{Origin: turnOriginSystem}, func(existing []agentTurnRecord) error {
			if len(existing) != 0 {
				return fmt.Errorf("guard saw unexpected operations: %#v", existing)
			}
			close(guardEntered)
			<-releaseGuard
			return nil
		})
		continuationDone <- err
	}()
	<-guardEntered

	newerDone := make(chan error, 1)
	go func() {
		_, _, err := store.beginTurn(hosted.ID, "newer-public", "newer-hash")
		newerDone <- err
	}()
	select {
	case err := <-newerDone:
		t.Fatalf("newer operation bypassed guarded creation boundary: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseGuard)
	if err := <-continuationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-newerDone; err != nil {
		t.Fatal(err)
	}

	guardCalled := false
	replay, created, err := store.beginTurnAttributedGuarded(hosted.ID, "nano:guarded", "guarded-hash", turnAttribution{Origin: turnOriginSystem}, func([]agentTurnRecord) error {
		guardCalled = true
		return errors.New("replay must not be re-evaluated")
	})
	if err != nil || created || replay.ClientActionID != "nano:guarded" || guardCalled {
		t.Fatalf("guarded replay=%#v created=%v guardCalled=%v err=%v", replay, created, guardCalled, err)
	}
}

func TestAgentExecutionStoreGuardRejectsWithoutCreatingOperation(t *testing.T) {
	store := newTestAgentExecutionStore(t, "guard-reject-run")
	hosted, _, err := store.beginSession("guard-reject-session", "request-hash", "codex", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindSession(hosted.ID, "nano-guard-reject", "native-guard-reject", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("superseded")
	if _, created, err := store.beginTurnAttributedGuarded(hosted.ID, "nano:rejected", "rejected-hash", turnAttribution{Origin: turnOriginSystem}, func([]agentTurnRecord) error {
		return rejected
	}); !errors.Is(err, rejected) || created {
		t.Fatalf("guarded rejection created=%v err=%v", created, err)
	}
	if operation, found := store.turnForAction(hosted.ID, "nano:rejected"); found {
		t.Fatalf("guarded rejection persisted operation %#v", operation)
	}
}

func TestAgentExecutionStoreFailureNeverLeaksCandidateResponse(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	session, _, _ := store.beginSession("caller-session", "request-hash", "claude", presentationBackground)
	_ = store.bindSession(session.ID, "nano-1", "native-1", "ready", presentationBackground)
	turn, _, _ := store.beginTurn(session.ID, "action-1", "prompt-hash")
	_ = store.markTurnDelivery(turn.ID, "claude:1", agentTurnRunning, nil)
	_ = store.observeCandidate("claude", "native-1", "claude:1", "assistant-1", "", "partial answer")
	wantErr := &agentExecutionError{Source: "provider", Code: "error", Message: "Claude failed"}
	if err := store.markTurnTerminal("claude", "native-1", "claude:1", agentTurnFailed, wantErr, "", ""); err != nil {
		t.Fatal(err)
	}

	got, _ := store.turn(turn.ID)
	if got.Status != agentTurnFailed || got.FinalResponse != "" || got.FinalItemID != "" || got.Error == nil {
		t.Fatalf("failed turn = %#v", got)
	}
	if !store.sessionsNeedingAttention()[session.ID] {
		t.Fatal("latest failed turn did not request attention")
	}
}

func TestAgentExecutionStoreKeepsProjectionTruncationExplicit(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	session, _, _ := store.beginSession("caller-session", "request-hash", "codex", presentationShared)
	_ = store.bindSession(session.ID, "nano-1", "native-1", "ready", presentationShared)
	turn, _, _ := store.beginTurn(session.ID, "action-1", "prompt-hash")
	_ = store.markTurnDelivery(turn.ID, "turn-1", agentTurnRunning, nil)
	if err := store.markTurnTerminal(
		"codex", "native-1", "turn-1", agentTurnCompleted, nil, "bounded projection", "item-1",
		agentResultMeta{Bytes: 100_000, Truncated: true},
	); err != nil {
		t.Fatal(err)
	}
	got, _ := store.turn(turn.ID)
	if !got.FinalResponseTruncated || got.FinalResponseBytes != 100_000 || got.FinalResponse != "bounded projection" {
		t.Fatalf("projected terminal result = %#v", got)
	}
}

func TestAgentExecutionStoreRestartMakesUncertainStatesExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "executions.sqlite")
	first, err := newAgentExecutionStore(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	session, _, _ := first.beginSession("creating", "request-hash", "codex", presentationShared)
	bound, _, _ := first.beginSession("bound", "request-hash-2", "claude", presentationBackground)
	_ = first.bindSession(bound.ID, "nano-1", "native-1", "ready", presentationBackground)
	turn, _, _ := first.beginTurn(bound.ID, "action-1", "prompt-hash")
	queuedSession, _, _ := first.beginSession("queued", "request-hash-3", "codex", presentationBackground)
	_ = first.bindSession(queuedSession.ID, "nano-2", "native-2", "ready", presentationBackground)
	queuedTurn, _, _ := first.beginTurn(queuedSession.ID, "action-2", "prompt-hash-2")
	_ = first.markTurnDelivery(queuedTurn.ID, "", agentTurnQueued, nil)

	second, err := newAgentExecutionStore(path, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	creating, _ := second.session(session.ID)
	if creating.Status != agentSessionCreationUnknown {
		t.Fatalf("creating state after restart = %q", creating.Status)
	}
	restartedTurn, _ := second.turn(turn.ID)
	if restartedTurn.Status != agentTurnDeliveryUnknown || restartedTurn.Error == nil || restartedTurn.Error.Code != "nano_restarted" {
		t.Fatalf("pending delivery after restart = %#v", restartedTurn)
	}
	if !second.sessionsNeedingAttention()[bound.ID] {
		t.Fatal("delivery-uncertain background session did not request attention")
	}
	restartedQueuedTurn, _ := second.turn(queuedTurn.ID)
	if restartedQueuedTurn.Status != agentTurnQueued || restartedQueuedTurn.Error != nil {
		t.Fatalf("queued operation changed across restart = %#v", restartedQueuedTurn)
	}
	boundAgain, _ := second.session(bound.ID)
	if boundAgain.PresentationMode != presentationBackground || boundAgain.NativeThreadID != "native-1" || boundAgain.Status != agentSessionDetached || boundAgain.NanoSessionID != "" {
		t.Fatalf("bound session lost durable identity: %#v", boundAgain)
	}
}

func TestAgentExecutionStoreLongPollWaitsForRevision(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	session, _, _ := store.beginSession("caller-session", "request-hash", "codex", presentationShared)
	turn, _, _ := store.beginTurn(session.ID, "action-1", "prompt-hash")
	done := make(chan agentTurnRecord, 1)
	go func() {
		rec, _ := store.waitTurn(context.Background(), turn.ID, turn.Revision, time.Second)
		done <- rec
	}()
	if err := store.markTurnDelivery(turn.ID, "provider-turn", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.Revision <= turn.Revision || got.Status != agentTurnRunning {
			t.Fatalf("long poll = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("long poll did not observe revision")
	}
}

func TestAgentExecutionStoreSettlesOpenTurnsWhenSessionEnds(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	session, _, _ := store.beginSession("caller-session", "request-hash", "codex", presentationShared)
	first, _, _ := store.beginTurn(session.ID, "action-1", "prompt-hash-1")
	_ = store.markTurnDelivery(first.ID, "provider-turn-1", agentTurnRunning, nil)
	if err := store.markSessionTurns(session.ID, agentTurnDeliveryUnknown, &agentExecutionError{Source: "transport", Message: "lost"}); err != nil {
		t.Fatal(err)
	}
	uncertain, _ := store.turn(first.ID)
	if uncertain.Status != agentTurnDeliveryUnknown || uncertain.Error == nil {
		t.Fatalf("unexpected session exit = %#v", uncertain)
	}

	second, _, _ := store.beginTurn(session.ID, "action-2", "prompt-hash-2")
	if err := store.markSessionTurns(session.ID, agentTurnInterrupted, &agentExecutionError{Source: "client", Message: "closed"}); err != nil {
		t.Fatal(err)
	}
	interrupted, _ := store.turn(second.ID)
	if interrupted.Status != agentTurnInterrupted || !agentTurnTerminal(interrupted.Status) {
		t.Fatalf("caller close = %#v", interrupted)
	}
}

func TestClaudeProjectionTerminalEvidence(t *testing.T) {
	status, detail := claudeProjectionTerminal([]byte(`{"type":"result","is_error":true,"subtype":"error_during_execution","result":"tool failed","terminal_reason":"error"}`))
	if status != agentTurnFailed || len(detail) == 0 {
		t.Fatalf("Claude error = %q, %s", status, detail)
	}
	status, _ = claudeProjectionTerminal([]byte(`{"type":"result","is_error":true,"terminal_reason":"aborted_by_user"}`))
	if status != agentTurnInterrupted {
		t.Fatalf("Claude abort = %q", status)
	}
	status, detail = claudeProjectionTerminal([]byte(`{"type":"result","is_error":false,"result":"done"}`))
	if status != agentTurnCompleted || len(detail) != 0 {
		t.Fatalf("Claude success = %q, %s", status, detail)
	}
}

// The presentation flip's durability contract: control.sqlite is the single
// authority, the cache mirrors it under the same lock, same-value writes are
// revision-neutral, and neither a rebind nor a resume carrying the stale
// spawn-time mode can snap the thread back.
func TestSetPresentationCanonicalAuthorityAndStaleRebinds(t *testing.T) {
	dir := t.TempDir()
	control, err := openControlMetadataStore(filepath.Join(dir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, _, err := store.beginSession("caller", "request-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}

	rec, revision, changed, err := store.setPresentation(hosted.ID, presentationBackground)
	if err != nil || !changed || rec.PresentationMode != presentationBackground {
		t.Fatalf("flip = %+v changed=%v err=%v", rec, changed, err)
	}
	var mode string
	if err := control.db.QueryRow(`SELECT presentation_mode FROM hosted_threads WHERE hosted_thread_id=?`, hosted.ID).Scan(&mode); err != nil || mode != presentationBackground {
		t.Fatalf("canonical mode = %q err=%v", mode, err)
	}

	// Idempotent repeat: no revision movement, no change signal.
	_, again, changedAgain, err := store.setPresentation(hosted.ID, presentationBackground)
	if err != nil || changedAgain || again != revision {
		t.Fatalf("same-value put revision %d -> %d changed=%v err=%v", revision, again, changedAgain, err)
	}

	// A rebind and a resume both carry the spawn-time mode; canonical wins.
	if err := store.bindSession(hosted.ID, "nano-1", "native-1", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.presentationModeFor(hosted.ID); !ok || got != presentationBackground {
		t.Fatalf("mode after stale rebind = %q ok=%v", got, ok)
	}
	if _, err := store.ensureSession(hosted.ID, "codex", presentationShared); err != nil {
		t.Fatalf("stale resume mode treated as identity conflict: %v", err)
	}

	// Restart: the reloaded cache must serve the committed mode.
	reloaded, err := newControlAgentExecutionStore(control, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.presentationModeFor(hosted.ID); !ok || got != presentationBackground {
		t.Fatalf("mode after restart = %q ok=%v", got, ok)
	}

	if _, _, _, err := store.setPresentation("nht_missing", presentationBackground); err == nil {
		t.Fatal("unknown hosted thread accepted")
	}
	if _, _, _, err := store.setPresentation(hosted.ID, "hidden"); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestSetSidebarPlacementIsDurableAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	control, err := openControlMetadataStore(filepath.Join(dir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := store.beginSession("parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := store.beginSessionWithInvoker("child", "child-hash", "grok", presentationShared, parent)
	if err != nil {
		t.Fatal(err)
	}
	if child.SidebarPlacement != sidebarPlacementInvoker {
		t.Fatalf("creation placement = %q", child.SidebarPlacement)
	}

	rec, revision, changed, err := store.setSidebarPlacement(child.ID, sidebarPlacementStandalone)
	if err != nil || !changed || rec.SidebarPlacement != sidebarPlacementStandalone {
		t.Fatalf("standalone = %+v revision=%d changed=%v err=%v", rec, revision, changed, err)
	}
	var placement string
	if err := control.db.QueryRow(`SELECT sidebar_placement FROM hosted_threads WHERE hosted_thread_id=?`, child.ID).Scan(&placement); err != nil || placement != sidebarPlacementStandalone {
		t.Fatalf("canonical placement = %q err=%v", placement, err)
	}
	_, again, changedAgain, err := store.setSidebarPlacement(child.ID, sidebarPlacementStandalone)
	if err != nil || changedAgain || again != revision {
		t.Fatalf("same-value put revision %d -> %d changed=%v err=%v", revision, again, changedAgain, err)
	}

	reloaded, err := newControlAgentExecutionStore(control, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.session(child.ID)
	if !ok || got.SidebarPlacement != sidebarPlacementStandalone || got.InvokerHostedThreadID != parent.ID {
		t.Fatalf("reloaded child = %+v ok=%v", got, ok)
	}
	if _, _, _, err := store.setSidebarPlacement(child.ID, "floating"); err == nil {
		t.Fatal("invalid placement accepted")
	}
}

// A correction steered into a running turn shares the turn with the prompt
// that started it, so the turn's terminal frame no longer names one
// operation: finishing "the" operation would close an arbitrary one, possibly
// for a prompt Codex never consumed. Each finishes only on its own proof, and
// every one that does gets the same outcome and final answer, found only in
// that native thread and turn.
func TestSharedProviderTurnFinishesEachOperationOnItsOwnProof(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	session, _, err := store.beginSession("caller-session", "request-hash", "codex", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindSession(session.ID, "nano-1", "native-1", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, action := range []string{"starter", "correction", "rejected"} {
		turn, _, err := store.beginTurn(session.ID, action, "hash-"+action)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.markTurnDelivery(turn.ID, "shared-turn", agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, turn.ID)
	}
	if err := store.observeCandidate("codex", "native-1", "shared-turn", "final", "final_answer", "answer to the correction"); err != nil {
		t.Fatal(err)
	}
	if err := store.markTurnTerminal("codex", "native-1", "shared-turn", agentTurnCompleted, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if got, _ := store.turn(id); agentTurnTerminal(got.Status) {
			t.Fatalf("terminal frame finished %s of a shared turn without its own proof: %#v", got.ClientActionID, got)
		}
	}
	for _, id := range ids[:2] {
		if err := store.finishJoinedTurn(id, "shared-turn", agentTurnCompleted, nil, "answer to the correction", "final", agentResultMeta{}); err != nil {
			t.Fatal(err)
		}
		got, _ := store.turn(id)
		if got.Status != agentTurnCompleted || got.FinalItemID != "final" || got.FinalResponse != "answer to the correction" {
			t.Fatalf("joined operation %s = %#v", got.ClientActionID, got)
		}
	}
	if got, _ := store.turn(ids[2]); agentTurnTerminal(got.Status) {
		t.Fatalf("an operation never proven to join finished: %#v", got)
	}
	// A late member takes the answer only from a member of the same native
	// thread and turn. A record that lost its native thread, as an old one
	// may have, supplies nothing, even with the named final id.
	if answer, _, ok := store.sharedTurnResult(session.ID, "codex", "native-1", "shared-turn", "final"); !ok || answer != "answer to the correction" {
		t.Fatalf("shared result = %q %v; want the finished members' answer", answer, ok)
	}
	if err := store.observeCandidate("codex", "native-1", "shared-turn", "final-unscoped", "final_answer", "unscoped answer"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	unscoped := store.turns[ids[2]]
	unscoped.NativeThreadID = ""
	store.setTurnLocked(unscoped.ID, &unscoped)
	store.mu.Unlock()
	if answer, _, ok := store.sharedTurnResult(session.ID, "codex", "native-1", "shared-turn", "final-unscoped"); ok {
		t.Fatalf("a record with no native thread supplied %q", answer)
	}
}

// A saved resume includes turns older than the retained operations. Looking up
// each of them across the fleet delays readiness even when none needs a write.
// Boundary: the real resume observer over a persisted execution store.
func TestAgentExecutionCodexResumeScansTheStoreOnce(t *testing.T) {
	store := newTestAgentExecutionStore(t, "resume-run")
	t.Cleanup(func() { _ = store.control.Close() })
	const threadCount = 200
	for thread := range threadCount {
		hostedID := fmt.Sprintf("hosted-%d", thread)
		if _, err := store.ensureSession(hostedID, "codex", presentationShared); err != nil {
			t.Fatal(err)
		}
		for turn := range maxAgentTurns {
			id := fmt.Sprintf("op-%d-%d", thread, turn)
			rec := agentTurnRecord{
				ID: id, HostedThreadID: hostedID, ClientActionID: id, TransportActionID: id, PromptHash: id,
				Provider: "codex", NativeThreadID: fmt.Sprintf("native-%d", thread), ProviderTurnID: fmt.Sprintf("turn-%d-%d", thread, turn),
				Status: agentTurnCompleted, FinalResponse: "already reported", FinalItemID: "saved-answer", RunID: "resume-run",
				CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z", Revision: 1,
			}
			store.setTurnLocked(rec.ID, &rec)
		}
	}
	store.mu.Lock()
	err := store.persistLocked(nil)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	const resumedTurns = 2000
	turns := make([]codexResumeProjectionTurn, resumedTurns)
	for i := range turns {
		turns[i] = codexResumeProjectionTurn{ID: fmt.Sprintf("turn-0-%d", i), Status: agentTurnCompleted}
	}
	response, err := json.Marshal(map[string]any{"thread": map[string]any{"id": "native-0", "turns": turns}})
	if err != nil {
		t.Fatal(err)
	}
	before, changed := maps.Clone(store.turns), store.changed
	sess := &session{provider: "codex", hostedThreadID: "hosted-0", agentExecutions: store}
	started := time.Now()
	sess.observeCodexResume(decodedCodexResume(response))
	took := time.Since(started)
	t.Logf("resume of %d terminal turns over %d retained operations took %s", resumedTurns, len(before), took)
	// Plain runs only: the per-turn path takes several seconds there, while
	// -race on a loaded machine can push the one-pass path toward the bound.
	if !raceEnabled && took > 2*time.Second {
		t.Fatalf("terminal resume took %s for %d saved turns over %d operations", took, resumedTurns, len(before))
	}
	if !maps.Equal(store.turns, before) || store.changed != changed {
		t.Fatal("already terminal or unknown resume turns changed the execution store")
	}
}

// Offline completion includes every open operation in the hosted thread,
// while terminal marking requires one owner under the lenient native match.
// A later duplicate reply must not overwrite a turn it just completed.
func TestAgentExecutionResumedTurnsPreserveMatchingAndReplyOrder(t *testing.T) {
	store := newTestAgentExecutionStore(t, "resume-matching")
	t.Cleanup(func() { _ = store.control.Close() })
	for _, hosted := range []struct{ id, provider string }{{"target", "codex"}, {"other", "codex"}, {"claude", "claude"}} {
		if _, err := store.ensureSession(hosted.id, hosted.provider, presentationShared); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct{ id, hosted, provider, native, turn string }{
		{"open", "target", "codex", "native", "turn-open"},
		{"legacy", "target", "codex", "", "turn-legacy"},
		{"failed", "target", "codex", "native", "turn-failed"},
		{"ambiguous", "target", "codex", "native", "turn-ambiguous"},
		{"ambiguous-other", "target", "codex", "old-native", "turn-ambiguous"},
		{"wrong-native", "target", "codex", "old-native", "turn-wrong-native"},
		{"other-hosted", "other", "codex", "native", "turn-other-hosted"},
		{"other-provider", "claude", "claude", "native", "turn-other-provider"},
		{"saved-proof", "target", "codex", "native", "turn-saved-proof"},
		{"shared-open", "target", "codex", "native", "turn-shared"},
		{"shared-terminal", "target", "codex", "native", "turn-shared"},
	} {
		rec := agentTurnRecord{
			ID: fixture.id, HostedThreadID: fixture.hosted, ClientActionID: fixture.id, TransportActionID: fixture.id, PromptHash: fixture.id,
			Provider: fixture.provider, NativeThreadID: fixture.native, ProviderTurnID: fixture.turn, Status: agentTurnRunning,
			CandidateResponse: "candidate", CandidateItemID: "candidate-item", CandidateBytes: 9,
			RunID: "resume-matching", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z", Revision: 1,
		}
		store.setTurnLocked(rec.ID, &rec)
	}
	terminal := store.turns["shared-terminal"]
	terminal.Status, terminal.FinalResponse, terminal.FinalItemID = agentTurnCompleted, "already reported", "saved-item"
	store.setTurnLocked(terminal.ID, &terminal)
	store.mu.Lock()
	err := store.persistLocked(nil)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	bookmark := store.changes.since("", nil).Bookmark
	failure := &agentExecutionError{Source: "provider", Code: "failed", Message: "failed offline"}
	marks := []agentTerminalTurn{
		{ProviderTurnID: "turn-open", Status: agentTurnCompleted, Response: "reply", ItemID: "reply-item"},
		{ProviderTurnID: "turn-legacy", Status: agentTurnCompleted},
		{ProviderTurnID: "turn-failed", Status: agentTurnFailed, Error: failure},
		{ProviderTurnID: "turn-ambiguous", Status: agentTurnCompleted},
		{ProviderTurnID: "turn-wrong-native", Status: agentTurnCompleted},
		{ProviderTurnID: "turn-other-hosted", Status: agentTurnCompleted},
		{ProviderTurnID: "turn-other-provider", Status: agentTurnCompleted},
		{ProviderTurnID: "turn-unknown", Status: agentTurnCompleted},
		{ProviderTurnID: "turn-open", Status: agentTurnFailed, Error: failure},
		{ProviderTurnID: "turn-saved-proof", Status: agentTurnCompleted, Response: "saved proof answer", ItemID: "saved-proof-item", FinishOnSavedProof: true},
		{ProviderTurnID: "turn-shared", Status: agentTurnCompleted, Response: "shared answer", ItemID: "shared-item"},
	}
	matches := store.resumedTurnMatches("target", "codex", "native", marks)
	ended, err := store.markResumedTurnsTerminal("target", "codex", "native", marks, matches)
	if err != nil {
		t.Fatal(err)
	}
	if want := []bool{true, true, true, true, true, false, false, false, false, true, true}; !slices.Equal(ended, want) {
		t.Fatalf("ended while down = %v, want %v", ended, want)
	}
	open, _ := store.turn("open")
	legacy, _ := store.turn("legacy")
	failed, _ := store.turn("failed")
	if open.Status != agentTurnCompleted || open.FinalResponse != "reply" || open.FinalItemID != "reply-item" || open.Error != nil {
		t.Fatalf("completion overwritten by duplicate reply: %+v", open)
	}
	if legacy.Status != agentTurnCompleted || legacy.FinalResponse != "candidate" || legacy.FinalItemID != "candidate-item" {
		t.Fatalf("legacy native match lost candidate fallback: %+v", legacy)
	}
	if failed.Status != agentTurnFailed || failed.Error != failure || failed.CandidateResponse != "" || failed.FinalResponse != "" {
		t.Fatalf("failure retained candidate output or lost its error: %+v", failed)
	}
	for _, id := range []string{"ambiguous-other", "wrong-native", "other-provider"} {
		if rec, _ := store.turn(id); rec.Status != agentTurnRunning || rec.Revision != 1 {
			t.Fatalf("unmatched operation %s changed: %+v", id, rec)
		}
	}
	for _, fixture := range []struct{ id, response, item string }{
		{"saved-proof", "saved proof answer", "saved-proof-item"},
		{"shared-open", "shared answer", "shared-item"},
	} {
		if rec, _ := store.turn(fixture.id); rec.Status != agentTurnRunning || rec.CandidateResponse != fixture.response || rec.CandidateItemID != fixture.item {
			t.Fatalf("operation awaiting its own proof lost the resumed answer: %+v", rec)
		}
	}
	if rec, _ := store.turn("shared-terminal"); rec.Status != agentTurnCompleted || rec.FinalResponse != "already reported" || rec.FinalItemID != "saved-item" || rec.Revision != 1 {
		t.Fatalf("already terminal member changed: %+v", rec)
	}
	transitions := store.changes.since(bookmark, nil).Transitions
	var ids []string
	for _, transition := range transitions {
		ids = append(ids, transition.OperationID)
	}
	if want := []string{"open", "legacy", "failed", "ambiguous", "other-hosted"}; !slices.Equal(ids, want) {
		t.Fatalf("terminal transition order = %v, want %v", ids, want)
	}
	var persisted int
	if err := store.control.db.QueryRow(`SELECT count(*) FROM hosted_turn_operations WHERE status IN ('completed','failed')`).Scan(&persisted); err != nil || persisted != 6 {
		t.Fatalf("persisted terminal rows = %d (%v), want 6", persisted, err)
	}
}

// Prompt-store reads release the execution lock. A resumed mark must not
// finish an operation that completed, disappeared, or moved to another turn
// or native thread during that gap.
func TestAgentExecutionResumedTurnsRevalidateMatches(t *testing.T) {
	for _, test := range []struct {
		name   string
		ended  bool
		change func(*agentExecutionStore, string, string) error
	}{
		{"finished", false, func(store *agentExecutionStore, hosted, operation string) error {
			return store.markTurnTerminal("codex", "native", "turn", agentTurnCompleted, nil, "live answer", "live-item")
		}},
		{"rebound", false, func(store *agentExecutionStore, hosted, operation string) error {
			return store.markTurnDelivery(operation, "other-turn", agentTurnRunning, nil)
		}},
		{"removed", false, func(store *agentExecutionStore, hosted, operation string) error {
			return store.removeSession(hosted)
		}},
		{"native changed", true, func(store *agentExecutionStore, hosted, operation string) error {
			return store.bindSession(hosted, "nano", "other-native", "ready", presentationShared)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestAgentExecutionStore(t, "resume-revalidate")
			t.Cleanup(func() { _ = store.control.Close() })
			hosted, _, err := store.beginSession("session", "hash", "codex", presentationShared)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.bindSession(hosted.ID, "nano", "native", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			op, _, err := store.beginTurn(hosted.ID, "action", "hash")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.markTurnDelivery(op.ID, "turn", agentTurnRunning, nil); err != nil {
				t.Fatal(err)
			}
			marks := []agentTerminalTurn{{ProviderTurnID: "turn", Status: agentTurnCompleted, Response: "resume answer", ItemID: "resume-item"}}
			matches := store.resumedTurnMatches(hosted.ID, "codex", "native", marks)
			if err := test.change(store, hosted.ID, op.ID); err != nil {
				t.Fatal(err)
			}
			before, existed := store.turn(op.ID)
			changed := store.changed
			ended, err := store.markResumedTurnsTerminal(hosted.ID, "codex", "native", marks, matches)
			if err != nil || !slices.Equal(ended, []bool{test.ended}) {
				t.Fatalf("ended while down = %v, err=%v; want %v", ended, err, test.ended)
			}
			if after, found := store.turn(op.ID); found != existed || after != before || store.changed != changed {
				t.Fatalf("resume changed an operation after its binding or status changed: before=%+v after=%+v", before, after)
			}
		})
	}
}

// Registration, one-row persistence and attention reads must cost only the
// changed thread's turns, even when the retained fleet is large.
func TestAgentExecutionThreadWorkDoesNotScanFleet(t *testing.T) {
	store := newTestAgentExecutionStore(t, "indexed-run")
	t.Cleanup(func() { _ = store.control.Close() })
	for thread := range 200 {
		if _, err := store.ensureSession(fmt.Sprintf("indexed-host-%d", thread), "codex", presentationShared); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	for thread := range 200 {
		for turn := range maxAgentTurns {
			id := fmt.Sprintf("indexed-op-%d-%d", thread, turn)
			rec := agentTurnRecord{ID: id, HostedThreadID: fmt.Sprintf("indexed-host-%d", thread), ClientActionID: id, TransportActionID: id, PromptHash: id, Provider: "codex", Status: agentTurnCompleted, RunID: "indexed-run", CreatedAt: fmt.Sprintf("%04d", turn), UpdatedAt: fmt.Sprintf("%04d", turn), Revision: 1}
			store.setTurnLocked(rec.ID, &rec)
		}
	}
	err := store.persistLocked(nil)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	store.sessionsNeedingAttention()
	for _, name := range []string{"bind", "persist", "attention"} {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			for range 100 {
				if name == "bind" {
					if err := store.bindSession("indexed-host-0", "nano", "", "ready", presentationShared); err != nil {
						t.Fatal(err)
					}
					continue
				}
				store.mu.Lock()
				rec := store.turns["indexed-op-0-99"]
				rec.Status = agentTurnFailed
				touchAgentTurn(&rec)
				store.setTurnLocked(rec.ID, &rec)
				if name == "persist" {
					err = store.persistLocked(nil)
				}
				store.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if name == "attention" && !store.sessionsNeedingAttention()["indexed-host-0"] {
					t.Fatal("changed latest turn did not need attention")
				}
			}
			took := time.Since(started)
			t.Logf("100 %s operations over 20000 turns took %s", name, took)
			// Plain runs only: no single wall-clock bound fits both -race on a
			// loaded machine and a clear margin under the plain base's scans.
			bound := map[string]time.Duration{"bind": 300 * time.Millisecond, "persist": 400 * time.Millisecond, "attention": 50 * time.Millisecond}[name]
			if !raceEnabled && took > bound {
				t.Fatalf("%s scanned the fleet: %s", name, took)
			}
		})
	}
}

// Reloaded rows pin preservation on load, eligible pruning after a change,
// and the update/delete batch. The database, rather than the cache, proves it.
func TestAgentExecutionIndexedPersistenceKeepsRetentionAndRows(t *testing.T) {
	store := newTestAgentExecutionStore(t, "retention-index")
	t.Cleanup(func() { _ = store.control.Close() })
	if _, err := store.ensureSession("retained", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	// Times as stored, a microsecond apart: RFC3339Nano trims trailing zeros, so
	// as text ".2374Z" (row 0) and ".23741Z" (row 10) sort after later rows.
	base := time.Date(2026, 1, 1, 0, 0, 4, 237400000, time.UTC)
	fixture := func(i int) agentTurnRecord {
		id := fmt.Sprintf("retained-%03d", i)
		at := base.Add(time.Duration(i) * time.Microsecond).Format(time.RFC3339Nano)
		return agentTurnRecord{
			ID: id, HostedThreadID: "retained", ClientActionID: id, TransportActionID: id, PromptHash: id,
			Provider: "codex", ProviderTurnID: "provider-" + id, Status: agentTurnCompleted,
			Origin: turnOriginAgent, SenderSubmittedAt: "2026-01-01T00:00:00Z",
			RunID: store.runID, CreatedAt: at, UpdatedAt: at, Revision: 1,
		}
	}
	tx, err := store.control.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range maxAgentTurns + 2 {
		if err := insertTurnOperation(tx, fixture(i)); err != nil {
			t.Fatal(err)
		}
	}
	open := fixture(999)
	open.Status = agentTurnRunning
	if err := insertTurnOperation(tx, open); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	store = reloadIndexedExecutionStore(t, store)
	store.pruneEligible = func(_ context.Context, _ string, actions []string) (map[string]bool, error) {
		eligible := make(map[string]bool, len(actions))
		for _, action := range actions {
			eligible[action] = true
		}
		return eligible, nil
	}
	for _, step := range []struct {
		name  string
		apply func()
		first int
		last  int
		skip  int
	}{
		{"unchanged first persist after load", func() {}, 0, 101, -1},
		{"eligible pruning after change", func() {
			rec := store.turns[open.ID]
			touchAgentTurn(&rec)
			store.setTurnLocked(rec.ID, &rec)
		}, 2, 101, -1},
		{"new terminal over limit", func() { rec := fixture(102); store.setTurnLocked(rec.ID, &rec) }, 3, 102, -1},
		{"update and dirty deletion", func() {
			rec := store.turns["retained-050"]
			rec.FinalResponse, rec.FinalItemID = "updated answer", "updated-item"
			rec.ProviderTurnID = "updated-provider-turn"
			touchAgentTurn(&rec)
			store.setTurnLocked(rec.ID, &rec)
			store.setTurnLocked("retained-051", nil)
		}, 3, 102, 51},
	} {
		t.Run(step.name, func(t *testing.T) {
			store.mu.Lock()
			step.apply()
			err := store.persistLocked(nil)
			store.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			reloaded := reloadIndexedExecutionStore(t, store)
			want := 1
			for i := range 103 {
				id := fixture(i).ID
				_, found := reloaded.turn(id)
				keep := i >= step.first && i <= step.last && i != step.skip
				if found != keep {
					t.Fatalf("row %s present=%v want=%v", id, found, keep)
				}
				if keep {
					want++
					rec, _ := reloaded.turn(id)
					providerTurn := fixture(i).ProviderTurnID
					if step.skip == 51 && i == 50 {
						providerTurn = "updated-provider-turn"
					}
					if rec.Origin != turnOriginAgent || rec.SenderSubmittedAt != "2026-01-01T00:00:00Z" || rec.TransportActionID != id || rec.ProviderTurnID != providerTurn {
						t.Fatalf("committed submission columns changed: %+v", rec)
					}
				}
			}
			if len(reloaded.turns) != want || reloaded.turns[open.ID].Status != agentTurnRunning {
				t.Fatalf("rows=%d want=%d; open=%+v", len(reloaded.turns), want, reloaded.turns[open.ID])
			}
			if step.skip == 51 {
				if rec := reloaded.turns["retained-050"]; rec.FinalResponse != "updated answer" || rec.FinalItemID != "updated-item" || rec.Revision != 2 {
					t.Fatalf("updated row=%+v", rec)
				}
			}
		})
	}
}

func reloadIndexedExecutionStore(t *testing.T, store *agentExecutionStore) *agentExecutionStore {
	t.Helper()
	reloaded, err := newControlAgentExecutionStore(store.control, store.runID)
	if err != nil {
		t.Fatal(err)
	}
	return reloaded
}

// Changing, moving or deleting the latest turn must invalidate exactly its
// threads. Equal submission times retain the old lexicographic ID rule.
func TestAgentExecutionIndexedAttentionTracksLatestAndMoves(t *testing.T) {
	store := newTestAgentExecutionStore(t, "attention-index")
	t.Cleanup(func() { _ = store.control.Close() })
	for _, hosted := range []string{"target", "other"} {
		if _, err := store.ensureSession(hosted, "codex", presentationShared); err != nil {
			t.Fatal(err)
		}
	}
	// Creation times as stored: RFC3339Nano trims trailing zeros, so as text
	// the earliest (".2374Z") sorts after the others (".237401Z", ".237402Z").
	at := time.Date(2026, 10, 15, 12, 0, 4, 237400000, time.UTC)
	earlier, later, latest := at.Format(time.RFC3339Nano), at.Add(time.Microsecond).Format(time.RFC3339Nano), at.Add(2*time.Microsecond).Format(time.RFC3339Nano)
	for index, step := range []struct {
		id, hosted, created, status string
		attention                   map[string]bool
	}{
		{"a", "target", later, agentTurnCompleted, map[string]bool{"target": false}},
		{"b", "target", later, agentTurnFailed, map[string]bool{"target": true}},
		{"c", "target", earlier, agentTurnFailed, map[string]bool{"target": true}},
		{"b", "target", later, agentTurnCompleted, map[string]bool{"target": false}},
		{"b", "target", later, agentTurnDeliveryUnknown, map[string]bool{"target": true}},
		{"d", "target", latest, agentTurnCompleted, map[string]bool{"target": false}},
		{"d", "", "", "", map[string]bool{"target": true}},
		{"b", "other", later, agentTurnDeliveryUnknown, map[string]bool{"target": false, "other": true}},
		{"a", "", "", "", map[string]bool{"target": true, "other": true}},
		{"c", "", "", "", map[string]bool{"other": true}},
	} {
		store.mu.Lock()
		if step.hosted == "" {
			store.setTurnLocked(step.id, nil)
		} else {
			rec := agentTurnRecord{ID: step.id, HostedThreadID: step.hosted, ClientActionID: step.id, TransportActionID: step.id, PromptHash: step.id, Provider: "codex", RunID: store.runID, CreatedAt: step.created, Status: step.status}
			rec.Revision = store.turns[step.id].Revision
			touchAgentTurn(&rec)
			store.setTurnLocked(rec.ID, &rec)
		}
		// Hosted identity moves are an in-memory index boundary; operation
		// hosted identity is immutable in SQLite. Persist the preceding cases.
		if index < 7 {
			if err := store.persistLocked(nil); err != nil {
				store.mu.Unlock()
				t.Fatal(err)
			}
		}
		store.mu.Unlock()
		if index < 7 {
			if got := reloadIndexedExecutionStore(t, store).sessionsNeedingAttention(); !maps.Equal(got, step.attention) {
				t.Fatalf("reloaded attention=%v want=%v", got, step.attention)
			}
		}
		for _, hosted := range []string{"target", "other"} {
			if got := store.sessionNeedsAttention(hosted); got != step.attention[hosted] {
				t.Fatalf("after %s/%s single attention[%s]=%v want=%v", step.id, step.status, hosted, got, step.attention[hosted])
			}
		}
		got := store.sessionsNeedingAttention()
		if !maps.Equal(got, step.attention) {
			t.Fatalf("after %s/%s fleet attention=%v want=%v", step.id, step.status, got, step.attention)
		}
		got["other"] = false
		if store.sessionNeedsAttention("other") != step.attention["other"] {
			t.Fatal("caller changed the cached attention map")
		}
	}
}

// Indexed binding still touches every open turn of its thread, and preserves
// terminal and unrelated operation revisions across both binding paths.
func TestAgentExecutionIndexedBindingsPreserveTurnUpdates(t *testing.T) {
	store := newTestAgentExecutionStore(t, "binding-index")
	t.Cleanup(func() { _ = store.control.Close() })
	for _, hosted := range []string{"bound", "unrelated"} {
		if _, err := store.ensureSession(hosted, "codex", presentationShared); err != nil {
			t.Fatal(err)
		}
	}
	open, _, err := store.beginTurn("bound", "open", "open")
	if err != nil {
		t.Fatal(err)
	}
	terminal, _, err := store.beginTurn("bound", "terminal", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.markTurnDelivery(terminal.ID, "", agentTurnCompleted, nil); err != nil {
		t.Fatal(err)
	}
	terminal, _ = store.turn(terminal.ID)
	unrelated, _, err := store.beginTurn("unrelated", "other", "other")
	if err != nil {
		t.Fatal(err)
	}
	for index, apply := range []func() error{
		func() error { return store.bindSession("bound", "bound-nano", "", "ready", presentationShared) },
		func() error { return store.updateSession("bound", "updated-nano", "", "reconnecting", "") },
	} {
		if err := apply(); err != nil {
			t.Fatal(err)
		}
		reloaded := reloadIndexedExecutionStore(t, store)
		if reloaded.turns[open.ID].Revision != open.Revision+int64(index)+1 || reloaded.turns[terminal.ID].Revision != terminal.Revision || reloaded.turns[unrelated.ID].Revision != unrelated.Revision {
			t.Fatal("binding lost an open-turn update or touched a terminal/unrelated turn")
		}
		if reloaded.turns[open.ID].NanoSessionID != store.sessions["bound"].NanoSessionID {
			t.Fatal("open turn lost the current transport binding")
		}
	}
}

// A point reference must build only its matching summary, preserving the fleet
// ordering (including timestamp precision), fields and exited-session filter.
func TestAgentExecutionLiveHostedSessionSelectsOnlyMatchingSummary(t *testing.T) {
	store := newTestAgentExecutionStore(t, "summary-index")
	t.Cleanup(func() { _ = store.control.Close() })
	if _, err := store.ensureSession("target", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	turn, _, err := store.beginTurn("target", "failed", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.markTurnDelivery(turn.ID, "", agentTurnFailed, nil); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	selected := &session{
		id: "a", provider: "codex", hostedThreadID: "target", threadID: "native", displayName: "display",
		model: "model", cwd: "/fixture", createdAt: created.Add(900 * time.Millisecond),
		invokerHostedThreadID: "parent", invokerNativeThreadID: "parent-native", invokerProvider: "claude",
		sidebarPlacement: sidebarPlacementStandalone, spawnMode: "resume", spawnError: "error",
		lifecycleState: testLifecycleSnapshot("ready", true), turnStartedAtMs: 1234,
		metadataDegraded: true, turnEffort: "high", permissionMode: "never", turnServiceTier: "fast",
	}
	unrelated := &session{id: "unrelated", hostedThreadID: "other", lifecycleState: testLifecycleSnapshot("ready", false)}
	s := &server{agentExecutions: store, sessions: map[string]*session{
		"a":         selected,
		"z":         {id: "z", hostedThreadID: "target", createdAt: created.Add(100 * time.Millisecond), lifecycleState: testLifecycleSnapshot("ready", false)},
		"exited":    {id: "exited", hostedThreadID: "target", createdAt: created.Add(-time.Hour), lifecycleState: testLifecycleSnapshot("exited", false)},
		"unrelated": unrelated,
	}}
	unrelated.stateMu.Lock()
	done := make(chan struct{})
	var got sessionSummary
	var found bool
	go func() {
		got, found = s.liveHostedSession("target")
		close(done)
	}()
	select {
	case <-done:
		unrelated.stateMu.Unlock()
	case <-time.After(time.Second):
		unrelated.stateMu.Unlock()
		<-done
		t.Fatal("point reference waited for an unrelated session's summary")
	}
	want := sessionSummary{
		ID: "a", Provider: "codex", HostedThreadID: "target", ThreadID: "native", Name: "display",
		Model: "model", Cwd: "/fixture", Status: "ready", CreatedAt: "2026-01-01T00:00:00Z",
		InvokerHostedThreadID: "parent", InvokerNativeThreadID: "parent-native", InvokerProvider: "claude",
		PresentationMode: presentationShared, SidebarPlacement: sidebarPlacementStandalone, SpawnMode: "resume",
		Error: "error", TurnActive: true, ThreadStatus: fleetWorking, TurnStartedAtMs: 1234, NeedsAttention: true, MetadataDegraded: true,
		Posture: sessionPosture{Model: "model", Effort: "high", PermissionMode: "never", ServiceTier: "fast"},
	}
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("point summary=%+v found=%v want=%+v", got, found, want)
	}
	if _, found := s.liveHostedSession("missing"); found {
		t.Fatal("unknown hosted thread resolved to a live session")
	}
}

// Failure caught: updateSession bumped revisions and saved under the store
// lock even when nothing changed, and each spawn repeats its status, so boot
// registrations queued behind saves of nothing. A repeated update must leave
// the session and its open turn untouched; a real change must still reach the
// open turn. Boundary: the store's updateSession.
func TestUpdateSessionThatChangesNothingSavesNothing(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run")
	hosted, _, err := store.beginSession("caller", "hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := store.beginTurn(hosted.ID, "action", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.updateSession(hosted.ID, "nano-1", "native", "ready", ""); err != nil {
		t.Fatal(err)
	}
	session, _ := store.session(hosted.ID)
	turn, _ := store.turn(op.ID)
	if err := store.updateSession(hosted.ID, "nano-1", "native", "ready", ""); err != nil {
		t.Fatal(err)
	}
	again, _ := store.session(hosted.ID)
	turnAgain, _ := store.turn(op.ID)
	if again.Revision != session.Revision || turnAgain.Revision != turn.Revision {
		t.Fatalf("an update that changes nothing moved revisions: session %d -> %d, turn %d -> %d", session.Revision, again.Revision, turn.Revision, turnAgain.Revision)
	}
	if err := store.updateSession(hosted.ID, "nano-2", "", "", ""); err != nil {
		t.Fatal(err)
	}
	moved, _ := store.turn(op.ID)
	if moved.Revision == turn.Revision || moved.NanoSessionID != "nano-2" {
		t.Fatalf("open turn after a new Nano session = revision %d (was %d), nano session %q; want it moved to nano-2", moved.Revision, turn.Revision, moved.NanoSessionID)
	}
}

// Catches: draining interactions resuming the older open turn and leaving the
// newer one waiting, when the older one's creation time trims to ".2374Z" and so
// sorts after the newer ".237401Z" as text. Boundary: the store's markWaiting.
func TestMarkWaitingResumesTheNewestOpenTurnAcrossTrimmedFractions(t *testing.T) {
	store := newTestAgentExecutionStore(t, "waiting-newest")
	t.Cleanup(func() { _ = store.control.Close() })
	if _, err := store.ensureSession("target", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 4, 237400000, time.UTC)
	store.mu.Lock()
	for _, step := range []struct {
		id, status string
		created    time.Time
	}{{"older", agentTurnRunning, at}, {"newer", agentTurnWaiting, at.Add(time.Microsecond)}} {
		rec := agentTurnRecord{ID: step.id, HostedThreadID: "target", ClientActionID: step.id, TransportActionID: step.id, PromptHash: step.id, Provider: "codex", RunID: store.runID, CreatedAt: step.created.Format(time.RFC3339Nano), Status: step.status}
		touchAgentTurn(&rec)
		store.setTurnLocked(rec.ID, &rec)
	}
	store.mu.Unlock()
	if err := store.markWaiting("target", "", false); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.turn("newer"); rec.Status != agentTurnRunning {
		t.Fatalf("newer turn status = %q after interactions drained, want %q", rec.Status, agentTurnRunning)
	}
}

// Catches: a hosted thread's earlier native ids listed out of recency (and so
// the wrong earlier name chosen) when one last-seen time trims to ".2374Z" and
// sorts after the later ".237401Z" as text. Boundary: nativeIDsForHosted.
func TestNativeIDsForHostedAreMostRecentFirstAcrossTrimmedFractions(t *testing.T) {
	store := newTestAgentExecutionStore(t, "native-recency")
	t.Cleanup(func() { _ = store.control.Close() })
	if _, err := store.ensureSession("target", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 4, 237400000, time.UTC)
	for native, seen := range map[string]time.Time{"native-older": at, "native-newer": at.Add(time.Microsecond)} {
		stamp := seen.Format(time.RFC3339Nano)
		if _, err := store.control.db.Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,provisional,retired_at,first_seen_at,last_seen_at) VALUES('target','codex',?,0,?,?,?)`, native, stamp, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := store.nativeIDsForHosted("target")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "native-newer,native-older" {
		t.Fatalf("native ids = %v, want the most recently seen first", ids)
	}
}
