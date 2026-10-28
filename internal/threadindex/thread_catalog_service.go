package threadindex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// TranscriptChange identifies a saved transcript whose committed rows changed.
type TranscriptChange struct {
	ThreadID       string
	Provider       string
	NativeThreadID string
}

type pendingTranscriptChange struct {
	change     TranscriptChange
	generation uint64
	queued     bool
	running    bool
	// since is the first notification the next callback covers; again is
	// the first one that arrived while a callback ran.
	since time.Time
	again time.Time
}

var (
	ErrInvalidThreadCatalogCursor = errors.New("invalid thread catalog cursor")
	// errInvalidThreadName marks a rename rejected by the loader's name rules
	// (trimmed, non-empty, ≤200 runes). It maps to a 400 invalid_name response.
	errInvalidThreadName = errors.New("invalid thread name")
	// errNamesFileDisabled marks a rename attempted while the names overlay is
	// off (no truth-file path configured). It maps to a 503 response.
	errNamesFileDisabled = errors.New("thread names file is not configured")
)

// NameInvalidError carries a user-facing validation message while still
// matching errInvalidThreadName through errors.Is, so the HTTP layer can surface
// the exact reason ("name exceeds 200 runes") without a second validation pass.
type NameInvalidError struct{ Reason string }

// Error reports the domain error’s user-facing message.
func (e NameInvalidError) Error() string { return e.Reason }

// Is matches the underlying name-validation error class.
func (e NameInvalidError) Is(target error) bool { return target == errInvalidThreadName }

// Options keeps provider-root resolution injectable
// without putting filesystem work on an HTTP request path. An empty Roots
// slice resolves the standard provider roots.
type Options struct {
	NativeIDProvisional func(string, string) bool
	ClaudeProjectSlug   func(string) string
	Roots               []history.SourceRoot
	RootOptions         history.RootOptions
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

// Service owns catalog ingestion, derived queries and catalog change publication.
type Service struct {
	nativeIDProvisional func(string, string) bool
	claudeProjectSlug   func(string) string
	store               *history.Store
	roots               []history.PersistedRoot
	catalogReady        chan struct{}
	namesPath           string
	cancel              context.CancelFunc
	done                chan struct{}
	logf                func(string, ...any)

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
	transcriptChanged        func(context.Context, TranscriptChange)
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

// SetTranscriptChanged connects durable-index commits to the live projection
// without making the catalog package know about session ownership. The setter
// is synchronized because the scanner starts during construction.
func (s *Service) SetTranscriptChanged(callback func(context.Context, TranscriptChange)) {
	s.transcriptChangedMu.Lock()
	s.transcriptChanged = callback
	s.transcriptChangedCond.Broadcast()
	s.transcriptChangedMu.Unlock()
}

// SetTranscriptRepairPriority sets the application priority for parser repair candidates.
func (s *Service) SetTranscriptRepairPriority(callback func(history.TranscriptParserBackfillCandidate) int) {
	s.repairPriorityMu.Lock()
	s.repairPriority = callback
	s.repairPriorityMu.Unlock()
}

func (s *Service) transcriptRepairPriority(candidate history.TranscriptParserBackfillCandidate) int {
	s.repairPriorityMu.RLock()
	callback := s.repairPriority
	s.repairPriorityMu.RUnlock()
	if callback == nil {
		return 2
	}
	return callback(candidate)
}

// SetCreationNameSource supplies canonical creation names for the derived name projection.
func (s *Service) SetCreationNameSource(source func(context.Context) ([]history.ThreadNameRecord, error)) {
	s.namesMu.Lock()
	s.creationNameSource = source
	s.namesMu.Unlock()
	s.requestNames()
}

func (s *Service) notifyTranscriptChanged(change TranscriptChange) {
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

func (s *Service) runTranscriptChangeWorker(ctx context.Context) {
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

func (s *Service) transcriptChangeStatus() (backlog, inFlight int) {
	s.transcriptChangedMu.Lock()
	defer s.transcriptChangedMu.Unlock()
	return len(s.transcriptChanges), s.transcriptChangeInFlight
}

// New initializes the durable roots and starts one
// serialized worker. It walks no provider root: attached threads are ingested
// as their own files change, and full discovery runs only on an explicit
// refresh. Provider-owned files are opened read-only and never mutated.
func New(ctx context.Context, store *history.Store, options Options) (*Service, error) {
	if options.NativeIDProvisional == nil || options.ClaudeProjectSlug == nil {
		return nil, errors.New("catalog provider identity and path functions are required")
	}
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
	service := &Service{
		nativeIDProvisional: options.NativeIDProvisional, claudeProjectSlug: options.ClaudeProjectSlug,
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
	} else if service.files, service.filesErr = catalog.StartFileWatcher(service.FileChanged, service.logf); service.filesErr != nil {
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

func (s *Service) publishCatalogThreads(version uint64, threadIDs []string) {
	s.publishCatalogChange(catalogChange{Version: version, ThreadIDs: threadIDs})
}

func (s *Service) publishCatalogChange(change catalogChange) {
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
func (s *Service) SubscribeCatalog() (<-chan catalogChange, func()) {
	ch := s.hub.Subscribe()
	return ch, func() { s.hub.Unsubscribe(ch) }
}

// CatalogVersion reports the current catalog version for the SSE
// version-on-connect event, clamped to the API's signed representation.
func (s *Service) CatalogVersion(ctx context.Context) (int64, error) {
	version, err := s.store.CatalogVersion(ctx)
	if err != nil {
		return 0, err
	}
	return CatalogVersion(version), nil
}
