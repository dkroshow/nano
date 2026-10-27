package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

func TestConversationSearchSchedulerStartsAsynchronouslyAndBuildsInitialModes(t *testing.T) {
	gate := make(chan struct{})
	runner := &fakeConversationSearchBackfillRunner{
		statuses:      schedulerStatuses(history.ConversationSearchNeedsBuild, history.ConversationSearchNeedsBuild),
		statusGate:    gate,
		statusStarted: make(chan struct{}),
	}
	service, _ := newConversationSearchSchedulerService(t, runner, 1)
	select {
	case <-runner.statusStarted:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not begin asynchronously")
	}
	// Construction has already returned while the scheduler is blocked in its
	// first state read; opening Nano therefore never waits for a build.
	close(gate)
	waitConversationSearchSchedulerDone(t, service)
	if runner.startCalls != 1 || runner.batchCalls != 1 || len(runner.starts) != 1 || !runner.starts[0].CorpusReset {
		t.Fatalf("scheduler calls start=%d batch=%d", runner.startCalls, runner.batchCalls)
	}
	assertSchedulerStates(t, runner.snapshot(), history.ConversationSearchReady, history.ConversationSearchReady)
}

func TestConversationSearchSchedulerResumesBuildingCheckpointWithoutReset(t *testing.T) {
	store := newConversationSearchSchedulerStore(t)
	modes := []history.ConversationSearchMode{history.ConversationSearchLiteral, history.ConversationSearchTerms}
	if _, err := store.StartConversationSearchBackfill(context.Background(), history.ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	before := schedulerStatusMap(t, store)[history.ConversationSearchLiteral]
	projection, item := int64(99), "checkpoint"
	for _, mode := range modes {
		if _, err := store.SetConversationSearchState(context.Background(), mode, history.ConversationSearchStateUpdate{State: history.ConversationSearchBuilding, CheckpointProjectionID: &projection, CheckpointItemID: &item}); err != nil {
			t.Fatal(err)
		}
	}
	service := newConversationSearchSchedulerWithStore(t, store, nil, 1)
	waitConversationSearchSchedulerDone(t, service)
	after := schedulerStatusMap(t, store)
	assertSchedulerStates(t, schedulerStatusSlice(after), history.ConversationSearchReady, history.ConversationSearchReady)
	if after[history.ConversationSearchLiteral].Revision != before.Revision || after[history.ConversationSearchLiteral].CheckpointItemID == nil || *after[history.ConversationSearchLiteral].CheckpointItemID != item {
		t.Fatalf("resume unexpectedly reset durable checkpoint: before=%#v after=%#v", before, after[history.ConversationSearchLiteral])
	}
}

func TestConversationSearchSchedulerReadyModesNoOp(t *testing.T) {
	runner := &fakeConversationSearchBackfillRunner{statuses: schedulerStatuses(history.ConversationSearchReady, history.ConversationSearchReady)}
	service, _ := newConversationSearchSchedulerService(t, runner, 1)
	waitConversationSearchSchedulerDone(t, service)
	if runner.startCalls != 0 || runner.batchCalls != 0 {
		t.Fatalf("ready scheduler did work: start=%d batch=%d", runner.startCalls, runner.batchCalls)
	}
}

func TestConversationSearchSchedulerBuildsFreshStoreToReady(t *testing.T) {
	store := newConversationSearchSchedulerStore(t)
	service := newConversationSearchSchedulerWithStore(t, store, nil, 1)
	waitConversationSearchSchedulerDone(t, service)
	assertSchedulerStates(t, schedulerStatusSlice(schedulerStatusMap(t, store)), history.ConversationSearchReady, history.ConversationSearchReady)
}

func TestConversationSearchSchedulerRetriesFailedModesOnlyAfterRestartWithSafeReset(t *testing.T) {
	runner := &fakeConversationSearchBackfillRunner{statuses: schedulerStatuses(history.ConversationSearchFailed, history.ConversationSearchFailed)}
	service, _ := newConversationSearchSchedulerService(t, runner, 1)
	waitConversationSearchSchedulerDone(t, service)
	if len(runner.starts) != 1 || !runner.starts[0].CorpusReset || !hasBothConversationSearchSchedulerModes(runner.starts[0].Modes) {
		t.Fatalf("failed modes were not safely reset on startup: %#v", runner.starts)
	}
	assertSchedulerStates(t, runner.snapshot(), history.ConversationSearchReady, history.ConversationSearchReady)
}

func TestConversationSearchSchedulerRebuildsOneModeWithoutResettingReadySibling(t *testing.T) {
	store := newConversationSearchSchedulerStore(t)
	modes := []history.ConversationSearchMode{history.ConversationSearchLiteral, history.ConversationSearchTerms}
	seedConversationSearchSchedulerItem(t, store)
	buildConversationSearchForSchedulerTest(t, store, modes, true)
	before := schedulerStatusMap(t, store)[history.ConversationSearchTerms]
	if _, err := store.SetConversationSearchState(context.Background(), history.ConversationSearchLiteral, history.ConversationSearchStateUpdate{State: history.ConversationSearchNeedsBuild}); err != nil {
		t.Fatal(err)
	}
	service := newConversationSearchSchedulerWithStore(t, store, nil, 1)
	waitConversationSearchSchedulerDone(t, service)
	after := schedulerStatusMap(t, store)
	assertSchedulerStates(t, schedulerStatusSlice(after), history.ConversationSearchReady, history.ConversationSearchReady)
	if after[history.ConversationSearchTerms].Revision != before.Revision || after[history.ConversationSearchTerms].State != history.ConversationSearchReady || after[history.ConversationSearchTerms].DocumentCount != 1 || after[history.ConversationSearchTerms].IndexedCount != 1 {
		t.Fatalf("ready terms sibling was reset: before=%#v after=%#v", before, after[history.ConversationSearchTerms])
	}
}

func TestConversationSearchSchedulerRetriesStaleReconciliation(t *testing.T) {
	store := newConversationSearchSchedulerStore(t)
	runner := &schedulerStoreRunner{Store: store, staleOnce: true}
	service := newConversationSearchSchedulerWithStore(t, store, runner, 1)
	waitConversationSearchSchedulerDone(t, service)
	if runner.batchCalls < 2 {
		t.Fatalf("stale reconciliation was not retried: batches=%d", runner.batchCalls)
	}
	assertSchedulerStates(t, schedulerStatusSlice(schedulerStatusMap(t, store)), history.ConversationSearchReady, history.ConversationSearchReady)
}

func TestConversationSearchSchedulerCancellationWaitsForCurrentBatch(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	runner := &fakeConversationSearchBackfillRunner{
		statuses:   schedulerStatuses(history.ConversationSearchNeedsBuild, history.ConversationSearchNeedsBuild),
		batchStart: started,
		batchGate:  release,
	}
	service, _ := newConversationSearchSchedulerService(t, runner, 1)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not enter batch")
	}
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("service closed before bounded batch returned")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("service did not close after batch returned")
	}
}

func TestConversationSearchSchedulerPersistsTerminalFailure(t *testing.T) {
	store := newConversationSearchSchedulerStore(t)
	modes := []history.ConversationSearchMode{history.ConversationSearchLiteral, history.ConversationSearchTerms}
	if _, err := store.StartConversationSearchBackfill(context.Background(), history.ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	runner := &schedulerStoreRunner{Store: store, terminal: errors.New("injected scheduler failure")}
	service := newConversationSearchSchedulerWithStore(t, store, runner, 1)
	waitConversationSearchSchedulerDone(t, service)
	for _, status := range schedulerStatusMap(t, store) {
		if status.State != history.ConversationSearchFailed || status.ErrorSummary == nil || *status.ErrorSummary != "injected scheduler failure" {
			t.Fatalf("terminal failure was not durable: %#v", status)
		}
	}
}

func TestConversationSearchSchedulerFailsBuildingModesAfterStartError(t *testing.T) {
	runner := &fakeConversationSearchBackfillRunner{
		statuses:   schedulerStatuses(history.ConversationSearchNeedsBuild, history.ConversationSearchNeedsBuild),
		startError: errors.New("start committed then reported failure"),
	}
	service, _ := newConversationSearchSchedulerService(t, runner, 1)
	waitConversationSearchSchedulerDone(t, service)
	if runner.startCalls != 1 || runner.batchCalls != 0 {
		t.Fatalf("start failure retried in process: start=%d batch=%d", runner.startCalls, runner.batchCalls)
	}
	for _, status := range runner.snapshot() {
		if status.State != history.ConversationSearchFailed || status.ErrorSummary == nil || *status.ErrorSummary != "start committed then reported failure" {
			t.Fatalf("start failure did not become durable failed state: %#v", status)
		}
	}
}

type schedulerStoreRunner struct {
	*history.Store
	mu         sync.Mutex
	staleOnce  bool
	terminal   error
	batchCalls int
}

func (r *schedulerStoreRunner) RunConversationSearchBackfillBatch(ctx context.Context, batch history.ConversationSearchBackfillBatch) (history.ConversationSearchBackfillProgress, error) {
	r.mu.Lock()
	r.batchCalls++
	stale, terminal := r.staleOnce, r.terminal
	r.staleOnce = false
	r.mu.Unlock()
	if stale {
		return history.ConversationSearchBackfillProgress{}, history.ErrConversationSearchReconciliationStale
	}
	if terminal != nil {
		return history.ConversationSearchBackfillProgress{}, terminal
	}
	return r.Store.RunConversationSearchBackfillBatch(ctx, batch)
}

type fakeConversationSearchBackfillRunner struct {
	mu            sync.Mutex
	statuses      []history.ConversationSearchStatus
	statusGate    <-chan struct{}
	statusStarted chan struct{}
	statusOnce    sync.Once
	batchStart    chan struct{}
	batchGate     <-chan struct{}
	batchOnce     sync.Once
	startCalls    int
	starts        []history.ConversationSearchBackfillStart
	startError    error
	batchCalls    int
}

func (r *fakeConversationSearchBackfillRunner) ConversationSearchStatuses(context.Context) ([]history.ConversationSearchStatus, error) {
	r.statusOnce.Do(func() {
		if r.statusStarted == nil {
			r.statusStarted = make(chan struct{})
		}
		close(r.statusStarted)
	})
	if r.statusGate != nil {
		<-r.statusGate
	}
	return r.snapshot(), nil
}

func (r *fakeConversationSearchBackfillRunner) StartConversationSearchBackfill(_ context.Context, start history.ConversationSearchBackfillStart) ([]history.ConversationSearchStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startCalls++
	r.starts = append(r.starts, history.ConversationSearchBackfillStart{Modes: append([]history.ConversationSearchMode(nil), start.Modes...), CorpusReset: start.CorpusReset})
	for index := range r.statuses {
		for _, mode := range start.Modes {
			if r.statuses[index].Mode == mode {
				r.statuses[index].State = history.ConversationSearchBuilding
			}
		}
	}
	return append([]history.ConversationSearchStatus(nil), r.statuses...), r.startError
}

func (r *fakeConversationSearchBackfillRunner) RunConversationSearchBackfillBatch(_ context.Context, batch history.ConversationSearchBackfillBatch) (history.ConversationSearchBackfillProgress, error) {
	r.batchOnce.Do(func() {
		if r.batchStart != nil {
			close(r.batchStart)
		}
	})
	if r.batchGate != nil {
		<-r.batchGate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batchCalls++
	for index := range r.statuses {
		for _, mode := range batch.Modes {
			if r.statuses[index].Mode == mode {
				r.statuses[index].State = history.ConversationSearchReady
			}
		}
	}
	return history.ConversationSearchBackfillProgress{Modes: append([]history.ConversationSearchStatus(nil), r.statuses...), Complete: true}, nil
}

func (r *fakeConversationSearchBackfillRunner) FailConversationSearchBackfill(_ context.Context, modes []history.ConversationSearchMode, cause error) ([]history.ConversationSearchStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	message := cause.Error()
	for index := range r.statuses {
		for _, mode := range modes {
			if r.statuses[index].Mode == mode && r.statuses[index].State == history.ConversationSearchBuilding {
				r.statuses[index].State, r.statuses[index].ErrorSummary = history.ConversationSearchFailed, &message
			}
		}
	}
	return append([]history.ConversationSearchStatus(nil), r.statuses...), nil
}

func (r *fakeConversationSearchBackfillRunner) snapshot() []history.ConversationSearchStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]history.ConversationSearchStatus(nil), r.statuses...)
}

func newConversationSearchSchedulerStore(t *testing.T) *history.Store {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newConversationSearchSchedulerService(t *testing.T, runner conversationSearchBackfillRunner, batchSize int) (*threadCatalogService, *history.Store) {
	t.Helper()
	store := newConversationSearchSchedulerStore(t)
	return newConversationSearchSchedulerWithStore(t, store, runner, batchSize), store
}

func newConversationSearchSchedulerWithStore(t *testing.T, store *history.Store, runner conversationSearchBackfillRunner, batchSize int) *threadCatalogService {
	t.Helper()
	rootPath := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: rootPath}}, DisableWatcher: true, EnableConversationSearchScheduler: true,
		ConversationSearchBatchSize: batchSize, searchBackfill: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service
}

func buildConversationSearchForSchedulerTest(t *testing.T, store *history.Store, modes []history.ConversationSearchMode, corpusReset bool) {
	t.Helper()
	if _, err := store.StartConversationSearchBackfill(context.Background(), history.ConversationSearchBackfillStart{Modes: modes, CorpusReset: corpusReset}); err != nil {
		t.Fatal(err)
	}
	for {
		progress, err := store.RunConversationSearchBackfillBatch(context.Background(), history.ConversationSearchBackfillBatch{Modes: modes, BatchSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if progress.Complete {
			return
		}
	}
}

func seedConversationSearchSchedulerItem(t *testing.T, store *history.Store) {
	t.Helper()
	ctx := context.Background()
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: filepath.Join(t.TempDir(), "seed")}
	if err := os.MkdirAll(root.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(root.Path, "seed.jsonl")
	metadata := history.SourceProjection{
		Source: history.SourceCandidate{Root: root, Path: path, RelativePath: "seed.jsonl", Kind: history.ThreadMain, Size: 1, ModTime: created, Device: 1, Inode: 1},
		Result: &history.ProjectionResult{Projection: history.ThreadProjection{Provider: history.ProviderCodex, NativeThreadID: "scheduler-seed", IdentityEvidence: history.EvidenceNativeDurable, Kind: history.ThreadMain, CreatedAt: &created,
			Metadata: history.Metadata{CWD: history.UnknownField(), Model: history.UnknownField(), Effort: history.UnknownField(), Priority: history.UnknownField(), Permission: history.UnknownField(), HistoricalBranch: history.UnknownField(), Origin: history.UnknownField()}}, CompleteOffset: 1, CompleteLines: 1},
		ParserVersion: "scheduler-test", Status: history.SourcePresent, IndexedAt: created,
	}
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	persistedRoot, err := store.EnsureRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	source, ok, err := store.LoadSource(ctx, persistedRoot.ID, path)
	if err != nil || !ok {
		t.Fatalf("seed source = %#v, %t, %v", source, ok, err)
	}
	result := history.TranscriptProjectionResult{
		ProjectionKey: "scheduler-seed-projection", CompleteOffset: 1, CompleteLines: 1, ParserState: json.RawMessage(`{}`),
		Items: []history.TranscriptItem{{ID: "seed-user", ProjectedSeq: 1, SourceKey: "seed-user", TurnKey: "turn", TurnOrdinal: 1, Type: history.ItemUserMessage, Status: history.ItemStatusCompleted, Role: history.RoleUser, Text: "scheduler retained message", CreatedAt: &created, SourceLineSHA256: strings.Repeat("a", 64)}},
		Turns: []history.TranscriptTurn{{Key: "turn", Ordinal: 1, StartSeq: 1, EndSeq: 1, Status: history.TurnCompleted, CompletionEvidence: history.CompletionTaskComplete}},
	}
	if _, err := store.ApplyTranscript(ctx, history.TranscriptApplyRequest{Mode: history.TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: source.ID, SourceGeneration: source.Generation, Provider: history.ProviderCodex, ParserVersion: "scheduler-test", CompletionRuleVersion: "scheduler-test", Result: result, IndexedAt: created}); err != nil {
		t.Fatal(err)
	}
}

func waitConversationSearchSchedulerDone(t *testing.T, service *threadCatalogService) {
	t.Helper()
	select {
	case <-service.searchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("conversation search scheduler did not finish")
	}
}

func schedulerStatuses(literal, terms history.ConversationSearchBuildState) []history.ConversationSearchStatus {
	return []history.ConversationSearchStatus{{Mode: history.ConversationSearchLiteral, State: literal}, {Mode: history.ConversationSearchTerms, State: terms}}
}

func schedulerStatusMap(t *testing.T, store *history.Store) map[history.ConversationSearchMode]history.ConversationSearchStatus {
	t.Helper()
	statuses, err := store.ConversationSearchStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[history.ConversationSearchMode]history.ConversationSearchStatus, len(statuses))
	for _, status := range statuses {
		result[status.Mode] = status
	}
	return result
}

func schedulerStatusSlice(statuses map[history.ConversationSearchMode]history.ConversationSearchStatus) []history.ConversationSearchStatus {
	return []history.ConversationSearchStatus{statuses[history.ConversationSearchLiteral], statuses[history.ConversationSearchTerms]}
}

func assertSchedulerStates(t *testing.T, statuses []history.ConversationSearchStatus, literal, terms history.ConversationSearchBuildState) {
	t.Helper()
	got := map[history.ConversationSearchMode]history.ConversationSearchBuildState{}
	for _, status := range statuses {
		got[status.Mode] = status.State
	}
	if got[history.ConversationSearchLiteral] != literal || got[history.ConversationSearchTerms] != terms {
		t.Fatalf("scheduler states = %#v, want literal=%s terms=%s", got, literal, terms)
	}
}
