package app

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/setup"
)

// Attached threads are the catalog's watch set, with the outside sessions
// found from file events (catalog_outside.go): every native file a live
// attachment owns is watched, visible or not, and only held files are read
// automatically. An attachment holds its files from native-identity binding
// until close, detach or failed spawn; ownership is keyed by attachment
// generation, so a late event from an old attachment cannot re-establish it.

// attachedSourceCatalog is the catalog side of an attachment's file holds.
// An owner is a Nano session id: each session is exactly one attachment
// generation, and a re-attach is a new session.
type attachedSourceCatalog interface {
	// bindAttachedSource holds the owner's native files. With background,
	// their catch-up pass waits behind foreground sources, for a bounded time.
	bindAttachedSource(owner, provider, nativeID, cwd, known string, background bool)
	releaseAttachedSource(owner string)
	// awaitAttachedCurrent waits until the owner's saved files are proven
	// current, and reports false when the owner is released or cancel closes
	// first. With fresh, proof taken before the call does not count.
	// neverPrompted, when set, reads the caller's evidence that its Grok
	// session was never prompted.
	awaitAttachedCurrent(owner string, fresh bool, neverPrompted func() bool, cancel <-chan struct{}) bool
}

// attachedOwner is one attachment's hold. An unresolved attachment holds no
// file and keeps the reason, so monitoring still counts it. changed closes
// when the hold is replaced or released.
type attachedOwner struct {
	paths      []string
	unresolved string
	catchUp    *attachedCatchUp
	changed    chan struct{}
}

// attachedCatchUp is a hold's proof that its files are current: targeted
// passes after the hold saved or verified every held file, or, for an
// unchanged saved transcript, the hold's own check after its watch exists
// (provenUnchanged). A pass that started before the hold does not count.
type attachedCatchUp struct {
	done    chan struct{}
	after   uint64
	pending map[string]bool // not yet proven
	err     error
}

type catchUpResult int

const (
	catchUpReady  catchUpResult = iota // the held files are proven current
	catchUpFailed                      // the hold was released first
)

func (c *attachedCatchUp) result() catchUpResult {
	if c.err != nil {
		return catchUpFailed
	}
	return catchUpReady
}

func (c *attachedCatchUp) finish(path string, outcome *ingestOutcome) {
	// A file is proven only when it exists and, if it is a canonical
	// transcript, a pass saved it or proved its committed checkpoint current.
	// Anything less leaves it pending, its waiter with it: the held-file
	// retry, the pass its watch queues once it is written, or the pass queued
	// once a deferred reread finishes, brings the next.
	if !c.pending[path] || outcome.err != nil || !outcome.found || outcome.canonical && !outcome.committed {
		return
	}
	delete(c.pending, path)
	if len(c.pending) == 0 {
		close(c.done)
	}
}

func (c *attachedCatchUp) abandon() {
	if len(c.pending) == 0 {
		return
	}
	c.pending = nil
	if c.err == nil {
		c.err = errors.New("attachment released before its catch-up finished")
	}
	close(c.done)
}

// heldSource is the catalog's transient state for one watched file. It exists
// only while an attachment or an outside hold holds the file.
type heldSource struct {
	refs     int
	failures int
	retry    *time.Timer
	waiters  []*attachedCatchUp
	// The last pass's observation: file size and committed offsets. Unread
	// bytes are measured against this size, never a fresh stat.
	observed       bool
	committed      bool
	size           int64
	readOffset     int64
	completeOffset int64
	progressAt     time.Time
	receipt        *ingestReceipt
}

const (
	attachedRetryFirst = time.Second
	attachedRetryMax   = 5 * time.Minute
)

// bindAttachedSource resolves an attachment's native files and holds them.
// An unresolved identity holds nothing; it is reported as an attachment
// without held files, never answered with a root walk.
func (s *threadCatalogService) bindAttachedSource(owner, provider, nativeID, cwd, known string, background bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	paths, unresolved := s.attachedSourcePaths(ctx, provider, nativeID, cwd, known)
	if len(paths) == 0 {
		s.attachedMu.Lock()
		s.releaseOwnerLocked(owner)
		s.owners[owner] = &attachedOwner{unresolved: unresolved, changed: make(chan struct{})}
		s.attachedMu.Unlock()
		return
	}
	s.hold(owner, paths, background)
}

// holdAttachedSource makes paths the owner's watched files, replacing any it
// held before, and queues their catch-up. Each file is watched before it is
// read, so a write after the catch-up's read is always a new event.
func (s *threadCatalogService) holdAttachedSource(owner string, paths []string) {
	s.hold(owner, paths, false)
}

// hold is holdAttachedSource whose catch-up pass, with background, waits
// behind foreground sources for at most catalogBackgroundMaxWait.
func (s *threadCatalogService) hold(owner string, paths []string, background bool) {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	s.attachedMu.Lock()
	if current := s.owners[owner]; current != nil && len(paths) > 0 && equalStrings(current.paths, paths) {
		s.attachedMu.Unlock()
		return
	}
	s.releaseOwnerLocked(owner)
	if len(paths) == 0 {
		s.attachedMu.Unlock()
		return
	}
	for _, path := range paths {
		s.holdLocked(path)
	}
	// An attachment takes over a file outside discovery held, as for a
	// hosted thread written before its re-attach bound it after a restart.
	for _, path := range paths {
		if outside := s.outside[path]; outside != nil {
			for _, held := range outside.paths {
				s.unholdLocked(held)
			}
			delete(s.outside, path)
		}
	}
	catchUp := s.newCatchUpLocked(paths)
	s.owners[owner] = &attachedOwner{paths: paths, catchUp: catchUp, changed: make(chan struct{})}
	s.attachedMu.Unlock()
	if proven := s.provenUnchanged(paths); len(proven) > 0 {
		s.attachedMu.Lock()
		if current := s.owners[owner]; current != nil && current.catchUp == catchUp {
			for _, path := range proven {
				catchUp.finish(path, &ingestOutcome{found: true, canonical: true, committed: true})
			}
		}
		s.attachedMu.Unlock()
	}
	s.requestPaths(paths, background)
}

// provenUnchanged returns the held transcripts whose saved state already
// matches the file, by the rule a targeted pass applies (sourceUnchanged), read
// after their watches exist. Boot holds hundreds of files that one worker
// passes two at a time; an unchanged one needs no pass to be proven current,
// so readiness waits only on files that changed. The queued pass still runs.
func (s *threadCatalogService) provenUnchanged(paths []string) []string {
	if s.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var proven []string
	for _, path := range paths {
		root, ok := s.rootFor(path)
		if !ok || root.Root.Provider == history.ProviderGrok {
			continue // a Grok session's summary and updates move together
		}
		candidate, found, err := history.DiscoverFile(ctx, root.Root, path)
		if err != nil || !found {
			continue
		}
		stored, exists, err := s.store.LoadSource(ctx, root.ID, candidate.Path)
		if err != nil || !exists || !stored.CanonicalTranscriptSource || stored.ThreadID == "" {
			continue
		}
		header, hasCheckpoint, err := s.store.LoadTranscriptCheckpointHeader(ctx, stored.ThreadID)
		if err != nil || !hasCheckpoint || header.ErrorSummary != nil {
			continue
		}
		if sourceUnchanged(stored, candidate, parserVersion(root.Root.Provider), header, hasCheckpoint) {
			proven = append(proven, path)
		}
	}
	return proven
}

// newCatchUpLocked waits on the next pass over each held path, or the hold's
// own unchanged check (provenUnchanged). Only a pass taken after every watch
// is registered counts as catch-up.
func (s *threadCatalogService) newCatchUpLocked(paths []string) *attachedCatchUp {
	catchUp := &attachedCatchUp{done: make(chan struct{}), pending: map[string]bool{}}
	for _, path := range paths {
		s.held[path].waiters = append(s.held[path].waiters, catchUp)
		catchUp.pending[path] = true
	}
	s.queueMu.Lock()
	catchUp.after = s.passSeq
	s.queueMu.Unlock()
	return catchUp
}

// releaseAttachedSource ends the owner's hold. Files no other attachment
// holds stop being watched and lose their transient state.
func (s *threadCatalogService) releaseAttachedSource(owner string) {
	s.attachedMu.Lock()
	s.releaseOwnerLocked(owner)
	s.attachedMu.Unlock()
}

func (s *threadCatalogService) releaseOwnerLocked(owner string) {
	current := s.owners[owner]
	if current == nil {
		return
	}
	delete(s.owners, owner)
	close(current.changed)
	if current.catchUp != nil {
		current.catchUp.abandon()
	}
	for _, path := range current.paths {
		s.unholdLocked(path)
	}
}

// holdLocked adds one reference to path's hold, watching it if it is new.
func (s *threadCatalogService) holdLocked(path string) {
	held := s.held[path]
	if held == nil {
		held = &heldSource{}
		s.held[path] = held
		if s.files != nil {
			s.files.Watch(path, s.providerHome(path))
		}
	}
	held.refs++
}

// unholdLocked drops one reference; a file nothing holds stops being watched
// and loses its transient state.
func (s *threadCatalogService) unholdLocked(path string) {
	held := s.held[path]
	if held.refs--; held.refs > 0 {
		return
	}
	if held.retry != nil {
		held.retry.Stop()
	}
	delete(s.held, path)
	if s.files != nil {
		s.files.Unwatch(path)
	}
}

func (s *threadCatalogService) releaseAllAttached() {
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	for owner := range s.owners {
		s.releaseOwnerLocked(owner)
	}
	for key, hold := range s.outside {
		for _, path := range hold.paths {
			s.unholdLocked(path)
		}
		delete(s.outside, key)
	}
}

func (s *threadCatalogService) holdsAttachedSource(path string) bool {
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	return s.held[path] != nil
}

// awaitAttachedCurrent is a resumed attachment's readiness wait
// (provider_lifecycle.go). Proof is skipped only on positive evidence that the
// thread was never prompted, so its first prompt, which creates the file, is
// not held. A provisional identity is such evidence: it holds no file. So is
// neverPrompted for a Grok session whose watched transcript does not exist:
// Grok writes updates.jsonl with the first turn. An owner holding no file for
// any other reason waits, and logs why, until an identity binds one.
func (s *threadCatalogService) awaitAttachedCurrent(owner string, fresh bool, neverPrompted func() bool, cancel <-chan struct{}) bool {
	for {
		s.attachedMu.Lock()
		current := s.owners[owner]
		if current == nil {
			s.attachedMu.Unlock()
			return false
		}
		if current.catchUp == nil {
			unresolved := current.unresolved
			s.attachedMu.Unlock()
			if unresolved == unresolvedProvisional {
				return true
			}
			s.logf("attachment %s not ready: it holds no saved file (%s)", owner, unresolved)
			select {
			case <-current.changed:
				continue
			case <-cancel:
				return false
			}
		}
		catchUp, refresh := current.catchUp, false
		if fresh && len(catchUp.pending) == 0 {
			catchUp, refresh = s.newCatchUpLocked(current.paths), true
			current.catchUp = catchUp
		}
		s.attachedMu.Unlock()
		if neverPrompted != nil && grokTranscriptAbsent(current.paths) && neverPrompted() {
			return true
		}
		if refresh {
			s.RequestPathRefresh(current.paths)
		}
		select {
		case <-catchUp.done:
		case <-cancel:
			return false
		}
		if s.catchUpResultOf(catchUp) == catchUpReady {
			return true
		}
		fresh = false // the hold was replaced or released: read it again
	}
}

// grokTranscriptAbsent reports that a hold's paths name a Grok transcript
// that does not exist.
func grokTranscriptAbsent(paths []string) bool {
	for _, path := range paths {
		if filepath.Base(path) == "updates.jsonl" {
			_, err := os.Stat(path)
			return errors.Is(err, fs.ErrNotExist)
		}
	}
	return false
}

// catchUpResultOf reads a finished catch-up's result.
func (s *threadCatalogService) catchUpResultOf(catchUp *attachedCatchUp) catchUpResult {
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	return catchUp.result()
}

// providerHome is the directory above path's root: watches for an absent file
// never climb past it.
func (s *threadCatalogService) providerHome(path string) string {
	if root, ok := s.rootFor(path); ok {
		return filepath.Dir(root.Root.Path)
	}
	return ""
}

func (s *threadCatalogService) rootFor(path string) (history.PersistedRoot, bool) {
	for _, root := range s.roots {
		if catalogPathsIncludeRoot(map[string]struct{}{path: {}}, root.Root.Path) {
			return root, true
		}
	}
	return history.PersistedRoot{}, false
}

func (s *threadCatalogService) rootOf(provider history.Provider, kind history.RootKind) (history.PersistedRoot, bool) {
	for _, root := range s.roots {
		if root.Root.Provider == provider && root.Root.Kind == kind {
			return root, true
		}
	}
	return history.PersistedRoot{}, false
}

// Reasons an attachment holds no file. The set is fixed, so monitoring
// counts stay bounded.
const (
	unresolvedAwaitingIdentity = "awaiting native identity"
	unresolvedProvisional      = "provisional identity"
	unresolvedNoCwd            = "no working directory"
	unresolvedNoRoot           = "provider root not configured"
	unresolvedOutsideRoots     = "reported path outside provider roots"
	unresolvedNoSavedSource    = "no single saved source for this identity"
	unresolvedProvider         = "unsupported provider"
)

// attachedSourcePaths resolves an attachment's native files from provider
// facts, never by walking a directory. known is a path the provider reported
// on the wire. An unresolved attachment returns no path and its reason.
func (s *threadCatalogService) attachedSourcePaths(ctx context.Context, provider, nativeID, cwd, known string) ([]string, string) {
	switch {
	case nativeID == "":
		return nil, unresolvedAwaitingIdentity
	case nativeThreadIDProvisional(provider, nativeID):
		return nil, unresolvedProvisional
	}
	switch provider {
	case "claude":
		root, ok := s.rootOf(history.ProviderClaude, history.RootClaudeProjects)
		switch {
		case !ok:
			return nil, unresolvedNoRoot
		case cwd == "":
			return nil, unresolvedNoCwd
		}
		return []string{filepath.Join(root.Root.Path, setup.ClaudeProjectSlug(cwd), nativeID+".jsonl")}, ""
	case "grok":
		root, ok := s.rootOf(history.ProviderGrok, history.RootGrokSessions)
		switch {
		case !ok:
			return nil, unresolvedNoRoot
		case cwd == "":
			return nil, unresolvedNoCwd
		}
		// Grok names a session directory by its percent-encoded cwd. Summary
		// carries metadata; updates.jsonl appears with the first turn.
		dir := filepath.Join(root.Root.Path, url.PathEscape(cwd), nativeID)
		return []string{filepath.Join(dir, "summary.json"), filepath.Join(dir, "updates.jsonl")}, ""
	case "codex":
		if known != "" {
			if path, ok := s.canonicalSourcePath(known); ok {
				return []string{path}, ""
			}
			return nil, unresolvedOutsideRoots
		}
		if paths := s.savedCanonicalSource(ctx, history.ProviderCodex, nativeID); len(paths) > 0 {
			return paths, ""
		}
		return nil, unresolvedNoSavedSource
	}
	return nil, unresolvedProvider
}

// canonicalSourcePath maps a provider-reported path onto its root's canonical
// spelling, so it matches the catalog's source rows.
func (s *threadCatalogService) canonicalSourcePath(path string) (string, bool) {
	path = filepath.Clean(path)
	if _, ok := s.rootFor(path); ok {
		return path, true
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(resolved, filepath.Base(path))
		if _, ok := s.rootFor(path); ok {
			return path, true
		}
	}
	return "", false
}

// savedCanonicalSource is the catalog's canonical transcript source for one
// exact provider identity. Any ambiguity leaves it unresolved.
func (s *threadCatalogService) savedCanonicalSource(ctx context.Context, provider history.Provider, nativeID string) []string {
	page, err := s.store.ListThreads(ctx, history.ListThreadsParams{
		Identities:       []history.ThreadIdentity{{Provider: provider, NativeThreadID: nativeID}},
		IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true,
		Archived: history.ArchivedInclude, Limit: 2,
	})
	if err != nil || len(page.Items) != 1 {
		return nil
	}
	sources, err := s.store.LoadThreadSources(ctx, page.Items[0].ID)
	if err != nil {
		return nil
	}
	var paths []string
	for _, source := range sources {
		if source.CanonicalTranscriptSource && source.Status == history.SourcePresent {
			paths = append(paths, source.Source.Path)
		}
	}
	if len(paths) != 1 {
		return nil
	}
	return paths
}

// ingestErrorKind is a bounded failure dimension for monitoring.
type ingestErrorKind string

const (
	ingestErrorRead     ingestErrorKind = "read"
	ingestErrorApply    ingestErrorKind = "apply"
	ingestErrorStore    ingestErrorKind = "store"
	ingestErrorIdentity ingestErrorKind = "identity_retry"
)

// ingestPass records what one targeted pass did to each source it took.
type ingestPass struct {
	seq     uint64
	sources map[string]*ingestOutcome
}

type ingestOutcome struct {
	take         dirtyTake
	provider     history.Provider
	found        bool
	size         int64
	err          error
	errKind      ingestErrorKind
	prepareStart time.Time
	prepareEnd   time.Time
	savedAt      time.Time
	rebuild      bool
	bytesRead    int64
	// canonical marks a thread's transcript source. Committed state after
	// the pass: offsets proven by an unchanged checkpoint or written by this
	// pass's apply.
	canonical      bool
	committed      bool
	readOffset     int64
	completeOffset int64
	advance        transcriptAdvance
	threadID       string
	nativeID       string
	sourceID       int64
	generation     int64
}

func newIngestPass(batch []dirtyTake, seq uint64) *ingestPass {
	pass := &ingestPass{seq: seq, sources: make(map[string]*ingestOutcome, len(batch))}
	for _, take := range batch {
		pass.sources[take.path] = &ingestOutcome{take: take}
	}
	return pass
}

// alias makes a hinted path share the outcome of the source actually read
// for it (a Grok summary's session transcript).
func (p *ingestPass) alias(path, source string) {
	if shared := p.sources[source]; shared != nil {
		p.sources[path] = shared
		return
	}
	p.sources[source] = p.sources[path]
}

func (o *ingestOutcome) fail(kind ingestErrorKind, err error) {
	if o == nil || o.err != nil {
		return
	}
	if errors.Is(err, history.ErrTranscriptChanged) {
		kind = ingestErrorIdentity
	}
	o.err, o.errKind = err, kind
}

func (o *ingestOutcome) observe(size int64) {
	o.found, o.size = true, size
}

// The pass methods are nil-safe: a recovery walk records no outcomes.
func (p *ingestPass) outcome(path string) *ingestOutcome {
	if p == nil {
		return nil
	}
	return p.sources[path]
}

func (p *ingestPass) progress(path string) *dirtySource {
	if o := p.outcome(path); o != nil {
		return o.take.source
	}
	return nil
}

func (p *ingestPass) fail(path string, kind ingestErrorKind, err error) {
	p.outcome(path).fail(kind, err)
}

// verified records an unchanged source: whether it is a canonical
// transcript, and the committed offsets its healthy checkpoint proves.
func (p *ingestPass) verified(path string, canonical bool, checkpoint history.TranscriptCheckpoint, proven bool) {
	if o := p.outcome(path); o != nil {
		o.canonical = o.canonical || canonical
		if canonical && proven {
			o.committed, o.readOffset, o.completeOffset = true, checkpoint.ReadOffset, checkpoint.CompleteOffset
		}
	}
}

func (p *ingestPass) prepared(path string, started, done time.Time, prepared *preparedTranscript) {
	o := p.outcome(path)
	if o == nil {
		return
	}
	o.prepareStart, o.prepareEnd = started, done
	if prepared != nil {
		o.rebuild = prepared.Mode == history.TranscriptRebuild
		o.bytesRead = prepared.File.Transcript.BytesRead
	}
}

// reconciled stamps metadata-only sources saved once their root commits.
func (p *ingestPass) reconciled(at time.Time) {
	if p == nil {
		return
	}
	for _, o := range p.sources {
		if o.found && o.err == nil && !o.prepareEnd.IsZero() && o.savedAt.IsZero() {
			o.savedAt = at
		}
	}
}

// saved records a canonical transcript's apply, or its verification when the
// apply had nothing to do. A preparation done inside the repair (an errored
// checkpoint's rebuild) is accounted here.
func (p *ingestPass) saved(path string, stored history.StoredSource, checkpoint history.TranscriptCheckpoint, hasCheckpoint bool, advance transcriptAdvance, err error) {
	o := p.outcome(path)
	if o == nil {
		return
	}
	o.canonical = true
	o.threadID, o.sourceID, o.generation = stored.ThreadID, stored.ID, stored.Generation
	if stored.Projection != nil {
		o.nativeID = stored.Projection.NativeThreadID
	}
	if err != nil {
		o.fail(ingestErrorApply, err)
		return
	}
	if o.prepareStart.IsZero() && !advance.prepareStart.IsZero() {
		o.prepareStart, o.prepareEnd, o.rebuild, o.bytesRead = advance.prepareStart, advance.prepareEnd, advance.rebuild, advance.bytesRead
	}
	o.advance = advance
	switch {
	case !advance.savedAt.IsZero():
		o.savedAt = advance.savedAt
		o.committed, o.readOffset, o.completeOffset = true, advance.endOffset, advance.completeOffset
	case hasCheckpoint && checkpoint.ErrorSummary == nil:
		o.committed, o.readOffset, o.completeOffset = true, checkpoint.ReadOffset, checkpoint.CompleteOffset
	}
}

// recordReceipt replaces a held file's receipt with this pass's apply. It
// runs before the apply is published, so the callback finds its receipt.
func (s *threadCatalogService) recordReceipt(o *ingestOutcome, path string) {
	if o == nil || !o.advance.applied {
		return
	}
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	if held := s.held[path]; held != nil {
		held.progressAt = o.savedAt
		held.receipt = newIngestReceipt(o)
	}
}

// completeIngest settles each source the pass took: metrics, the held
// file's state and receipt, waiting catch-ups, and a capped retry for a held
// file that failed. It never requests a root walk.
func (s *threadCatalogService) completeIngest(pass *ingestPass) {
	now := time.Now()
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	recorded := map[*ingestOutcome]bool{}
	for path, o := range pass.sources {
		if _, ok := s.rootFor(path); !ok && o.err == nil {
			o.found = false
		}
		if !recorded[o] {
			recorded[o] = true
			s.ingest.recordSource(o, now)
		}
		held := s.held[path]
		if held == nil {
			continue
		}
		if o.found {
			held.observed, held.size = true, o.size
		}
		if o.committed {
			held.committed, held.readOffset, held.completeOffset = true, o.readOffset, o.completeOffset
		}
		switch {
		case o.err != nil:
			s.heldFailedLocked(path, held, o)
		case held.failures > 0:
			s.logf("attached source ingest recovered after %d failure(s): %s", held.failures, path)
			held.failures = 0
			if held.retry != nil {
				held.retry.Stop()
				held.retry = nil
			}
		}
		waiting := held.waiters[:0]
		for _, catchUp := range held.waiters {
			if catchUp.after < pass.seq {
				catchUp.finish(path, o)
			}
			if catchUp.pending[path] {
				waiting = append(waiting, catchUp)
			}
		}
		held.waiters = waiting
	}
}

// heldFailedLocked logs a held file's first failure and schedules a retry
// with capped backoff while the file stays held.
func (s *threadCatalogService) heldFailedLocked(path string, held *heldSource, o *ingestOutcome) {
	held.failures++
	if held.failures == 1 {
		s.logf("attached source ingest failed (%s), retrying: %s: %s", o.errKind, path, boundedCatalogError(o.err))
	}
	delay := attachedRetryMax
	if held.failures < 20 {
		delay = min(attachedRetryMax, attachedRetryFirst<<(held.failures-1))
	}
	if held.retry != nil {
		held.retry.Stop()
	}
	held.retry = time.AfterFunc(delay, func() {
		s.attachedMu.Lock()
		current := s.held[path] == held
		s.attachedMu.Unlock()
		if current {
			s.enqueue(path, time.Now(), false, false) // a retry is an event: it moves nothing forward
		}
	})
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

type readProgressKey struct{}

// withReadProgress lets the parser's reader report consumed bytes to the
// queued source it is ingesting, so a long rebuild is visibly progressing.
func withReadProgress(ctx context.Context, source *dirtySource) context.Context {
	if source == nil {
		return ctx
	}
	return context.WithValue(ctx, readProgressKey{}, source)
}

func readProgressFrom(ctx context.Context, reader io.Reader) io.Reader {
	if source, ok := ctx.Value(readProgressKey{}).(*dirtySource); ok {
		return &progressReader{reader: reader, source: source}
	}
	return reader
}

type progressReader struct {
	reader io.Reader
	source *dirtySource
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.source.readAt.Store(ingestNanos())
	}
	return n, err
}
