package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestConversationSearchMaintenanceAppendReadyAtomicallyUpdatesBothModes(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := addSearchBackfillFixture(t, ctx, store)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 50)
	before := searchStatusByMode(mustConversationSearchStatuses(t, store))
	if err := appendEligibleSearchBackfillItem(ctx, store, fixture); err != nil {
		t.Fatal(err)
	}
	var documentID int64
	if err := store.db.QueryRowContext(ctx, `SELECT document_id FROM conversation_search_documents WHERE item_id = 'f-concurrent'`).Scan(&documentID); err != nil {
		t.Fatalf("appended membership: %v", err)
	}
	for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
		var rows int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+searchFTSTable(mode)+` WHERE rowid = ?`, documentID).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("%s appended FTS rows = %d, %v", mode, rows, err)
		}
	}
	after := searchStatusByMode(mustConversationSearchStatuses(t, store))
	for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
		if got := after[mode]; got.State != ConversationSearchReady || got.DocumentCount != before[mode].DocumentCount+1 || got.IndexedCount != before[mode].IndexedCount+1 || got.Revision != before[mode].Revision+1 {
			t.Fatalf("%s append state = %#v, before %#v", mode, got, before[mode])
		}
	}
}

func TestConversationSearchMaintenanceBuildAcceptsPreAndPostCheckpointAppends(t *testing.T) {
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
	if err := appendEligibleSearchBackfillItem(ctx, store, fixture); err != nil {
		t.Fatal(err)
	}
	if progress, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 1}); err != nil || progress.Examined != 1 {
		t.Fatalf("first bounded build batch = %#v, %v", progress, err)
	}
	if err := appendSearchBackfillItem(ctx, store, fixture, "g-after-checkpoint", 7); err != nil {
		t.Fatal(err)
	}
	for {
		progress, err := store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if progress.Complete {
			break
		}
	}
	for _, itemID := range []string{"f-concurrent", "g-after-checkpoint"} {
		var documentID int64
		if err := store.db.QueryRowContext(ctx, `SELECT document_id FROM conversation_search_documents WHERE item_id = ?`, itemID).Scan(&documentID); err != nil {
			t.Fatalf("%s membership: %v", itemID, err)
		}
		for _, mode := range modes {
			assertCount(t, store.db, `SELECT COUNT(*) FROM `+searchFTSTable(mode)+` WHERE rowid = `+fmt.Sprint(documentID), 1)
		}
	}
}

func TestConversationSearchMaintenanceRebuildGuardianAndCollection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := addSearchBackfillFixture(t, ctx, store)
	modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	buildSearchModes(t, ctx, store, modes, true, 50)

	replacement := searchEligibilityProjection("search-maintenance-replacement", []TranscriptItem{
		searchEligibilityItem("replacement-user", 1, ItemUserMessage, RoleUser, "replacement corpus"),
	}, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: fixture.MainThreadID, SourceID: fixture.MainSourceID, SourceGeneration: fixture.MainGeneration, Provider: ProviderCodex, ParserVersion: "backfill", CompletionRuleVersion: "backfill", Result: replacement, IndexedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents WHERE item_id IN ('a-user', 'b-agent', 'c-interaction')`, 0)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents WHERE item_id = 'replacement-user'`, 1)
	// The contentless tables must be cleared explicitly before their membership
	// rows cascade.
	for index := 2; index <= 3; index++ {
		projection := searchEligibilityProjection(fmt.Sprintf("search-maintenance-gc-%d", index), []TranscriptItem{
			searchEligibilityItem(fmt.Sprintf("gc-user-%d", index), 1, ItemUserMessage, RoleUser, "replacement corpus"),
		}, nil)
		if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: fixture.MainThreadID, SourceID: fixture.MainSourceID, SourceGeneration: fixture.MainGeneration, Provider: ProviderCodex, ParserVersion: "backfill", CompletionRuleVersion: "backfill", Result: projection, IndexedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents WHERE item_id IN ('replacement-user', 'gc-user-2')`, 0)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents WHERE item_id = 'gc-user-3'`, 1)
	for _, mode := range modes {
		assertCount(t, store.db, `SELECT COUNT(*) FROM `+searchFTSTable(mode)+` WHERE rowid NOT IN (SELECT document_id FROM conversation_search_documents)`, 0)
	}

	metadata := fixture.MainMetadata
	metadata.Result.Projection.Kind = ThreadGuardian
	if _, err := store.ApplySource(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents WHERE item_id = 'gc-user-3'`, 0)
	metadata.Result.Projection.Kind = ThreadMain
	if _, err := store.ApplySource(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents WHERE item_id = 'gc-user-3'`, 1)
	var restoredDocumentID int64
	if err := store.db.QueryRowContext(ctx, `SELECT document_id FROM conversation_search_documents WHERE item_id = 'gc-user-3'`).Scan(&restoredDocumentID); err != nil {
		t.Fatal(err)
	}
	for _, mode := range modes {
		assertCount(t, store.db, `SELECT COUNT(*) FROM `+searchFTSTable(mode)+` WHERE rowid = `+fmt.Sprint(restoredDocumentID), 1)
		assertCount(t, store.db, `SELECT COUNT(*) FROM `+searchFTSTable(mode)+` WHERE rowid NOT IN (SELECT document_id FROM conversation_search_documents)`, 0)
	}
}

func TestConversationSearchMaintenanceMetadataOwnershipChangeAdvancesRevision(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := addSearchBackfillFixture(t, ctx, store)
	modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	buildSearchModes(t, ctx, store, modes, true, 50)
	beforeRows := searchBackfillRowSets(t, store)
	before := searchStatusByMode(mustConversationSearchStatuses(t, store))
	metadata := fixture.MainMetadata
	metadata.Result.Projection.Kind = ThreadSubagent
	if _, err := store.ApplySource(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	afterRows := searchBackfillRowSets(t, store)
	if !equalSearchBackfillRowSets(beforeRows, afterRows) {
		t.Fatalf("eligible kind change rewrote derived rows: before=%#v after=%#v", beforeRows, afterRows)
	}
	after := searchStatusByMode(mustConversationSearchStatuses(t, store))
	for _, mode := range modes {
		if got := after[mode]; got.Revision != before[mode].Revision+1 || got.DocumentCount != before[mode].DocumentCount || got.IndexedCount != before[mode].IndexedCount {
			t.Fatalf("%s metadata ownership state = %#v, before %#v", mode, got, before[mode])
		}
	}
}

var errSearchCandidateArgumentCapture = errors.New("captured candidate query")

type searchCandidateArgumentCapture struct{ args []any }

func (capture *searchCandidateArgumentCapture) QueryContext(_ context.Context, _ string, args ...any) (*sql.Rows, error) {
	capture.args = append([]any(nil), args...)
	return nil, errSearchCandidateArgumentCapture
}

func TestConversationSearchAppendCandidateRangeHasFixedParameterCount(t *testing.T) {
	capture := &searchCandidateArgumentCapture{}
	err := VisitPersistedConversationSearchCandidatesForProjectionSequenceRange(context.Background(), capture, 71, 9, 1<<30, func(ConversationSearchCandidate, EligibleConversationSearchItem, bool) error {
		return nil
	})
	if !errors.Is(err, errSearchCandidateArgumentCapture) {
		t.Fatalf("range candidate query error = %v", err)
	}
	if len(capture.args) != 3 {
		t.Fatalf("range candidate bind count = %d, want 3", len(capture.args))
	}
}

func TestConversationSearchMaintenanceInvalidationClearsOnlyRequiredRows(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addSearchBackfillFixture(t, ctx, store)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 50)
	versions := defaultConversationSearchVersions()
	versions.Literal = "trigram-maintenance-v2"
	if _, err := store.EnsureConversationSearchVersions(ctx, versions); err != nil {
		t.Fatal(err)
	}
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents`, 3)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_literal_fts`, 0)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_terms_fts`, 3)
	versions.CorpusRule = "conversation-maintenance-v2"
	if _, err := store.EnsureConversationSearchVersions(ctx, versions); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"conversation_search_documents", "conversation_search_literal_fts", "conversation_search_terms_fts"} {
		assertCount(t, store.db, `SELECT COUNT(*) FROM `+table, 0)
	}
}

func TestConversationSearchStatePartialLossResetsBothModesAndRows(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addSearchBackfillFixture(t, ctx, store)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 50)
	if _, err := store.db.ExecContext(ctx, `DELETE FROM conversation_search_state WHERE mode = ?`, ConversationSearchLiteral); err != nil {
		t.Fatal(err)
	}
	versions := defaultConversationSearchVersions()
	versions.CorpusRule = "partial-state-corpus-v2"
	versions.Literal = "partial-state-trigram-v2"
	versions.Terms = "partial-state-terms-v2"
	statuses, err := store.EnsureConversationSearchVersions(ctx, versions)
	if err != nil {
		t.Fatal(err)
	}
	byMode := searchStatusByMode(statuses)
	for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
		status := byMode[mode]
		wantTokenizer, _ := versions.tokenizer(mode)
		if status.State != ConversationSearchNeedsBuild || status.CorpusRuleVersion != versions.CorpusRule || status.TokenizerVersion != wantTokenizer || status.DocumentCount != 0 || status.IndexedCount != 0 || status.CheckpointProjectionID != nil || status.CheckpointItemID != nil || status.CheckpointDocumentID != nil {
			t.Fatalf("partial state reset %s = %#v", mode, status)
		}
	}
	for _, table := range []string{"conversation_search_documents", "conversation_search_literal_fts", "conversation_search_terms_fts"} {
		assertCount(t, store.db, `SELECT COUNT(*) FROM `+table, 0)
	}
}

func TestConversationSearchMaintenanceAppendDuringReconciliationReadies(t *testing.T) {
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
	beforeConversationSearchReadinessForTest = func() { close(reached); <-release }
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
	if err := appendEligibleSearchBackfillItem(ctx, store, fixture); err != nil {
		t.Fatalf("append during reconciliation: %v", err)
	}
	close(release)
	completed := <-result
	if completed.err != nil || !completed.progress.Complete {
		t.Fatalf("maintained reconciliation = %#v, %v", completed.progress, completed.err)
	}
	beforeConversationSearchReadinessForTest = nil
	assertSearchStates(t, mustConversationSearchStatuses(t, store), map[ConversationSearchMode]ConversationSearchBuildState{ConversationSearchLiteral: ConversationSearchReady, ConversationSearchTerms: ConversationSearchReady})
	var documentID int64
	if err := store.db.QueryRowContext(ctx, `SELECT document_id FROM conversation_search_documents WHERE item_id = 'f-concurrent'`).Scan(&documentID); err != nil {
		t.Fatalf("maintained membership: %v", err)
	}
	for _, mode := range modes {
		assertCount(t, store.db, `SELECT COUNT(*) FROM `+searchFTSTable(mode)+` WHERE rowid = `+fmt.Sprint(documentID), 1)
	}
}

func appendSearchBackfillItem(ctx context.Context, store *Store, fixture searchBackfillFixture, itemID string, sequence int64) error {
	item := searchEligibilityItem(itemID, sequence, ItemUserMessage, RoleUser, itemID+" corpus mutation")
	result := TranscriptProjectionResult{ProjectionKey: fixture.MainProjectionKey, CompleteOffset: sequence, CompleteLines: int(sequence), ParserState: []byte(`{}`), Items: []TranscriptItem{item}, Turns: []TranscriptTurn{{Key: "turn", Ordinal: 1, StartSeq: 1, EndSeq: sequence, Status: TurnCompleted, CompletionEvidence: CompletionTaskComplete}}}
	_, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptAppend, ThreadID: fixture.MainThreadID, SourceID: fixture.MainSourceID, SourceGeneration: fixture.MainGeneration, Provider: ProviderCodex, ParserVersion: "backfill", CompletionRuleVersion: "backfill", ExpectedProjectionKey: fixture.MainProjectionKey, Result: result, IndexedAt: time.Now().UTC()})
	return err
}
