package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
)

func newTestControlMetadata(t *testing.T) *MetadataStore {
	t.Helper()
	store, err := openTestMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
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
		_, err := control.View(record.ID)
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
func migrateTestControl(t *testing.T, metadata *MetadataStore) {
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
func resolveTestProvisional(t *testing.T, control *MetadataStore, hostedID, provider, handle, native string) {
	t.Helper()
	if err := control.ClaimNativeIDResolving(hostedID, provider, native, false, &nativeResolution{Handle: handle, Native: native}); err != nil {
		t.Fatal(err)
	}
}

func newTestHostedControl(t *testing.T) (*MetadataStore, *ExecutionStore) {
	t.Helper()
	metadata := newTestControlMetadata(t)
	migrateTestControl(t, metadata)
	executions, err := newControlAgentExecutionStore(metadata, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	return metadata, executions
}

func TestControlMetadataRequestedThenConfirmedIsOneCurrentRecord(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_priority", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	requested, err := store.AppendEvent(metadataEvent{
		HostedThreadID: "nht_priority", Field: postureFieldServiceTier, Value: "priority",
		Disposition: metadataDispositionRequested, Source: postureSourceCreation,
		Freshness: metadataFreshnessLive, SourceEventKey: "creation-priority",
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.AppendEvent(metadataEvent{
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
	if err := store.EnsureHostedThread("nht_replay", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_replay", Field: postureFieldModel, Value: "new", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_replay", Field: postureFieldModel, Value: "old", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessReplayed}); err != nil {
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
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "missing", Field: postureFieldModel, Value: "x", Disposition: metadataDispositionRequested, Source: postureSourceCreation}); err == nil {
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
	if err := store.EnsureHostedThread("nht_native", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimNativeID("nht_native", "claude", "cs_provisional", true); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimNativeID("nht_native", "claude", "native-successor", false); !errors.Is(err, errUnresolvedProvisional) {
		t.Fatalf("an unresolved provisional handle was retired: %v", err)
	}
	pair := nativeResolution{Handle: "cs_provisional", Native: "native-successor"}
	if err := store.ClaimNativeIDResolving("nht_native", "claude", "native-successor", false, &pair); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimNativeIDResolving("nht_native", "claude", "native-successor", false, &pair); err != nil {
		t.Fatalf("retrying a recorded resolution = %v", err)
	}
	other := nativeResolution{Handle: "cs_provisional", Native: "native-other"}
	if err := store.ClaimNativeIDResolving("nht_native", "claude", "native-other", false, &other); !errors.Is(err, errUnresolvedProvisional) {
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
	if err := store.EnsureHostedThread("nht_generation", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginTransportBinding("nht_generation")
	if err != nil || generation != 1 {
		t.Fatalf("binding generation = %d, %v", generation, err)
	}
	requested, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "new", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "old", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation - 1}); err != nil {
		t.Fatal(err)
	}
	assertNoEffective(t, store, "nht_generation", postureFieldModel)
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "new", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation}); err != nil {
		t.Fatal(err)
	}
	assertCurrentValue(t, store, "nht_generation", postureFieldModel, `"new"`, metadataDispositionConfirmed)
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "linked", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation - 1, CausationEventID: &requested.ID}); err != nil {
		t.Fatal(err)
	}
	assertCurrentValue(t, store, "nht_generation", postureFieldModel, `"linked"`, metadataDispositionConfirmed)
	other, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldEffort, Value: "high", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	wrong := other.ID
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_generation", Field: postureFieldModel, Value: "wrong", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation - 1, CausationEventID: &wrong}); err != nil {
		t.Fatal(err)
	}
	assertCurrentValue(t, store, "nht_generation", postureFieldModel, `"linked"`, metadataDispositionConfirmed)
}

func TestControlMetadataCoalescesObservationsButRetainsOperatorAuditEvents(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_coalesce", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginTransportBinding("nht_coalesce")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_coalesce", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_coalesce", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate observation ids = %d, %d", first.ID, second.ID)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_coalesce", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: generation}); err != nil {
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
	if err := store.EnsureHostedThread("nht_invalid", "", presentationShared, "", "", ""); err == nil {
		t.Fatal("empty provider accepted")
	}
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_missing", Field: postureFieldModel, Value: "x", Disposition: metadataDispositionRequested, Source: postureSourceCreation}); err == nil {
		t.Fatal("unknown hosted identity accepted")
	}
}

func TestMetadataChangesAreMonotonicAndRecoverableByCurrentRefetch(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_changes", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := store.BeginTransportBinding("nht_changes")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendEvent(metadataEvent{HostedThreadID: "nht_changes", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: g})
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.View("nht_changes")
	if err != nil {
		t.Fatal(err)
	}
	changes := store.ChangesAfter("nht_changes", 0)
	if len(changes) != 3 || changes[2].Revision != view.Revision || !reflect.DeepEqual(changes[0].Fields, []string{"identity"}) || !reflect.DeepEqual(changes[1].Fields, []string{"attachmentGeneration"}) || !reflect.DeepEqual(changes[2].Fields, []string{postureFieldServiceTier}) {
		t.Fatalf("changes=%+v view=%+v", changes, view)
	}
	// An exact source-key retry is a no-op and emits no revision hint.
	keyed := metadataEvent{HostedThreadID: "nht_changes", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, SourceEventKey: "change-retry"}
	if _, err := store.AppendEvent(keyed); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.View("nht_changes")
	if _, err := store.AppendEvent(keyed); err != nil {
		t.Fatal(err)
	}
	if got := store.ChangesAfter("nht_changes", latest.Revision); len(got) != 0 {
		t.Fatalf("duplicate emitted change %#v", got)
	}
}

func TestControlMetadataCurrentOnlyDedupAndIdentityValidation(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_dedup", "codex", presentationShared, "parent", "native-parent", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureHostedThread("nht_dedup", "claude", presentationShared, "parent", "native-parent", "claude"); err == nil {
		t.Fatal("provider conflict accepted")
	}
	if err := store.EnsureHostedThread("nht_dedup", "codex", presentationShared, "other", "native-parent", "claude"); err == nil {
		t.Fatal("lineage conflict accepted")
	}
	g, _ := store.BeginTransportBinding("nht_dedup")
	for _, value := range []string{"a", "b", "a"} {
		if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_dedup", Field: postureFieldModel, Value: value, Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err != nil {
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
	if err := store.ClaimNativeID("nht_dedup", "claude", "native", false); err == nil {
		t.Fatal("native provider conflict accepted")
	}
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_dedup", Field: "bogus", Value: "x", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestControlMetadataSourceEventKeyIsIdempotentOnlyForSameEvidence(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_key", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, _ := store.BeginTransportBinding("nht_key")
	e := metadataEvent{HostedThreadID: "nht_key", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, SourceEventKey: "provider-event-1"}
	first, err := store.AppendEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Committed {
		t.Fatal("first source-key evidence did not report a committed revision")
	}
	second, err := store.AppendEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.Committed {
		t.Fatalf("same retry result = ids %d/%d committed=%v", first.ID, second.ID, second.Committed)
	}
	reused, err := store.AppendEvent(first)
	if err != nil || reused.Committed || reused.ID != first.ID {
		t.Fatalf("returned event reused as retry = %#v, err=%v", reused, err)
	}
	e.Value = "luna"
	if _, err := store.AppendEvent(e); err == nil {
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
	if err := store.EnsureHostedThread("nht_reject", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, _ := store.BeginTransportBinding("nht_reject")
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_reject", Field: postureFieldServiceTier, Value: "default", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err != nil {
		t.Fatal(err)
	}
	req, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_reject", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: g})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_reject", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRejected, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, CausationEventID: &req.ID})
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
	if err := store.EnsureHostedThread("nht_confirm", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := store.BeginTransportBinding("nht_confirm")
	if err != nil {
		t.Fatal(err)
	}
	requested, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_confirm", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: g})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_confirm", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, CausationEventID: &requested.ID})
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
	if err := store.EnsureHostedThread("nht_reject_empty", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := store.BeginTransportBinding("nht_reject_empty")
	if err != nil {
		t.Fatal(err)
	}
	requested, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_reject_empty", Field: postureFieldModel, Value: "bad-model", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: g})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_reject_empty", Field: postureFieldModel, Value: "bad-model", Disposition: metadataDispositionRejected, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g, CausationEventID: &requested.ID})
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
	attachments, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	failed := newTestControlMetadata(t)
	if err := failed.EnsureHostedThread("nht_scrub", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := ImportAndScrubLegacyAttachments(failed, attachments); err == nil {
		t.Fatal("conflicting attachment migration succeeded")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(original) {
		t.Fatalf("failed migration scrubbed source: %q, %v", data, err)
	}

	success := newTestControlMetadata(t)
	if err := ImportAndScrubLegacyAttachments(success, attachments); err != nil {
		t.Fatal(err)
	}
	if got := success.Posture("nht_scrub"); got.Model != "legacy-model" || got.ServiceTier != "priority" {
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
	attachments.Remember(attachmentRecord{Provider: "codex", NativeThreadID: "native-scrub", HostedThreadID: "nht_scrub", Cwd: "/attempted-return", Model: "attempted-return", TurnServiceTier: "default", Bin: "/codex", Args: []string{"--resume", "native-scrub"}, AttachedAt: "2026-08-10T00:01:00Z"})
	reopened, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("codex", "native-scrub")
	if !ok || got.Cwd != "" || got.Model != "" || got.TurnServiceTier != "" || got.Bin != "/codex" || !reflect.DeepEqual(got.Args, []string{"--resume", "native-scrub"}) {
		t.Fatalf("transport refresh restored retired metadata or lost transport: %+v", got)
	}
}

func TestLegacyAttachmentImportResumesWithoutOverwritingStrongerEvidence(t *testing.T) {
	store := newTestControlMetadata(t)
	Records := []attachmentRecord{{HostedThreadID: "nht_attachment", Provider: "codex", Cwd: "/work", Model: "attachment-model", TurnServiceTier: "priority", InvokerHostedThreadID: "nht_parent", InvokerNativeThreadID: "parent-native", InvokerProvider: "claude"}, {HostedThreadID: "nht_empty_native", Provider: "claude", Model: "haiku", TurnServiceTier: "priority"}}
	if err := store.importLegacyAttachmentsUntil(Records, 1); err == nil {
		t.Fatal("bounded attachment import unexpectedly completed")
	}
	var markers int
	_ = store.db.QueryRow(`SELECT count(*) FROM control_migrations WHERE name='legacy-attachments-v1'`).Scan(&markers)
	if markers != 0 {
		t.Fatalf("interrupted attachment import wrote completion marker %d", markers)
	}
	if err := store.importLegacyAttachments(Records); err != nil {
		t.Fatal(err)
	}
	if err := store.importLegacyAttachments(Records); err != nil {
		t.Fatal(err)
	}
	view, err := store.View("nht_attachment")
	if err != nil {
		t.Fatal(err)
	}
	if view.InvokerHostedThreadID != "nht_parent" || view.InvokerNativeThreadID != "parent-native" || view.InvokerProvider != "claude" {
		t.Fatalf("attachment lineage = %+v", view)
	}
	if got := store.Posture("nht_attachment"); got.Model != "attachment-model" || got.ServiceTier != "priority" {
		t.Fatalf("attachment baseline posture = %+v", got)
	}
	if empty, err := store.View("nht_empty_native"); err != nil || empty.NativeThreadID != "" || empty.Provider != "claude" {
		t.Fatalf("empty native attachment identity = %+v err=%v", empty, err)
	}
	conflict := newTestControlMetadata(t)
	if err := conflict.EnsureHostedThread("nht_attachment", "codex", presentationShared, "nht_parent", "parent-native", "claude"); err != nil {
		t.Fatal(err)
	}
	g, err := conflict.BeginTransportBinding("nht_attachment")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conflict.AppendEvent(metadataEvent{HostedThreadID: "nht_attachment", Field: postureFieldServiceTier, Value: "default", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err != nil {
		t.Fatal(err)
	}
	if err := conflict.importLegacyAttachments(Records[:1]); err != nil {
		t.Fatal(err)
	}
	if got := conflict.Posture("nht_attachment").ServiceTier; got != "default" {
		t.Fatalf("historical attachment overwrote live provider tier %q", got)
	}
}

func TestLegacyAttachmentImportTransfersActiveNativeOwnership(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_old_owner", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimNativeID("nht_old_owner", "codex", "native-shared", false); err != nil {
		t.Fatal(err)
	}
	Records := []attachmentRecord{{
		HostedThreadID: "nht_attachment_owner",
		Provider:       "codex",
		NativeThreadID: "native-shared",
		AttachedAt:     "2026-08-12T13:37:00Z",
	}}
	if err := store.importLegacyAttachments(Records); err != nil {
		t.Fatal(err)
	}
	if err := store.importLegacyAttachments(Records); err != nil {
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
	view, err := store.View("nht_attachment_owner")
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
	if err := store.EnsureHostedThread("nht_native_history", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimNativeID("nht_native_history", "claude", "cs_provisional", true); err != nil {
		t.Fatal(err)
	}
	before, _ := store.View("nht_native_history")
	resolveTestProvisional(t, store, "nht_native_history", "claude", "cs_provisional", "native-real")
	after, _ := store.View("nht_native_history")
	if after.Revision != before.Revision+1 || after.NativeThreadID != "native-real" || len(after.NativeIDHistory) != 2 || after.NativeIDHistory[0].RetiredAt == "" || after.NativeIDHistory[1].NativeThreadID != "native-real" {
		t.Fatalf("native succession = %+v", after)
	}
	changes := store.ChangesAfter("nht_native_history", before.Revision)
	if len(changes) != 1 || !containsAll(strings.Join(changes[0].Fields, ","), "nativeThreadId") {
		t.Fatalf("succession changes = %+v", changes)
	}
	if err := store.ClaimNativeID("nht_native_history", "claude", "native-real", false); err != nil {
		t.Fatal(err)
	}
	repeat, _ := store.View("nht_native_history")
	if repeat.Revision != after.Revision {
		t.Fatalf("identical native claim advanced revision %d→%d", after.Revision, repeat.Revision)
	}
}

func TestMetadataEventVolumeWarningTracksCommittedEventsOnly(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_volume", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	store.eventVolume = map[string]int{"nht_volume\x00" + postureFieldModel: metadataEventWarningThreshold - 1}
	store.warnedVolume = map[string]bool{}
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_volume", Field: postureFieldModel, Value: "x", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatal(err)
	}
	if !store.warnedVolume["nht_volume\x00"+postureFieldModel] {
		t.Fatal("threshold warning was not recorded")
	}
}

func TestMetadataEventVolumeSurvivesRestartAndIgnoresDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openTestMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureHostedThread("nht_volume_restart", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < metadataEventWarningThreshold-2; i++ {
		if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_volume_restart", Field: postureFieldModel, Value: fmt.Sprintf("model-%03d", i), Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive}); err != nil {
			t.Fatal(err)
		}
	}
	provider := metadataEvent{HostedThreadID: "nht_volume_restart", Field: postureFieldModel, Value: "provider-model", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, SourceEventKey: "volume-restart-provider"}
	if event, err := store.AppendEvent(provider); err != nil || !event.Committed {
		t.Fatalf("provider seed = %+v, %v", event, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openTestMetadataStore(path)
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
	duplicate, err := store.AppendEvent(provider)
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
	if _, err := store.AppendEvent(metadataEvent{HostedThreadID: "nht_volume_restart", Field: postureFieldModel, Value: "operator-after-restart", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatal(err)
	}
	if got := store.eventVolume[key]; got != metadataEventWarningThreshold || !store.warnedVolume[key] {
		t.Fatalf("next committed event did not trigger restart warning: count=%d warned=%v", got, store.warnedVolume[key])
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
	if err := store.EnsureHostedThread(id, "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	observe := func(generation int64, value, key string, cause *int64) metadataEvent {
		t.Helper()
		event, err := store.AppendEvent(metadataEvent{HostedThreadID: id, Field: postureFieldModel, Value: value, Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation, CausationEventID: cause, SourceEventKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	first := observe(0, "gpt", "gen0", nil)
	if !first.Committed {
		t.Fatal("first observation was not recorded")
	}
	before, err := store.View(id)
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
	after, err := store.View(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("re-observation bumped revision %d -> %d", before.Revision, after.Revision)
	}
	if changed := observe(1, "gpt-2", "gen1-changed", nil); !changed.Committed {
		t.Fatal("a changed value was suppressed as a repeat")
	}
	requested, err := store.AppendEvent(metadataEvent{HostedThreadID: id, Field: postureFieldModel, Value: "gpt-2", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive})
	if err != nil || !requested.Committed {
		t.Fatalf("operator request = %+v, %v", requested, err)
	}
	cause := requested.ID
	settled := observe(1, "gpt-2", "gen1-causal", &cause)
	if !settled.Committed {
		t.Fatal("a causal confirmation of the value already in effect was suppressed, leaving the request pending")
	}
	view, err := store.View(id)
	if err != nil {
		t.Fatal(err)
	}
	field := view.Fields[postureFieldModel]
	if field.DesiredDisposition != metadataDispositionConfirmed || field.DesiredSettlementEventID == nil || *field.DesiredSettlementEventID != settled.ID {
		t.Fatalf("request not settled by the causal confirmation: %+v", field)
	}
}
