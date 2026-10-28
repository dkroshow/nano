package threadindex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

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
func (s *Service) run(ctx context.Context) {
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
func (s *Service) initialize(ctx context.Context) {
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

func (s *Service) servesRoot(id int64) bool {
	for _, root := range s.roots {
		if root.ID == id {
			return true
		}
	}
	return false
}

func (s *Service) takeDirtyLocked(limit int) []dirtyTake {
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
func (s *Service) finishPass(batch []dirtyTake) {
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

func (s *Service) signalWork() {
	select {
	case s.work <- struct{}{}:
	default:
	}
}

func (s *Service) logScanCycle(delay, duration time.Duration, stats []catalogRootScanStats) {
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
func (s *Service) Close() {
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
func (s *Service) RequestRefresh() bool {
	s.queueMu.Lock()
	if s.recoveryAt.IsZero() {
		s.recoveryAt = time.Now()
	}
	s.queueMu.Unlock()
	s.signalWork()
	return true
}

// requestPathRefresh queues known sources for targeted passes. A path that is
// not a provider file under a configured root is a cheap no-op.
func (s *Service) requestPathRefresh(paths []string) { s.requestPaths(paths, false) }

// requestPaths queues sources, in the background queue when background. A
// foreground request moves a waiting background source forward; a file event
// never does.
func (s *Service) requestPaths(paths []string, background bool) {
	now := time.Now()
	for _, path := range paths {
		if path = filepath.Clean(path); path != "." && path != "" {
			s.enqueue(path, now, background, !background)
		}
	}
}

// FileChanged queues a provider file hint without promoting background work.
func (s *Service) FileChanged(path string) { s.enqueue(path, time.Now(), false, false) }

func (s *Service) enqueue(path string, at time.Time, background, promote bool) {
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

func (s *Service) queueLocked(path string, background bool) {
	if background {
		s.dirtyBackground = append(s.dirtyBackground, path)
	} else {
		s.dirtyOrder = append(s.dirtyOrder, path)
	}
}

// requestNames queues the names-only reconciliation on the worker, which
// publishes the change without walking a provider root.
func (s *Service) requestNames() {
	s.queueMu.Lock()
	s.namesWanted = true
	s.queueMu.Unlock()
	s.signalWork()
}

// ingestBatch runs one targeted pass over the sources it took, root by root,
// and records each source's outcome. A root-wide failure fails each of its
// sources that has no outcome of its own.
func (s *Service) ingestBatch(ctx context.Context, batch []dirtyTake, seq uint64) {
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
func (s *Service) refreshThreadNames(ctx context.Context) error {
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

// refreshRoot walks one root (recovery) or inspects the given paths in it
// (a targeted pass, which records each path's outcome in pass). A targeted
// pass writes no root index state: it proves nothing about the root, and a
// state write per pass would make every open Explorer poll while threads work.
func (s *Service) refreshRoot(ctx context.Context, root history.PersistedRoot, mode catalogScanMode, paths map[string]struct{}, pass *ingestPass) (catalogRootScanStats, error) {
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

func (s *Service) loadTranscriptRepairState(ctx context.Context, rootID int64, mode catalogScanMode) (transcriptRepairState, error) {
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
func (s *Service) repairTranscripts(ctx context.Context, rootID int64, mode catalogScanMode, repairs []transcriptRepairCandidate, indexedAt time.Time, pass *ingestPass) (int, []string) {
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
func (s *Service) adoptSourceIdentity(ctx context.Context, stored history.StoredSource, candidate history.SourceCandidate) bool {
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
