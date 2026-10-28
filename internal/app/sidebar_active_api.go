package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
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
	catalogThread
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
	thread      catalogThread
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
	overlays  map[attachmentKey]runtimeInfo
	batched   bool
	summaries []sessionSummary
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
		cursor, err = decodeThreadCatalogCursor(rawCursor)
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

func parseSidebarActiveQuery(r *http.Request, now time.Time) (catalogListQuery, error) {
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

func (s *threadCatalogService) ListSidebarGraph(ctx context.Context, params history.SidebarGraphParams) (history.SidebarGraph, error) {
	if s == nil || s.store == nil {
		return history.SidebarGraph{}, errThreadCatalogUnavailable
	}
	return s.store.ListSidebarGraph(ctx, params)
}

func (s *threadCatalogService) LoadThreadsByIDs(ctx context.Context, ids []string) ([]history.Thread, error) {
	if s == nil || s.store == nil {
		return nil, errThreadCatalogUnavailable
	}
	return s.store.LoadThreadsByIDs(ctx, ids)
}

func (s *server) sidebarGraphCatalog() (sidebarGraphSource, error) {
	if s.catalog == nil {
		return nil, errThreadCatalogUnavailable
	}
	source, ok := s.catalog.(sidebarGraphSource)
	if !ok {
		return nil, errThreadCatalogUnavailable
	}
	return source, nil
}

func (s *server) loadSidebarPopulation(ctx context.Context, query catalogListQuery, runtime sidebarRuntimeSnapshot, now time.Time) (history.SidebarGraph, []catalogThread, error) {
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
	var threads []catalogThread
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
		for _, rec := range s.attachments.detachedExcept(nil) {
			add(rec.Key().Provider, rec.Key().NativeThreadID)
		}
	}
	if s.threadStars != nil {
		for _, key := range s.threadStars.starredIdentities() {
			add(key.Provider, key.NativeThreadID)
		}
	}
	return keys
}

func (s *server) graphRowsToThreads(rows []history.SidebarGraphRow, now time.Time) []catalogThread {
	out := make([]catalogThread, 0, len(rows))
	for _, row := range rows {
		thread := mapCatalogThread(row.Thread, nil)
		court := fleetCourtFromHistory(row.Court)
		thread.court = court
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

func (s *server) attachRuntimeOverlayBatch(ctx context.Context, threads []catalogThread, runtime sidebarRuntimeSnapshot) {
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
	controlAvailable := s.controlMetadata != nil && s.controlMetadataErr == "" && s.controlMetadata.db.PingContext(ctx) == nil
	assignments := map[string]*orchestrationAssignmentView{}
	metadata := map[string]catalogMetadata{}
	if controlAvailable {
		assignments = s.controlMetadata.currentAssignmentsByHostedIDs(hostedIDs)
		metadata = s.controlMetadata.catalogMetadataByHostedIDs(hostedIDs)
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
func (s *server) volatileSessionSummaries() []sessionSummary {
	needsAttention := map[string]bool{}
	if s.agentExecutions != nil {
		needsAttention = s.agentExecutions.sessionsNeedingAttention()
	}
	return s.volatileSummaries(needsAttention)
}

// sidebarHostedRecords resolves the durable hosted identity overlay in one
// control-store read. The ordinary point helper is ideal for one detail row,
// but calling it for every graph node turns a cold sidebar read into more than
// a thousand serialized SQLite queries.
func (s *server) sidebarHostedRecords(threads []catalogThread) ([]agentSessionRecord, []bool) {
	records := make([]agentSessionRecord, len(threads))
	found := make([]bool, len(threads))
	store := s.agentExecutions
	if store == nil || len(threads) == 0 {
		return records, found
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	requested := map[attachmentKey]bool{}
	for i, thread := range threads {
		if thread.HostedThreadID != "" {
			if record, ok := store.sessions[thread.HostedThreadID]; ok {
				records[i], found[i] = record, true
			}
			continue
		}
		if thread.Provider != "" && thread.NativeID != "" {
			requested[attachmentKey{Provider: thread.Provider, NativeThreadID: thread.NativeID}] = true
		}
	}

	claims := map[attachmentKey]string{}
	if store.control != nil && len(requested) > 0 {
		rows, err := store.control.db.Query(`SELECT provider,native_thread_id,hosted_thread_id
			FROM hosted_thread_native_ids WHERE retired_at IS NULL`)
		if err != nil {
			return records, found
		}
		for rows.Next() {
			var provider, nativeID, hostedID string
			if err := rows.Scan(&provider, &nativeID, &hostedID); err != nil {
				rows.Close()
				return records, found
			}
			key := attachmentKey{Provider: provider, NativeThreadID: nativeID}
			if requested[key] {
				claims[key] = hostedID
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return records, found
		}
		rows.Close()
	}

	fallback := map[attachmentKey]agentSessionRecord{}
	for _, record := range store.sessions {
		key := attachmentKey{Provider: record.Provider, NativeThreadID: record.NativeThreadID}
		if !requested[key] {
			continue
		}
		if _, claimed := claims[key]; claimed {
			continue
		}
		best := fallback[key]
		bestOpen := best.ID != "" && best.Status != sessionStatusExited
		recordOpen := record.Status != sessionStatusExited
		// Compared as times: RFC3339Nano trims trailing zeros, so as text
		// ".2374Z" sorts after the later ".237401Z".
		if best.ID == "" || (recordOpen != bestOpen && recordOpen) || (recordOpen == bestOpen && timestampBefore(best.UpdatedAt, record.UpdatedAt)) {
			fallback[key] = record
		}
	}
	for i, thread := range threads {
		if found[i] || thread.Provider == "" || thread.NativeID == "" {
			continue
		}
		key := attachmentKey{Provider: thread.Provider, NativeThreadID: thread.NativeID}
		if hostedID, claimed := claims[key]; claimed {
			records[i], found[i] = store.sessions[hostedID]
			if records[i].ID == "" {
				found[i] = false
			}
			continue
		}
		if record, ok := fallback[key]; ok {
			records[i], found[i] = record, true
		}
	}
	return records, found
}

func (s *controlMetadataStore) currentAssignmentsByHostedIDs(ids []string) map[string]*orchestrationAssignmentView {
	out := map[string]*orchestrationAssignmentView{}
	if s == nil || len(ids) == 0 {
		return out
	}
	// control.sqlite intentionally has one open connection. Resolve audit
	// target existence before opening the assignment rows so projection never
	// attempts a nested query while that sole connection is occupied.
	hostedThreadIDs := map[string]bool{}
	hostedRows, err := s.db.Query(`SELECT hosted_thread_id FROM hosted_threads`)
	if err != nil {
		return out
	}
	for hostedRows.Next() {
		var hostedID string
		if err := hostedRows.Scan(&hostedID); err != nil {
			hostedRows.Close()
			return out
		}
		hostedThreadIDs[hostedID] = true
	}
	if err := hostedRows.Err(); err != nil {
		hostedRows.Close()
		return out
	}
	hostedRows.Close()

	unique := uniqueStrings(ids)
	for start := 0; start < len(unique); start += 400 {
		end := start + 400
		if end > len(unique) {
			end = len(unique)
		}
		batch := unique[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch)+1)
		args = append(args, orchestrationAssignmentField)
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := s.db.Query(`SELECT hosted_thread_id,desired_event_id,desired_disposition,desired_value_json
			FROM thread_metadata_current WHERE field=? AND hosted_thread_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return out
		}
		for rows.Next() {
			var hostedID, disposition string
			var eventID sql.NullInt64
			var raw sql.NullString
			if err := rows.Scan(&hostedID, &eventID, &disposition, &raw); err != nil {
				rows.Close()
				return out
			}
			if disposition == metadataDispositionCleared || !eventID.Valid || !raw.Valid {
				continue
			}
			var assignment orchestrationAssignment
			if err := json.Unmarshal([]byte(raw.String), &assignment); err != nil {
				continue
			}
			for i := range assignment.AuditTargets {
				assignment.AuditTargets[i].Unresolved = !hostedThreadIDs[assignment.AuditTargets[i].HostedThreadID]
			}
			out[hostedID] = &orchestrationAssignmentView{
				EventID: eventID.Int64, AssignmentRole: assignment.AssignmentRole, Focus: assignment.Focus,
				RelatedHostedThreadID: assignment.RelatedHostedThreadID, AuditTargets: assignment.AuditTargets,
			}
		}
		rows.Close()
	}
	return out
}

func (s *controlMetadataStore) catalogMetadataByHostedIDs(ids []string) map[string]catalogMetadata {
	out := map[string]catalogMetadata{}
	if s == nil || len(ids) == 0 {
		return out
	}
	views := map[string]map[string]metadataCurrentView{}
	unique := uniqueStrings(ids)
	for start := 0; start < len(unique); start += 400 {
		end := start + 400
		if end > len(unique) {
			end = len(unique)
		}
		batch := unique[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.db.Query(`SELECT c.hosted_thread_id,c.field,c.desired_value_json,c.desired_disposition,c.desired_source,c.desired_event_id,c.effective_value_json,c.effective_disposition,c.effective_source,c.effective_event_id,
			COALESCE(d.evidence_freshness,''),COALESCE(e.evidence_freshness,'')
			FROM thread_metadata_current c
			LEFT JOIN thread_metadata_events d ON d.event_id=c.desired_event_id
			LEFT JOIN thread_metadata_events e ON e.event_id=c.effective_event_id
			WHERE c.hosted_thread_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return out
		}
		for rows.Next() {
			var hostedID, field string
			var dVal, dDisp, dSource, eVal, eDisp, eSource sql.NullString
			var desiredFreshness, effectiveFreshness string
			var dID, eID sql.NullInt64
			if err := rows.Scan(&hostedID, &field, &dVal, &dDisp, &dSource, &dID, &eVal, &eDisp, &eSource, &eID, &desiredFreshness, &effectiveFreshness); err != nil {
				rows.Close()
				return out
			}
			entry := metadataCurrentView{DesiredDisposition: nonnullDisposition(dDisp), DesiredSource: dSource.String, EffectiveDisposition: nonnullDisposition(eDisp), EffectiveSource: eSource.String}
			if dVal.Valid {
				entry.DesiredValue = json.RawMessage(dVal.String)
			}
			if dID.Valid {
				id := dID.Int64
				entry.DesiredEventID = &id
			}
			if eVal.Valid {
				entry.EffectiveValue = json.RawMessage(eVal.String)
			}
			if eID.Valid {
				id := eID.Int64
				entry.EffectiveEventID = &id
			}
			projectMetadataDisplay(&entry, desiredFreshness, effectiveFreshness)
			fields := views[hostedID]
			if fields == nil {
				fields = map[string]metadataCurrentView{}
				views[hostedID] = fields
			}
			fields[field] = entry
		}
		rows.Close()
	}
	canonical := func(fields map[string]metadataCurrentView, field string) catalogValue {
		value, ok := canonicalMetadataString(fields[field])
		if !ok {
			return unknownCatalogValue()
		}
		evidence := sidebarMetadataEvidence
		return catalogValue{State: string(history.MetadataKnown), Value: &value, Evidence: &evidence}
	}
	for hostedID, fields := range views {
		out[hostedID] = catalogMetadata{
			CWD: canonical(fields, "cwd"), Model: canonical(fields, postureFieldModel),
			Effort: canonical(fields, postureFieldEffort), Priority: canonical(fields, postureFieldServiceTier),
			Permission: canonical(fields, postureFieldPermissionMode),
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

func (s *server) assembleSidebarGraph(ctx context.Context, graph history.SidebarGraph, threads []catalogThread, runtime sidebarRuntimeSnapshot) ([]*sidebarNode, int64) {
	memoryRows := s.memorySidebarThreadRows(threads, runtime.summaries)
	if len(memoryRows) > 0 {
		s.attachRuntimeOverlayBatch(ctx, memoryRows, runtime)
		threads = append(threads, memoryRows...)
	}
	s.attachStarredOverlay(threads)
	return assembleSidebarThreads(threads), catalogVersion(graph.CatalogVersion)
}

// memorySidebarThreadRows can trust the sidebar graph's rescue keys: every
// live native identity was requested explicitly, so an absent row is not
// indexed and needs a transient memory row. This avoids one catalog point
// query per newly starting session on every sidebar request.
func (s *server) memorySidebarThreadRows(durable []catalogThread, live []sessionSummary) []catalogThread {
	if len(live) == 0 {
		return nil
	}
	onPage := make(map[attachmentKey]bool, len(durable))
	for _, item := range durable {
		if item.NativeID != "" {
			onPage[attachmentKey{Provider: item.Provider, NativeThreadID: item.NativeID}] = true
		}
	}
	rows := make([]catalogThread, 0)
	for _, sess := range live {
		if sess.ThreadID != "" && onPage[attachmentKey{Provider: sess.Provider, NativeThreadID: sess.ThreadID}] {
			continue
		}
		rows = append(rows, memoryThreadRow(sess))
	}
	return rows
}

func (s *server) missingSidebarParents(threads []catalogThread) []history.SidebarGraphKey {
	index := map[string]catalogThread{}
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
					if record, ok := s.agentExecutions.session(hosted); ok && record.NativeThreadID != "" {
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

func indexSidebarThreads(threads []catalogThread) *sidebarIndex {
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

func sidebarIdentityKeys(thread catalogThread) []string {
	keys := []string{thread.ID}
	if thread.HostedThreadID != "" {
		keys = append(keys, "hosted:"+thread.HostedThreadID)
	}
	if thread.NativeID != "" {
		keys = append(keys, thread.Provider+":"+thread.NativeID)
	}
	return keys
}

func sidebarParentKeys(thread catalogThread) []string {
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

func resolveSidebarParent(thread catalogThread, index *sidebarIndex) *sidebarNode {
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

func sidebarIndependentlyActive(thread catalogThread) bool {
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
	if thread.Fleet != nil && thread.HostedStatus != sessionStatusExited {
		switch thread.Fleet.Label {
		case fleetWorking, fleetNeedsYou:
			return true
		}
	}
	return false
}

func resolveThreadRoleCatalog(thread catalogThread) string {
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
		catalogThread: thread,
		Sidebar: sidebarBlock{
			ParentID: parent, RootID: node.rootID, AncestorIDs: ancestors, ChildrenIDs: children,
			Depth: node.depth, Active: node.independent && node.included, Structural: node.structural,
			HistoricalDescendantCount: node.historical, DescendantActivity: node.activity,
			Sort: sidebarSortPrimitives(thread), Lineage: node.lineage,
		},
	}
}

func sidebarSortPrimitives(thread catalogThread) sidebarSort {
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

func sidebarTitle(thread catalogThread) string {
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

func sidebarLivenessRank(thread catalogThread) int {
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

func sidebarSelfWork(thread catalogThread) bool {
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

func sidebarUpdatedAfter(a, b catalogThread) bool {
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
