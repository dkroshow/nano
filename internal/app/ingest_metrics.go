package app

import (
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
	"github.com/dkroshow/nano/internal/history"
)

// Attached-file ingestion reports its speed, throughput, backlog and failures
// through GET /api/v1/monitor, in the minute convention request timing uses.
// Memory is bounded: ten closed minutes of fixed-bucket histograms and
// counters by provider and stage, and one latest receipt per held file.
// Stage clocks are monotonic; receipts pair them with UTC instants so a local
// probe can join them to its own observations.

const (
	ingestMinutesKept = 10
	// ingestStallAfter is how long observed dirty work may go without read or
	// committed progress before the monitor reports it stalled.
	ingestStallAfter = 30 * time.Second
)

// ingestLatencyBounds are the histogram bucket upper bounds. Quantiles are
// reported as the bound of the bucket holding them, so they are approximate.
var ingestLatencyBounds = [...]time.Duration{
	time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
	20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
	500 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second,
	30 * time.Second, time.Minute, 2 * time.Minute,
}

var ingestClockStart = time.Now()

// ingestNanos is a monotonic clock reading that fits in an atomic integer.
func ingestNanos() int64 { return int64(time.Since(ingestClockStart)) }

type latencyHistogram struct {
	counts [len(ingestLatencyBounds) + 1]int
	max    time.Duration
}

func (h *latencyHistogram) add(d time.Duration) {
	d = max(d, 0)
	bucket, _ := slices.BinarySearch(ingestLatencyBounds[:], d)
	h.counts[bucket]++
	h.max = max(h.max, d)
}

type latencySummary struct {
	Count    int     `json:"count"`
	MedianMS float64 `json:"medianMs"`
	P95MS    float64 `json:"p95Ms"`
	MaxMS    float64 `json:"maxMs"`
}

func (h *latencyHistogram) summary() latencySummary {
	total := 0
	for _, n := range h.counts {
		total += n
	}
	quantile := func(q float64) float64 {
		rank := int(q*float64(total-1)) + 1
		seen := 0
		for bucket, n := range h.counts {
			if seen += n; seen >= rank {
				if bucket < len(ingestLatencyBounds) {
					return milliseconds(min(ingestLatencyBounds[bucket], h.max))
				}
				break
			}
		}
		return milliseconds(h.max)
	}
	return latencySummary{Count: total, MedianMS: quantile(0.5), P95MS: quantile(0.95), MaxMS: milliseconds(h.max)}
}

// ingestCounts are one provider's per-minute totals. Source records are
// complete native lines, not browser rows; bytes and records count only
// what a successful append committed within its projection.
type ingestCounts struct {
	Passes            int            `json:"passes"`
	Unchanged         int            `json:"unchanged,omitempty"`
	Missing           int            `json:"missing,omitempty"`
	BytesRead         int64          `json:"bytesRead"`
	CommittedBytes    int64          `json:"committedBytes"`
	CommittedRecords  int            `json:"committedRecords"`
	Rebuilds          int            `json:"rebuilds,omitempty"`
	RebuildBytes      int64          `json:"rebuildBytes,omitempty"`
	ParserDiagnostics int            `json:"parserDiagnostics,omitempty"`
	Errors            map[string]int `json:"errors,omitempty"`
}

type ingestProviderMinute struct {
	counts ingestCounts
	stages map[string]*latencyHistogram
}

func (m *ingestProviderMinute) stage(name string, d time.Duration) {
	h := m.stages[name]
	if h == nil {
		h = &latencyHistogram{}
		m.stages[name] = h
	}
	h.add(d)
}

type ingestMinute struct {
	providers map[history.Provider]*ingestProviderMinute
	recovery  latencyHistogram
}

// ingestMetrics' zero value is ready to use.
type ingestMetrics struct {
	mu      sync.Mutex
	minutes map[int64]*ingestMinute
}

func (m *ingestMetrics) minute(at time.Time) *ingestMinute {
	if m.minutes == nil {
		m.minutes = map[int64]*ingestMinute{}
	}
	current := at.Unix() / 60
	for minute := range m.minutes {
		if minute < current-ingestMinutesKept {
			delete(m.minutes, minute)
		}
	}
	entry := m.minutes[current]
	if entry == nil {
		entry = &ingestMinute{providers: map[history.Provider]*ingestProviderMinute{}}
		m.minutes[current] = entry
	}
	return entry
}

func (m *ingestMetrics) provider(at time.Time, provider history.Provider) *ingestProviderMinute {
	minute := m.minute(at)
	entry := minute.providers[provider]
	if entry == nil {
		entry = &ingestProviderMinute{stages: map[string]*latencyHistogram{}}
		minute.providers[provider] = entry
	}
	return entry
}

// recordSource files one source's targeted pass. Stage names are
// "<append|rebuild>.<queue|prepare|save>".
func (m *ingestMetrics) recordSource(o *ingestOutcome, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.provider(at, o.provider)
	counts := &entry.counts
	counts.Passes++
	switch {
	case o.err != nil:
		if counts.Errors == nil {
			counts.Errors = map[string]int{}
		}
		counts.Errors[string(o.errKind)]++
	case !o.found:
		counts.Missing++
	case o.prepareStart.IsZero():
		counts.Unchanged++
	}
	if o.prepareStart.IsZero() {
		return
	}
	class := "append"
	if o.rebuild {
		class = "rebuild"
	}
	entry.stage(class+".queue", o.prepareStart.Sub(o.take.since))
	entry.stage(class+".prepare", o.prepareEnd.Sub(o.prepareStart))
	counts.BytesRead += o.bytesRead
	if o.err != nil || o.savedAt.IsZero() {
		return
	}
	entry.stage(class+".save", o.savedAt.Sub(o.prepareEnd))
	if !o.advance.applied {
		return
	}
	counts.ParserDiagnostics += o.advance.diagnostics
	if o.advance.rebuild {
		counts.Rebuilds++
		counts.RebuildBytes += o.advance.endOffset
		return
	}
	counts.CommittedBytes += o.advance.endOffset - o.advance.startOffset
	counts.CommittedRecords += o.advance.records
}

// recordPublication files one transcript-change callback: the wait from the
// oldest apply it covers to its start, and its own run time. One callback
// can cover several applies, so these are batch times, not row latency.
func (m *ingestMetrics) recordPublication(provider history.Provider, wait, run time.Duration, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.provider(at, provider)
	entry.stage("publish.wait", wait)
	entry.stage("publish.run", run)
}

func (m *ingestMetrics) recordRecovery(at time.Time, took time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.minute(at).recovery.add(took)
}

type ingestProviderReport struct {
	ingestCounts
	Stages map[string]latencySummary `json:"stages,omitempty"`
}

type ingestMinuteReport struct {
	Minute    time.Time                       `json:"minute"`
	Providers map[string]ingestProviderReport `json:"providers"`
	// Recovery is explicit full discovery, reported apart from attached work.
	Recovery *latencySummary `json:"recovery,omitempty"`
}

// report returns the closed minutes kept, oldest first.
func (m *ingestMetrics) report(now time.Time) []ingestMinuteReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := now.Unix() / 60
	out := []ingestMinuteReport{}
	for minute, entry := range m.minutes {
		if minute >= current || minute < current-ingestMinutesKept {
			continue
		}
		report := ingestMinuteReport{Minute: time.Unix(minute*60, 0).UTC(), Providers: map[string]ingestProviderReport{}}
		for provider, values := range entry.providers {
			stages := make(map[string]latencySummary, len(values.stages))
			for name, h := range values.stages {
				stages[name] = h.summary()
			}
			report.Providers[string(provider)] = ingestProviderReport{ingestCounts: values.counts, Stages: stages}
		}
		if recovery := entry.recovery.summary(); recovery.Count > 0 {
			report.Recovery = &recovery
		}
		out = append(out, report)
	}
	slices.SortFunc(out, func(a, b ingestMinuteReport) int { return a.Minute.Compare(b.Minute) })
	return out
}

// ingestReceipt is the latest ingestion that advanced one held file. It is
// replaced only by actual ingestion, so its stages describe one real pass.
type ingestReceipt struct {
	Provider         string     `json:"provider"`
	ThreadID         string     `json:"threadId"`
	NativeThreadID   string     `json:"nativeThreadId,omitempty"`
	SourceID         int64      `json:"sourceId"`
	SourceGeneration int64      `json:"sourceGeneration"`
	ProjectionKey    string     `json:"projectionKey"`
	Mode             string     `json:"mode"`
	StartReadOffset  int64      `json:"startReadOffset"`
	EndReadOffset    int64      `json:"endReadOffset"`
	CompleteOffset   int64      `json:"completeOffset"`
	ObservedSize     int64      `json:"observedSize"`
	Records          int        `json:"records"`
	HintAt           time.Time  `json:"hintAt"`
	PrepareStartedAt time.Time  `json:"prepareStartedAt"`
	PreparedAt       time.Time  `json:"preparedAt"`
	SavedAt          time.Time  `json:"savedAt"`
	PublishStartedAt *time.Time `json:"publishStartedAt,omitempty"`
	PublishedAt      *time.Time `json:"publishedAt,omitempty"`
	QueueMS          float64    `json:"queueMs"`
	PrepareMS        float64    `json:"prepareMs"`
	SaveMS           float64    `json:"saveMs"`
	PublishWaitMS    *float64   `json:"publishWaitMs,omitempty"`
	PublishMS        *float64   `json:"publishMs,omitempty"`
	saved            time.Time
}

func newIngestReceipt(o *ingestOutcome) *ingestReceipt {
	mode := "append"
	if o.advance.rebuild {
		mode = "rebuild"
	}
	return &ingestReceipt{
		Provider: string(o.provider), ThreadID: o.threadID, NativeThreadID: o.nativeID,
		SourceID: o.sourceID, SourceGeneration: o.generation, ProjectionKey: o.advance.projectionKey, Mode: mode,
		StartReadOffset: o.advance.startOffset, EndReadOffset: o.advance.endOffset,
		CompleteOffset: o.advance.completeOffset, ObservedSize: o.size, Records: o.advance.records,
		HintAt: o.take.since.UTC(), PrepareStartedAt: o.prepareStart.UTC(), PreparedAt: o.prepareEnd.UTC(), SavedAt: o.savedAt.UTC(),
		QueueMS: milliseconds(o.prepareStart.Sub(o.take.since)), PrepareMS: milliseconds(o.prepareEnd.Sub(o.prepareStart)),
		SaveMS: milliseconds(o.savedAt.Sub(o.prepareEnd)), saved: o.savedAt,
	}
}

// notePublication stamps the held receipts a transcript-change callback
// covered and files its timing.
func (s *threadCatalogService) notePublication(change transcriptChange, since, started, finished time.Time) {
	s.ingest.recordPublication(history.Provider(change.Provider), started.Sub(since), finished.Sub(started), finished)
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	for _, held := range s.held {
		receipt := held.receipt
		if receipt == nil || receipt.ThreadID != change.ThreadID || receipt.PublishStartedAt != nil || receipt.saved.After(started) {
			continue
		}
		startedAt, finishedAt := started.UTC(), finished.UTC()
		wait, run := milliseconds(started.Sub(receipt.saved)), milliseconds(finished.Sub(started))
		receipt.PublishStartedAt, receipt.PublishedAt, receipt.PublishWaitMS, receipt.PublishMS = &startedAt, &finishedAt, &wait, &run
	}
}

// ingestReceipts copies the latest receipt of every held file.
func (s *threadCatalogService) ingestReceipts() []ingestReceipt {
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	receipts := []ingestReceipt{}
	for _, held := range s.held {
		if held.receipt != nil {
			receipts = append(receipts, *held.receipt)
		}
	}
	slices.SortFunc(receipts, func(a, b ingestReceipt) int { return b.SavedAt.Compare(a.SavedAt) })
	return receipts
}

type ingestWatchStatus struct {
	// Mode is kqueue, disabled (-no-watch) or unavailable.
	Mode  string `json:"mode"`
	Error string `json:"error,omitempty"`
	catalog.WatchStats
}

type ingestHeap struct {
	AllocatedBytes uint64 `json:"allocatedBytes"`
	InUseBytes     uint64 `json:"inUseBytes"`
	ReservedBytes  uint64 `json:"reservedBytes"`
}

// ingestStatus is the monitor's view of attached ingestion now.
type ingestStatus struct {
	Watch       ingestWatchStatus `json:"watch"`
	Attachments int               `json:"attachments"`
	// UnresolvedAttachments counts attachments holding no file, by reason.
	UnresolvedAttachments map[string]int `json:"unresolvedAttachments"`
	HeldSources           int            `json:"heldSources"`
	// OutsideSources counts held sessions Nano did not start, at most
	// outsideHoldLimit; their files are also in HeldSources.
	OutsideSources int `json:"outsideSources"`
	Queued         int `json:"queued"`
	InFlight       int `json:"inFlight"`
	// OldestPendingMs includes running work, so a stuck pass keeps aging.
	OldestPendingMS *float64 `json:"oldestPendingMs"`
	// Byte backlog is measured at each held file's last observed size; no
	// file is stat'ed to report it. A partial final line is held in parser
	// continuation and is not unread.
	UnreadBytes       int64 `json:"unreadBytes"`
	PartialLineBytes  int64 `json:"partialLineBytes"`
	UnobservedSources int   `json:"unobservedSources"`
	// StalledSources is dirty work with neither read nor committed progress
	// for ingestStallAfter. An idle or partial-line source is not stalled.
	StalledSources         int `json:"stalledSources"`
	FailingSources         int `json:"failingSources"`
	MaxConsecutiveFailures int `json:"maxConsecutiveFailures"`
	// ManualRefreshMs is the running explicit recovery walk's age; it names
	// the cause when it delays attached work.
	ManualRefreshMS     *float64             `json:"manualRefreshMs"`
	ManualRefreshQueued bool                 `json:"manualRefreshQueued"`
	PublicationBacklog  int                  `json:"publicationBacklog"`
	PublicationInFlight int                  `json:"publicationInFlight"`
	Heap                ingestHeap           `json:"heap"`
	Minutes             []ingestMinuteReport `json:"minutes"`
}

func (s *threadCatalogService) ingestStatus() ingestStatus {
	now := time.Now()
	nowNanos := ingestNanos()
	status := ingestStatus{Watch: ingestWatchStatus{Mode: "kqueue"}}
	switch {
	case s.files != nil:
		status.Watch.WatchStats = s.files.Stats()
	case s.filesErr == errAttachedWatchDisabled:
		status.Watch.Mode = "disabled"
	default:
		status.Watch.Mode = "unavailable"
		if s.filesErr != nil {
			status.Watch.Error = s.filesErr.Error()
		}
	}

	type pending struct {
		path    string
		since   time.Time
		running bool
		readAt  int64
	}
	s.queueMu.Lock()
	dirty := make([]pending, 0, len(s.dirty))
	for path, source := range s.dirty {
		dirty = append(dirty, pending{path: path, since: source.since, running: source.running, readAt: source.readAt.Load()})
	}
	if !s.recoveringSince.IsZero() {
		ms := milliseconds(now.Sub(s.recoveringSince))
		status.ManualRefreshMS = &ms
	}
	status.ManualRefreshQueued = !s.recoveryAt.IsZero()
	s.queueMu.Unlock()

	s.attachedMu.Lock()
	status.Attachments, status.HeldSources, status.OutsideSources = len(s.owners), len(s.held), len(s.outside)
	status.UnresolvedAttachments = map[string]int{}
	for _, owner := range s.owners {
		if owner.unresolved != "" {
			status.UnresolvedAttachments[owner.unresolved]++
		}
	}
	progressAt := make(map[string]time.Time, len(dirty))
	for path, held := range s.held {
		progressAt[path] = held.progressAt
		switch {
		case !held.observed:
			status.UnobservedSources++
		case held.committed:
			status.UnreadBytes += max(0, held.size-held.readOffset)
			status.PartialLineBytes += max(0, held.readOffset-held.completeOffset)
		}
		if held.failures > 0 {
			status.FailingSources++
			status.MaxConsecutiveFailures = max(status.MaxConsecutiveFailures, held.failures)
		}
	}
	s.attachedMu.Unlock()

	var oldest time.Time
	for _, item := range dirty {
		if item.running {
			status.InFlight++
		} else {
			status.Queued++
		}
		if oldest.IsZero() || item.since.Before(oldest) {
			oldest = item.since
		}
		read := item.readAt != 0 && time.Duration(nowNanos-item.readAt) < ingestStallAfter
		committed := now.Sub(progressAt[item.path]) < ingestStallAfter
		if now.Sub(item.since) >= ingestStallAfter && !read && !committed {
			status.StalledSources++
		}
	}
	if !oldest.IsZero() {
		ms := milliseconds(now.Sub(oldest))
		status.OldestPendingMS = &ms
	}
	status.PublicationBacklog, status.PublicationInFlight = s.transcriptChangeStatus()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	status.Heap = ingestHeap{AllocatedBytes: memory.HeapAlloc, InUseBytes: memory.HeapInuse, ReservedBytes: memory.HeapSys}
	status.Minutes = s.ingest.report(now)
	return status
}
