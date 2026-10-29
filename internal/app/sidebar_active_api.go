package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

const (
	sidebarHistoryDefaultLimit = 100
	sidebarMetadataEvidence    = "canonical_hosted_metadata"
)

type sidebarGraphSource interface {
	ListSidebarGraph(context.Context, history.SidebarGraphParams) (history.SidebarGraph, error)
	LoadThreadsByIDs(context.Context, []string) ([]history.Thread, error)
}

type sidebarDescendantActivity struct {
	Total   int `json:"total"`
	Active  int `json:"active"`
	Workers int `json:"workers"`
}

type sidebarSort struct {
	Title     string `json:"title"`
	Project   string `json:"project"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Liveness  int    `json:"liveness"`
}

type sidebarBlock struct {
	ParentID                  *string                   `json:"parentId"`
	RootID                    string                    `json:"rootId"`
	AncestorIDs               []string                  `json:"ancestorIds"`
	ChildrenIDs               []string                  `json:"childrenIds"`
	Depth                     int                       `json:"depth"`
	Active                    bool                      `json:"active"`
	Structural                bool                      `json:"structural"`
	HistoricalDescendantCount int                       `json:"historicalDescendantCount"`
	DescendantActivity        sidebarDescendantActivity `json:"descendantActivity"`
	Sort                      sidebarSort               `json:"sort"`
	Lineage                   string                    `json:"lineage,omitempty"`
}

type sidebarThread struct {
	sidebar.Thread
	Sidebar sidebarBlock `json:"sidebar"`
}

type sidebarActiveResponse struct {
	Items           []sidebarThread `json:"items"`
	Roots           []string        `json:"roots"`
	HistoricalCount int             `json:"historicalCount"`
	CatalogVersion  int64           `json:"catalogVersion"`
	Generation      string          `json:"generation,omitempty"` // see sidebarDelta
	Revision        uint64          `json:"revision"`
	NextCursor      *string         `json:"nextCursor,omitempty"`
}

type sidebarNode struct {
	thread      sidebar.Thread
	independent bool
	structural  bool
	included    bool
	parentID    string
	lineage     string
	children    []string
	allChildren []string
	ancestorIDs []string
	rootID      string
	depth       int
	activity    sidebarDescendantActivity
	historical  int
}

type sidebarRuntimeSnapshot struct {
	overlays  map[attachmentKey]sidebar.RuntimeInfo
	batched   bool
	summaries []sessions.Summary
}

func (s *server) handleSidebarThreads(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	now := time.Now()
	query, err := parseSidebarActiveQuery(r, now)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	result, err := s.sidebar.activeSnapshot(query, strings.TrimSpace(r.URL.Query().Get("updatedFrom")) == "")
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	writeCatalogJSON(w, http.StatusOK, result)
}

func (s *server) handleSidebarThreadHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	id := r.PathValue("threadId")
	if strings.TrimSpace(id) == "" {
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
		return
	}
	decoded, err := url.PathUnescape(id)
	if err != nil || strings.TrimSpace(decoded) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	query, err := parseSidebarActiveQuery(r, time.Now())
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	limit := sidebarHistoryDefaultLimit
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		parsed, err := parsePositiveLimit(rawLimit, sidebarHistoryDefaultLimit)
		if err != nil {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		limit = parsed
	}
	var cursor *history.ThreadCursor
	if rawCursor := strings.TrimSpace(r.URL.Query().Get("cursor")); rawCursor != "" {
		cursor, err = threadindex.DecodeThreadCatalogCursor(rawCursor)
		if err != nil {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_cursor", "Invalid thread catalog cursor")
			return
		}
	}
	result, err := s.sidebar.historySnapshot(query, strings.TrimSpace(r.URL.Query().Get("updatedFrom")) == "", decoded, cursor, limit)
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	writeCatalogJSON(w, http.StatusOK, result)
}

func parsePositiveLimit(raw string, fallback int) (int, error) {
	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil || value <= 0 {
		return fallback, fmt.Errorf("limit must be a positive integer")
	}
	if value > sidebarHistoryDefaultLimit {
		return sidebarHistoryDefaultLimit, nil
	}
	return value, nil
}

func parseSidebarActiveQuery(r *http.Request, now time.Time) (threadindex.ListQuery, error) {
	query := sidebarBaseQuery()
	rawFrom := strings.TrimSpace(r.URL.Query().Get("updatedFrom"))
	if rawFrom == "" {
		from := now.Add(-30 * 24 * time.Hour)
		query.UpdatedFrom = &from
		return query, nil
	}
	parsed, err := time.Parse(time.RFC3339, rawFrom)
	if err != nil {
		return query, fmt.Errorf("updatedFrom must be an RFC3339 timestamp")
	}
	query.UpdatedFrom = &parsed
	return query, nil
}

func (s *server) sidebarGraphCatalog() (sidebarGraphSource, error) {
	if s.catalog == nil {
		return nil, threadindex.ErrThreadCatalogUnavailable
	}
	source, ok := s.catalog.(sidebarGraphSource)
	if !ok {
		return nil, threadindex.ErrThreadCatalogUnavailable
	}
	return source, nil
}

func (s *server) loadSidebarPopulation(ctx context.Context, query threadindex.ListQuery, runtime sidebarRuntimeSnapshot, now time.Time) (history.SidebarGraph, []sidebar.Thread, error) {
	source, err := s.sidebarGraphCatalog()
	if err != nil {
		return history.SidebarGraph{}, nil, err
	}
	params := history.SidebarGraphParams{
		List: history.ListThreadsParams{
			IncludeSubagents:    true,
			IncludeProgrammatic: false,
			Archived:            history.ArchivedFilter(query.Archived),
			UpdatedFrom:         query.UpdatedFrom,
			UpdatedTo:           query.UpdatedTo,
		},
		Extra: s.sidebarRescueKeys(),
	}
	graph, err := source.ListSidebarGraph(ctx, params)
	if err != nil {
		return history.SidebarGraph{}, nil, err
	}
	seen := map[string]bool{}
	for _, row := range graph.Rows {
		seen[row.Thread.ID] = true
	}
	var threads []sidebar.Thread
	for {
		threads = s.graphRowsToThreads(graph.Rows, now)
		// Hosted identity and assignment edges are needed to discover missing
		// parents. Keep this overlaid population for assembly so the expensive
		// control-store projection is not repeated for every catalog row.
		s.attachRuntimeOverlayBatch(ctx, threads, runtime)
		missing := s.missingSidebarParents(threads)
		if len(missing) == 0 {
			break
		}
		extra, err := source.ListSidebarGraph(ctx, history.SidebarGraphParams{Extra: missing, OnlyExtra: true})
		if err != nil {
			return history.SidebarGraph{}, nil, err
		}
		added := 0
		for _, row := range extra.Rows {
			if seen[row.Thread.ID] {
				continue
			}
			seen[row.Thread.ID] = true
			graph.Rows = append(graph.Rows, row)
			added++
		}
		if extra.CatalogVersion > graph.CatalogVersion {
			graph.CatalogVersion = extra.CatalogVersion
		}
		if added == 0 {
			break
		}
	}
	return graph, threads, nil
}

func (s *server) sidebarRescueKeys() []history.SidebarGraphKey {
	seen := map[string]bool{}
	var keys []history.SidebarGraphKey
	add := func(provider, native string) {
		if provider == "" || native == "" {
			return
		}
		id := provider + "\x00" + native
		if seen[id] {
			return
		}
		seen[id] = true
		keys = append(keys, history.SidebarGraphKey{Provider: history.Provider(provider), NativeID: native})
	}
	// Attached first (live sessions, oldest first), then detached records.
	for _, sess := range s.volatileSessionSummaries() {
		add(sess.Provider, sess.ThreadID)
	}
	if s.attachments != nil {
		for _, rec := range s.attachments.DetachedExcept(nil) {
			add(rec.Key().Provider, rec.Key().NativeThreadID)
		}
	}
	if s.threadStars != nil {
		for _, key := range s.threadStars.StarredIdentities() {
			add(key.Provider, key.NativeThreadID)
		}
	}
	return keys
}

func (s *server) graphRowsToThreads(rows []history.SidebarGraphRow, now time.Time) []sidebar.Thread {
	out := make([]sidebar.Thread, 0, len(rows))
	for _, row := range rows {
		thread := mapCatalogThread(row.Thread, nil)
		court := fleetCourtFromHistory(row.Court)
		thread.Court = court
		thread.Fleet = court.State(thread.ID, now)
		out = append(out, thread)
	}
	return out
}

func fleetCourtFromHistory(record history.FleetCourtMessage) fleetCourtMessage {
	court := fleetCourtMessage{ItemType: string(record.ItemType), Head: record.Head, Tail: record.Tail, Interrupted: record.Interrupted}
	if record.LastActivity != nil {
		court.LastActivity = *record.LastActivity
	}
	return court
}

func (s *server) attachRuntimeOverlayBatch(ctx context.Context, threads []sidebar.Thread, runtime sidebarRuntimeSnapshot) {
	hostedIDs := make([]string, 0, len(threads))
	hostedRecords, hostedRecordOK := s.sidebarHostedRecords(threads)
	for i := range threads {
		if !threads[i].Transient || threads[i].NativeID != "" {
			threads[i].Runtime = nil
		}
		threads[i].Assignment = nil
		threads[i].AssignmentUnavailable = false
		if runtime.batched {
			if info, ok := runtime.overlays[attachmentKey{Provider: threads[i].Provider, NativeThreadID: threads[i].NativeID}]; ok {
				threads[i].Runtime = &info
			}
		} else if s.runtime != nil {
			if info, ok := s.runtime.RuntimeFor(threads[i].Provider, threads[i].NativeID); ok {
				threads[i].Runtime = &info
			}
		}
		if !hostedRecordOK[i] {
			continue
		}
		record := hostedRecords[i]
		threads[i].HostedThreadID = record.ID
		threads[i].HostedStatus = record.Status
		threads[i].InvokerHostedThreadID = record.InvokerHostedThreadID
		threads[i].InvokerNativeThreadID = record.InvokerNativeThreadID
		threads[i].InvokerProvider = record.InvokerProvider
		threads[i].SidebarPlacement = record.SidebarPlacement
		hostedIDs = append(hostedIDs, record.ID)
	}
	controlAvailable := s.controlMetadata != nil && s.controlMetadataErr == "" && s.controlMetadata.CanonicalDatabase().PingContext(ctx) == nil
	assignments := map[string]*orchestrationAssignmentView{}
	metadata := map[string]threadindex.Metadata{}
	if controlAvailable {
		assignments = s.controlMetadata.CurrentAssignmentsByHostedIDs(hostedIDs)
		metadata = catalogMetadataByHostedIDs(s.controlMetadata, hostedIDs)
	}
	for i := range threads {
		hostedID := threads[i].HostedThreadID
		if hostedID == "" {
			continue
		}
		// Provider transcripts are evidence for non-hosted rows only. Once Nano
		// knows the hosted identity, absent canonical control metadata means
		// unknown; it must not fall back to stale provider posture.
		threads[i].Metadata.CWD = unknownCatalogValue()
		threads[i].Metadata.Model = unknownCatalogValue()
		threads[i].Metadata.Effort = unknownCatalogValue()
		threads[i].Metadata.Priority = unknownCatalogValue()
		threads[i].Metadata.Permission = unknownCatalogValue()
		if !controlAvailable {
			threads[i].AssignmentUnavailable = true
			continue
		}
		if assignment, ok := assignments[hostedID]; ok {
			threads[i].Assignment = assignment
		}
		if meta, ok := metadata[hostedID]; ok {
			threads[i].Metadata.CWD = meta.CWD
			threads[i].Metadata.Model = meta.Model
			threads[i].Metadata.Effort = meta.Effort
			threads[i].Metadata.Priority = meta.Priority
			threads[i].Metadata.Permission = meta.Permission
		}
	}
	for i := range threads {
		if nanoHolds(threads[i]) {
			threads[i].Fleet = liveFleetState(threads[i])
		}
	}
}

// sidebarRuntimeSnapshot snapshots the server-owned runtime population once.
// RuntimeFor is intentionally a point helper; using it for every graph row
// repeatedly scans and summarizes the complete hosted session registry.
func (s *server) sidebarRuntimeSnapshot() sidebarRuntimeSnapshot {
	summaries := s.volatileSessionSummaries()
	runtimeServer, ok := s.runtime.(*server)
	if !ok || runtimeServer != s {
		return sidebarRuntimeSnapshot{summaries: summaries}
	}
	return sidebarRuntimeSnapshot{overlays: s.runtimeOverlays(summaries), batched: true, summaries: summaries}
}

// volatileSessionSummaries snapshots every live session from memory alone,
// oldest first: identity, lifecycle, turn and attention fields, with no
// control-store read. Every fleet-wide caller (runtime deltas, list rows,
// identity and reference resolution) uses it; canonical metadata is applied
// afterward in one batch, never once per session.
func (s *server) volatileSessionSummaries() []sessions.Summary {
	needsAttention := map[string]bool{}
	if s.agentExecutions != nil {
		needsAttention = s.agentExecutions.SessionsNeedingAttention()
	}
	return s.VolatileSummaries(needsAttention)
}

// sidebarHostedRecords resolves one batch of catalog references through the hosted owner.
func (s *server) sidebarHostedRecords(threads []sidebar.Thread) ([]agentSessionRecord, []bool) {
	refs := make([]recordReference, len(threads))
	for i, thread := range threads {
		refs[i] = recordReference{HostedThreadID: thread.HostedThreadID, Provider: thread.Provider, NativeThreadID: thread.NativeID}
	}
	return s.agentExecutions.RecordsForReferences(refs)
}

func catalogMetadataByHostedIDs(store *controlMetadataStore, ids []string) map[string]threadindex.Metadata {
	out := map[string]threadindex.Metadata{}
	views := store.CurrentMetadataByHostedIDs(ids)
	canonical := func(fields map[string]metadataCurrentView, field string) threadindex.Value {
		value, ok := hostedstore.MetadataString(fields[field])
		if !ok {
			return unknownCatalogValue()
		}
		evidence := sidebarMetadataEvidence
		return threadindex.Value{State: string(history.MetadataKnown), Value: &value, Evidence: &evidence}
	}
	for hostedID, fields := range views {
		out[hostedID] = threadindex.Metadata{
			CWD: canonical(fields, "cwd"), Model: canonical(fields, hostedstore.PostureFieldModel),
			Effort: canonical(fields, hostedstore.PostureFieldEffort), Priority: canonical(fields, hostedstore.PostureFieldServiceTier),
			Permission: canonical(fields, hostedstore.PostureFieldPermissionMode),
		}
	}
	return out
}
func uniqueStrings(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (s *server) assembleSidebarGraph(ctx context.Context, graph history.SidebarGraph, threads []sidebar.Thread, runtime sidebarRuntimeSnapshot) ([]*sidebarNode, int64) {
	memoryRows := s.memorySidebarThreadRows(threads, runtime.summaries)
	if len(memoryRows) > 0 {
		s.attachRuntimeOverlayBatch(ctx, memoryRows, runtime)
		threads = append(threads, memoryRows...)
	}
	s.attachStarredOverlay(threads)
	return assembleSidebarThreads(threads), threadindex.CatalogVersion(graph.CatalogVersion)
}

// memorySidebarThreadRows can trust the sidebar graph's rescue keys: every
// live native identity was requested explicitly, so an absent row is not
// indexed and needs a transient memory row. This avoids one catalog point
// query per newly starting session on every sidebar request.
func (s *server) memorySidebarThreadRows(durable []sidebar.Thread, live []sessions.Summary) []sidebar.Thread {
	if len(live) == 0 {
		return nil
	}
	onPage := make(map[attachmentKey]bool, len(durable))
	for _, item := range durable {
		if item.NativeID != "" {
			onPage[attachmentKey{Provider: item.Provider, NativeThreadID: item.NativeID}] = true
		}
	}
	rows := make([]sidebar.Thread, 0)
	for _, sess := range live {
		if sess.ThreadID != "" && onPage[attachmentKey{Provider: sess.Provider, NativeThreadID: sess.ThreadID}] {
			continue
		}
		rows = append(rows, memoryThreadRow(sess))
	}
	return rows
}

func (s *server) missingSidebarParents(threads []sidebar.Thread) []history.SidebarGraphKey {
	index := map[string]sidebar.Thread{}
	for _, thread := range threads {
		for _, key := range sidebarIdentityKeys(thread) {
			index[key] = thread
		}
	}
	var missing []history.SidebarGraphKey
	seen := map[string]bool{}
	for _, thread := range threads {
		for _, key := range sidebarParentKeys(thread) {
			if _, ok := index[key]; ok {
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			provider, native, hosted := parseSidebarKey(key)
			if hosted != "" {
				if s.agentExecutions != nil {
					if record, ok := s.agentExecutions.Session(hosted); ok && record.NativeThreadID != "" {
						missing = append(missing, history.SidebarGraphKey{Provider: history.Provider(record.Provider), NativeID: record.NativeThreadID})
					}
				}
				continue
			}
			if provider != "" && native != "" {
				missing = append(missing, history.SidebarGraphKey{Provider: history.Provider(provider), NativeID: native})
			}
		}
	}
	return missing
}

type sidebarIndex struct {
	byID  map[string]*sidebarNode
	byKey map[string]*sidebarNode
	order []*sidebarNode
}

func indexSidebarThreads(threads []sidebar.Thread) *sidebarIndex {
	index := &sidebarIndex{byID: map[string]*sidebarNode{}, byKey: map[string]*sidebarNode{}, order: make([]*sidebarNode, 0, len(threads))}
	for _, thread := range threads {
		if _, ok := index.byID[thread.ID]; ok {
			continue
		}
		node := &sidebarNode{thread: thread, children: []string{}, allChildren: []string{}, ancestorIDs: []string{}}
		index.byID[thread.ID] = node
		index.order = append(index.order, node)
		for _, key := range sidebarIdentityKeys(thread) {
			index.byKey[key] = node
		}
	}
	for _, node := range index.order {
		if parent := resolveSidebarParent(node.thread, index); parent != nil && parent.thread.ID != node.thread.ID {
			node.parentID = parent.thread.ID
		}
	}
	return index
}

func sidebarIdentityKeys(thread sidebar.Thread) []string {
	keys := []string{thread.ID}
	if thread.HostedThreadID != "" {
		keys = append(keys, "hosted:"+thread.HostedThreadID)
	}
	if thread.NativeID != "" {
		keys = append(keys, thread.Provider+":"+thread.NativeID)
	}
	return keys
}

func sidebarParentKeys(thread sidebar.Thread) []string {
	keys := []string{}
	push := func(key string) {
		if key != "" {
			keys = append(keys, key)
		}
	}
	if thread.Assignment != nil && !thread.AssignmentUnavailable && thread.Assignment.RelatedHostedThreadID != "" {
		push("hosted:" + thread.Assignment.RelatedHostedThreadID)
		if thread.Assignment.RelatedHostedThreadID == thread.InvokerHostedThreadID && thread.InvokerNativeThreadID != "" {
			provider := thread.InvokerProvider
			if provider == "" {
				provider = thread.Provider
			}
			push(provider + ":" + thread.InvokerNativeThreadID)
		}
		return keys
	}
	if thread.SidebarPlacement == sidebarPlacementStandalone {
		return keys
	}
	if thread.InvokerHostedThreadID != "" {
		push("hosted:" + thread.InvokerHostedThreadID)
	}
	if thread.InvokerNativeThreadID != "" {
		provider := thread.InvokerProvider
		if provider == "" {
			provider = thread.Provider
		}
		push(provider + ":" + thread.InvokerNativeThreadID)
	}
	if thread.ParentNativeID != "" {
		push(thread.Provider + ":" + thread.ParentNativeID)
	}
	return keys
}

func parseSidebarKey(key string) (provider, native, hosted string) {
	if strings.HasPrefix(key, "hosted:") {
		return "", "", strings.TrimPrefix(key, "hosted:")
	}
	provider, native, _ = strings.Cut(key, ":")
	return provider, native, ""
}

func resolveSidebarParent(thread sidebar.Thread, index *sidebarIndex) *sidebarNode {
	for _, key := range sidebarParentKeys(thread) {
		if node := index.byKey[key]; node != nil {
			return node
		}
	}
	return nil
}

func breakSidebarCycles(index *sidebarIndex) {
	color := map[string]int{}
	var stack []string
	var visit func(id string)
	visit = func(id string) {
		color[id] = 1
		stack = append(stack, id)
		node := index.byID[id]
		if node != nil && node.parentID != "" {
			switch color[node.parentID] {
			case 0:
				visit(node.parentID)
			case 1:
				cycleStart := 0
				for i, item := range stack {
					if item == node.parentID {
						cycleStart = i
						break
					}
				}
				cycle := append([]string{}, stack[cycleStart:]...)
				sort.Strings(cycle)
				root := cycle[0]
				broken := index.byID[root]
				if broken != nil {
					broken.parentID = ""
					broken.lineage = "cycle_broken"
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = 2
	}
	for _, node := range index.order {
		if color[node.thread.ID] == 0 {
			visit(node.thread.ID)
		}
	}
}

func buildSidebarTree(index *sidebarIndex) {
	for _, node := range index.order {
		node.children = node.children[:0]
		node.allChildren = node.allChildren[:0]
	}
	for _, node := range index.order {
		if node.parentID == "" {
			continue
		}
		parent := index.byID[node.parentID]
		if parent == nil {
			node.parentID = ""
			continue
		}
		parent.allChildren = append(parent.allChildren, node.thread.ID)
		if node.included && parent.included {
			parent.children = append(parent.children, node.thread.ID)
		}
	}
	var walk func(node *sidebarNode, ancestors []string)
	walk = func(node *sidebarNode, ancestors []string) {
		node.ancestorIDs = append([]string{}, ancestors...)
		if len(ancestors) == 0 {
			node.rootID = node.thread.ID
			node.depth = 0
		} else {
			node.rootID = ancestors[0]
			node.depth = len(ancestors)
		}
		next := append(append([]string{}, ancestors...), node.thread.ID)
		for _, childID := range node.allChildren {
			if child := index.byID[childID]; child != nil {
				walk(child, next)
			}
		}
	}
	for _, node := range index.order {
		if node.parentID == "" {
			walk(node, nil)
		}
	}
	var rollup func(node *sidebarNode) (sidebarDescendantActivity, int)
	rollup = func(node *sidebarNode) (sidebarDescendantActivity, int) {
		var activity sidebarDescendantActivity
		historical := 0
		for _, childID := range node.allChildren {
			child := index.byID[childID]
			if child == nil {
				continue
			}
			activity.Total++
			if child.independent {
				activity.Active++
			}
			if resolveThreadRoleCatalog(child.thread) == "worker" {
				activity.Workers++
			}
			if !child.included {
				historical++
			}
			childActivity, childHistorical := rollup(child)
			activity.Total += childActivity.Total
			activity.Active += childActivity.Active
			activity.Workers += childActivity.Workers
			historical += childHistorical
		}
		node.activity = activity
		node.historical = historical
		return activity, historical
	}
	for _, node := range index.order {
		if node.parentID == "" {
			rollup(node)
		}
	}
}

func sidebarDescendants(nodes []*sidebarNode, parentID string) []*sidebarNode {
	index := map[string]*sidebarNode{}
	children := map[string][]*sidebarNode{}
	for _, node := range nodes {
		index[node.thread.ID] = node
		if node.parentID != "" {
			children[node.parentID] = append(children[node.parentID], node)
		}
	}
	out := []*sidebarNode{}
	var walk func(id string)
	walk = func(id string) {
		for _, child := range children[id] {
			out = append(out, child)
			walk(child.thread.ID)
		}
	}
	walk(parentID)
	return out
}

func findSidebarNode(nodes []*sidebarNode, ref string) *sidebarNode {
	for _, node := range nodes {
		if node.thread.ID == ref || node.thread.NativeID == ref || node.thread.HostedThreadID == ref {
			return node
		}
	}
	return nil
}

func sidebarIndependentlyActive(thread sidebar.Thread) bool {
	if thread.Transient {
		return true
	}
	// A detached attachment is a reversible resume handle, not active work.
	// Stars may still pin it below, while Explorer/history remains the durable
	// place to find an unstarred detached row.
	if thread.Runtime != nil && thread.Runtime.Attached {
		return true
	}
	// A thread resting since a restart is held like an attached one; it
	// attaches once a browser shows it, so it keeps its place.
	if thread.Runtime != nil && thread.Runtime.Status == "resting" {
		return true
	}
	if thread.Starred {
		return true
	}
	// Provider transcripts can still look working or needs-you after Nano has
	// explicitly closed their hosted record. That durable close wins for the
	// active sidebar. Stars remain a separate operator fact above this branch.
	if thread.Fleet != nil && thread.HostedStatus != sessions.StatusExited {
		switch thread.Fleet.Label {
		case fleetWorking, fleetNeedsYou:
			return true
		}
	}
	return false
}

func resolveThreadRoleCatalog(thread sidebar.Thread) string {
	if thread.AssignmentUnavailable {
		return ""
	}
	if thread.HostedThreadID != "" && thread.Assignment != nil {
		role := thread.Assignment.AssignmentRole
		if role == "worker" || role == "auditor" {
			return role
		}
	}
	if thread.HostedThreadID == "" && (thread.Kind == string(history.ThreadSubagent)) {
		return "worker"
	}
	return ""
}

func sidebarThreadFromNode(node *sidebarNode) sidebarThread {
	var parent *string
	if node.parentID != "" {
		id := node.parentID
		parent = &id
	}
	children := node.children
	if children == nil {
		children = []string{}
	}
	ancestors := node.ancestorIDs
	if ancestors == nil {
		ancestors = []string{}
	}
	thread := node.thread
	return sidebarThread{
		Thread: thread,
		Sidebar: sidebarBlock{
			ParentID: parent, RootID: node.rootID, AncestorIDs: ancestors, ChildrenIDs: children,
			Depth: node.depth, Active: node.independent && node.included, Structural: node.structural,
			HistoricalDescendantCount: node.historical, DescendantActivity: node.activity,
			Sort: sidebarSortPrimitives(thread), Lineage: node.lineage,
		},
	}
}

func sidebarSortPrimitives(thread sidebar.Thread) sidebarSort {
	title := strings.ToLower(sidebarTitle(thread))
	project := ""
	if thread.Metadata.CWD.State == string(history.MetadataKnown) && thread.Metadata.CWD.Value != nil {
		project = strings.ToLower(*thread.Metadata.CWD.Value)
	}
	return sidebarSort{
		Title: title, Project: project,
		CreatedAt: catalogTimeMillis(thread.CreatedAt), UpdatedAt: catalogTimeMillis(thread.UpdatedAt),
		Liveness: sidebarLivenessRank(thread),
	}
}

func sidebarTitle(thread sidebar.Thread) string {
	if thread.Name != nil && thread.Name.Value != "" {
		return thread.Name.Value
	}
	if thread.Title != nil && *thread.Title != "" {
		return *thread.Title
	}
	if thread.Preview != nil {
		return *thread.Preview
	}
	return ""
}

func sidebarLivenessRank(thread sidebar.Thread) int {
	if sidebarSelfWork(thread) {
		return 0
	}
	if thread.Runtime != nil && thread.Runtime.Attached {
		return 1
	}
	if thread.Runtime != nil && thread.Runtime.Detached {
		return 2
	}
	return 3
}

func sidebarSelfWork(thread sidebar.Thread) bool {
	return thread.Fleet != nil && thread.Fleet.Label == fleetWorking
}

func catalogTimeMillis(value *string) int64 {
	parsed := parseCatalogTimePtr(value)
	if parsed == nil {
		return 0
	}
	return parsed.UnixMilli()
}

func parseCatalogTimePtr(value *string) *time.Time {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, *value)
	}
	if err != nil {
		return nil
	}
	return &parsed
}

func sidebarUpdatedAfter(a, b sidebar.Thread) bool {
	ta := parseCatalogTimePtr(a.UpdatedAt)
	tb := parseCatalogTimePtr(b.UpdatedAt)
	switch {
	case ta == nil && tb == nil:
		return a.ID > b.ID
	case ta == nil:
		return false
	case tb == nil:
		return true
	case ta.Equal(*tb):
		return a.ID > b.ID
	default:
		return ta.After(*tb)
	}
}
