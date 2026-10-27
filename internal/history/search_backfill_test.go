package history

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestConversationSearchBackfillResumesFromCommittedCheckpoint(t *testing.T) {
	ctx := context.Background()
	interrupted, err := Open(filepath.Join(t.TempDir(), "interrupted.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer interrupted.Close()
	addSearchBackfillFixture(t, ctx, interrupted)
	if _, err := interrupted.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{
		Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, CorpusReset: true,
	}); err != nil {
		t.Fatal(err)
	}
	first, err := interrupted.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, BatchSize: 2})
	if err != nil || first.Complete || first.Examined != 2 || first.Processed != 1 {
		t.Fatalf("first batch = %#v, %v", first, err)
	}
	checkpointed := searchStatusByMode(mustConversationSearchStatuses(t, interrupted))[ConversationSearchLiteral]
	if checkpointed.CheckpointProjectionID == nil || checkpointed.CheckpointItemID == nil || checkpointed.CheckpointDocumentID == nil || checkpointed.IndexedCount != 1 {
		t.Fatalf("first checkpoint = %#v", checkpointed)
	}
	assertCount(t, interrupted.db, `SELECT COUNT(*) FROM conversation_search_documents`, 1)
	assertCount(t, interrupted.db, `SELECT COUNT(*) FROM conversation_search_literal_fts`, 1)
	assertCount(t, interrupted.db, `SELECT COUNT(*) FROM conversation_search_terms_fts`, 1)
	for {
		progress, err := interrupted.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, BatchSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		if progress.Complete {
			break
		}
	}
	interruptedRows := searchBackfillRowSets(t, interrupted)

	clean, err := Open(filepath.Join(t.TempDir(), "clean.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()
	addSearchBackfillFixture(t, ctx, clean)
	if _, err := clean.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{
		Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, CorpusReset: true,
	}); err != nil {
		t.Fatal(err)
	}
	for {
		progress, err := clean.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, BatchSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		if progress.Complete {
			break
		}
	}
	if got := searchBackfillRowSets(t, clean); !equalSearchBackfillRowSets(interruptedRows, got) {
		t.Fatalf("resumed row sets = %#v, clean row sets = %#v", interruptedRows, got)
	}
	assertSearchStates(t, mustConversationSearchStatuses(t, interrupted), map[ConversationSearchMode]ConversationSearchBuildState{
		ConversationSearchLiteral: ConversationSearchReady,
		ConversationSearchTerms:   ConversationSearchReady,
	})
}

func TestConversationSearchBackfillResumesAcrossLargeProjectionsWithoutSkippingRawIdentities(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	expected := addLargeSearchBackfillProjections(t, ctx, store, 3, 24)
	modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	var previousProjection int64
	var previousItem string
	for {
		progress, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 7})
		if err != nil {
			t.Fatal(err)
		}
		if progress.Complete {
			break
		}
		status := searchStatusByMode(mustConversationSearchStatuses(t, store))[ConversationSearchLiteral]
		if progress.Examined > 7 || status.CheckpointProjectionID == nil || status.CheckpointItemID == nil || (*status.CheckpointProjectionID < previousProjection || (*status.CheckpointProjectionID == previousProjection && *status.CheckpointItemID <= previousItem)) {
			t.Fatalf("raw checkpoint did not advance by bounded source identity: progress=%#v status=%#v", progress, status)
		}
		previousProjection, previousItem = *status.CheckpointProjectionID, *status.CheckpointItemID
	}
	rows := searchBackfillRowSets(t, store)
	if len(rows.Membership) != len(expected) || len(rows.Literal) != len(expected) || len(rows.Terms) != len(expected) {
		t.Fatalf("resumed large projections lost or duplicated identities: expected=%d rows=%#v", len(expected), rows)
	}
	for _, identity := range expected {
		if _, ok := rows.Membership[identity]; !ok {
			t.Fatalf("missing resumed raw identity %q", identity)
		}
	}
}

func TestConversationSearchBackfillTokenizerResetPreservesReadyOtherMode(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addSearchBackfillFixture(t, ctx, store)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 2)
	before := searchBackfillRowSets(t, store)
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: []ConversationSearchMode{ConversationSearchLiteral}}); err != nil {
		t.Fatal(err)
	}
	if got := searchStatusByMode(mustConversationSearchStatuses(t, store))[ConversationSearchTerms]; got.State != ConversationSearchReady {
		t.Fatalf("terms status after literal reset = %#v", got)
	}
	for {
		progress, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral}, BatchSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if progress.Complete {
			break
		}
	}
	after := searchBackfillRowSets(t, store)
	if !reflect.DeepEqual(before.Membership, after.Membership) || !reflect.DeepEqual(before.Terms, after.Terms) || !reflect.DeepEqual(before.Literal, after.Literal) {
		t.Fatalf("tokenizer-only rebuild changed shared rows: before=%#v after=%#v", before, after)
	}
}

func TestFailConversationSearchBackfillFailsDivergentBuildingCheckpoints(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	checkpoints := map[ConversationSearchMode]struct {
		projection int64
		item       string
	}{
		ConversationSearchLiteral: {projection: 11, item: "literal-checkpoint"},
		ConversationSearchTerms:   {projection: 22, item: "terms-checkpoint"},
	}
	for mode, checkpoint := range checkpoints {
		if _, err := store.SetConversationSearchState(ctx, mode, ConversationSearchStateUpdate{State: ConversationSearchBuilding, CheckpointProjectionID: &checkpoint.projection, CheckpointItemID: &checkpoint.item}); err != nil {
			t.Fatal(err)
		}
	}
	before := searchStatusByMode(mustConversationSearchStatuses(t, store))
	if _, err := store.FailConversationSearchBackfill(ctx, modes, errors.New("divergent checkpoint failure")); err != nil {
		t.Fatal(err)
	}
	after := searchStatusByMode(mustConversationSearchStatuses(t, store))
	for mode, checkpoint := range checkpoints {
		status := after[mode]
		if status.State != ConversationSearchFailed || status.ErrorSummary == nil || *status.ErrorSummary != "divergent checkpoint failure" || status.CheckpointProjectionID == nil || *status.CheckpointProjectionID != checkpoint.projection || status.CheckpointItemID == nil || *status.CheckpointItemID != checkpoint.item || status.Revision != before[mode].Revision || status.DocumentCount != before[mode].DocumentCount || status.IndexedCount != before[mode].IndexedCount {
			t.Fatalf("failure state for %s = %#v", mode, status)
		}
	}
}

func TestBoundedConversationSearchErrorPreservesUTF8(t *testing.T) {
	got := boundedConversationSearchError(strings.Repeat("界", maxSearchErrorSummaryBytes))
	if len(got) > maxSearchErrorSummaryBytes || !utf8.ValidString(got) {
		t.Fatalf("bounded error bytes=%d valid=%t", len(got), utf8.ValidString(got))
	}
}

func TestConversationSearchBackfillCorpusResetAndCorruptionFailure(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := addSearchBackfillFixture(t, ctx, store)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 10)
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{
		Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, CorpusReset: true,
	}); err != nil {
		t.Fatal(err)
	}
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents`, 0)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_literal_fts`, 0)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_terms_fts`, 0)
	assertSearchStates(t, mustConversationSearchStatuses(t, store), map[ConversationSearchMode]ConversationSearchBuildState{
		ConversationSearchLiteral: ConversationSearchBuilding,
		ConversationSearchTerms:   ConversationSearchBuilding,
	})
	progress, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, BatchSize: 10})
	if err != nil || progress.Processed == 0 {
		t.Fatalf("build before deliberate corruption = %#v, %v", progress, err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO conversation_search_documents(projection_id, item_id) VALUES (?, ?)`, fixture.GuardianProjectionID, fixture.GuardianItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, BatchSize: 10}); err == nil {
		t.Fatal("corrupt Guardian membership unexpectedly reconciled")
	}
	for _, status := range mustConversationSearchStatuses(t, store) {
		if status.State != ConversationSearchFailed || status.ErrorSummary == nil || len(*status.ErrorSummary) > maxSearchErrorSummaryBytes {
			t.Fatalf("corruption status = %#v", status)
		}
	}
}

func TestConversationSearchBackfillCanceledContextKeepsLastCommittedCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addSearchBackfillFixture(t, ctx, store)
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	before := searchStatusByMode(mustConversationSearchStatuses(t, store))[ConversationSearchLiteral]
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.RunConversationSearchBackfillBatch(canceled, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, BatchSize: 1}); err == nil {
		t.Fatal("canceled batch unexpectedly succeeded")
	}
	after := searchStatusByMode(mustConversationSearchStatuses(t, store))[ConversationSearchLiteral]
	if !equalInt64Pointers(before.CheckpointProjectionID, after.CheckpointProjectionID) || !equalStringPointers(before.CheckpointItemID, after.CheckpointItemID) || before.IndexedCount != after.IndexedCount {
		t.Fatalf("canceled batch changed committed checkpoint: before=%#v after=%#v", before, after)
	}
}

func TestConversationSearchBackfillReconciliationRejectsMissingFTSRow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addSearchBackfillFixture(t, ctx, store)
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, BatchSize: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM conversation_search_literal_fts WHERE rowid = (SELECT MIN(rowid) FROM conversation_search_literal_fts)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, BatchSize: 10}); err == nil {
		t.Fatal("missing FTS row unexpectedly reconciled")
	}
	for _, status := range mustConversationSearchStatuses(t, store) {
		if status.State != ConversationSearchFailed || status.ErrorSummary == nil {
			t.Fatalf("missing FTS row status = %#v", status)
		}
	}
}

func TestConversationSearchBackfillBoundsExcludedSourceRuns(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addExcludedSearchBackfillTail(t, ctx, store, 8)
	modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	var previous string
	for _, wantExamined := range []int{3, 3, 2} {
		progress, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 3})
		if err != nil || progress.Examined != wantExamined || progress.Processed != 0 || progress.Complete {
			t.Fatalf("excluded batch = %#v, %v", progress, err)
		}
		status := searchStatusByMode(mustConversationSearchStatuses(t, store))[ConversationSearchLiteral]
		if status.CheckpointItemID == nil || *status.CheckpointItemID <= previous {
			t.Fatalf("excluded checkpoint did not advance: previous=%q status=%#v", previous, status)
		}
		previous = *status.CheckpointItemID
	}
	progress, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 3})
	if err != nil || !progress.Complete || progress.Examined != 0 {
		t.Fatalf("excluded tail completion = %#v, %v", progress, err)
	}
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents`, 0)
}

func TestConversationSearchBackfillReconciliationAllowsMaintainedAppendAndReadies(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := addSearchBackfillFixture(t, ctx, store)
	modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 100}); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	beforeConversationSearchReadinessForTest = func() {
		close(reached)
		<-release
	}
	defer func() { beforeConversationSearchReadinessForTest = nil }()
	type batchResult struct {
		progress ConversationSearchBackfillProgress
		err      error
	}
	result := make(chan batchResult, 1)
	go func() {
		progress, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 100})
		result <- batchResult{progress: progress, err: err}
	}()
	<-reached
	ingested := make(chan error, 1)
	go func() { ingested <- appendEligibleSearchBackfillItem(ctx, store, fixture) }()
	if err := <-ingested; err != nil {
		t.Fatalf("ingestion did not commit during paused reconciliation: %v", err)
	}
	close(release)
	completed := <-result
	if completed.err != nil || !completed.progress.Complete {
		t.Fatalf("maintained reconciliation = %#v, %v", completed.progress, completed.err)
	}
	beforeConversationSearchReadinessForTest = nil
	assertSearchStates(t, mustConversationSearchStatuses(t, store), map[ConversationSearchMode]ConversationSearchBuildState{
		ConversationSearchLiteral: ConversationSearchReady,
		ConversationSearchTerms:   ConversationSearchReady,
	})
	var documentID int64
	if err := store.db.QueryRowContext(ctx, `SELECT document_id FROM conversation_search_documents WHERE item_id = 'f-concurrent'`).Scan(&documentID); err != nil {
		t.Fatalf("maintained membership: %v", err)
	}
	var membershipCount int64
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_search_documents`).Scan(&membershipCount); err != nil {
		t.Fatalf("fresh membership count: %v", err)
	}
	for _, status := range mustConversationSearchStatuses(t, store) {
		if status.DocumentCount != membershipCount || status.IndexedCount != membershipCount {
			t.Fatalf("fresh ready counts for %s = %#v; membership=%d", status.Mode, status, membershipCount)
		}
	}
	for _, mode := range modes {
		assertCount(t, store.db, `SELECT COUNT(*) FROM `+searchFTSTable(mode)+` WHERE rowid = `+fmt.Sprint(documentID), 1)
	}
}

func TestConversationSearchBackfillReconciliationRejectsBuildResetAfterProof(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addSearchBackfillFixture(t, ctx, store)
	modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 100}); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	beforeConversationSearchReadinessForTest = func() { close(reached); <-release }
	defer func() { beforeConversationSearchReadinessForTest = nil }()
	result := make(chan error, 1)
	go func() {
		_, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 100})
		result <- err
	}()
	<-reached
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatalf("replace build after proof: %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrConversationSearchReconciliationStale) {
		t.Fatalf("replaced reconciliation error = %v", err)
	}
	assertSearchStates(t, mustConversationSearchStatuses(t, store), map[ConversationSearchMode]ConversationSearchBuildState{
		ConversationSearchLiteral: ConversationSearchBuilding,
		ConversationSearchTerms:   ConversationSearchBuilding,
	})
}

func TestConversationSearchBackfillReconciliationRejectsDocumentCheckpointReplacement(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addSearchBackfillFixture(t, ctx, store)
	modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 100}); err != nil {
		t.Fatal(err)
	}
	before := searchStatusByMode(mustConversationSearchStatuses(t, store))[ConversationSearchLiteral]
	if before.CheckpointProjectionID == nil || before.CheckpointItemID == nil {
		t.Fatalf("expected complete checkpoint before replacement: %#v", before)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	beforeConversationSearchReadinessForTest = func() { close(reached); <-release }
	defer func() { beforeConversationSearchReadinessForTest = nil }()
	result := make(chan error, 1)
	go func() {
		_, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 100})
		result <- err
	}()
	<-reached
	replacementDocument := int64(1)
	if before.CheckpointDocumentID != nil {
		replacementDocument = *before.CheckpointDocumentID + 1
	}
	if _, err := store.SetConversationSearchState(ctx, ConversationSearchLiteral, ConversationSearchStateUpdate{
		State: ConversationSearchBuilding, CheckpointProjectionID: before.CheckpointProjectionID,
		CheckpointItemID: before.CheckpointItemID, CheckpointDocumentID: &replacementDocument,
		DocumentCount: before.DocumentCount, IndexedCount: before.IndexedCount,
	}); err != nil {
		t.Fatalf("replace only document checkpoint: %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrConversationSearchReconciliationStale) {
		t.Fatalf("document-checkpoint replacement error = %v", err)
	}
	assertSearchStates(t, mustConversationSearchStatuses(t, store), map[ConversationSearchMode]ConversationSearchBuildState{
		ConversationSearchLiteral: ConversationSearchBuilding,
		ConversationSearchTerms:   ConversationSearchBuilding,
	})
}

func TestConversationSearchBackfillReconciliationDoesNotFailSelectedModeAfterSiblingReplacement(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addSearchBackfillFixture(t, ctx, store)
	allModes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	buildSearchModes(t, ctx, store, allModes, true, 100)
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: []ConversationSearchMode{ConversationSearchLiteral}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral}, BatchSize: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM conversation_search_literal_fts WHERE rowid = (SELECT MIN(rowid) FROM conversation_search_literal_fts)`); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	beforeConversationSearchReadinessForTest = func() { close(reached); <-release }
	defer func() { beforeConversationSearchReadinessForTest = nil }()
	result := make(chan error, 1)
	go func() {
		_, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: []ConversationSearchMode{ConversationSearchLiteral}, BatchSize: 100})
		result <- err
	}()
	<-reached
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: []ConversationSearchMode{ConversationSearchTerms}}); err != nil {
		t.Fatalf("replace ready sibling after failed proof: %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrConversationSearchReconciliationStale) {
		t.Fatalf("sibling replacement error = %v", err)
	}
	assertSearchStates(t, mustConversationSearchStatuses(t, store), map[ConversationSearchMode]ConversationSearchBuildState{
		ConversationSearchLiteral: ConversationSearchBuilding,
		ConversationSearchTerms:   ConversationSearchBuilding,
	})
}

type searchBackfillFixture struct {
	GuardianProjectionID int64
	GuardianItemID       string
	MainThreadID         string
	MainSourceID         int64
	MainGeneration       int64
	MainProjectionKey    string
	MainMetadata         SourceProjection
}

func addSearchBackfillFixture(t *testing.T, ctx context.Context, store *Store) searchBackfillFixture {
	t.Helper()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	main := testSource(root, "main.jsonl", "search-backfill-main", ThreadMain, nil)
	mainApplied, err := store.ApplySource(ctx, main)
	if err != nil {
		t.Fatal(err)
	}
	mainSource, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), main.Source.Path)
	if err != nil || !ok {
		t.Fatalf("main source = %#v, %t, %v", mainSource, ok, err)
	}
	inactiveProjection := searchEligibilityProjection("search-backfill-inactive-projection", []TranscriptItem{
		searchEligibilityItem("inactive-user", 1, ItemUserMessage, RoleUser, "superseded exclusion"),
	}, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: mainApplied.ThreadID, SourceID: mainSource.ID, SourceGeneration: mainSource.Generation, Provider: ProviderCodex, ParserVersion: "backfill-old", CompletionRuleVersion: "backfill", Result: inactiveProjection, IndexedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	injected := searchEligibilityItem("e-injected", 5, ItemInteraction, RoleUser, "injected exclusion")
	injected.Synthetic = true
	mainProjection := searchEligibilityProjection("search-backfill-main-projection", []TranscriptItem{
		searchEligibilityItem("a-user", 1, ItemUserMessage, RoleUser, "alpha user"),
		searchEligibilityItem("b-agent", 2, ItemAgentMessage, RoleAssistant, "beta agent"),
		searchEligibilityItem("c-interaction", 3, ItemInteraction, RoleUser, "gamma answer"),
		searchEligibilityItem("d-tool", 4, ItemToolCall, RoleAssistant, "tool exclusion"),
		injected,
	}, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: mainApplied.ThreadID, SourceID: mainSource.ID, SourceGeneration: mainSource.Generation, Provider: ProviderCodex, ParserVersion: "backfill", CompletionRuleVersion: "backfill", Result: mainProjection, IndexedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	guardian := testSource(root, "guardian.jsonl", "search-backfill-guardian", ThreadMain, nil)
	guardian.Result.Projection.Kind = ThreadGuardian
	guardianApplied, err := store.ApplySource(ctx, guardian)
	if err != nil {
		t.Fatal(err)
	}
	guardianSource, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), guardian.Source.Path)
	if err != nil || !ok {
		t.Fatalf("guardian source = %#v, %t, %v", guardianSource, ok, err)
	}
	guardianProjection := searchEligibilityProjection("search-backfill-guardian-projection", []TranscriptItem{
		searchEligibilityItem("guardian-user", 1, ItemUserMessage, RoleUser, "guardian exclusion"),
	}, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: guardianApplied.ThreadID, SourceID: guardianSource.ID, SourceGeneration: guardianSource.Generation, Provider: ProviderCodex, ParserVersion: "backfill", CompletionRuleVersion: "backfill", Result: guardianProjection, IndexedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var guardianProjectionID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM conversation_projections WHERE projection_key = ?`, guardianProjection.ProjectionKey).Scan(&guardianProjectionID); err != nil {
		t.Fatal(err)
	}
	return searchBackfillFixture{
		GuardianProjectionID: guardianProjectionID, GuardianItemID: "guardian-user",
		MainThreadID: mainApplied.ThreadID, MainSourceID: mainSource.ID,
		MainGeneration: mainSource.Generation, MainProjectionKey: mainProjection.ProjectionKey, MainMetadata: main,
	}
}

func addExcludedSearchBackfillTail(t *testing.T, ctx context.Context, store *Store, count int) {
	t.Helper()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(root, "excluded.jsonl", "search-backfill-excluded", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	source, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("excluded source = %#v, %t, %v", source, ok, err)
	}
	items := make([]TranscriptItem, 0, count)
	for index := 0; index < count; index++ {
		items = append(items, searchEligibilityItem(fmt.Sprintf("excluded-%03d", index), int64(index+1), ItemToolCall, RoleAssistant, "excluded"))
	}
	projection := searchEligibilityProjection("search-backfill-excluded-projection", items, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: source.ID, SourceGeneration: source.Generation, Provider: ProviderCodex, ParserVersion: "backfill", CompletionRuleVersion: "backfill", Result: projection, IndexedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

// addLargeSearchBackfillProjections produces several independently ordered
// active projections so resume tests cross the projection boundary repeatedly.
func addLargeSearchBackfillProjections(t *testing.T, ctx context.Context, store *Store, projections, itemsPerProjection int) []string {
	t.Helper()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "large-sessions")}
	expected := make([]string, 0, projections*itemsPerProjection)
	for projectionIndex := 0; projectionIndex < projections; projectionIndex++ {
		name := fmt.Sprintf("large-%02d.jsonl", projectionIndex)
		metadata := testSource(root, name, fmt.Sprintf("large-native-%02d", projectionIndex), ThreadMain, nil)
		applied, err := store.ApplySource(ctx, metadata)
		if err != nil {
			t.Fatal(err)
		}
		source, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
		if err != nil || !ok {
			t.Fatalf("large source = %#v, %t, %v", source, ok, err)
		}
		items := make([]TranscriptItem, 0, itemsPerProjection)
		for itemIndex := 0; itemIndex < itemsPerProjection; itemIndex++ {
			itemID := fmt.Sprintf("large-%02d-%03d", projectionIndex, itemIndex)
			items = append(items, searchEligibilityItem(itemID, int64(itemIndex+1), ItemUserMessage, RoleUser, "large projection search message"))
		}
		projection := searchEligibilityProjection(fmt.Sprintf("large-projection-%02d", projectionIndex), items, nil)
		if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: source.ID, SourceGeneration: source.Generation, Provider: ProviderCodex, ParserVersion: "large-backfill", CompletionRuleVersion: "large-backfill", Result: projection, IndexedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		var projectionID int64
		if err := store.db.QueryRowContext(ctx, `SELECT id FROM conversation_projections WHERE projection_key = ?`, projection.ProjectionKey).Scan(&projectionID); err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			expected = append(expected, fmt.Sprintf("%d/%s", projectionID, item.ID))
		}
	}
	return expected
}

func appendEligibleSearchBackfillItem(ctx context.Context, store *Store, fixture searchBackfillFixture) error {
	item := searchEligibilityItem("f-concurrent", 6, ItemUserMessage, RoleUser, "concurrent corpus mutation")
	result := TranscriptProjectionResult{
		ProjectionKey: fixture.MainProjectionKey, CompleteOffset: 2, CompleteLines: 2,
		ParserState: []byte(`{}`), Items: []TranscriptItem{item},
		Turns: []TranscriptTurn{{Key: "turn", Ordinal: 1, StartSeq: 1, EndSeq: 6, Status: TurnCompleted, CompletionEvidence: CompletionTaskComplete}},
	}
	_, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptAppend, ThreadID: fixture.MainThreadID, SourceID: fixture.MainSourceID, SourceGeneration: fixture.MainGeneration, Provider: ProviderCodex, ParserVersion: "backfill", CompletionRuleVersion: "backfill", ExpectedProjectionKey: fixture.MainProjectionKey, Result: result, IndexedAt: time.Now().UTC()})
	return err
}

func TestConversationSearchBackfillExcludesGuardianSyntheticAndSupersededItems(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addSearchBackfillFixture(t, ctx, store)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 1)
	for _, itemID := range []string{"guardian-user", "inactive-user", "d-tool", "e-injected"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM conversation_search_documents csd
JOIN conversation_items ci ON ci.projection_id = csd.projection_id AND ci.item_id = csd.item_id
WHERE ci.item_id = ?`, itemID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("excluded item %q has %d search membership rows", itemID, count)
		}
	}
}

func buildSearchModes(t *testing.T, ctx context.Context, store *Store, modes []ConversationSearchMode, corpusReset bool, batchSize int) {
	t.Helper()
	if _, err := store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: modes, CorpusReset: corpusReset}); err != nil {
		t.Fatal(err)
	}
	for {
		progress, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: batchSize})
		if err != nil {
			t.Fatal(err)
		}
		if progress.Complete {
			return
		}
	}
}

type searchBackfillRows struct {
	Membership map[string]int64
	Literal    map[int64]struct{}
	Terms      map[int64]struct{}
}

func searchBackfillRowSets(t *testing.T, store *Store) searchBackfillRows {
	t.Helper()
	result := searchBackfillRows{Membership: map[string]int64{}, Literal: map[int64]struct{}{}, Terms: map[int64]struct{}{}}
	rows, err := store.db.Query(`SELECT document_id, projection_id, item_id FROM conversation_search_documents`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var documentID, projectionID int64
		var itemID string
		if err := rows.Scan(&documentID, &projectionID, &itemID); err != nil {
			t.Fatal(err)
		}
		result.Membership[fmt.Sprintf("%d/%s", projectionID, itemID)] = documentID
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for mode, target := range map[ConversationSearchMode]map[int64]struct{}{ConversationSearchLiteral: result.Literal, ConversationSearchTerms: result.Terms} {
		rows, err := store.db.Query(`SELECT rowid FROM ` + searchFTSTable(mode))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var documentID int64
			if err := rows.Scan(&documentID); err != nil {
				t.Fatal(err)
			}
			target[documentID] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func equalSearchBackfillRowSets(left, right searchBackfillRows) bool {
	return reflect.DeepEqual(left, right)
}
