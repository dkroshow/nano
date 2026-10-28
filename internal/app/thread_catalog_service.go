package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/catalog"
	"github.com/dkroshow/nano/internal/history"
)

const (
	// catalogIngestBatch is how many known dirty sources one targeted pass
	// takes. Parsing within a pass already runs catalogParseConcurrency wide.
	catalogIngestBatch = 2
	// catalogBackgroundMaxWait is how long a background source waits behind
	// foreground ones before it takes one place in each pass, so threads that
	// keep writing cannot starve it.
	catalogBackgroundMaxWait = 30 * time.Second
	// catalogMaintenanceInterval paces the store-driven maintenance loops:
	// the parser backfill's idle check and superseded-projection collection.
	// Neither walks a provider root.
	catalogMaintenanceInterval         = 5 * time.Minute
	maxThreadCatalogErrorBytes         = 512
	maxThreadCatalogCursorBytes        = 1024
	maxThreadCatalogDiagnosticDetails  = 25
	maxThreadTranscriptCursorBytes     = 2048
	threadTranscriptCursorVersion      = 2
	threadTranscriptProfile            = "categories-v2"
	conversationSearchStaleRetryDelay  = 25 * time.Millisecond
	conversationSearchProgressInterval = 10_000
	transcriptChangeWorkerCount        = 2
	catalogParseConcurrency            = 2
	defaultTranscriptBackfillBatchSize = 2
)

type transcriptChange struct {
	ThreadID       string
	Provider       string
	NativeThreadID string
}

type pendingTranscriptChange struct {
	change     transcriptChange
	generation uint64
	queued     bool
	running    bool
	// since is the first notification the next callback covers; again is
	// the first one that arrived while a callback ran.
	since time.Time
	again time.Time
}

var (
	errInvalidThreadCatalogCursor = errors.New("invalid thread catalog cursor")
	// errInvalidThreadName marks a rename rejected by the loader's name rules
	// (trimmed, non-empty, ≤200 runes). It maps to a 400 invalid_name response.
	errInvalidThreadName = errors.New("invalid thread name")
	// errNamesFileDisabled marks a rename attempted while the names overlay is
	// off (no truth-file path configured). It maps to a 503 response.
	errNamesFileDisabled = errors.New("thread names file is not configured")
)

// threadNameInvalidError carries a user-facing validation message while still
// matching errInvalidThreadName through errors.Is, so the HTTP layer can surface
// the exact reason ("name exceeds 200 runes") without a second validation pass.
type threadNameInvalidError struct{ reason string }

func (e threadNameInvalidError) Error() string { return e.reason }

func (e threadNameInvalidError) Is(target error) bool { return target == errInvalidThreadName }

// threadCatalogServiceOptions keeps provider-root resolution injectable
// without putting filesystem work on an HTTP request path. An empty Roots
// slice resolves the standard provider roots.
type threadCatalogServiceOptions struct {
	Roots       []history.SourceRoot
	RootOptions history.RootOptions
	// NamesPath is the nano-owned thread-names truth file. Empty disables the
	// names overlay entirely (no file is ever read or written).
	NamesPath string
	// DisableWatcher (the -no-watch flag) starts no attached-file watches.
	// Attached threads then advance only on provider wire hints and explicit
	// refresh, and monitoring reports watching as disabled.
	DisableWatcher bool
	Logf           func(string, ...any)
	// EnableConversationSearchScheduler opts the production service into the
	// automatic resumable derived-index build. Tests leave it off unless they
	// specifically exercise this lifecycle, so unrelated catalog tests never
	// start a historical build.
	EnableConversationSearchScheduler bool
	// ConversationSearchBatchSize is an internal test/operator tuning seam.
	// Zero leaves the Store's conservative production default in effect.
	ConversationSearchBatchSize int
	searchBackfill              conversationSearchBackfillRunner
	// supersededCollector is a test seam for proving that cold catalog
	// readiness does not wait for derived-history collection.
	supersededCollector supersededProjectionCollector
	// EnableParserStateCompaction rewrites every active checkpoint once under
	// the readers' retention rule after boot.
	EnableParserStateCompaction bool
	// EnableSupersededProjectionCollector opts into the deferred destructive
	// sweep. It stays off unless the operator explicitly enables it.
	EnableSupersededProjectionCollector bool
	// ParserBackfillPauseFile stops new backfill batches while the file
	// exists, so an operator can hold a long parser upgrade without a deploy.
	ParserBackfillPauseFile string
	// EnableUsageBackfillScheduler rebuilds parser-behind transcripts in
	// bounded batches so a parser bump does not stall attached ingestion.
	EnableUsageBackfillScheduler bool
	UsageBackfillBatchSize       int
	projectSource                catalogProjectSourceFunc
	projectSourceStarted         func(history.SourceCandidate)
}

type catalogProjectSourceFunc func(context.Context, history.SourceCandidate, history.StoredSource, bool, history.TranscriptCheckpoint, bool, time.Time) (history.SourceProjection, *preparedTranscript, error)

type conversationSearchBackfillRunner interface {
	ConversationSearchStatuses(context.Context) ([]history.ConversationSearchStatus, error)
	StartConversationSearchBackfill(context.Context, history.ConversationSearchBackfillStart) ([]history.ConversationSearchStatus, error)
	RunConversationSearchBackfillBatch(context.Context, history.ConversationSearchBackfillBatch) (history.ConversationSearchBackfillProgress, error)
	FailConversationSearchBackfill(context.Context, []history.ConversationSearchMode, error) ([]history.ConversationSearchStatus, error)
}

type supersededProjectionCollector interface {
	CollectSupersededProjectionsBatch(context.Context, int) (history.SupersededProjectionBatch, error)
}

// catalogScanMode distinguishes targeted passes over known dirty sources from
// the explicit full recovery walk. Both trust persisted file signatures and
// parser versions before parsing; recovery adds discovery, deletion detection
// and retry of every errored checkpoint.
type catalogScanMode string

const (
	catalogScanFreshness catalogScanMode = "freshness"
	catalogScanRecovery  catalogScanMode = "recovery"
)

type threadCatalogService struct {
	store        *history.Store
	roots        []history.PersistedRoot
	catalogReady chan struct{}
	namesPath    string
	cancel       context.CancelFunc
	done         chan struct{}
	logf         func(string, ...any)

	// queueMu guards the worker's requests: the dirty-source queue, a pending
	// explicit recovery and a pending names refresh. work wakes the worker.
	queueMu    sync.Mutex
	work       chan struct{}
	dirty      map[string]*dirtySource
	dirtyOrder []string
	// dirtyBackground waits behind foreground sources, for at most
	// catalogBackgroundMaxWait: the holds of threads hidden at boot, so the
	// visible threads catch up first.
	dirtyBackground []string
	// recoveryAt is when the oldest unserved explicit refresh arrived; zero
	// when none waits. recoveringSince is set while one runs.
	recoveryAt      time.Time
	recoveringSince time.Time
	namesWanted     bool
	// scanned is closed when the next worker pass finishes, so a backfill
	// batch can wait for queued freshness work.
	scanned chan struct{}
	passSeq uint64

	// files watches every attached thread's native files; nil with -no-watch
	// or where kqueue is unavailable (filesErr says why).
	files    *catalog.FileWatcher
	filesErr error
	// discovery reports changed files under the provider roots, so sessions
	// Nano did not start are found without a walk (catalog_outside.go); nil
	// whenever files is.
	discovery *catalog.Discovery
	// attachedMu guards which attachments and outside holds hold which native
	// files.
	attachedMu sync.Mutex
	owners     map[string]*attachedOwner
	outside    map[string]*outsideHold
	held       map[string]*heldSource
	ingest     ingestMetrics

	// projectionMu is held by each worker pass for its whole run, and by a
	// parser backfill batch only while it checks and commits one source, so
	// a batch never commits over a pass that changed the source after the
	// batch read it.
	projectionMu    sync.Mutex
	projectSourceFn catalogProjectSourceFunc
	projectStarted  func(history.SourceCandidate)

	// hub fans catalog_version bumps out to SSE subscribers. publishMu guards the
	// last version handed to the hub so a repeated or lower bump never re-emits.
	hub              *catalogHub
	publishMu        sync.Mutex
	publishedVersion uint64

	closeOnce                sync.Once
	searchBackfill           conversationSearchBackfillRunner
	searchBatchSize          int
	searchDone               chan struct{}
	supersededCollector      supersededProjectionCollector
	supersededCollection     chan struct{}
	supersededDone           chan struct{}
	compactionDone           chan struct{}
	usageBackfillBatch       int
	backfillPauseFile        string
	usageDone                chan struct{}
	usageBackfillEnabled     bool
	rereadReady              chan struct{}
	rereadWake               chan struct{}
	rereadMu                 sync.Mutex
	rereadBuilderMu          sync.Mutex
	rereadQueue              []string
	rereads                  map[string]*pendingReread
	rereadRetry              map[string]rereadRetry
	rereadBuilding           string
	rereadAbandons           map[string]int
	repairPriorityMu         sync.RWMutex
	repairPriority           func(history.TranscriptParserBackfillCandidate) int
	transcriptChangedMu      sync.Mutex
	transcriptChangedCond    *sync.Cond
	transcriptChanged        func(context.Context, transcriptChange)
	transcriptChanges        map[string]*pendingTranscriptChange
	transcriptChangeQueue    []string
	transcriptChangeInFlight int
	transcriptChangeStopping bool
	transcriptChangeWorkerWg sync.WaitGroup

	// namesMu guards the truth-file change-detection signature and the last
	// surfaced load error, both read by IndexStatus from the request path. It
	// also retains the last valid truth snapshot so canonical creation facts can
	// refresh independently of the file's mtime.
	namesMu            sync.Mutex
	namesProcessed     bool
	namesFileValid     bool
	namesSig           threadNamesSignature
	namesError         *string
	namesRecords       []history.ThreadNameRecord
	creationNameSource func(context.Context) ([]history.ThreadNameRecord, error)
	namesRefreshMu     sync.Mutex

	// namesWriteMu serializes the read-modify-write of the whole truth-file
	// snapshot so concurrent renames never lose an update. It guards only the
	// file; namesRefreshMu separately serializes derived-store publication by
	// the scanner and the synchronous creation-name maintenance path.
	namesWriteMu sync.Mutex
}

// setTranscriptChanged connects durable-index commits to the live projection
// without making the catalog package know about session ownership. The setter
// is synchronized because the scanner starts during construction.
func (s *threadCatalogService) setTranscriptChanged(callback func(context.Context, transcriptChange)) {
	s.transcriptChangedMu.Lock()
	s.transcriptChanged = callback
	s.transcriptChangedCond.Broadcast()
	s.transcriptChangedMu.Unlock()
}

func (s *threadCatalogService) setTranscriptRepairPriority(callback func(history.TranscriptParserBackfillCandidate) int) {
	s.repairPriorityMu.Lock()
	s.repairPriority = callback
	s.repairPriorityMu.Unlock()
}

func (s *threadCatalogService) transcriptRepairPriority(candidate history.TranscriptParserBackfillCandidate) int {
	s.repairPriorityMu.RLock()
	callback := s.repairPriority
	s.repairPriorityMu.RUnlock()
	if callback == nil {
		return 2
	}
	return callback(candidate)
}

func (s *threadCatalogService) setCreationNameSource(source func(context.Context) ([]history.ThreadNameRecord, error)) {
	s.namesMu.Lock()
	s.creationNameSource = source
	s.namesMu.Unlock()
	s.requestNames()
}

func (s *threadCatalogService) notifyTranscriptChanged(change transcriptChange) {
	if change.ThreadID == "" {
		return
	}
	now := time.Now()
	s.transcriptChangedMu.Lock()
	pending := s.transcriptChanges[change.ThreadID]
	if pending == nil {
		pending = &pendingTranscriptChange{change: change, generation: 1, queued: true, since: now}
		s.transcriptChanges[change.ThreadID] = pending
		s.transcriptChangeQueue = append(s.transcriptChangeQueue, change.ThreadID)
	} else {
		pending.change = change
		pending.generation++
		if pending.running && pending.again.IsZero() {
			pending.again = now
		}
		if !pending.queued && !pending.running {
			pending.queued = true
			s.transcriptChangeQueue = append(s.transcriptChangeQueue, change.ThreadID)
		}
	}
	s.transcriptChangedCond.Signal()
	s.transcriptChangedMu.Unlock()
}

func (s *threadCatalogService) runTranscriptChangeWorker(ctx context.Context) {
	defer s.transcriptChangeWorkerWg.Done()
	for {
		s.transcriptChangedMu.Lock()
		for !s.transcriptChangeStopping && (s.transcriptChanged == nil || len(s.transcriptChangeQueue) == 0) {
			s.transcriptChangedCond.Wait()
		}
		if s.transcriptChangeStopping {
			s.transcriptChangedMu.Unlock()
			return
		}
		threadID := s.transcriptChangeQueue[0]
		s.transcriptChangeQueue = s.transcriptChangeQueue[1:]
		pending := s.transcriptChanges[threadID]
		pending.queued = false
		pending.running = true
		change, generation, since := pending.change, pending.generation, pending.since
		callback := s.transcriptChanged
		s.transcriptChangeInFlight++
		s.transcriptChangedMu.Unlock()

		started := time.Now()
		callback(ctx, change)
		s.notePublication(change, since, started, time.Now())

		s.transcriptChangedMu.Lock()
		s.transcriptChangeInFlight--
		pending = s.transcriptChanges[threadID]
		pending.running = false
		if pending.generation != generation {
			pending.since, pending.again = pending.again, time.Time{}
			pending.queued = true
			s.transcriptChangeQueue = append(s.transcriptChangeQueue, threadID)
			s.transcriptChangedCond.Signal()
		} else {
			delete(s.transcriptChanges, threadID)
		}
		s.transcriptChangedMu.Unlock()
	}
}

func (s *threadCatalogService) transcriptChangeStatus() (backlog, inFlight int) {
	s.transcriptChangedMu.Lock()
	defer s.transcriptChangedMu.Unlock()
	return len(s.transcriptChanges), s.transcriptChangeInFlight
}

// threadNamesSignature is the (existence, size, mtime) fingerprint used to skip
// re-reading an unchanged truth file each refresh cycle.
type threadNamesSignature struct {
	exists      bool
	size        int64
	modUnixNano int64
}

// newThreadCatalogService initializes the durable roots and starts one
// serialized worker. It walks no provider root: attached threads are ingested
// as their own files change, and full discovery runs only on an explicit
// refresh. Provider-owned files are opened read-only and never mutated.
func newThreadCatalogService(ctx context.Context, store *history.Store, options threadCatalogServiceOptions) (*threadCatalogService, error) {
	if ctx == nil {
		return nil, errors.New("thread catalog context is nil")
	}
	if store == nil {
		return nil, errors.New("thread catalog store is nil")
	}
	roots := append([]history.SourceRoot(nil), options.Roots...)
	if len(roots) == 0 {
		var err error
		roots, err = history.ResolveStandardRoots(options.RootOptions)
		if err != nil {
			return nil, fmt.Errorf("resolve thread history roots: %w", err)
		}
	}
	persisted := make([]history.PersistedRoot, 0, len(roots))
	for _, root := range roots {
		value, err := store.EnsureRoot(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("initialize thread history root: %w", err)
		}
		persisted = append(persisted, value)
	}

	workerContext, cancel := context.WithCancel(ctx)
	service := &threadCatalogService{
		store: store, roots: persisted, namesPath: strings.TrimSpace(options.NamesPath),
		catalogReady: make(chan struct{}),
		rereadReady:  make(chan struct{}), rereadWake: make(chan struct{}, 1),
		rereads: make(map[string]*pendingReread), rereadRetry: make(map[string]rereadRetry),
		cancel: cancel, done: make(chan struct{}), logf: options.Logf,
		work: make(chan struct{}, 1), dirty: make(map[string]*dirtySource),
		owners: make(map[string]*attachedOwner), held: make(map[string]*heldSource),
		outside:             make(map[string]*outsideHold),
		hub:                 newCatalogHub(),
		searchBackfill:      options.searchBackfill,
		searchBatchSize:     options.ConversationSearchBatchSize,
		supersededCollector: options.supersededCollector,
		transcriptChanges:   make(map[string]*pendingTranscriptChange),
		scanned:             make(chan struct{}),
		projectStarted:      options.projectSourceStarted,
	}
	service.projectSourceFn = service.projectSource
	if options.projectSource != nil {
		service.projectSourceFn = options.projectSource
	}
	service.transcriptChangedCond = sync.NewCond(&service.transcriptChangedMu)
	if service.searchBackfill == nil {
		service.searchBackfill = store
	}
	if service.supersededCollector == nil {
		service.supersededCollector = store
	}
	if options.EnableSupersededProjectionCollector {
		service.supersededCollection = make(chan struct{}, 1)
		service.supersededDone = make(chan struct{})
	}
	if options.EnableParserStateCompaction {
		service.compactionDone = make(chan struct{})
	}
	if service.logf == nil {
		service.logf = func(string, ...any) {}
	}
	if options.DisableWatcher {
		service.filesErr = errAttachedWatchDisabled
	} else if service.files, service.filesErr = catalog.StartFileWatcher(service.fileChanged, service.logf); service.filesErr != nil {
		service.logf("attached-file watching unavailable; attached threads advance on provider hints and explicit refresh: %v", service.filesErr)
	} else {
		// A root that does not exist yet is not watched until the next start.
		var live []string
		for _, root := range persisted {
			if _, err := os.Stat(root.Root.Path); err == nil && !root.Root.Archived {
				live = append(live, root.Root.Path)
			}
		}
		if len(live) > 0 {
			var err error
			if service.discovery, err = catalog.StartDiscovery(live, service.outsideChanged, service.logf); err != nil {
				service.logf("outside-session discovery unavailable; such sessions enter on explicit refresh: %v", err)
			}
		}
	}
	go service.run(workerContext)
	if service.supersededDone != nil {
		go service.runSupersededProjectionCollector(workerContext)
	}
	if service.compactionDone != nil {
		go service.runParserStateCompaction(workerContext)
	}
	service.transcriptChangeWorkerWg.Add(transcriptChangeWorkerCount)
	for worker := 0; worker < transcriptChangeWorkerCount; worker++ {
		go service.runTranscriptChangeWorker(workerContext)
	}
	if options.EnableConversationSearchScheduler {
		service.searchDone = make(chan struct{})
		go service.runConversationSearchScheduler(workerContext)
	}
	service.backfillPauseFile = options.ParserBackfillPauseFile
	service.usageBackfillEnabled = options.EnableUsageBackfillScheduler
	service.usageBackfillBatch = options.UsageBackfillBatchSize
	service.usageDone = make(chan struct{})
	go service.runUsageBackfillScheduler(workerContext)
	return service, nil
}

// errAttachedWatchDisabled is the monitoring state of the -no-watch flag.
var errAttachedWatchDisabled = errors.New("attached-file watching disabled by -no-watch")

// dirtySource is one known source waiting for, or in, a targeted pass.
type dirtySource struct {
	// since is when the first unserved hint arrived; later hints never reset
	// it, so queue time includes every wait.
	since   time.Time
	queued  bool
	running bool
	// again is the first hint that arrived during the running pass. The pass
	// read a snapshot before it, so the source gets exactly one more pass.
	again time.Time
	// readAt is the monotonic time (ingestNanos) the running pass last read
	// source bytes, so a long rebuild shows progress before its one commit.
	readAt atomic.Int64
	// background queues the source behind foreground ones (dirtyBackground).
	background bool
}

// dirtyTake is one source a pass took from the queue.
type dirtyTake struct {
	path   string
	since  time.Time
	source *dirtySource
}

// run is the only catalog worker. It first publishes the saved catalog as
// ready, then serves an explicit recovery walk before anything else, then a
// names refresh and a targeted pass over at most catalogIngestBatch dirty
// sources, oldest hint first. No pass waits for a settle window.
func (s *threadCatalogService) run(ctx context.Context) {
	defer close(s.done)
	s.initialize(ctx)
	if !s.initializeTranscriptRebuilds(ctx) {
		return
	}
	for {
		s.queueMu.Lock()
		idle := s.recoveryAt.IsZero() && !s.namesWanted && len(s.dirtyOrder)+len(s.dirtyBackground) == 0
		s.queueMu.Unlock()
		if idle {
			select {
			case <-ctx.Done():
				return
			case <-s.work:
			}
			continue
		}
		// Work is taken under projectionMu, so a backfill batch never sees
		// an empty queue while taken work waits to start.
		s.projectionMu.Lock()
		s.queueMu.Lock()
		recoverySince, names := s.recoveryAt, s.namesWanted
		var batch []dirtyTake
		var seq uint64
		if recoverySince.IsZero() {
			s.namesWanted = false
			batch = s.takeDirtyLocked(catalogIngestBatch)
			seq = s.passSeq
		} else {
			s.recoveryAt, s.namesWanted, s.recoveringSince = time.Time{}, false, time.Now()
		}
		s.queueMu.Unlock()
		if !recoverySince.IsZero() {
			started := time.Now()
			stats := s.refreshAll(ctx, catalogScanRecovery)
			finished := time.Now()
			s.ingest.recordRecovery(finished, finished.Sub(started))
			s.logScanCycle(started.Sub(recoverySince), finished.Sub(started), stats)
		} else {
			if names {
				if err := s.refreshThreadNames(ctx); err != nil {
					s.logf("thread names refresh failed: %v", err)
				}
			}
			if len(batch) > 0 {
				s.ingestBatch(ctx, batch, seq)
			}
		}
		s.finishPass(batch)
		s.projectionMu.Unlock()
		s.requestSupersededCollection()
		if ctx.Err() != nil {
			return
		}
	}
}

// initialize publishes the saved catalog before any provider file is read:
// each root is ready, or degraded with its preserved error, and names are
// reconciled once. Ready means the saved catalog is available, not that
// outside files were discovered, so no completion time is recorded.
func (s *threadCatalogService) initialize(ctx context.Context) {
	defer close(s.catalogReady)
	status, err := s.store.IndexStatus(ctx)
	if err != nil {
		s.logf("thread catalog index status: %v", err)
		return
	}
	for _, root := range status.Roots {
		if !s.servesRoot(root.Root.ID) {
			continue
		}
		state := history.IndexReady
		if root.Error != nil {
			state = history.IndexDegraded
		}
		if err := s.store.SetIndexState(ctx, root.Root.ID, state, root.Error); err != nil {
			s.logf("thread catalog root %s/%s state: %v", root.Root.Root.Provider, root.Root.Root.Kind, err)
		}
	}
	s.projectionMu.Lock()
	defer s.projectionMu.Unlock()
	if err := s.refreshThreadNames(ctx); err != nil {
		s.logf("thread names refresh failed: %v", err)
	}
}

func (s *threadCatalogService) servesRoot(id int64) bool {
	for _, root := range s.roots {
		if root.ID == id {
			return true
		}
	}
	return false
}

func (s *threadCatalogService) takeDirtyLocked(limit int) []dirtyTake {
	var batch []dirtyTake
	// An aged background source takes one place per pass, not the whole pass:
	// boot holds of hidden threads age before the visible ones are queued.
	agedTaken := false
	for len(batch) < limit && len(s.dirtyOrder)+len(s.dirtyBackground) > 0 {
		var path string
		switch {
		case len(s.dirtyOrder) == 0:
			path, s.dirtyBackground = s.dirtyBackground[0], s.dirtyBackground[1:]
		case len(s.dirtyBackground) > 0 && !agedTaken && time.Since(s.dirty[s.dirtyBackground[0]].since) >= catalogBackgroundMaxWait:
			agedTaken = true
			path, s.dirtyBackground = s.dirtyBackground[0], s.dirtyBackground[1:]
		default:
			path, s.dirtyOrder = s.dirtyOrder[0], s.dirtyOrder[1:]
		}
		source := s.dirty[path]
		source.queued, source.running = false, true
		batch = append(batch, dirtyTake{path: path, since: source.since, source: source})
	}
	if len(batch) > 0 {
		s.passSeq++
	}
	return batch
}

// finishPass requeues each source hinted during its pass and releases a
// backfill batch waiting for this pass.
func (s *threadCatalogService) finishPass(batch []dirtyTake) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	for _, take := range batch {
		source := s.dirty[take.path]
		source.running = false
		if source.again.IsZero() {
			delete(s.dirty, take.path)
			continue
		}
		source.since, source.again, source.queued = source.again, time.Time{}, true
		source.readAt.Store(0)
		s.queueLocked(take.path, source.background)
	}
	s.recoveringSince = time.Time{}
	close(s.scanned)
	s.scanned = make(chan struct{})
}

func (s *threadCatalogService) signalWork() {
	select {
	case s.work <- struct{}{}:
	default:
	}
}

func (s *threadCatalogService) logScanCycle(delay, duration time.Duration, stats []catalogRootScanStats) {
	segments := make([]string, 0, len(stats))
	repairs := 0
	for _, stat := range stats {
		repairs += stat.Repairs
		segments = append(segments, fmt.Sprintf("%s sources=%d changed=%d projected=%d transcripts=%d",
			stat.Root, stat.Sources, stat.Changed, stat.Projected, stat.Repairs))
	}
	s.logf("thread catalog recovery delay=%s duration=%s transcripts=%d roots=[%s]",
		delay.Round(time.Millisecond), duration.Round(time.Millisecond), repairs, strings.Join(segments, ", "))
}

// Close stops the worker and every watch. It does not close the store, whose
// lifecycle is owned by the process wiring.
func (s *threadCatalogService) Close() {
	s.closeOnce.Do(func() {
		if s.discovery != nil {
			s.discovery.Close()
		}
		s.cancel()
		s.transcriptChangedMu.Lock()
		s.transcriptChangeStopping = true
		s.transcriptChangedCond.Broadcast()
		s.transcriptChangedMu.Unlock()
		<-s.done
		if s.searchDone != nil {
			<-s.searchDone
		}
		if s.usageDone != nil {
			<-s.usageDone
		}
		if s.supersededDone != nil {
			<-s.supersededDone
		}
		if s.compactionDone != nil {
			<-s.compactionDone
		}
		s.transcriptChangeWorkerWg.Wait()
		s.releaseAllAttached()
		if s.files != nil {
			s.files.Close()
		}
		s.hub.Close()
	})
}

// RequestRefresh is the explicit discovery request (Explorer's Refresh). It
// queues one complete recovery walk: discovery of every provider file,
// absence reconciliation and a retry of every errored checkpoint. Queued
// attached work keeps its place and drains after the walk.
func (s *threadCatalogService) RequestRefresh() bool {
	s.queueMu.Lock()
	if s.recoveryAt.IsZero() {
		s.recoveryAt = time.Now()
	}
	s.queueMu.Unlock()
	s.signalWork()
	return true
}

// RequestPathRefresh queues known sources for targeted passes. A path that is
// not a provider file under a configured root is a cheap no-op.
func (s *threadCatalogService) RequestPathRefresh(paths []string) { s.requestPaths(paths, false) }

// requestPaths queues sources, in the background queue when background. A
// foreground request moves a waiting background source forward; a file event
// never does.
func (s *threadCatalogService) requestPaths(paths []string, background bool) {
	now := time.Now()
	for _, path := range paths {
		if path = filepath.Clean(path); path != "." && path != "" {
			s.enqueue(path, now, background, !background)
		}
	}
}

func (s *threadCatalogService) fileChanged(path string) { s.enqueue(path, time.Now(), false, false) }

func (s *threadCatalogService) enqueue(path string, at time.Time, background, promote bool) {
	s.queueMu.Lock()
	source := s.dirty[path]
	if source == nil {
		s.dirty[path] = &dirtySource{since: at, queued: true, background: background}
		s.queueLocked(path, background)
	} else {
		if promote && source.background {
			source.background = false
			if source.queued {
				s.dirtyBackground = slices.DeleteFunc(s.dirtyBackground, func(queued string) bool { return queued == path })
				s.dirtyOrder = append(s.dirtyOrder, path)
			}
		}
		if source.running && source.again.IsZero() {
			source.again = at
		}
	}
	s.queueMu.Unlock()
	s.signalWork()
}

func (s *threadCatalogService) queueLocked(path string, background bool) {
	if background {
		s.dirtyBackground = append(s.dirtyBackground, path)
	} else {
		s.dirtyOrder = append(s.dirtyOrder, path)
	}
}

// requestNames queues the names-only reconciliation on the worker, which
// publishes the change without walking a provider root.
func (s *threadCatalogService) requestNames() {
	s.queueMu.Lock()
	s.namesWanted = true
	s.queueMu.Unlock()
	s.signalWork()
}

func (s *threadCatalogService) requestSupersededCollection() {
	select {
	case s.supersededCollection <- struct{}{}:
	default:
	}
}

// runSupersededProjectionCollector starts after the store opens. It collects
// derived generations in small, independently committed batches with an
// explicit yield between them. A periodic pass covers projections superseded
// by live rebuilds after startup.
func (s *threadCatalogService) runSupersededProjectionCollector(ctx context.Context) {
	defer close(s.supersededDone)
	if !s.waitTranscriptRebuildStartup(ctx) {
		return
	}
	ticker := time.NewTicker(catalogMaintenanceInterval)
	defer ticker.Stop()
	for {
		if err := s.collectSupersededProjections(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logf("superseded projection collection: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.supersededCollection:
		case <-ticker.C:
		}
	}
}

// runParserStateCompaction starts after the store opens and rewrites stored
// checkpoints once, in small, independently committed batches with a yield
// between them. A checkpoint written since the readers' retention rule changed
// is slim already, so there is no recurring sweep; a checkpoint it did not
// reach (an error, a shutdown) waits for the next boot.
func (s *threadCatalogService) runParserStateCompaction(ctx context.Context) {
	defer close(s.compactionDone)
	if !s.waitTranscriptRebuildStartup(ctx) {
		return
	}
	if err := s.compactParserState(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logf("parser state compaction: %v", err)
	}
}

func (s *threadCatalogService) compactParserState(ctx context.Context) error {
	var projections, skipped int
	var before, after int64
	started := time.Now()
	for {
		batch, err := s.store.CompactParserStateBatch(ctx, 32)
		projections, skipped, before, after = projections+batch.Projections, skipped+batch.Skipped, before+batch.BytesBefore, after+batch.BytesAfter
		if err != nil || !batch.Remaining {
			if projections+skipped > 0 {
				s.logf("parser state compaction: projections=%d skipped=%d bytes=%d->%d elapsed=%s", projections, skipped, before, after, time.Since(started))
			}
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *threadCatalogService) collectSupersededProjections(ctx context.Context) error {
	for pass := 1; ; pass++ {
		started := time.Now()
		batch, err := s.supersededCollector.CollectSupersededProjectionsBatch(ctx, 64)
		// A pass that found nothing is the common case; only work is news.
		if batch.Collected > 0 || len(batch.Progress) > 0 {
			s.logf("history collector: pass=%d collected=%d items=%d elapsed=%s", pass, batch.Collected, batch.Items, time.Since(started))
		}
		for _, progress := range batch.Progress {
			s.logf("history collector: pass=%d projection=%d rows=%s", pass, progress.ProjectionID, progress.RowsSummary())
		}
		if err != nil {
			return err
		}
		if !batch.Remaining {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// runConversationSearchScheduler owns one automatic historical-build attempt
// for this process. Each Store batch is independently committed before this
// goroutine yields, so catalog scans and transcript ingestion may acquire the
// history write lock between batches. Lifecycle cancellation is checked only
// between Store calls: shutdown therefore waits for the current bounded
// transaction rather than turning it into a partial write.
func (s *threadCatalogService) runConversationSearchScheduler(ctx context.Context) {
	defer close(s.searchDone)
	select {
	case <-ctx.Done():
		return
	case <-s.catalogReady:
	}
	retryFailedAfterRestart := true
	for {
		if ctx.Err() != nil {
			return
		}
		statuses, err := s.searchBackfill.ConversationSearchStatuses(context.Background())
		if err != nil {
			s.logf("conversation search scheduler could not read state: %v", err)
			return
		}
		modes, start, corpusReset := conversationSearchSchedulerModes(statuses, retryFailedAfterRestart)
		if len(modes) == 0 {
			return
		}
		if start {
			if _, err := s.searchBackfill.StartConversationSearchBackfill(context.Background(), history.ConversationSearchBackfillStart{Modes: modes, CorpusReset: corpusReset}); err != nil {
				s.logf("conversation search scheduler could not start modes=%s: %v", conversationSearchModeNames(modes), err)
				s.failConversationSearchBuild(modes, err)
				return
			}
			if corpusReset {
				s.logf("conversation search scheduler started fresh corpus build modes=%s", conversationSearchModeNames(modes))
			} else {
				s.logf("conversation search scheduler started rebuild modes=%s", conversationSearchModeNames(modes))
			}
		} else {
			s.logf("conversation search scheduler resumed modes=%s", conversationSearchModeNames(modes))
		}
		if !s.runConversationSearchBuild(ctx, modes) {
			return
		}
		if start {
			retryFailedAfterRestart = false
		}
	}
}

// conversationSearchSchedulerModes selects at most one checkpoint-compatible
// build. An existing building mode always resumes first. On process startup,
// failed modes are deliberately reset once; a failure produced by this process
// instead stops the scheduler to avoid an unbounded retry loop.
func conversationSearchSchedulerModes(statuses []history.ConversationSearchStatus, retryFailedAfterRestart bool) ([]history.ConversationSearchMode, bool, bool) {
	var building, reset []history.ConversationSearchMode
	ready := false
	for _, status := range statuses {
		switch status.State {
		case history.ConversationSearchBuilding:
			building = append(building, status.Mode)
		case history.ConversationSearchNeedsBuild:
			reset = append(reset, status.Mode)
		case history.ConversationSearchFailed:
			if retryFailedAfterRestart {
				reset = append(reset, status.Mode)
			}
		case history.ConversationSearchReady:
			ready = true
		}
	}
	if len(building) > 0 {
		return building, false, false
	}
	if len(reset) == 0 {
		return nil, false, false
	}
	corpusReset := !ready && hasBothConversationSearchSchedulerModes(reset)
	return reset, true, corpusReset
}

func hasBothConversationSearchSchedulerModes(modes []history.ConversationSearchMode) bool {
	if len(modes) != 2 {
		return false
	}
	seen := map[history.ConversationSearchMode]bool{}
	for _, mode := range modes {
		seen[mode] = true
	}
	return seen[history.ConversationSearchLiteral] && seen[history.ConversationSearchTerms]
}

func conversationSearchModeNames(modes []history.ConversationSearchMode) string {
	values := make([]string, len(modes))
	for index, mode := range modes {
		values[index] = string(mode)
	}
	return strings.Join(values, ",")
}

func (s *threadCatalogService) runConversationSearchBuild(ctx context.Context, modes []history.ConversationSearchMode) bool {
	var lastMilestone int64 = -1
	staleLogged := false
	for {
		if ctx.Err() != nil {
			return false
		}
		progress, err := s.searchBackfill.RunConversationSearchBackfillBatch(context.Background(), history.ConversationSearchBackfillBatch{Modes: modes, BatchSize: s.searchBatchSize})
		if errors.Is(err, history.ErrConversationSearchReconciliationStale) {
			if !staleLogged {
				s.logf("conversation search scheduler reconciliation became stale modes=%s; retrying", conversationSearchModeNames(modes))
				staleLogged = true
			}
			if !waitConversationSearchSchedulerRetry(ctx) {
				return false
			}
			continue
		}
		if err != nil {
			s.failConversationSearchBuild(modes, err)
			return false
		}
		staleLogged = false
		if progress.Complete {
			s.logf("conversation search scheduler completed modes=%s", conversationSearchModeNames(modes))
			return true
		}
		for _, status := range progress.Modes {
			milestone := status.IndexedCount / conversationSearchProgressInterval
			if milestone > lastMilestone {
				lastMilestone = milestone
				s.logf("conversation search scheduler progress modes=%s indexed=%d", conversationSearchModeNames(modes), status.IndexedCount)
			}
		}
	}
}

func waitConversationSearchSchedulerRetry(ctx context.Context) bool {
	timer := time.NewTimer(conversationSearchStaleRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *threadCatalogService) failConversationSearchBuild(modes []history.ConversationSearchMode, cause error) {
	statuses, statusErr := s.searchBackfill.ConversationSearchStatuses(context.Background())
	if statusErr != nil {
		s.logf("conversation search scheduler failed modes=%s: %v (could not read failure state: %v)", conversationSearchModeNames(modes), cause, statusErr)
		return
	}
	building := make([]history.ConversationSearchMode, 0, len(modes))
	for _, status := range statuses {
		for _, mode := range modes {
			if status.Mode == mode && status.State == history.ConversationSearchBuilding {
				building = append(building, mode)
			}
		}
	}
	if len(building) > 0 {
		if _, err := s.searchBackfill.FailConversationSearchBackfill(context.Background(), building, cause); err != nil {
			s.logf("conversation search scheduler failed modes=%s: %v (could not persist failure: %v)", conversationSearchModeNames(modes), cause, err)
			return
		}
	}
	s.logf("conversation search scheduler failed modes=%s: %v", conversationSearchModeNames(modes), cause)
}

func (s *threadCatalogService) runUsageBackfillScheduler(ctx context.Context) {
	defer close(s.usageDone)
	if !s.waitTranscriptRebuildStartup(ctx) {
		return
	}
	for {
		var rebuilt int
		var worked time.Duration
		var err error
		if s.usageBackfillEnabled || s.hasDirectTranscriptReread(time.Now()) {
			rebuilt, worked, err = s.RunTranscriptParserBackfillBatch(ctx, s.usageBackfillBatch)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			s.logf("transcript usage backfill: %v", err)
		}
		// Preserve the ordinary small backfill pacing; the shared lane paces chunks.
		wait := catalogMaintenanceInterval
		if rebuilt > 0 {
			wait = 2 * worked
		}
		if retryWait, ok := s.nextTranscriptRereadRetry(time.Now()); ok {
			wait = min(wait, retryWait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.rereadWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// RunTranscriptParserBackfillBatch rebuilds at most batchSize parser-behind
// transcripts whose canonical source is still present. It returns how many it
// inspected and how long it worked once admitted, not counting its waits for
// projectionMu.
func (s *threadCatalogService) RunTranscriptParserBackfillBatch(ctx context.Context, batchSize int) (int, time.Duration, error) {
	if !s.waitTranscriptRebuildStartup(ctx) {
		return 0, 0, ctx.Err()
	}
	s.rereadBuilderMu.Lock()
	defer s.rereadBuilderMu.Unlock()
	if batchSize <= 0 {
		batchSize = defaultTranscriptBackfillBatchSize
	}
	if rebuilt, worked, handled, err := s.runDirectTranscriptRereads(ctx, batchSize); handled {
		return rebuilt, worked, err
	}
	// Explicit callers still inspect the backlog, even with its automatic loop off.

	// A batch never starts during a worker pass or while attached work or an
	// explicit recovery is queued: it waits for the pass that takes the work,
	// then checks again. Under a steady stream of file changes the backfill
	// waits; freshness always goes first. Once started it reads and parses
	// without projectionMu and takes it only to commit each source, so worker
	// passes keep committing while a batch parses.
	for {
		s.projectionMu.Lock()
		s.queueMu.Lock()
		queued, scanned := len(s.dirtyOrder)+len(s.dirtyBackground) > 0 || !s.recoveryAt.IsZero(), s.scanned
		s.queueMu.Unlock()
		s.projectionMu.Unlock()
		if !queued {
			break
		}
		select {
		case <-scanned:
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	// Checked after admission, so a pause made while this batch waited holds it.
	if s.backfillPauseFile != "" {
		// Anything but a clear "not there" holds the batch: the backfill can wait.
		if _, err := os.Stat(s.backfillPauseFile); !errors.Is(err, os.ErrNotExist) {
			s.logf("transcript parser backfill paused: %s (stat: %v)", s.backfillPauseFile, err)
			return 0, 0, nil
		}
	}
	started := time.Now()
	candidates, err := s.store.ListTranscriptParserBackfillCandidates(ctx, -1)
	if err != nil {
		return 0, time.Since(started), err
	}
	candidates, unaffected := s.markUnaffectedTranscripts(ctx, candidates, time.Now().UTC())
	priorities := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		priorities[candidate.ThreadID] = s.transcriptRepairPriority(candidate)
	}
	prioritizeTranscriptBackfillCandidates(candidates, priorities)
	if len(candidates) > batchSize {
		candidates = candidates[:batchSize]
	}
	rebuilt := 0
	repaired := 0
	var waited, held time.Duration
	now := time.Now().UTC()
	for _, candidate := range candidates {
		if s.transcriptRereadCooling(candidate.ThreadID, time.Now()) || s.deferredTranscriptReread(candidate.ThreadID) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return rebuilt, time.Since(started) - waited, err
		}
		checkpoint, exists, err := s.store.LoadTranscriptCheckpointHeader(ctx, candidate.ThreadID)
		if err != nil {
			s.logf("transcript usage backfill checkpoint %s: %v", candidate.ThreadID, err)
			continue
		}
		base, prepared, prepareErr := s.prepareTranscriptSource(ctx, candidate.Source, candidate.Source.Source, true, checkpoint, exists)
		if errors.Is(prepareErr, history.ErrTranscriptChanged) {
			continue
		}
		if prepareErr != nil && base.ID == 0 {
			// The source row itself could not be read: nothing to check a
			// commit or an error mark against.
			s.logf("transcript usage backfill source %s: %v", candidate.ThreadID, prepareErr)
			continue
		}
		if prepareErr == nil && shouldDeferTranscriptReread(base, prepared.Source, checkpoint, exists) {
			outcome, advance, buildErr := s.buildPreparedTranscriptReread(ctx, candidate, checkpoint, base, prepared, false, time.Time{}, now)
			prepared.File.Close()
			s.publishApplied(ctx, advance)
			if outcome != backfillSkipped {
				rebuilt++
			}
			if buildErr != nil && !errors.Is(buildErr, history.ErrTranscriptRebuildPreempted) && !errors.Is(buildErr, history.ErrTranscriptChanged) {
				s.logf("transcript reread: %v", buildErr)
			}
			if s.hasDirectTranscriptReread(time.Now()) {
				break
			}
			continue
		}
		lockStart := time.Now()
		s.projectionMu.Lock()
		locked := time.Now()
		waited += locked.Sub(lockStart)
		outcome, advance := s.commitBackfill(ctx, candidate, checkpoint, exists, base, prepared, prepareErr, now)
		s.projectionMu.Unlock()
		held += time.Since(locked)
		if prepared != nil {
			prepared.File.Close()
		}
		s.publishApplied(ctx, advance)
		switch outcome {
		case backfillRepaired:
			s.rereadMu.Lock()
			delete(s.rereadRetry, candidate.ThreadID)
			delete(s.rereadAbandons, candidate.ThreadID)
			s.rereadMu.Unlock()
			repaired++
			rebuilt++
		case backfillMarked:
			if shouldDeferTranscriptReread(base, candidate.Source.Source, checkpoint, exists) {
				s.recordTranscriptRereadFailure(candidate.ThreadID, time.Now())
			}
			rebuilt++
		}
	}
	worked := time.Since(started) - waited
	if rebuilt+unaffected > 0 {
		s.logf("transcript parser backfill batch inspected=%d repaired=%d unaffected=%d elapsed=%s locked=%s lock_wait=%s", rebuilt+unaffected, repaired, unaffected, worked, held, waited)
	}
	return rebuilt + unaffected, worked, nil
}

// unaffectedScanBatch bounds how many candidate files one batch reads to
// decide whether a reader upgrade affects them; the scheduler's wait of
// twice the work keeps the backfill's duty time to a third. Candidates past
// the bound are left for the next batch: never handed to a full re-read
// unclassified.
const unaffectedScanBatch = 64

// markUnaffectedTranscripts records, for candidates whose upgrade names a
// marker, those whose file lacks it, and returns the candidates that still
// need a re-read: the ones with the marker, those a marker cannot classify,
// and those it could not read. Candidates beyond this batch's bound are not
// returned; they wait. A newly recorded proof makes the thread's saved
// transcript the current reader's, as a re-read would, so it is announced as
// a re-read's commit is: a reader waiting for it wakes.
func (s *threadCatalogService) markUnaffectedTranscripts(ctx context.Context, candidates []history.TranscriptParserBackfillCandidate, now time.Time) ([]history.TranscriptParserBackfillCandidate, int) {
	rest := candidates[:0]
	marked, scanned := 0, 0
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		target := history.CurrentTranscriptParserVersion(candidate.Provider)
		marker, ok := history.TranscriptUpgradeMarker(candidate.ParserVersion, target)
		if !ok || candidate.CompletionRuleVersion != history.CurrentTranscriptCompletionRuleVersion(candidate.Provider) {
			rest = append(rest, candidate)
			continue
		}
		if scanned == unaffectedScanBatch {
			continue // waits for the next batch, unclassified
		}
		scanned++
		found, err := history.SourceContainsMarker(candidate.Source.Source.Path, marker)
		if err != nil || found {
			rest = append(rest, candidate)
			continue
		}
		recorded, err := s.store.RecordTranscriptParserUnaffected(ctx, candidate, target, candidate.CompletionRuleVersion, now)
		if err != nil {
			s.logf("transcript parser backfill unaffected %s: %v", candidate.ThreadID, err)
			rest = append(rest, candidate)
			continue
		}
		if recorded {
			s.notifyTranscriptChanged(transcriptChange{ThreadID: candidate.ThreadID, Provider: string(candidate.Provider), NativeThreadID: candidate.NativeThreadID})
		}
		marked++
	}
	return rest, marked
}

func prioritizeTranscriptBackfillCandidates(candidates []history.TranscriptParserBackfillCandidate, priorities map[string]int) {
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := priorities[candidates[i].ThreadID], priorities[candidates[j].ThreadID]
		if left != right {
			return left < right
		}
		if !candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
		}
		return candidates[i].ThreadID < candidates[j].ThreadID
	})
}

// backfillOutcome is what one candidate's commit did.
type backfillOutcome int

const (
	// backfillSkipped wrote nothing; the source stays a candidate while it is
	// still behind.
	backfillSkipped backfillOutcome = iota
	backfillRepaired
	// backfillMarked marked the transcript with its rebuild error.
	backfillMarked
)

// commitBackfill runs under projectionMu with what a batch prepared without
// it: base and checkpoint are the source row and transcript checkpoint read
// before parsing. It writes only if it can read both again and they are
// unchanged: a worker pass may have committed a newer source row or
// transcript meanwhile. An older parse committed over a newer source row
// reads as a replaced file, and an error mark would land on the newer
// transcript, so an unchanged check is required for the commit and for the
// mark alike.
func (s *threadCatalogService) commitBackfill(ctx context.Context, candidate history.TranscriptParserBackfillCandidate, checkpoint history.TranscriptCheckpoint, exists bool, base history.StoredSource, prepared *preparedTranscript, prepareErr error, indexedAt time.Time) (backfillOutcome, transcriptAdvance) {
	current, found, err := s.store.LoadSource(ctx, candidate.Source.RootID, candidate.Source.Source.Path)
	var header history.TranscriptCheckpoint
	var hasHeader bool
	if err == nil {
		header, hasHeader, err = s.store.LoadTranscriptCheckpointHeader(ctx, candidate.ThreadID)
	}
	if err != nil {
		s.logf("transcript usage backfill check %s: %v", candidate.ThreadID, err)
		return backfillSkipped, transcriptAdvance{}
	}
	if !found || !reflect.DeepEqual(current, base) || hasHeader != exists || !reflect.DeepEqual(header, checkpoint) {
		return backfillSkipped, transcriptAdvance{}
	}
	advance, err := transcriptAdvance{}, prepareErr
	if err == nil {
		advance, err = s.commitTranscriptSource(ctx, base, prepared, indexedAt)
	}
	switch {
	case errors.Is(err, history.ErrTranscriptChanged):
		return backfillSkipped, advance
	case err != nil:
		s.logf("transcript usage backfill rebuild %s: %v", candidate.ThreadID, err)
		summary := strings.TrimSpace(err.Error())
		if len(summary) > 480 {
			summary = summary[:480]
		}
		if markErr := s.store.MarkTranscriptError(ctx, candidate.ThreadID, candidate.Source.ID, "usage backfill: "+summary, indexedAt); markErr != nil {
			s.logf("transcript usage backfill skip %s: %v", candidate.ThreadID, markErr)
			return backfillSkipped, advance
		}
		return backfillMarked, advance
	default:
		return backfillRepaired, advance
	}
}

// refreshAll is the explicit recovery walk over every root, then names.
func (s *threadCatalogService) refreshAll(ctx context.Context, mode catalogScanMode) []catalogRootScanStats {
	stats := make([]catalogRootScanStats, 0, len(s.roots))
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return stats
		}
		rootStats, err := s.refreshRoot(ctx, root, mode, nil, nil)
		stats = append(stats, rootStats)
		if err != nil {
			s.logf("thread history scan failed for %s: %v", root.Root.Path, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return stats
	}
	if err := s.refreshThreadNames(ctx); err != nil {
		s.logf("thread names refresh failed: %v", err)
	}
	return stats
}

// ingestBatch runs one targeted pass over the sources it took, root by root,
// and records each source's outcome. A root-wide failure fails each of its
// sources that has no outcome of its own.
func (s *threadCatalogService) ingestBatch(ctx context.Context, batch []dirtyTake, seq uint64) {
	pass := newIngestPass(batch, seq)
	for _, root := range s.roots {
		paths := make(map[string]struct{})
		for path := range pass.sources {
			if catalogPathsIncludeRoot(map[string]struct{}{path: {}}, root.Root.Path) {
				paths[path] = struct{}{}
			}
		}
		if len(paths) == 0 {
			continue
		}
		for path := range paths {
			pass.sources[path].provider = root.Root.Provider
		}
		if _, err := s.refreshRoot(ctx, root, catalogScanFreshness, paths, pass); err != nil {
			for path := range paths {
				pass.sources[path].fail(ingestErrorStore, err)
			}
		}
	}
	s.completeIngest(pass)
}

// grokTranscriptFor maps a Grok summary.json to its session's updates.jsonl
// once that exists: the catalog keeps one source per session, and a summary
// rewrite is a metadata change of that source.
func grokTranscriptFor(root history.SourceRoot, path string) string {
	if root.Provider != history.ProviderGrok || filepath.Base(path) != "summary.json" {
		return path
	}
	updates := filepath.Join(filepath.Dir(path), "updates.jsonl")
	if _, err := os.Lstat(updates); err != nil {
		return path
	}
	return updates
}

func catalogPathsIncludeRoot(paths map[string]struct{}, root string) bool {
	for path := range paths {
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// refreshThreadNames reconciles the derived thread_names table with both name
// sources. The truth file has precedence; immutable creation facts fill only
// keys absent from that file. A malformed file leaves the last good derived
// copy intact and refuses to publish a partial source set.
func (s *threadCatalogService) refreshThreadNames(ctx context.Context) error {
	if s.namesPath == "" {
		return nil
	}
	s.namesRefreshMu.Lock()
	defer s.namesRefreshMu.Unlock()
	signature, err := statThreadNames(s.namesPath)
	if err != nil {
		// A stat failure (not a missing file) is transient; keep retrying by
		// leaving the change-detection signature unadvanced.
		s.setNamesError(boundedCatalogError(fmt.Errorf("inspect thread names file: %w", err)))
		return err
	}
	s.namesMu.Lock()
	unchanged := s.namesProcessed && signature == s.namesSig
	fileValid := s.namesFileValid
	records := append([]history.ThreadNameRecord(nil), s.namesRecords...)
	source := s.creationNameSource
	s.namesMu.Unlock()
	if unchanged && !fileValid {
		return errors.New("thread names file remains malformed")
	}
	if !unchanged {
		records, err = history.LoadThreadNames(s.namesPath)
		if err != nil {
			// Malformed file: surface the error but skip this identical bad file on
			// later cycles until it changes. The derived copy is left untouched.
			s.markNamesProcessed(signature, boundedCatalogError(err))
			return err
		}
	}
	truthRecords := append([]history.ThreadNameRecord(nil), records...)
	if source != nil {
		creationRecords, sourceErr := source(ctx)
		if sourceErr != nil {
			s.setNamesError(boundedCatalogError(fmt.Errorf("load creation names: %w", sourceErr)))
			return sourceErr
		}
		records = mergeThreadNameSources(truthRecords, creationRecords)
	}
	changed, err := s.store.SyncThreadNames(ctx, records)
	if err != nil {
		// A store failure is transient; retry without advancing the signature.
		s.setNamesError(boundedCatalogError(fmt.Errorf("sync thread names: %w", err)))
		return err
	}
	if changed {
		// SyncThreadNames bumps the version but does not return it; a follow-up
		// read is exact because the scan goroutine is the only writer.
		if version, err := s.store.CatalogVersion(ctx); err == nil {
			s.publishCatalogVersion(version)
		}
	}
	s.namesMu.Lock()
	s.namesProcessed = true
	s.namesFileValid = true
	s.namesSig = signature
	s.namesError = nil
	s.namesRecords = truthRecords
	s.namesMu.Unlock()
	return nil
}

func mergeThreadNameSources(primary, creation []history.ThreadNameRecord) []history.ThreadNameRecord {
	merged := append([]history.ThreadNameRecord(nil), primary...)
	seen := make(map[string]bool, len(primary))
	for _, record := range primary {
		seen[string(record.Provider)+"\x00"+record.NativeThreadID] = true
	}
	for _, record := range creation {
		key := string(record.Provider) + "\x00" + record.NativeThreadID
		if !seen[key] {
			merged = append(merged, record)
			seen[key] = true
		}
	}
	return merged
}

func statThreadNames(path string) (threadNamesSignature, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return threadNamesSignature{}, nil
	}
	if err != nil {
		return threadNamesSignature{}, err
	}
	return threadNamesSignature{exists: true, size: info.Size(), modUnixNano: info.ModTime().UnixNano()}, nil
}

func (s *threadCatalogService) setNamesError(message string) {
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	s.namesError = &message
}

// markNamesProcessed advances the change-detection signature and sets or clears
// the surfaced error (empty message clears it).
func (s *threadCatalogService) markNamesProcessed(signature threadNamesSignature, message string) {
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	s.namesProcessed = true
	s.namesFileValid = message == ""
	s.namesSig = signature
	if message == "" {
		s.namesError = nil
		return
	}
	s.namesError = &message
}

func (s *threadCatalogService) namesErrorSnapshot() *string {
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	if s.namesError == nil {
		return nil
	}
	message := *s.namesError
	return &message
}

// catalogRootScanStats is the per-root cost of one cycle, logged once per scan.
type catalogRootScanStats struct {
	Root      string
	Sources   int
	Changed   int
	Projected int
	Repairs   int
}

// refreshRoot walks one root (recovery) or inspects the given paths in it
// (a targeted pass, which records each path's outcome in pass). A targeted
// pass writes no root index state: it proves nothing about the root, and a
// state write per pass would make every open Explorer poll while threads work.
func (s *threadCatalogService) refreshRoot(ctx context.Context, root history.PersistedRoot, mode catalogScanMode, paths map[string]struct{}, pass *ingestPass) (catalogRootScanStats, error) {
	stats := catalogRootScanStats{Root: fmt.Sprintf("%s/%s", root.Root.Provider, root.Root.Kind)}
	targeted := len(paths) > 0
	// metadataChanged names canonical sources re-read for a sibling
	// metadata file's change even when their own bytes are unchanged.
	metadataChanged := map[string]bool{}
	if !targeted {
		if err := s.store.SetIndexState(ctx, root.ID, history.IndexScanning, nil); err != nil {
			return stats, fmt.Errorf("mark root scanning: %w", err)
		}
	}
	storedByPath := make(map[string]history.StoredSource)
	discovery := history.DiscoveryResult{Root: root.Root, Present: true}
	var err error
	if !targeted {
		storedByPath, err = s.store.LoadSourcesByRoot(ctx, root.ID)
		if err != nil {
			summary := boundedCatalogError(fmt.Errorf("load stored sources: %w", err))
			_ = s.store.SetIndexState(ctx, root.ID, history.IndexDegraded, &summary)
			return stats, err
		}
		discovery, err = history.DiscoverRoot(ctx, root.Root)
		if err != nil {
			summary := boundedCatalogError(err)
			_ = s.store.SetIndexState(ctx, root.ID, history.IndexDegraded, &summary)
			return stats, err
		}
	} else {
		discovered := map[string]bool{}
		for path := range paths {
			source := grokTranscriptFor(root.Root, path)
			if source != path {
				pass.alias(path, source)
				metadataChanged[source] = true
			}
			if discovered[source] {
				continue
			}
			discovered[source] = true
			candidate, found, discoverErr := history.DiscoverFile(ctx, root.Root, source)
			if discoverErr != nil {
				pass.fail(source, ingestErrorRead, discoverErr)
				continue
			}
			if !found {
				continue
			}
			pass.outcome(source).observe(candidate.Size)
			discovery.Candidates = append(discovery.Candidates, candidate)
			stored, exists, loadErr := s.store.LoadSource(ctx, root.ID, candidate.Path)
			if loadErr != nil {
				return stats, fmt.Errorf("load stored source: %w", loadErr)
			}
			if exists {
				storedByPath[candidate.Path] = stored
			}
		}
	}
	observedPaths := make([]string, 0, len(discovery.Candidates))
	for _, candidate := range discovery.Candidates {
		observedPaths = append(observedPaths, candidate.Path)
	}
	sort.Strings(observedPaths)
	sort.Slice(discovery.Candidates, func(i, j int) bool {
		return discovery.Candidates[i].Path < discovery.Candidates[j].Path
	})

	checkpointHeaders := make(map[string]history.TranscriptCheckpoint)
	if !targeted {
		checkpointHeaders, err = s.store.LoadTranscriptCheckpointHeadersByRoot(ctx, root.ID)
		if err != nil {
			return stats, fmt.Errorf("load transcript checkpoint headers: %w", err)
		}
	}
	var preparedFiles []*preparedTranscript
	defer func() {
		for _, prepared := range preparedFiles {
			_ = prepared.File.Close()
		}
	}()
	indexedAt := time.Now().UTC()
	rootParserVersion := parserVersion(root.Root.Provider)
	changedSources := make([]history.SourceProjection, 0)
	transcriptRepairs := make([]transcriptRepairCandidate, 0)
	var projectionErrors []string
	stats.Sources = len(discovery.Candidates)
	type projectionWork struct {
		candidate     history.SourceCandidate
		stored        history.StoredSource
		exists        bool
		header        history.TranscriptCheckpoint
		hasCheckpoint bool
		input         history.SourceProjection
		prepared      *preparedTranscript
		err           error
		started, done time.Time
	}
	var work []projectionWork
	for _, candidate := range discovery.Candidates {
		stored, exists := storedByPath[candidate.Path]
		// A new Grok session starts with summary.json and creates updates.jsonl
		// once the first turn begins. ReconcileRoot preserves the source row by
		// renaming summary to updates, so transcript preparation must use that
		// same prior source and checkpoint. Treating updates as brand new prepares
		// an ExpectNoProjection rebuild that reconciliation immediately makes
		// stale, leaving the metadata-only projection degraded until a sweep.
		if !exists && candidate.Root.Provider == history.ProviderGrok && filepath.Base(candidate.Path) == "updates.jsonl" {
			summaryPath := filepath.Join(filepath.Dir(candidate.Path), "summary.json")
			stored, exists = storedByPath[summaryPath]
			if targeted && !exists {
				stored, exists, err = s.store.LoadSource(ctx, root.ID, summaryPath)
				if err != nil {
					return stats, fmt.Errorf("load prior Grok summary source: %w", err)
				}
			}
		}
		header, hasCheckpoint := checkpointHeaders[stored.ThreadID]
		if targeted && stored.ThreadID != "" {
			header, hasCheckpoint, err = s.store.LoadTranscriptCheckpointHeader(ctx, stored.ThreadID)
			if err != nil {
				return stats, fmt.Errorf("load transcript checkpoint header: %w", err)
			}
		}
		if exists && history.DeviceRenumbered(stored.Source, candidate) && s.adoptSourceIdentity(ctx, stored, candidate) {
			stored.Source.Device = candidate.Device
		}
		// An unchanged file reproduces its stored projection exactly, so the old
		// per-file guarded upsert was always a no-op. Skip it, and carry no
		// projection into reconcile — its observed path still counts for absence.
		unchanged := exists && !metadataChanged[candidate.Path] && sourceUnchanged(stored, candidate, rootParserVersion, header, hasCheckpoint)
		if exists && (!unchanged || header.ErrorSummary != nil) && shouldDeferTranscriptReread(stored, candidate, header, hasCheckpoint) {
			s.enqueueTranscriptReread(stored.ThreadID)
			pass.verified(candidate.Path, stored.CanonicalTranscriptSource, header, false)
			continue
		}
		// Targeted passes and recovery walks trust the same persisted
		// signature. Only a file or parser-version change reaches a parser,
		// except that an errored checkpoint is retried whenever its source is
		// named: by recovery, or by a hint or retry for that source.
		if unchanged {
			pass.verified(candidate.Path, stored.CanonicalTranscriptSource, header, hasCheckpoint && header.ErrorSummary == nil)
			if hasCheckpoint && header.ErrorSummary != nil {
				transcriptRepairs = append(transcriptRepairs, transcriptRepairCandidate{Source: candidate})
			}
			continue
		}
		stats.Changed++
		work = append(work, projectionWork{candidate: candidate, stored: stored, exists: exists, header: header, hasCheckpoint: hasCheckpoint})
	}
	jobs := make([]func(), len(work))
	for index := range work {
		item := &work[index]
		jobs[index] = func() {
			if s.projectStarted != nil {
				s.projectStarted(item.candidate)
			}
			item.started = time.Now()
			item.input, item.prepared, item.err = s.projectSourceFn(withReadProgress(ctx, pass.progress(item.candidate.Path)), item.candidate, item.stored, item.exists, item.header, item.hasCheckpoint, indexedAt)
			item.done = time.Now()
		}
	}
	runCatalogParseJobs(jobs, catalogParseConcurrency)
	for _, item := range work {
		repair := transcriptRepairCandidate{Source: item.candidate, Prepared: item.prepared}
		if item.exists {
			repair.ForceRebuild = transcriptSourceChanged(item.stored, item.candidate)
		}
		if item.prepared != nil {
			preparedFiles = append(preparedFiles, item.prepared)
		}
		pass.prepared(item.candidate.Path, item.started, item.done, item.prepared)
		if item.err != nil {
			pass.fail(item.candidate.Path, ingestErrorRead, item.err)
			message := boundedCatalogError(item.err)
			projectionErrors = append(projectionErrors, message)
			item.input = history.SourceProjection{
				Source: item.candidate, ParserVersion: parserVersion(item.candidate.Root.Provider),
				Status: history.SourceError, ErrorSummary: &message, IndexedAt: indexedAt,
			}
		} else {
			stats.Projected++
			transcriptRepairs = append(transcriptRepairs, repair)
		}
		changedSources = append(changedSources, item.input)
	}

	// A filesystem diagnostic means the walk skipped some evidence, so it
	// cannot prove that previously indexed files disappeared.
	state := history.IndexReady
	var reconciliationError *string
	if discovery.Summary.Filesystem > 0 {
		state = history.IndexDegraded
		message := "history discovery was incomplete"
		reconciliationError = &message
	}
	reconciled, err := s.store.ReconcileRoot(ctx, history.RootReconciliation{
		RootID: root.ID, ObservedPaths: observedPaths, Sources: changedSources, State: state,
		Partial: targeted, CompletedAt: time.Now().UTC(), Error: reconciliationError,
	})
	if err != nil {
		if !targeted {
			message := boundedCatalogError(err)
			_ = s.store.SetIndexState(ctx, root.ID, history.IndexDegraded, &message)
		}
		return stats, fmt.Errorf("reconcile history root: %w", err)
	}
	if reconciled.Changed {
		if reconciled.Structural {
			s.publishCatalogVersion(reconciled.CatalogVersion)
		} else {
			s.publishCatalogThreads(reconciled.CatalogVersion, reconciled.ChangedThreadIDs)
		}
	}
	pass.reconciled(time.Now())
	repairs, repairErrors := s.repairTranscripts(ctx, root.ID, mode, transcriptRepairs, indexedAt, pass)
	stats.Repairs = repairs
	projectionErrors = append(projectionErrors, repairErrors...)
	if !targeted && state == history.IndexReady && len(projectionErrors) > 0 {
		message := fmt.Sprintf("%d history source(s) could not be projected: %s", len(projectionErrors), projectionErrors[0])
		message = boundedCatalogError(errors.New(message))
		if err := s.store.SetIndexState(ctx, root.ID, history.IndexDegraded, &message); err != nil {
			return stats, fmt.Errorf("mark projection errors: %w", err)
		}
	}
	return stats, nil
}

func runCatalogParseJobs(jobs []func(), limit int) {
	if len(jobs) == 0 {
		return
	}
	if limit < 1 {
		limit = 1
	}
	permits := make(chan struct{}, limit)
	var workers sync.WaitGroup
	for _, job := range jobs {
		permits <- struct{}{}
		workers.Add(1)
		go func(job func()) {
			defer workers.Done()
			defer func() { <-permits }()
			job()
		}(job)
	}
	workers.Wait()
}

// transcriptRepairCandidate carries only the source-change evidence that would
// be lost once metadata reconciliation commits the newly observed stat. The
// transcript checkpoint itself remains independent from metadata checkpoints.
type transcriptRepairCandidate struct {
	Source       history.SourceCandidate
	ForceRebuild bool
	Prepared     *preparedTranscript
}

func transcriptSourceChanged(stored history.StoredSource, current history.SourceCandidate) bool {
	if stored.Source.Kind != current.Kind || stored.Source.Archived != current.Archived {
		return true
	}
	if grokLineageChanged(stored, current) {
		return true
	}
	if identitiesComparable(stored.Source, current) && !sameSourceIdentity(stored.Source, current) {
		return true
	}
	if current.Size < stored.Source.Size {
		return true
	}
	// Growth on the same source is append-safe. Any other stat change can be an
	// in-place rewrite and must mint a new projection rather than silently keep
	// a checkpoint at the old end of file.
	if current.Size > stored.Source.Size && !current.ModTime.Before(stored.Source.ModTime) {
		return false
	}
	return current.Size != stored.Source.Size || !current.ModTime.Equal(stored.Source.ModTime)
}

// transcriptSourceTouchedOnly reports a stat change that is only a new
// modification time on the same file at the same size. Idle Claude CLI
// sessions touch their files hourly; rebuilding each touch left about 1.2 GB
// of superseded copies a day. Whole-document Grok .json
// sources are excluded: they may be rewritten in place.
func transcriptSourceTouchedOnly(stored history.StoredSource, current history.SourceCandidate) bool {
	return stored.Source.Kind == current.Kind && stored.Source.Archived == current.Archived &&
		!grokLineageChanged(stored, current) &&
		identitiesComparable(stored.Source, current) && sameSourceIdentity(stored.Source, current) &&
		current.Size == stored.Source.Size && !current.ModTime.Equal(stored.Source.ModTime) &&
		!(current.Root.Provider == history.ProviderGrok && filepath.Ext(current.Path) == ".json")
}

func parserVersions(provider history.Provider) (string, string) {
	parser, completion, _ := history.ParsedFileVersions(provider)
	return parser, completion
}

func parserVersion(provider history.Provider) string {
	parser, _ := parserVersions(provider)
	return parser
}

// transcriptRepairState resolves the committed source row and transcript
// checkpoint a repair applies against. ReconcileRoot can change a source's
// generation, thread association, and canonical-transcript flag, and
// ApplyTranscript is guarded on exactly those committed values, so they are
// always read back after the commit — never carried over from the pre-commit
// scan. A recovery sweep reads the whole root in two queries; a freshness cycle
// reads only the handful of rows its walk changed.
type transcriptRepairState struct {
	store       *history.Store
	rootID      int64
	batched     bool
	sources     map[string]history.StoredSource
	checkpoints map[string]history.TranscriptCheckpoint
}

func (r transcriptRepairState) source(ctx context.Context, path string) (history.StoredSource, bool, error) {
	if r.batched {
		stored, exists := r.sources[path]
		return stored, exists, nil
	}
	return r.store.LoadSource(ctx, r.rootID, path)
}

func (r transcriptRepairState) checkpoint(ctx context.Context, threadID string) (history.TranscriptCheckpoint, bool, error) {
	if r.batched {
		checkpoint, exists := r.checkpoints[threadID]
		return checkpoint, exists, nil
	}
	return r.store.LoadTranscriptCheckpointHeader(ctx, threadID)
}

func (s *threadCatalogService) loadTranscriptRepairState(ctx context.Context, rootID int64, mode catalogScanMode) (transcriptRepairState, error) {
	state := transcriptRepairState{store: s.store, rootID: rootID, batched: mode == catalogScanRecovery}
	if !state.batched {
		return state, nil
	}
	sources, err := s.store.LoadSourcesByRoot(ctx, rootID)
	if err != nil {
		return state, fmt.Errorf("load transcript sources: %w", err)
	}
	checkpoints, err := s.store.LoadTranscriptCheckpointHeadersByRoot(ctx, rootID)
	if err != nil {
		return state, fmt.Errorf("load transcript checkpoints: %w", err)
	}
	state.sources, state.checkpoints = sources, checkpoints
	return state, nil
}

// repairTranscripts runs after ReconcileRoot commits and reports how many
// candidates it examined. An unchanged transcript at its complete-file boundary
// is recognized without opening the provider file.
func (s *threadCatalogService) repairTranscripts(ctx context.Context, rootID int64, mode catalogScanMode, repairs []transcriptRepairCandidate, indexedAt time.Time, pass *ingestPass) (int, []string) {
	failures := make([]string, 0)
	if len(repairs) == 0 {
		return 0, failures
	}
	committed, err := s.loadTranscriptRepairState(ctx, rootID, mode)
	if err != nil {
		for _, repair := range repairs {
			pass.fail(repair.Source.Path, ingestErrorStore, err)
		}
		return 0, append(failures, boundedCatalogError(err))
	}
	attempted := 0
	for index, repair := range repairs {
		if err := ctx.Err(); err != nil {
			for _, rest := range repairs[index:] {
				pass.fail(rest.Source.Path, ingestErrorStore, err)
			}
			return attempted, append(failures, boundedCatalogError(err))
		}
		stored, exists, err := committed.source(ctx, repair.Source.Path)
		if err != nil {
			pass.fail(repair.Source.Path, ingestErrorStore, err)
			failures = append(failures, boundedCatalogError(fmt.Errorf("load transcript source: %w", err)))
			continue
		}
		// Not a thread's canonical transcript: its metadata already committed.
		if !exists || stored.ThreadID == "" || !stored.CanonicalTranscriptSource {
			continue
		}
		checkpoint, checkpointExists, err := committed.checkpoint(ctx, stored.ThreadID)
		if err != nil {
			pass.fail(repair.Source.Path, ingestErrorStore, err)
			failures = append(failures, boundedCatalogError(fmt.Errorf("load transcript checkpoint: %w", err)))
			continue
		}
		attempted++
		var advance transcriptAdvance
		var applyErr error
		if repair.Prepared != nil {
			advance, applyErr = s.applyPreparedTranscript(ctx, stored, checkpoint, checkpointExists, repair.Prepared, indexedAt)
			_ = repair.Prepared.File.Close()
		} else {
			advance, applyErr = s.projectTranscriptSource(withReadProgress(ctx, pass.progress(repair.Source.Path)), stored, repair.Source, repair.ForceRebuild, checkpoint, checkpointExists, indexedAt)
		}
		pass.saved(repair.Source.Path, stored, checkpoint, checkpointExists, advance, applyErr)
		// The receipt is recorded before the notification it describes.
		s.recordReceipt(pass.outcome(repair.Source.Path), repair.Source.Path)
		s.publishApplied(ctx, advance)
		if err := applyErr; err != nil {
			message := boundedCatalogError(fmt.Errorf("project transcript: %w", err))
			failures = append(failures, message)
			if markErr := s.store.MarkTranscriptError(ctx, stored.ThreadID, stored.ID, message, indexedAt); markErr != nil {
				failures = append(failures, boundedCatalogError(fmt.Errorf("record transcript error: %w", markErr)))
			}
		}
	}
	return attempted, failures
}

func mergeAppendDiagnostics(result *history.ProjectionResult, stored history.StoredSource) {
	details := make([]history.Diagnostic, 0, maxThreadCatalogDiagnosticDetails)
	for _, source := range [][]history.Diagnostic{stored.Diagnostics, result.Diagnostics} {
		remaining := maxThreadCatalogDiagnosticDetails - len(details)
		if remaining <= 0 {
			break
		}
		if len(source) > remaining {
			source = source[:remaining]
		}
		details = append(details, source...)
	}
	summary := result.Summary
	summary.Total += stored.DiagnosticSummary.Total
	summary.Malformed += stored.DiagnosticSummary.Malformed
	summary.UnknownRows += stored.DiagnosticSummary.UnknownRows
	summary.MissingIdentity += stored.DiagnosticSummary.MissingIdentity
	summary.IdentityConflicts += stored.DiagnosticSummary.IdentityConflicts
	summary.OversizedLines += stored.DiagnosticSummary.OversizedLines
	summary.Filesystem += stored.DiagnosticSummary.Filesystem
	summary.Dropped = summary.Total - len(details)
	if summary.Dropped < 0 {
		summary.Dropped = 0
	}
	result.Diagnostics = details
	result.Summary = summary
}

// sourceUnchanged reports whether a scan can skip a source. A parser-version
// difference alone is skipped when this source's own transcript checkpoint is
// healthy and also behind: that thread is a backfill candidate, and the paced
// backfill advances both versions, so a parser bump never re-projects a whole
// provider in one sweep. Otherwise (no checkpoint of its own, a checkpoint
// error, a current transcript) the scan re-derives it.
func sourceUnchanged(stored history.StoredSource, current history.SourceCandidate, parserVersion string, checkpoint history.TranscriptCheckpoint, hasCheckpoint bool) bool {
	parserCompatible := stored.ParserVersion == parserVersion || (hasCheckpoint && checkpoint.SourceID == stored.ID &&
		checkpoint.ErrorSummary == nil && checkpoint.ParserVersion != parserVersion)
	// The source row commits before the transcript rows, so a scan stopped
	// between them can leave a thread's transcript source with no transcript,
	// one of an older generation, or one short of the file's end. Only that
	// source's own current transcript, read to the end, proves the file was
	// read; a recorded transcript error keeps its recovery path.
	transcriptRead := !stored.CanonicalTranscriptSource || hasCheckpoint && (checkpoint.ErrorSummary != nil ||
		checkpoint.SourceID == stored.ID && checkpoint.SourceGeneration == stored.Generation && checkpoint.ReadOffset == current.Size)
	return stored.Status == history.SourcePresent && stored.Projection != nil &&
		parserCompatible && sameSourceIdentity(stored.Source, current) &&
		stored.Source.Size == current.Size && stored.Source.ModTime.Equal(current.ModTime) &&
		stored.Source.Kind == current.Kind && stored.Source.Archived == current.Archived &&
		!grokLineageChanged(stored, current) && transcriptRead
}

// grokLineageChanged is intentionally one-way for a missing sidecar: preserve
// the last known native edge through a provider's transient sidecar rewrite.
// A present sidecar may establish or reparent an edge and forces a metadata
// and transcript reconciliation from existing catalog fields.
func grokLineageChanged(stored history.StoredSource, current history.SourceCandidate) bool {
	if current.Root.Provider != history.ProviderGrok || current.ParentNativeID == "" {
		return false
	}
	if stored.Projection == nil {
		return true
	}
	return stored.Projection.ParentNativeID != current.ParentNativeID || stored.Projection.Kind != history.ThreadSubagent
}

func canAppendProjection(stored history.StoredSource, current history.SourceCandidate, parserVersion string) bool {
	// Grok metadata always comes from the sibling summary.json snapshot, even
	// when discovery is anchored on append-only updates.jsonl. Re-read that
	// small snapshot whole so its replacement semantics never masquerade as a
	// metadata append. Transcript append safety is decided separately below.
	if current.Root.Provider == history.ProviderGrok {
		return false
	}
	if stored.Projection == nil || stored.ParserVersion != parserVersion ||
		current.Size <= stored.Source.Size || current.Size < stored.CompleteOffset ||
		stored.Source.Kind != current.Kind || stored.Source.Archived != current.Archived {
		return false
	}
	if identitiesComparable(stored.Source, current) {
		return sameSourceIdentity(stored.Source, current)
	}
	// On filesystems without stable device/inode evidence, a backwards mtime
	// is treated as replacement rather than an append.
	return !current.ModTime.Before(stored.Source.ModTime)
}

func identitiesComparable(left, right history.SourceCandidate) bool {
	return left.Device != 0 && left.Inode != 0 && right.Device != 0 && right.Inode != 0
}

func sameSourceIdentity(left, right history.SourceCandidate) bool {
	if identitiesComparable(left, right) {
		return left.Device == right.Device && left.Inode == right.Inode
	}
	return true
}

// adoptSourceIdentity lets a source keep its projection across a renumbered
// volume when its own saved bytes prove the file at its path unchanged
// (history.Store.AdoptSourceIdentity). Anything unproven stays replaced and is
// read once.
func (s *threadCatalogService) adoptSourceIdentity(ctx context.Context, stored history.StoredSource, candidate history.SourceCandidate) bool {
	file, opened, err := openDiscoveredSource(candidate)
	if err != nil {
		return false
	}
	defer file.Close()
	adopted, err := s.store.AdoptSourceIdentity(ctx, stored, opened, file)
	if err != nil {
		s.logf("adopt source identity %s: %v", stored.ThreadID, err)
		return false
	}
	return adopted
}

func openDiscoveredSource(candidate history.SourceCandidate) (*os.File, history.SourceCandidate, error) {
	pathInfo, err := os.Lstat(candidate.Path)
	if err != nil {
		return nil, candidate, fmt.Errorf("inspect history source: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, candidate, errors.New("history source is no longer a regular file")
	}
	file, err := os.Open(candidate.Path)
	if err != nil {
		return nil, candidate, fmt.Errorf("open history source: %w", err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, candidate, fmt.Errorf("inspect open history source: %w", err)
	}
	if !os.SameFile(pathInfo, fileInfo) {
		file.Close()
		return nil, candidate, errors.New("history source changed during discovery")
	}
	device, inode := catalogFileIdentity(fileInfo)
	candidate.Size = fileInfo.Size()
	candidate.ModTime = fileInfo.ModTime()
	candidate.Device = device
	candidate.Inode = inode
	return file, candidate, nil
}

func catalogFileIdentity(info os.FileInfo) (uint64, uint64) {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return 0, 0
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, 0
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 0, 0
	}
	return catalogUintField(value, "Dev"), catalogUintField(value, "Ino")
}

func catalogUintField(value reflect.Value, name string) uint64 {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Int() >= 0 {
			return uint64(field.Int())
		}
	}
	return 0
}

func boundedCatalogError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) <= maxThreadCatalogErrorBytes {
		return message
	}
	return message[:maxThreadCatalogErrorBytes]
}

func (s *threadCatalogService) ListThreads(ctx context.Context, query catalogListQuery) (catalogPage, error) {
	params := catalogQueryParams(query)
	params.ThreadID = query.ThreadID
	for _, key := range query.Identities {
		params.Identities = append(params.Identities, history.ThreadIdentity{Provider: history.Provider(key.Provider), NativeThreadID: key.NativeThreadID})
	}
	params.Limit = query.Limit
	if query.Cursor != "" {
		cursor, err := decodeThreadCatalogCursor(query.Cursor)
		if err != nil {
			return catalogPage{}, err
		}
		params.After = cursor
	}
	page, err := s.store.ListThreads(ctx, params)
	if err != nil {
		return catalogPage{}, err
	}
	result := catalogPage{Items: make([]catalogThread, 0, len(page.Items)), CatalogVersion: catalogVersion(page.CatalogVersion)}
	for _, item := range page.Items {
		result.Items = append(result.Items, mapCatalogThread(item, nil))
	}
	if page.NextCursor != nil {
		encoded, err := encodeThreadCatalogCursor(page.NextCursor)
		if err != nil {
			return catalogPage{}, err
		}
		result.NextCursor = &encoded
	}
	return result, nil
}

func (s *threadCatalogService) SearchConversationItems(ctx context.Context, query history.ConversationSearchQuery) (history.ConversationSearchPage, error) {
	return s.store.SearchConversationItems(ctx, query)
}

func (s *threadCatalogService) ConversationSearchStatus(ctx context.Context) ([]history.ConversationSearchStatus, error) {
	return s.store.ConversationSearchStatus(ctx)
}

func (s *threadCatalogService) LoadThread(ctx context.Context, id string) (catalogThread, error) {
	detail, err := s.store.GetThread(ctx, id)
	if errors.Is(err, history.ErrThreadNotFound) {
		return catalogThread{}, errCatalogThreadNotFound
	}
	if err != nil {
		return catalogThread{}, err
	}
	sources := make([]catalogSourceDetail, 0, len(detail.Sources))
	for _, source := range detail.Sources {
		diagnostics := make([]string, 0, len(source.Diagnostics))
		for _, diagnostic := range source.Diagnostics {
			message := string(diagnostic.Kind)
			if diagnostic.Line > 0 {
				message += fmt.Sprintf(" at line %d", diagnostic.Line)
			}
			if diagnostic.Message != "" {
				message += ": " + diagnostic.Message
			}
			diagnostics = append(diagnostics, message)
		}
		sources = append(sources, catalogSourceDetail{
			RootKind: string(source.RootKind), Path: source.Path, Generation: source.Generation,
			ParserVersion: source.ParserVersion, Status: string(source.Status), Archived: source.Archived,
			IdentityEvidence: string(source.IdentityEvidence), DiagnosticCount: source.DiagnosticSummary.Total,
			Diagnostics: diagnostics, Error: source.ErrorSummary, LastIndexedAt: formatCatalogTime(source.LastIndexedAt),
		})
	}
	thread := mapCatalogThread(detail.Thread, &sources)
	// Zero coverage drops the field rather than reporting "0s · 0 turns": a
	// thread whose provider never recorded a duration has an unknown total, not
	// a zero one (D2/D8). A query failure degrades the same way — the detail
	// response is worth more than the one field.
	if totals, err := s.store.ThreadTurnTotals(ctx, id); err != nil {
		log.Printf("thread turn totals %s: %v", id, err)
	} else if totals.Turns > 0 {
		thread.Took = &catalogThreadTook{DurationMS: totals.DurationMS, Turns: totals.Turns}
	}
	return thread, nil
}

func (s *threadCatalogService) LoadThreadUsage(ctx context.Context, id string, afterOrdinal, limit int) (history.ThreadUsagePage, error) {
	page, err := s.store.ThreadTurnUsage(ctx, id, afterOrdinal, limit)
	if errors.Is(err, history.ErrThreadNotFound) {
		return history.ThreadUsagePage{}, errCatalogThreadNotFound
	}
	return page, err
}

func (s *threadCatalogService) ListThreadUsageTotals(ctx context.Context, query history.ThreadUsageListQuery) (history.ThreadUsageListPage, error) {
	return s.store.ListThreadUsageTotals(ctx, query)
}

func (s *threadCatalogService) ReadSavedTurnStatus(ctx context.Context, threadID, nativeTurnID string) (history.SavedTurnStatus, bool, error) {
	return s.store.ReadSavedTurnStatus(ctx, threadID, nativeTurnID)
}

func (s *threadCatalogService) LoadCompletedTranscriptTurn(ctx context.Context, id, nativeTurnID string) (transcriptResponse, error) {
	page, err := s.store.ReadCompletedTranscriptTurn(ctx, id, nativeTurnID)
	if errors.Is(err, history.ErrTranscriptChanged) {
		return transcriptResponse{}, errTranscriptCursorStale
	}
	if err != nil {
		return transcriptResponse{}, err
	}
	response := transcriptResponse{ThreadID: id, Projection: transcriptProjection{Key: page.Projection.Key, State: string(page.Projection.State), SourceStatus: string(page.Projection.SourceStatus), EvidenceAvailable: page.Projection.EvidenceAvailable}, Page: transcriptPageInfo{StartSeq: page.StartSeq, EndSeq: page.EndSeq, SnapshotHeadSeq: page.SnapshotHeadSeq}}
	for _, item := range page.Items {
		response.Items = append(response.Items, transcriptItem{ID: item.ID, NativeItemID: item.NativeItemID, TurnID: item.TurnKey, ProjectedSeq: item.ProjectedSeq, Type: string(item.Type), Role: string(item.Role), Text: item.Text, Phase: item.Phase, IsFinalAnswer: item.IsFinalAnswer, Detail: append(json.RawMessage(nil), item.Detail...), ContentTruncated: item.ContentTruncated})
	}
	for _, turn := range page.Turns {
		response.Turns = append(response.Turns, transcriptTurn{ID: turn.Key, NativeTurnID: turn.NativeTurnID, Ordinal: turn.Ordinal, StartSeq: turn.StartSeq, EndSeq: turn.EndSeq, Status: string(turn.Status), FinalItemID: turn.FinalItemID, CompletionEvidence: string(turn.CompletionEvidence), FinalitySource: turn.FinalitySource, FinalityConfidence: turn.FinalityConfidence})
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return transcriptResponse{}, err
	}
	if len(encoded) > maxThreadTranscriptBytes {
		return transcriptResponse{}, history.ErrCompletedTranscriptUnavailable
	}
	return response, nil
}

func (s *threadCatalogService) LoadTranscript(ctx context.Context, id string, query transcriptListQuery) (transcriptResponse, error) {
	responses, _, err := s.LoadTranscripts(ctx, id, []transcriptListQuery{query}, nil)
	if err != nil {
		return transcriptResponse{}, err
	}
	return responses[0], nil
}

// LoadTranscripts reads several pages of one thread, and which saved rows
// carry the given exact keys, from one consistent snapshot.
func (s *threadCatalogService) LoadTranscripts(ctx context.Context, id string, queries []transcriptListQuery, match []history.TranscriptMatch) ([]transcriptResponse, map[string]string, error) {
	requests := make([]history.TranscriptReadRequest, len(queries))
	for i := range queries {
		request, err := transcriptReadRequest(id, &queries[i])
		if err != nil {
			return nil, nil, err
		}
		requests[i] = request
	}
	pages, matched, err := s.store.ReadTranscripts(ctx, id, requests, match)
	switch {
	case errors.Is(err, history.ErrThreadNotFound):
		return nil, nil, errCatalogThreadNotFound
	case errors.Is(err, history.ErrTranscriptChanged):
		return nil, nil, errTranscriptCursorStale
	case errors.Is(err, history.ErrInvalidTranscriptPage):
		return nil, nil, errInvalidTranscriptCursor
	case err != nil:
		return nil, nil, err
	}
	responses := make([]transcriptResponse, len(pages))
	for i, page := range pages {
		if responses[i], err = transcriptResponseFor(id, queries[i], page); err != nil {
			return nil, nil, err
		}
	}
	return responses, matched, nil
}

// transcriptReadRequest is one page query as the store reads it. It fills in
// the default profile's hidden categories on the query itself.
func transcriptReadRequest(id string, query *transcriptListQuery) (history.TranscriptReadRequest, error) {
	if !query.HiddenSpecified && query.View == defaultThreadTranscriptView && len(query.HiddenCategories) == 0 {
		query.HiddenCategories = append([]string(nil), enhancedHiddenCategories...)
	}
	hidden := make([]history.TranscriptCategory, 0, len(query.HiddenCategories))
	for _, category := range query.HiddenCategories {
		hidden = append(hidden, history.TranscriptCategory(category))
	}
	request := history.TranscriptReadRequest{
		ThreadID: id, Limit: query.Limit, MaxBytes: maxThreadTranscriptBytes,
		HiddenCategories: hidden, BeforeNativeTurnID: query.BeforeNativeTurn,
		BeforeTurnOrdinal: query.BeforeTurn, TurnLimit: query.TurnLimit,
	}
	// The default Enhanced profile is the message rows themselves, whatever
	// their status, not a category filter: a failed message keeps its error
	// category. Every other profile filters by category.
	if query.View == defaultThreadTranscriptView && slices.Equal(query.HiddenCategories, enhancedHiddenCategories) {
		request.MessagesOnly, request.HiddenCategories = true, nil
	}
	if query.Before != "" {
		cursor, err := decodeThreadTranscriptCursor(query.Before)
		if err != nil || cursor.ThreadID != id || cursor.View != query.View || cursor.Profile != threadTranscriptProfile ||
			cursor.HiddenCategories != strings.Join(query.HiddenCategories, ",") {
			return history.TranscriptReadRequest{}, errInvalidTranscriptCursor
		}
		request.ExpectedProjectionKey = cursor.ProjectionKey
		request.SnapshotHeadSeq = cursor.SnapshotHeadSeq
		request.BeforeSeq = cursor.ExclusiveBeforeSeq
	}
	return request, nil
}

func transcriptResponseFor(id string, query transcriptListQuery, page history.TranscriptPage) (transcriptResponse, error) {
	response := transcriptResponse{
		ThreadID: id, View: query.View, Taxonomy: threadTranscriptTaxonomy,
		HiddenCategories: append([]string{}, query.HiddenCategories...),
		Items:            make([]transcriptItem, 0, len(page.Items)),
		Turns:            make([]transcriptTurn, 0, len(page.Turns)),
		Page: transcriptPageInfo{
			StartSeq: page.StartSeq, EndSeq: page.EndSeq, LatestSeq: page.LatestSeq,
			SnapshotHeadSeq: page.SnapshotHeadSeq, HasMoreBefore: page.HasMoreBefore,
			StartsMidTurn: page.StartsMidTurn,
		},
		Projection: transcriptProjection{
			Key: page.Projection.Key, ParserVersion: page.Projection.ParserVersion,
			SchemaVersion: page.Projection.SchemaVersion, State: string(page.Projection.State),
			SourceStatus:      string(page.Projection.SourceStatus),
			LastIndexedAt:     formatCatalogTime(page.Projection.LastIndexedAt),
			EvidenceAvailable: page.Projection.EvidenceAvailable, ItemCount: page.Projection.ItemCount,
			ErrorSummary: page.Projection.ErrorSummary,
		},
	}
	// A backward cursor is a fixed historical snapshot. Keep volatile append
	// freshness out of that representation so the page and its ETag remain
	// reusable while the same projection grows. Source-health changes still
	// invalidate it through state/status/evidence fields.
	if query.Before != "" {
		response.Page.LatestSeq = page.SnapshotHeadSeq
		response.Projection.ItemCount = page.SnapshotHeadSeq
		response.Projection.LastIndexedAt = nil
	}
	futureFinalItems := make(map[string]bool)
	for _, turn := range page.Turns {
		if turn.CompletionSeq > page.SnapshotHeadSeq && turn.FinalItemID != "" {
			futureFinalItems[turn.FinalItemID] = true
		}
	}
	for _, item := range page.Items {
		response.Items = append(response.Items, transcriptItem{
			ID: item.ID, OwnerItemID: item.SourceKey, ProjectedSeq: item.ProjectedSeq, TurnID: item.TurnKey,
			NativeItemID: item.NativeItemID,
			Type:         string(item.Type), Category: string(item.Category), Status: string(item.Status), Role: string(item.Role),
			Text: item.Text, ToolName: item.ToolName, CallID: item.CallID,
			Detail: append(json.RawMessage(nil), item.Detail...), CreatedAt: formatCatalogTime(item.CreatedAt),
			Phase: item.Phase, FilterTags: append([]string{}, item.FilterTags...),
			Synthetic: item.Synthetic, Sidechain: item.Sidechain,
			IsFinalAnswer: item.IsFinalAnswer && !futureFinalItems[item.ID],
			ContentRef:    item.ContentRef, ContentTruncated: item.ContentTruncated,
			OriginalBytes: item.OriginalBytes,
		})
	}
	for _, turn := range page.Turns {
		endSeq := turn.EndSeq
		if endSeq > page.SnapshotHeadSeq {
			endSeq = page.SnapshotHeadSeq
		}
		status, completionEvidence, finalItemID := string(turn.Status), string(turn.CompletionEvidence), turn.FinalItemID
		finalitySource, finalityConfidence, durationMS := turn.FinalitySource, turn.FinalityConfidence, turn.DurationMS
		completedAt := turn.CompletedAt
		if turn.CompletionSeq > page.SnapshotHeadSeq {
			status, completionEvidence, finalItemID = string(history.TurnInProgress), string(history.CompletionNone), ""
			finalitySource, finalityConfidence, durationMS = "", "", 0
			completedAt = nil
		}
		response.Turns = append(response.Turns, transcriptTurn{
			ID: turn.Key, Ordinal: turn.Ordinal, StartSeq: turn.StartSeq, EndSeq: endSeq,
			Status: status, CompletionEvidence: completionEvidence,
			FinalItemID: finalItemID, StartedAt: formatCatalogTime(turn.StartedAt),
			CompletedAt:    formatCatalogTime(completedAt),
			FinalitySource: finalitySource, FinalityConfidence: finalityConfidence,
			DurationMS: durationMS, NativeTurnID: turn.NativeTurnID,
			Config: append(json.RawMessage(nil), turn.Config...),
		})
	}
	if err := finalizeTranscriptResponse(&response); err != nil {
		return transcriptResponse{}, err
	}
	return response, nil
}

func (s *threadCatalogService) LoadTranscriptBody(ctx context.Context, threadID, itemID string) (history.TranscriptRetainedBody, error) {
	body, err := s.store.LoadTranscriptBody(ctx, threadID, itemID)
	switch {
	case errors.Is(err, history.ErrThreadNotFound):
		return history.TranscriptRetainedBody{}, errCatalogThreadNotFound
	case errors.Is(err, history.ErrInvalidTranscriptPage):
		return history.TranscriptRetainedBody{}, errInvalidTranscriptCursor
	case errors.Is(err, history.ErrTranscriptBodyUnavailable):
		return history.TranscriptRetainedBody{}, history.ErrTranscriptBodyUnavailable
	case err != nil:
		return history.TranscriptRetainedBody{}, err
	default:
		return body, nil
	}
}

func (s *threadCatalogService) LoadTranscriptEvidence(ctx context.Context, threadID, itemID string) (transcriptEvidenceResponse, error) {
	ref, err := s.store.LoadTranscriptEvidenceRef(ctx, threadID, itemID)
	switch {
	case errors.Is(err, history.ErrThreadNotFound):
		return transcriptEvidenceResponse{}, errCatalogThreadNotFound
	case errors.Is(err, history.ErrInvalidTranscriptPage):
		return transcriptEvidenceResponse{}, errInvalidTranscriptCursor
	case errors.Is(err, history.ErrTranscriptEvidenceUnavailable):
		return transcriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	case errors.Is(err, history.ErrTranscriptChanged):
		return transcriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	case err != nil:
		return transcriptEvidenceResponse{}, err
	}
	if ref.SourceStatus != history.SourcePresent || ref.SourceGeneration != ref.CurrentGeneration {
		return transcriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	}
	file, err := os.Open(ref.SourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return transcriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
		}
		return transcriptEvidenceResponse{}, fmt.Errorf("open transcript evidence: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return transcriptEvidenceResponse{}, fmt.Errorf("stat transcript evidence: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (ref.Device > 0 && int64(stat.Dev) != ref.Device) ||
		(ref.Inode > 0 && int64(stat.Ino) != ref.Inode) || info.Size() < ref.CompleteOffset {
		return transcriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	}
	var raw []byte
	if ref.HasByteRange {
		raw, err = readTranscriptEvidenceRange(ctx, file, ref)
	} else {
		raw, err = readTranscriptEvidenceLine(ctx, file, ref.SourceLine)
	}
	if err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return transcriptEvidenceResponse{}, errTranscriptEvidenceLarge
		}
		return transcriptEvidenceResponse{}, fmt.Errorf("read transcript evidence: %w", err)
	}
	if len(raw) == 0 {
		return transcriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	}
	if len(raw) > maxThreadEvidenceBytes {
		return transcriptEvidenceResponse{}, errTranscriptEvidenceLarge
	}
	digest := sha256.Sum256(raw)
	digestText := fmt.Sprintf("%x", digest[:])
	if digestText != ref.SourceLineSHA256 {
		return transcriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	}
	if ref.SourceRowType != "" && json.Valid(raw) {
		var row map[string]json.RawMessage
		var nativeType string
		if json.Unmarshal(raw, &row) != nil || json.Unmarshal(row["type"], &nativeType) != nil || nativeType != ref.SourceRowType {
			return transcriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
		}
	}
	response := transcriptEvidenceResponse{
		ThreadID: ref.ThreadID, ItemID: ref.ItemID, ProjectionKey: ref.ProjectionKey,
		Provider: string(ref.Provider), SourcePath: ref.SourcePath, SourceLine: ref.SourceLine,
		SourceRowType: ref.SourceRowType, SourcePayloadType: ref.SourcePayloadType,
		RawBytes: len(raw), SHA256: digestText, ValidJSON: json.Valid(raw), ValidUTF8: utf8.Valid(raw),
	}
	if response.ValidUTF8 {
		value := string(raw)
		response.Raw = &value
	} else {
		response.RawBase64 = base64.StdEncoding.EncodeToString(raw)
	}
	return response, nil
}

// Ranges include a JSONL terminator, while native row hashes exclude it.
// A document may include trailing whitespace in its hash, so test exact bytes
// first and remove only one CRLF/LF terminator when the digest requires it.
func readTranscriptEvidenceRange(ctx context.Context, reader io.ReaderAt, ref history.TranscriptEvidenceRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref.ByteStart < 0 || ref.ByteLength <= 0 || ref.ByteStart > ref.CompleteOffset || ref.ByteLength > ref.CompleteOffset-ref.ByteStart {
		return nil, errTranscriptEvidenceUnavailable
	}
	if ref.ByteLength > int64(maxThreadEvidenceBytes)+2 {
		return nil, errTranscriptEvidenceLarge
	}
	raw, err := io.ReadAll(io.NewSectionReader(reader, ref.ByteStart, ref.ByteLength))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(raw)) != ref.ByteLength {
		return nil, errTranscriptEvidenceUnavailable
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) == ref.SourceLineSHA256 {
		return raw, nil
	}
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
		if fmt.Sprintf("%x", sha256.Sum256(raw)) == ref.SourceLineSHA256 {
			return raw, nil
		}
	}
	return nil, errTranscriptEvidenceUnavailable
}

func readTranscriptEvidenceLine(ctx context.Context, reader io.Reader, wantedLine int) ([]byte, error) {
	if wantedLine < 1 {
		return nil, errTranscriptEvidenceUnavailable
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxThreadEvidenceBytes+1)
	line := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !scanner.Scan() {
			break
		}
		line++
		if line == wantedLine {
			return append([]byte(nil), scanner.Bytes()...), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errTranscriptEvidenceUnavailable
}

func finalizeTranscriptResponse(response *transcriptResponse) error {
	if response.Items == nil {
		response.Items = []transcriptItem{}
	}
	if response.Turns == nil {
		response.Turns = []transcriptTurn{}
	}
	for {
		if err := setTranscriptBeforeCursor(response); err != nil {
			return err
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		if len(encoded) <= maxThreadTranscriptBytes {
			return nil
		}
		if len(response.Items) <= 1 {
			if len(response.Items) == 1 && response.Items[0].ContentRef != "" &&
				(response.Items[0].Text != "" || len(response.Items[0].Detail) != 0) {
				// The retained-body endpoint owns the full value. Keep the row and
				// its paging identity reachable even when its preview alone would
				// exceed the page budget.
				response.Items[0].Text = ""
				response.Items[0].Detail = nil
				response.Items[0].ContentTruncated = true
				continue
			}
			return errors.New("bounded transcript item exceeds response budget")
		}
		response.Items = response.Items[1:]
		response.Page.StartSeq = response.Items[0].ProjectedSeq
		response.Page.HasMoreBefore = true
		response.Page.StartsMidTurn = true
		response.Turns = transcriptTurnsForItems(response.Turns, response.Items)
	}
}

func setTranscriptBeforeCursor(response *transcriptResponse) error {
	response.Page.NextBeforeCursor = nil
	if !response.Page.HasMoreBefore || len(response.Items) == 0 {
		return nil
	}
	if response.Projection.Key == "" || response.Page.StartSeq <= 0 || response.Page.SnapshotHeadSeq <= 0 {
		return errors.New("transcript page cannot encode its backward cursor")
	}
	encoded, err := encodeThreadTranscriptCursor(threadTranscriptCursor{
		Version: threadTranscriptCursorVersion, ThreadID: response.ThreadID,
		ProjectionKey: response.Projection.Key, View: response.View, Profile: threadTranscriptProfile,
		HiddenCategories: strings.Join(response.HiddenCategories, ","),
		SnapshotHeadSeq:  response.Page.SnapshotHeadSeq, ExclusiveBeforeSeq: response.Page.StartSeq,
	})
	if err != nil {
		return err
	}
	response.Page.NextBeforeCursor = &encoded
	return nil
}

func transcriptTurnsForItems(turns []transcriptTurn, items []transcriptItem) []transcriptTurn {
	used := make(map[string]bool, len(items))
	for _, item := range items {
		if item.TurnID != "" {
			used[item.TurnID] = true
		}
	}
	filtered := make([]transcriptTurn, 0, len(turns))
	for _, turn := range turns {
		if used[turn.ID] {
			filtered = append(filtered, turn)
		}
	}
	return filtered
}

// catalogQueryParams maps the filter portion of an API query onto store
// params: toggles, archived, and time bounds.
func catalogQueryParams(query catalogListQuery) history.ListThreadsParams {
	return history.ListThreadsParams{
		IncludeSubagents: query.IncludeSubagents, IncludeGuardians: query.IncludeGuardians,
		IncludeProgrammatic: query.IncludeProgrammatic,
		Archived:            history.ArchivedFilter(query.Archived),
		CreatedFrom:         query.CreatedFrom, CreatedTo: query.CreatedTo,
		UpdatedFrom: query.UpdatedFrom, UpdatedTo: query.UpdatedTo,
	}
}

func (s *threadCatalogService) IndexStatus(ctx context.Context) (catalogIndexStatus, error) {
	status, err := s.store.IndexStatus(ctx)
	if err != nil {
		return catalogIndexStatus{}, err
	}
	result := catalogIndexStatus{
		State: string(status.State), CatalogVersion: catalogVersion(status.CatalogVersion),
		Roots:                   make([]catalogRootStatus, 0, len(status.Roots)),
		InteractionCounts:       catalogInteractionCounts(status.InteractionCounts),
		TranscriptChangeWorkers: transcriptChangeWorkerCount,
	}
	result.TranscriptChangeBacklog, result.TranscriptChangeInFlight = s.transcriptChangeStatus()
	for _, root := range status.Roots {
		result.Roots = append(result.Roots, catalogRootStatus{
			Provider: string(root.Root.Root.Provider), RootKind: string(root.Root.Root.Kind),
			State: string(root.State), SourceCount: root.SourceCount,
			LastCompletedAt: formatCatalogTime(root.LastCompletedAt), Error: root.Error,
		})
	}
	if namesError := s.namesErrorSnapshot(); namesError != nil {
		result.State = string(history.IndexDegraded)
		result.NamesError = namesError
	}
	return result, nil
}

// publishCatalogVersion is the single point where a service-triggered version
// bump reaches SSE subscribers. Store writes are serialized on the scan
// goroutine, so a simple monotonic guard suppresses the redundant re-emits that
// unchanged reconciliations would otherwise produce.
func (s *threadCatalogService) publishCatalogVersion(version uint64) {
	s.publishCatalogChange(catalogChange{Version: version, Refresh: true})
}

func (s *threadCatalogService) publishCatalogThreads(version uint64, threadIDs []string) {
	s.publishCatalogChange(catalogChange{Version: version, ThreadIDs: threadIDs})
}

func (s *threadCatalogService) publishCatalogChange(change catalogChange) {
	version := change.Version
	if version == 0 {
		return
	}
	s.publishMu.Lock()
	// A scan publishes its version before it commits the transcripts it
	// changed; each commit then names its thread again at that same version.
	// That second event must go out: a reader the first one sent read the old
	// rows, and nothing else tells it to read again.
	if version < s.publishedVersion || version == s.publishedVersion && len(change.ThreadIDs) == 0 {
		s.publishMu.Unlock()
		return
	}
	s.publishedVersion = version
	s.publishMu.Unlock()
	s.hub.Notify(change)
}

// SubscribeCatalog registers an SSE subscriber and returns its version channel
// plus the unsubscribe to run on disconnect.
func (s *threadCatalogService) SubscribeCatalog() (<-chan catalogChange, func()) {
	ch := s.hub.Subscribe()
	return ch, func() { s.hub.Unsubscribe(ch) }
}

// CatalogVersion reports the current catalog version for the SSE
// version-on-connect event, clamped to the API's signed representation.
func (s *threadCatalogService) CatalogVersion(ctx context.Context) (int64, error) {
	version, err := s.store.CatalogVersion(ctx)
	if err != nil {
		return 0, err
	}
	return catalogVersion(version), nil
}

func catalogVersion(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

// RenameThread writes the user-assigned name for a nano thread id straight to
// the truth file — never the store — then requests a names refresh. The
// serialized catalog worker picks up the changed file signature, syncs the
// derived table, bumps catalog_version, and the SSE hub pushes it, without
// walking a provider root;
// this reuses the whole existing chain and keeps the version-read-after-sync
// safety argument intact. Any rename claims the record as provenance "manual".
// AssignThreadName writes a user-assigned name directly by the names store's
// own key (provider, native thread id). This is the spawn-time path: the New
// Window dialog names a session before any catalog row exists, so resolving a
// catalog id — RenameThread's first step — has nothing to resolve yet. Same
// truth file, same manual provenance, same refresh chain.
func (s *threadCatalogService) AssignThreadName(provider, nativeID, name string) error {
	validated, err := history.ValidateThreadName(name)
	if err != nil {
		return threadNameInvalidError{reason: err.Error()}
	}
	if provider == "" || nativeID == "" {
		return fmt.Errorf("assign thread name: provider and native id required")
	}
	changed, err := s.mutateThreadNames(history.ThreadNameMutation{
		Provider: history.Provider(provider), NativeThreadID: nativeID,
		Name: validated, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if changed {
		s.requestNames()
	}
	return nil
}

// AssignThreadNameIfEmpty is the pre-bind pending-rename path. The names-file
// lock makes the absence check and write one operation, so a concurrent or
// earlier operator rename always wins. Immutable creation names use the
// separate derived projection and never enter this truth file.
func (s *threadCatalogService) AssignThreadNameIfEmpty(provider, nativeID, name string) error {
	validated, err := history.ValidateThreadName(name)
	if err != nil {
		return threadNameInvalidError{reason: err.Error()}
	}
	if provider == "" || nativeID == "" {
		return fmt.Errorf("assign thread name: provider and native id required")
	}
	changed, err := s.mutateThreadNames(history.ThreadNameMutation{
		Provider: history.Provider(provider), NativeThreadID: nativeID,
		Name: validated, UpdatedAt: time.Now().UTC(), OnlyIfAbsent: true,
	})
	if err != nil {
		return err
	}
	if changed {
		s.requestNames()
	}
	return nil
}

// PublishCreationNames ensures the derived names table projects canonical
// creation facts, refreshing it synchronously when it does not yet. The
// supplied records are verification targets,
// not a second source: manual and imported truth may legitimately win, while a
// creation projection must match the canonical value exactly.
func (s *threadCatalogService) PublishCreationNames(ctx context.Context, expected []creationNameProjectionRecord) error {
	for _, record := range expected {
		if record.Provider == "" || record.NativeThreadID == "" {
			return errors.New("publish creation name: provider and native id required")
		}
		if _, err := history.ValidateThreadName(record.Name); err != nil {
			return fmt.Errorf("publish creation name: %w", err)
		}
	}
	// Every resume of a named thread publishes a name the previous run already
	// projected. A full refresh for each, in series under the one names lock,
	// held boot's Claude handshake slots, so an unchanged truth file whose
	// projection already holds needs none.
	if s.namesTruthUnchanged() && s.creationNamesProjected(ctx, expected) == nil {
		return nil
	}
	if err := s.refreshThreadNames(ctx); err != nil {
		return err
	}
	return s.creationNamesProjected(ctx, expected)
}

// namesTruthUnchanged reports whether the truth file is the valid one the last
// refresh already synced.
func (s *threadCatalogService) namesTruthUnchanged() bool {
	if s.namesPath == "" {
		return false
	}
	signature, err := statThreadNames(s.namesPath)
	if err != nil {
		return false
	}
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	return s.namesProcessed && s.namesFileValid && signature == s.namesSig
}

// creationNamesProjected checks that each expected creation name is the
// projected name, or that a manual or imported name outranks it.
func (s *threadCatalogService) creationNamesProjected(ctx context.Context, expected []creationNameProjectionRecord) error {
	for _, record := range expected {
		ref, ok := s.ThreadNameRefFor(ctx, record.Provider, record.NativeThreadID)
		if !ok || ref == nil {
			return fmt.Errorf("publish creation name: %s/%s was not projected", record.Provider, record.NativeThreadID)
		}
		switch ref.Provenance {
		case history.ThreadNameManual, history.ThreadNameHistoricalImport:
			continue
		case history.ThreadNameCreation:
			if ref.Value != record.Name {
				return fmt.Errorf("publish creation name: %s/%s projected %q, want %q", record.Provider, record.NativeThreadID, ref.Value, record.Name)
			}
		default:
			return fmt.Errorf("publish creation name: %s/%s has unknown provenance %q", record.Provider, record.NativeThreadID, ref.Provenance)
		}
	}
	return nil
}

// ThreadNameFor serves the durable name for one (provider, nativeID) — the
// live-session read half of the names surface (posture item 3): boot and
// click-resume hydrate the summary's display name from it. A read failure
// reads as unnamed; the catalog overlay remains the durable presentation.
func (s *threadCatalogService) ThreadNameFor(ctx context.Context, provider, nativeThreadID string) (string, bool) {
	ref, ok := s.ThreadNameRefFor(ctx, provider, nativeThreadID)
	if !ok {
		return "", false
	}
	return ref.Value, true
}

// ThreadNameRefFor keeps hosted metadata's read-time join faithful to the
// merged name sources: both value and provenance come from the catalog's
// derived name row.
func (s *threadCatalogService) ThreadNameRefFor(ctx context.Context, provider, nativeThreadID string) (*history.ThreadNameRef, bool) {
	ref, err := s.store.ThreadNameRefFor(ctx, history.Provider(provider), nativeThreadID)
	if err != nil {
		log.Printf("read thread name %s/%s: %v", provider, nativeThreadID, err)
		return nil, false
	}
	return ref, ref != nil
}

// NamedThreads returns catalog rows whose current user-assigned name equals
// name exactly. A name record with no indexed catalog row is still returned
// so identity resolve can follow native-id history after a Claude respawn.
func (s *threadCatalogService) NamedThreads(ctx context.Context, name string) ([]catalogThread, error) {
	records, err := s.store.ThreadNamesNamed(ctx, name)
	if err != nil {
		return nil, err
	}
	rows := make([]catalogThread, 0, len(records))
	for _, record := range records {
		page, err := s.ListThreads(ctx, catalogListQuery{
			ThreadID: record.NativeThreadID, IncludeSubagents: true, IncludeGuardians: true,
			IncludeProgrammatic: true, Archived: archivedIncludeFilter, Limit: maxThreadCatalogLimit,
		})
		if err != nil {
			return nil, err
		}
		found := false
		for _, item := range page.Items {
			if item.Provider == string(record.Provider) && item.NativeID == record.NativeThreadID {
				rows = append(rows, item)
				found = true
			}
		}
		if found {
			continue
		}
		nameRef := catalogThreadName{
			Value: record.Name, Provenance: string(record.Provenance), Evidence: threadNameEvidence,
		}
		row := catalogThread{
			Provider: string(record.Provider), NativeID: record.NativeThreadID, Name: &nameRef,
		}
		if record.UpdatedAt != nil {
			stamp := record.UpdatedAt.UTC().Format(time.RFC3339Nano)
			row.UpdatedAt = &stamp
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *threadCatalogService) RenameThread(ctx context.Context, id, name string) (catalogThreadName, error) {
	validated, err := history.ValidateThreadName(name)
	if err != nil {
		return catalogThreadName{}, threadNameInvalidError{reason: err.Error()}
	}
	provider, nativeID, err := s.resolveThreadKey(ctx, id)
	if err != nil {
		return catalogThreadName{}, err
	}
	changed, err := s.mutateThreadNames(history.ThreadNameMutation{
		Provider: provider, NativeThreadID: nativeID,
		Name: validated, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		return catalogThreadName{}, err
	}
	if changed {
		s.requestNames()
	}
	return catalogThreadName{
		Value: validated, Provenance: string(history.ThreadNameManual), Evidence: threadNameEvidence,
	}, nil
}

// ClearThreadName removes the user-assigned name for a nano thread id from the
// truth file (regardless of provenance), then requests a names refresh through
// the same worker → sync → version-bump → SSE chain as RenameThread.
func (s *threadCatalogService) ClearThreadName(ctx context.Context, id string) error {
	provider, nativeID, err := s.resolveThreadKey(ctx, id)
	if err != nil {
		return err
	}
	changed, err := s.mutateThreadNames(history.ThreadNameMutation{
		Provider: provider, NativeThreadID: nativeID, Clear: true,
	})
	if err != nil {
		return err
	}
	if changed {
		s.requestNames()
	}
	return nil
}

// resolveThreadKey maps a nano thread id to its durable (provider, native id)
// through the catalog, which is the truth-file key. An unknown id is a 404.
func (s *threadCatalogService) resolveThreadKey(ctx context.Context, id string) (history.Provider, string, error) {
	detail, err := s.store.GetThread(ctx, id)
	if errors.Is(err, history.ErrThreadNotFound) {
		return "", "", errCatalogThreadNotFound
	}
	if err != nil {
		return "", "", err
	}
	return detail.Thread.Provider, detail.Thread.NativeID, nil
}

// mutateThreadNames serializes the read-modify-write of the whole truth-file
// snapshot under namesWriteMu: it reloads the file fresh (so external hand-edits
// are picked up), refuses to overwrite a malformed file, applies the single
// mutation, and writes atomically. It never touches the store.
func (s *threadCatalogService) mutateThreadNames(mutation history.ThreadNameMutation) (bool, error) {
	if s.namesPath == "" {
		return false, errNamesFileDisabled
	}
	s.namesWriteMu.Lock()
	defer s.namesWriteMu.Unlock()
	existing, err := os.ReadFile(s.namesPath)
	if errors.Is(err, os.ErrNotExist) {
		existing = nil
	} else if err != nil {
		return false, fmt.Errorf("read thread names file: %w", err)
	}
	result, changed, err := history.ApplyThreadNameSnapshot(existing, mutation)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := writeThreadNamesAtomic(s.namesPath, result); err != nil {
		return false, err
	}
	return true, nil
}

// writeThreadNamesAtomic writes data to a temp file in the same directory and
// renames it over the target, so a reader never sees a half-written snapshot.
func writeThreadNamesAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".thread-names.*.tmp")
	if err != nil {
		return fmt.Errorf("create thread names temp file: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write thread names temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close thread names temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace thread names file: %w", err)
	}
	committed = true
	return nil
}

func mapCatalogThread(thread history.Thread, sources *[]catalogSourceDetail) catalogThread {
	var name *catalogThreadName
	if thread.Name != nil {
		name = &catalogThreadName{
			Value: thread.Name.Value, Provenance: string(thread.Name.Provenance),
			Evidence: threadNameEvidence,
		}
	}
	return catalogThread{
		ID: thread.ID, Provider: string(thread.Provider), NativeID: thread.NativeID,
		Kind: string(thread.Kind), ParentNativeID: thread.ParentNativeID,
		ProviderVersion: thread.ProviderVersion,
		Name:            name, Title: thread.Title, Preview: thread.Preview,
		CreatedAt: formatCatalogTime(thread.CreatedAt), UpdatedAt: formatCatalogTime(thread.UpdatedAt),
		Archived: thread.Archived, Metadata: mapCatalogMetadata(thread.Metadata),
		Interaction: string(thread.Interaction),
		Source: catalogSourceSummary{
			Status: string(thread.Source.Status), Conflict: thread.Source.Conflict,
			DiagnosticCount: thread.Source.DiagnosticCount,
			LastIndexedAt:   formatCatalogTimeValue(thread.Source.LastIndexedAt),
		},
		Sources: sources,
	}
}

func mapCatalogMetadata(metadata history.Metadata) catalogMetadata {
	return catalogMetadata{
		CWD: mapCatalogValue(metadata.CWD), Model: mapCatalogValue(metadata.Model),
		Effort: mapCatalogValue(metadata.Effort), Priority: mapCatalogValue(metadata.Priority),
		Permission:       mapCatalogValue(metadata.Permission),
		HistoricalBranch: mapCatalogValue(metadata.HistoricalBranch),
		Origin:           mapCatalogValue(metadata.Origin),
	}
}

func mapCatalogValue(field history.MetadataField) catalogValue {
	var evidence *string
	if field.Evidence != nil {
		value := string(*field.Evidence)
		evidence = &value
	}
	return catalogValue{State: string(field.State), Value: field.Value, Evidence: evidence}
}

func formatCatalogTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func formatCatalogTimeValue(value *time.Time) string {
	formatted := formatCatalogTime(value)
	if formatted == nil {
		return ""
	}
	return *formatted
}

// The opaque cursor carries the updated_at of the page boundary (the list
// order is fixed updated-desc).
type threadCatalogCursor struct {
	V  *string `json:"v"`
	ID string  `json:"id"`
}

func encodeThreadCatalogCursor(cursor *history.ThreadCursor) (string, error) {
	value := threadCatalogCursor{ID: cursor.ID, V: formatCatalogTime(cursor.UpdatedAt)}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeThreadCatalogCursor(encoded string) (*history.ThreadCursor, error) {
	if len(encoded) == 0 || len(encoded) > maxThreadCatalogCursorBytes {
		return nil, errInvalidThreadCatalogCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errInvalidThreadCatalogCursor
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var value threadCatalogCursor
	if err := decoder.Decode(&value); err != nil || strings.TrimSpace(value.ID) == "" {
		return nil, errInvalidThreadCatalogCursor
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errInvalidThreadCatalogCursor
	}
	cursor := &history.ThreadCursor{ID: value.ID}
	if value.V != nil {
		parsed, err := time.Parse(time.RFC3339Nano, *value.V)
		if err != nil {
			return nil, errInvalidThreadCatalogCursor
		}
		cursor.UpdatedAt = &parsed
	}
	return cursor, nil
}

type threadTranscriptCursor struct {
	Version            int    `json:"v"`
	ThreadID           string `json:"threadId"`
	ProjectionKey      string `json:"projectionKey"`
	View               string `json:"view"`
	Profile            string `json:"profile"`
	HiddenCategories   string `json:"hiddenCategories"`
	SnapshotHeadSeq    int64  `json:"snapshotHeadSeq"`
	ExclusiveBeforeSeq int64  `json:"beforeSeq"`
}

func encodeThreadTranscriptCursor(cursor threadTranscriptCursor) (string, error) {
	if err := validateThreadTranscriptCursor(cursor); err != nil {
		return "", err
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > maxThreadTranscriptCursorBytes {
		return "", errInvalidTranscriptCursor
	}
	return encoded, nil
}

func decodeThreadTranscriptCursor(encoded string) (threadTranscriptCursor, error) {
	if len(encoded) == 0 || len(encoded) > maxThreadTranscriptCursorBytes {
		return threadTranscriptCursor{}, errInvalidTranscriptCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return threadTranscriptCursor{}, errInvalidTranscriptCursor
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var cursor threadTranscriptCursor
	if err := decoder.Decode(&cursor); err != nil {
		return threadTranscriptCursor{}, errInvalidTranscriptCursor
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return threadTranscriptCursor{}, errInvalidTranscriptCursor
	}
	if err := validateThreadTranscriptCursor(cursor); err != nil {
		return threadTranscriptCursor{}, err
	}
	return cursor, nil
}

func validateThreadTranscriptCursor(cursor threadTranscriptCursor) error {
	if cursor.Version != threadTranscriptCursorVersion || strings.TrimSpace(cursor.ThreadID) == "" ||
		strings.TrimSpace(cursor.ProjectionKey) == "" || (cursor.View != defaultThreadTranscriptView && cursor.View != devThreadTranscriptView) ||
		cursor.Profile != threadTranscriptProfile || cursor.SnapshotHeadSeq <= 0 ||
		cursor.ExclusiveBeforeSeq <= 0 || cursor.ExclusiveBeforeSeq > cursor.SnapshotHeadSeq+1 {
		return errInvalidTranscriptCursor
	}
	var hidden []string
	if cursor.HiddenCategories != "" {
		hidden = strings.Split(cursor.HiddenCategories, ",")
	}
	canonical, err := canonicalTranscriptCategories(hidden)
	if err != nil || strings.Join(canonical, ",") != cursor.HiddenCategories {
		return errInvalidTranscriptCursor
	}
	return nil
}

// FleetCourt answers the fleet classifier's read: the message that decides
// whose court each thread's ball is in (fleet_status.go). Computed per request
// and never stored — fleet state is a read-time projection.
func (s *threadCatalogService) FleetCourt(ctx context.Context, threadIDs []string) (map[string]fleetCourtMessage, error) {
	records, err := s.store.FleetCourtMessages(ctx, threadIDs)
	if err != nil {
		return nil, err
	}
	courts := make(map[string]fleetCourtMessage, len(records))
	for id, record := range records {
		court := fleetCourtMessage{
			ItemType: string(record.ItemType), Head: record.Head, Tail: record.Tail,
			Interrupted: record.Interrupted,
		}
		if record.LastActivity != nil {
			court.LastActivity = *record.LastActivity
		}
		courts[id] = court
	}
	return courts, nil
}

// ResolveThreadReference turns an agent-supplied reference — an exact nano or
// native id, or an unambiguous prefix of either — into the durable nano id.
// Ambiguity is an error carrying the candidates, never a silent pick.
func (s *threadCatalogService) ResolveThreadReference(ctx context.Context, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", errCatalogThreadNotFound
	}
	matches, err := s.store.ResolveThreadReferences(ctx, reference, maxThreadReferenceCandidates)
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", errCatalogThreadNotFound
	case 1:
		return matches[0].ID, nil
	}
	candidates := make([]string, 0, len(matches))
	for _, match := range matches {
		candidates = append(candidates, match.ID)
	}
	return "", &ambiguousThreadReferenceError{Candidates: candidates}
}

var _ threadCatalog = (*threadCatalogService)(nil)
var _ threadCatalogNamer = (*threadCatalogService)(nil)
var _ threadTranscriptCatalog = (*threadCatalogService)(nil)
var _ transcriptAuditCatalog = (*threadCatalogService)(nil)
var _ threadTranscriptEvidenceCatalog = (*threadCatalogService)(nil)
var _ catalog.FleetCourtSource = (*threadCatalogService)(nil)
var _ threadReferenceResolver = (*threadCatalogService)(nil)
