package main

import (
	"context"
	"log"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

var errSidebarMaterializedUnavailable = errThreadCatalogUnavailable

// sidebarDelta is a complete patch against one prior sidebar revision. Slow
// stream consumers receive Refresh instead of an unsafe partial merge.
// Generation names the process whose revisions these are: revisions count
// from 1 again after a restart, so a browser compares both.
type sidebarDelta struct {
	Generation      string          `json:"generation"`
	Revision        uint64          `json:"revision"`
	Rows            []sidebarThread `json:"rows,omitempty"`
	RemovedIDs      []string        `json:"removedIds,omitempty"`
	Roots           []string        `json:"roots,omitempty"`
	HistoricalCount int             `json:"historicalCount"`
	CatalogVersion  int64           `json:"catalogVersion"`
	Refresh         bool            `json:"refresh,omitempty"`
}

type sidebarHub struct {
	mu   sync.Mutex
	subs map[chan sidebarDelta]struct{}
}

func newSidebarHub() *sidebarHub {
	return &sidebarHub{subs: map[chan sidebarDelta]struct{}{}}
}

func (h *sidebarHub) subscribe() chan sidebarDelta {
	ch := make(chan sidebarDelta, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sidebarHub) unsubscribe(ch chan sidebarDelta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *sidebarHub) notify(delta sidebarDelta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		next := delta
		select {
		case <-ch:
			next = sidebarDelta{Generation: delta.Generation, Revision: delta.Revision, CatalogVersion: delta.CatalogVersion, Refresh: true}
		default:
		}
		select {
		case ch <- next:
		default:
		}
	}
}

type sidebarMaterializedState struct {
	updateMu     sync.Mutex
	mu           sync.RWMutex
	all          []catalogThread
	defaultQuery catalogListQuery
	active       sidebarActiveResponse
	nodes        []*sidebarNode
	version      int64
	revision     uint64
	generation   string // this process's revision sequence; see sidebarDelta
	ready        bool
	events       *sidebarHub
	// rowsChanged wakes the fleet feed after any change to all, including
	// ones the default view does not show. One pending wake covers many.
	rowsChanged chan struct{}
}

func newSidebarMaterializedState() *sidebarMaterializedState {
	return &sidebarMaterializedState{
		events: newSidebarHub(), generation: strconv.FormatInt(time.Now().UnixNano(), 36),
		rowsChanged: make(chan struct{}, 1),
	}
}

// rows returns a copy of every materialized thread, and whether the sidebar
// has been built.
func (m *sidebarMaterializedState) rows() ([]catalogThread, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]catalogThread(nil), m.all...), m.ready
}

// marker is the refresh marker a stream sends on connect: the current
// generation and revision, which a browser compares with its list's.
func (m *sidebarMaterializedState) marker(version int64) (sidebarDelta, bool) {
	if m == nil {
		return sidebarDelta{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.revision == 0 {
		return sidebarDelta{}, false
	}
	return sidebarDelta{Generation: m.generation, Revision: m.revision, CatalogVersion: version, Refresh: true}, true
}

// defaultNodes returns the default view's whole thread tree, history rows
// included, and whether it has been built. Callers only read it.
func (m *sidebarMaterializedState) defaultNodes() ([]*sidebarNode, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.nodes, m.ready
}

func (m *sidebarMaterializedState) activeSnapshot(query catalogListQuery, useDefault bool) (sidebarActiveResponse, error) {
	if m == nil {
		return sidebarActiveResponse{}, errSidebarMaterializedUnavailable
	}
	m.mu.RLock()
	if !m.ready {
		m.mu.RUnlock()
		return sidebarActiveResponse{}, errSidebarMaterializedUnavailable
	}
	if useDefault || sameSidebarQuery(query, m.defaultQuery) {
		result := m.active
		m.mu.RUnlock()
		return result, nil
	}
	all := append([]catalogThread(nil), m.all...)
	version := m.version
	revision := m.revision
	m.mu.RUnlock()
	result, _ := projectSidebar(all, query, version)
	result.Generation, result.Revision = m.generation, revision
	return result, nil
}

func (m *sidebarMaterializedState) historySnapshot(query catalogListQuery, useDefault bool, parentRef string, cursor *history.ThreadCursor, limit int) (sidebarActiveResponse, error) {
	if m == nil {
		return sidebarActiveResponse{}, errSidebarMaterializedUnavailable
	}
	m.mu.RLock()
	if !m.ready {
		m.mu.RUnlock()
		return sidebarActiveResponse{}, errSidebarMaterializedUnavailable
	}
	var nodes []*sidebarNode
	version := m.version
	revision := m.revision
	if useDefault || sameSidebarQuery(query, m.defaultQuery) {
		nodes = m.nodes
		m.mu.RUnlock()
	} else {
		all := append([]catalogThread(nil), m.all...)
		m.mu.RUnlock()
		_, nodes = projectSidebar(all, query, version)
	}
	return pageSidebarHistory(nodes, parentRef, cursor, limit, version, revision)
}

func sameSidebarQuery(a, b catalogListQuery) bool {
	return sameSidebarTime(a.UpdatedFrom, b.UpdatedFrom) && sameSidebarTime(a.UpdatedTo, b.UpdatedTo)
}

func sameSidebarTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func (s *server) materializeSidebar(ctx context.Context, now time.Time) error {
	if s.sidebar == nil {
		s.sidebar = newSidebarMaterializedState()
	}
	s.sidebar.updateMu.Lock()
	defer s.sidebar.updateMu.Unlock()
	runtime := s.sidebarRuntimeSnapshot()
	baseQuery := sidebarBaseQuery()
	graph, threads, err := s.loadSidebarPopulation(ctx, baseQuery, runtime, now)
	if err != nil {
		return err
	}
	threads = append(threads, s.memorySidebarThreadRows(threads, runtime.summaries)...)
	s.composeCatalogThreads(ctx, threads, runtime, true)
	query := sidebarDefaultQuery(now)
	active, nodes := projectSidebar(threads, query, catalogVersion(graph.CatalogVersion))
	s.sidebar.replace(threads, query, active, nodes, catalogVersion(graph.CatalogVersion))
	return nil
}

func sidebarBaseQuery() catalogListQuery {
	return catalogListQuery{
		IncludeSubagents: true, IncludeProgrammatic: false,
		Archived: archivedIncludeFilter, Limit: sidebarHistoryDefaultLimit,
	}
}

func sidebarDefaultQuery(now time.Time) catalogListQuery {
	query := sidebarBaseQuery()
	from := now.Add(-30 * 24 * time.Hour)
	query.UpdatedFrom = &from
	return query
}

func (s *server) composeCatalogThreads(ctx context.Context, threads []catalogThread, runtime sidebarRuntimeSnapshot, fleetReady bool) {
	for i := range threads {
		threads[i].Mail = nil
	}
	s.attachRuntimeOverlayBatch(ctx, threads, runtime)
	if !fleetReady {
		s.attachFleetStates(ctx, threads)
	}
	s.attachStarredOverlay(threads)
	s.attachMailOverlay(threads)
}

func (m *sidebarMaterializedState) replace(all []catalogThread, query catalogListQuery, active sidebarActiveResponse, nodes []*sidebarNode, version int64) {
	m.mu.Lock()
	wasReady := m.ready
	previous := m.active
	m.all = all
	m.defaultQuery = query
	m.nodes = nodes
	m.version = version
	m.ready = true
	select {
	case m.rowsChanged <- struct{}{}:
	default:
	}
	if wasReady && sidebarResponsesEqual(previous, active) {
		active.Generation, active.Revision = m.generation, m.revision
		m.active = active
		m.mu.Unlock()
		return
	}
	m.revision++
	active.Generation, active.Revision = m.generation, m.revision
	m.active = active
	delta := diffSidebar(previous, active)
	m.mu.Unlock()
	if wasReady && delta != nil {
		m.events.notify(*delta)
	}
}

func (m *sidebarMaterializedState) applyMailDelta(delta mailDelta) {
	if m == nil || len(delta.Counts) == 0 {
		return
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	m.mu.RLock()
	if !m.ready {
		m.mu.RUnlock()
		return
	}
	all := append([]catalogThread(nil), m.all...)
	query := m.defaultQuery
	version := m.version
	m.mu.RUnlock()
	changed := false
	for i := range all {
		counts, ok := delta.Counts[all[i].HostedThreadID]
		if !ok {
			continue
		}
		var mail *agentMailCounts
		if counts != (agentMailCounts{}) {
			copy := counts
			mail = &copy
		}
		if !reflect.DeepEqual(all[i].Mail, mail) {
			all[i].Mail = mail
			changed = true
		}
	}
	if !changed {
		return
	}
	active, nodes := projectSidebar(all, query, version)
	m.replace(all, query, active, nodes, version)
}

func (m *sidebarMaterializedState) applyRuntimeDelta(delta runtimeDelta) {
	if m == nil {
		return
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	overlays := make(map[attachmentKey]*runtimeInfo, len(delta.Rows))
	for _, row := range delta.Rows {
		overlays[attachmentKey{provider: row.Provider, nativeThreadID: row.NativeID}] = row.Runtime
	}
	m.mu.RLock()
	if !m.ready {
		m.mu.RUnlock()
		return
	}
	all := append([]catalogThread(nil), m.all...)
	query := m.defaultQuery
	version := m.version
	m.mu.RUnlock()
	for i := range all {
		// A delta names only the threads whose runtime changed; the rest keep
		// theirs. A named nil overlay is a thread Nano no longer holds.
		runtime, named := overlays[attachmentKey{provider: all[i].Provider, nativeThreadID: all[i].NativeID}]
		if !named {
			continue
		}
		all[i].Runtime = runtime
		if nanoHolds(all[i]) {
			all[i].Fleet = liveFleetState(all[i])
		}
	}
	active, nodes := projectSidebar(all, query, version)
	m.replace(all, query, active, nodes, version)
}

func (m *sidebarMaterializedState) applyStar(provider, nativeID string, starred bool) bool {
	if m == nil || provider == "" || nativeID == "" {
		return false
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	m.mu.RLock()
	if !m.ready {
		m.mu.RUnlock()
		return false
	}
	all := append([]catalogThread(nil), m.all...)
	query := m.defaultQuery
	version := m.version
	m.mu.RUnlock()
	found := false
	for i := range all {
		if all[i].Provider == provider && all[i].NativeID == nativeID {
			all[i].Starred = starred
			found = true
		}
	}
	if !found {
		return false
	}
	active, nodes := projectSidebar(all, query, version)
	m.replace(all, query, active, nodes, version)
	return true
}

func (m *sidebarMaterializedState) applyCatalogRows(rows []catalogThread, version int64) {
	if m == nil || len(rows) == 0 {
		return
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	m.mu.RLock()
	if !m.ready {
		m.mu.RUnlock()
		return
	}
	all := append([]catalogThread(nil), m.all...)
	query := m.defaultQuery
	m.mu.RUnlock()
	positions := make(map[string]int, len(all))
	for i := range all {
		positions[all[i].ID] = i
	}
	for _, row := range rows {
		if index, ok := positions[row.ID]; ok {
			all[index] = row
			continue
		}
		positions[row.ID] = len(all)
		all = append(all, row)
	}
	active, nodes := projectSidebar(all, query, version)
	m.replace(all, query, active, nodes, version)
}

func diffSidebar(previous, next sidebarActiveResponse) *sidebarDelta {
	if sidebarResponsesEqual(previous, next) {
		return nil
	}
	old := make(map[string]sidebarThread, len(previous.Items))
	for _, row := range previous.Items {
		old[row.ID] = row
	}
	rows := make([]sidebarThread, 0)
	for _, row := range next.Items {
		if before, ok := old[row.ID]; !ok || !reflect.DeepEqual(before, row) {
			rows = append(rows, row)
		}
		delete(old, row.ID)
	}
	removed := make([]string, 0, len(old))
	for id := range old {
		removed = append(removed, id)
	}
	sort.Strings(removed)
	return &sidebarDelta{
		Generation: next.Generation, Revision: next.Revision, Rows: rows, RemovedIDs: removed,
		Roots: append([]string(nil), next.Roots...), HistoricalCount: next.HistoricalCount,
		CatalogVersion: next.CatalogVersion,
	}
}

func sidebarResponsesEqual(left, right sidebarActiveResponse) bool {
	left.Generation, left.Revision = "", 0
	right.Generation, right.Revision = "", 0
	return reflect.DeepEqual(left, right)
}

func projectSidebar(all []catalogThread, query catalogListQuery, version int64) (sidebarActiveResponse, []*sidebarNode) {
	full := indexSidebarThreads(all)
	selected := map[string]bool{}
	for _, node := range full.order {
		if !sidebarThreadInPopulation(node.thread, query) {
			continue
		}
		cursor := node
		seen := map[string]bool{}
		for cursor != nil && !seen[cursor.thread.ID] {
			seen[cursor.thread.ID] = true
			selected[cursor.thread.ID] = true
			if cursor.parentID == "" {
				break
			}
			cursor = full.byID[cursor.parentID]
		}
	}
	population := make([]catalogThread, 0, len(selected))
	for _, thread := range all {
		if selected[thread.ID] {
			population = append(population, thread)
		}
	}
	nodes := assembleSidebarThreads(population)
	included := make([]*sidebarNode, 0)
	roots := make([]string, 0)
	historical := 0
	for _, node := range nodes {
		if !node.included {
			historical++
			continue
		}
		included = append(included, node)
		if node.parentID == "" {
			roots = append(roots, node.thread.ID)
		}
	}
	items := make([]sidebarThread, 0, len(included))
	for _, node := range included {
		items = append(items, sidebarThreadFromNode(node))
	}
	return sidebarActiveResponse{
		Items: items, Roots: roots, HistoricalCount: historical, CatalogVersion: version,
	}, nodes
}

func sidebarThreadInPopulation(thread catalogThread, query catalogListQuery) bool {
	if thread.Transient || thread.Runtime != nil || thread.Starred {
		return true
	}
	if thread.Kind == string(history.ThreadGuardian) || thread.Interaction == string(history.InteractionProgrammatic) {
		return false
	}
	updated := parseCatalogTimePtr(thread.UpdatedAt)
	if query.UpdatedFrom != nil && (updated == nil || updated.Before(*query.UpdatedFrom)) {
		return false
	}
	if query.UpdatedTo != nil && (updated == nil || updated.After(*query.UpdatedTo)) {
		return false
	}
	return true
}

func assembleSidebarThreads(threads []catalogThread) []*sidebarNode {
	index := indexSidebarThreads(threads)
	breakSidebarCycles(index)
	for _, node := range index.order {
		node.independent = sidebarIndependentlyActive(node.thread)
	}
	for _, node := range index.order {
		if !node.independent {
			continue
		}
		cursor := node
		seen := map[string]bool{}
		for cursor != nil && !seen[cursor.thread.ID] {
			seen[cursor.thread.ID] = true
			cursor.included = true
			if !cursor.independent {
				cursor.structural = true
			}
			if cursor.parentID == "" {
				break
			}
			cursor = index.byID[cursor.parentID]
		}
	}
	for _, node := range index.order {
		if node.independent {
			node.structural = false
			node.included = true
		}
	}
	buildSidebarTree(index)
	return index.order
}

func pageSidebarHistory(nodes []*sidebarNode, parentRef string, cursor *history.ThreadCursor, limit int, version int64, revision uint64) (sidebarActiveResponse, error) {
	parent := findSidebarNode(nodes, parentRef)
	if parent == nil {
		return sidebarActiveResponse{}, errCatalogThreadNotFound
	}
	descendants := sidebarDescendants(nodes, parent.thread.ID)
	inactive := make([]*sidebarNode, 0)
	for _, node := range descendants {
		if !node.included {
			inactive = append(inactive, node)
		}
	}
	sort.SliceStable(inactive, func(i, j int) bool {
		return sidebarUpdatedAfter(inactive[i].thread, inactive[j].thread)
	})
	start := 0
	if cursor != nil {
		start = -1
		for i, node := range inactive {
			if node.thread.ID == cursor.ID {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return sidebarActiveResponse{}, errInvalidThreadCatalogCursor
		}
	}
	end := start + limit
	if end > len(inactive) {
		end = len(inactive)
	}
	items := make([]sidebarThread, 0, end-start)
	for _, node := range inactive[start:end] {
		item := sidebarThreadFromNode(node)
		item.Sidebar.Active = false
		item.Sidebar.Structural = false
		item.Sidebar.ChildrenIDs = append([]string{}, node.allChildren...)
		items = append(items, item)
	}
	result := sidebarActiveResponse{Items: items, Roots: []string{}, CatalogVersion: version, Revision: revision}
	if end < len(inactive) {
		last := inactive[end-1].thread
		encoded, err := encodeThreadCatalogCursor(&history.ThreadCursor{ID: last.ID, UpdatedAt: parseCatalogTimePtr(last.UpdatedAt)})
		if err != nil {
			return sidebarActiveResponse{}, err
		}
		result.NextCursor = &encoded
	}
	return result, nil
}

// sidebarRebuildMinInterval bounds catalog-driven full sidebar rebuilds. A
// rebuild reads the whole sidebar graph from SQLite (seconds of CPU on a large
// catalog), and in a busy spell catalog bursts past the hub's id limit turned
// into back-to-back rebuilds. Runtime refreshes stay immediate: they change
// session identity, which the sidebar must show at once. Row repairs and
// runtime deltas are not paced.
const sidebarRebuildMinInterval = 5 * time.Second

// sidebarRebuildPacer turns full-rebuild requests into at most one rebuild per
// interval: the first runs at once, and requests inside the interval share one
// trailing rebuild at its end, which reads everything they asked for.
type sidebarRebuildPacer struct {
	interval time.Duration
	last     time.Time
	waiting  bool
}

// request reports how long to wait before rebuilding (0 means now), and false
// when a trailing rebuild is already scheduled to cover this request.
func (p *sidebarRebuildPacer) request(now time.Time) (time.Duration, bool) {
	if p.waiting {
		return 0, false
	}
	if wait := p.interval - now.Sub(p.last); !p.last.IsZero() && wait > 0 {
		p.waiting = true
		return wait, true
	}
	return 0, true
}

func (p *sidebarRebuildPacer) rebuilt(now time.Time) {
	p.last = now
	p.waiting = false
}

func (s *server) startSidebarMaterializer(ctx context.Context, now time.Time) error {
	if s.sidebar == nil {
		s.sidebar = newSidebarMaterializedState()
	}
	var catalogEvents <-chan catalogChange
	var stopCatalog func()
	if stream, ok := s.catalog.(threadCatalogStream); ok {
		catalogEvents, stopCatalog = stream.SubscribeCatalog()
	}
	var runtimeEvents <-chan runtimeDelta
	var runtimeChannel chan runtimeDelta
	if s.runtimeEvents != nil {
		runtimeChannel = s.runtimeEvents.subscribe()
		runtimeEvents = runtimeChannel
	}
	err := s.materializeSidebar(ctx, now)
	pacer := sidebarRebuildPacer{interval: sidebarRebuildMinInterval, last: now}
	go func() {
		if stopCatalog != nil {
			defer stopCatalog()
		}
		if runtimeChannel != nil {
			defer s.runtimeEvents.unsubscribe(runtimeChannel)
		}
		window := time.NewTicker(time.Hour)
		defer window.Stop()
		var trailing <-chan time.Time
		deferred := 0 // catalog requests the trailing rebuild will cover
		// A successful rebuild reads everything committed so far, so it also
		// covers a trailing catalog rebuild that was waiting. A failed one
		// leaves that waiting rebuild in place.
		rebuild := func(reason string) {
			start := time.Now()
			if rebuildErr := s.materializeSidebar(ctx, start); rebuildErr != nil {
				log.Printf("sidebar %s rebuild: %v", reason, rebuildErr)
				return
			}
			pacer.rebuilt(start)
			trailing = nil
			log.Printf("sidebar full rebuild (%s, %d deferred) took %s", reason, deferred, time.Since(start).Round(time.Millisecond))
			deferred = 0
		}
		requestCatalogRebuild := func() {
			wait, ok := pacer.request(time.Now())
			if !ok {
				deferred++
				return
			}
			if wait > 0 {
				deferred++
				trailing = time.After(wait)
				return
			}
			rebuild("catalog")
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-window.C:
				s.sidebar.advanceWindow(time.Now())
			case <-trailing:
				// The timer is spent: a failed read must not leave the pacer
				// believing a rebuild is still scheduled.
				trailing = nil
				pacer.waiting = false
				rebuild("catalog, paced")
			case change, ok := <-catalogEvents:
				if !ok {
					catalogEvents = nil
					continue
				}
				if change.Refresh || len(change.ThreadIDs) == 0 || !s.repairSidebarCatalogRows(ctx, change) {
					requestCatalogRebuild()
				}
			case delta, ok := <-runtimeEvents:
				if !ok {
					runtimeEvents = nil
					continue
				}
				// A refresh marker may carry no rows, so it is never applied as
				// a snapshot; the rebuild reads the runtime state itself.
				if delta.sidebarCurrent || !delta.rebuild && !delta.Refresh {
					s.sidebar.applyRuntimeDelta(delta)
				} else {
					rebuild("runtime")
				}
			}
		}
	}()
	return err
}

// repairSidebarCatalogRows re-reads only the named rows. It reports false when
// a row is gone, which only a full rebuild can reflect.
func (s *server) repairSidebarCatalogRows(ctx context.Context, change catalogChange) bool {
	source, err := s.sidebarGraphCatalog()
	if err != nil {
		log.Printf("sidebar catalog repair: %v", err)
		return true
	}
	extra := make([]history.SidebarGraphKey, 0, len(change.ThreadIDs))
	for _, id := range change.ThreadIDs {
		extra = append(extra, history.SidebarGraphKey{ID: id})
	}
	graph, err := source.ListSidebarGraph(ctx, history.SidebarGraphParams{Extra: extra, OnlyExtra: true})
	if err != nil {
		log.Printf("sidebar catalog repair: %v", err)
		return true
	}
	if len(graph.Rows) == 0 {
		return false
	}
	rows := s.graphRowsToThreads(graph.Rows, time.Now())
	s.composeCatalogThreads(ctx, rows, s.sidebarRuntimeSnapshot(), true)
	version := catalogVersion(change.Version)
	if graphVersion := catalogVersion(graph.CatalogVersion); graphVersion > version {
		version = graphVersion
	}
	s.sidebar.applyCatalogRows(rows, version)
	return true
}

func (m *sidebarMaterializedState) advanceWindow(now time.Time) {
	if m == nil {
		return
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	m.mu.RLock()
	if !m.ready {
		m.mu.RUnlock()
		return
	}
	all := append([]catalogThread(nil), m.all...)
	version := m.version
	m.mu.RUnlock()
	query := sidebarDefaultQuery(now)
	active, nodes := projectSidebar(all, query, version)
	m.replace(all, query, active, nodes, version)
}
