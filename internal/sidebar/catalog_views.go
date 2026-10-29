package sidebar

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dkroshow/nano/internal/catalog"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/historical"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/threads"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CatalogViews owns full-list and per-thread visible projections and their event stream.
type CatalogViews struct {
	Catalog          CatalogReader
	Runtime          *Controller
	HistoricalCodex  *historical.CodexResolver
	HistoricalClaude *historical.ClaudeResolver
	MailEvents       *featuremail.Hub
	Audits           *observability.AuditHub
}

// CatalogReader reads saved rows before the visible projection is assembled.
type CatalogReader interface {
	ListThreads(context.Context, threadindex.ListQuery) (CatalogPage, error)
	LoadThread(context.Context, string) (Thread, error)
	IndexStatus(context.Context) (threadindex.IndexStatus, error)
	RequestRefresh() bool
}

// catalogStream is the SSE push surface: a subscription for version bumps
// plus the version-on-connect read. Optional so a catalog without live push
// (or a stub in tests) simply reports the endpoint unavailable.
type catalogStream interface {
	SubscribeCatalog() (<-chan catalog.Change, func())
	CatalogVersion(context.Context) (int64, error)
}

// HandleThreads serves the visible catalog projection.
func (s *CatalogViews) HandleThreads(w http.ResponseWriter, r *http.Request) {
	if s.Catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	query, err := threadindex.ParseCatalogListQuery(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	page, err := s.listCatalogPage(r.Context(), query)
	if err != nil {
		threadindex.WriteCatalogBackendError(w, err)
		return
	}
	threadindex.WriteCatalogPageJSON(w, r, http.StatusOK, page)
}

// listCatalogPage is the unified list read behind GET /api/v1/threads: durable
// rows plus memory, rescue and lineage rows, with the runtime and fleet
// overlays stamped. The compact sidebar reads a cheaper active graph through
// GET /api/v1/sidebar/threads instead of paging this full window.
func (s *CatalogViews) listCatalogPage(ctx context.Context, query threadindex.ListQuery) (CatalogPage, error) {
	page, err := s.Catalog.ListThreads(ctx, query)
	if err != nil {
		return CatalogPage{}, err
	}
	if page.Items == nil {
		page.Items = []Thread{}
	}
	// Runtime rows take their place in the durable list order, so pages walk
	// one ordered stream and the cursor may point at any row. A runtime row at
	// or before the cursor went out on an earlier page; one past the last
	// durable row waits for the page that reaches it. All rows join before the
	// overlay pass, so the one compose pass stamps `runtime` on every class.
	var after, last *history.ThreadCursor
	if query.Cursor != "" {
		after, _ = threadindex.DecodeThreadCatalogCursor(query.Cursor)
	}
	if page.NextCursor != nil {
		key := listKey(page.Items[len(page.Items)-1])
		last = &key
	}
	runtime := s.Runtime.SidebarRuntimeSnapshot()
	items := page.Items
	for _, row := range s.runtimeThreadRows(ctx, query, page.Items, runtime.Summaries) {
		if key := listKey(row); (after == nil || listsBefore(*after, key)) && (last == nil || !listsBefore(*last, key)) {
			items = append(items, row)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return listsBefore(listKey(items[i]), listKey(items[j])) })
	// A worker is never returned without every indexed supervisor that connects
	// it to the thread tree. Parents can sit outside the requested time window
	// because Claude writes worker progress to a different file. They count
	// toward the limit, so the page gives up its last rows until they fit. A
	// supervisor chain is never split: when the first row's chain alone does
	// not fit, the page holds that row and its chain and exceeds the limit.
	cut := len(items) > query.Limit
	items = items[:min(len(items), query.Limit)]
	parents := s.lineageParentRows(ctx, query, items)
	for len(items) > 1 && len(items)+len(parents) > query.Limit {
		items, cut = items[:max(1, query.Limit-len(parents))], true
		parents = s.lineageParentRows(ctx, query, items)
	}
	page.NextCursor = nil
	if len(items) > 0 && (cut || last != nil) {
		key := listKey(items[len(items)-1])
		encoded, err := threadindex.EncodeThreadCatalogCursor(&key)
		if err != nil {
			return CatalogPage{}, err
		}
		page.NextCursor = &encoded
	}
	page.Items = append(items, parents...)
	s.Runtime.ComposeCatalogThreads(ctx, page.Items, runtime, false)
	return page, nil
}

// listKey is a row's place in the list order the store pages by.
func listKey(row Thread) history.ThreadCursor {
	key := history.ThreadCursor{ID: row.ID}
	if row.UpdatedAt != nil {
		if at, err := time.Parse(time.RFC3339Nano, *row.UpdatedAt); err == nil {
			key.UpdatedAt = &at
		}
	}
	return key
}

// listsBefore reports whether a comes before b in that order: newest update
// first, rows without one last, then id descending.
func listsBefore(a, b history.ThreadCursor) bool {
	switch {
	case (a.UpdatedAt == nil) != (b.UpdatedAt == nil):
		return b.UpdatedAt == nil
	case a.UpdatedAt != nil && !a.UpdatedAt.Equal(*b.UpdatedAt):
		return a.UpdatedAt.After(*b.UpdatedAt)
	}
	return a.ID > b.ID
}

func (s *CatalogViews) runtimeThreadRows(ctx context.Context, query threadindex.ListQuery, page []Thread, live []sessions.Summary) []Thread {
	if query.Archived == archivedOnlyFilter {
		return nil
	}
	covered := make(map[hostedstore.AttachmentKey]bool, len(page))
	for _, item := range page {
		if item.NativeID != "" {
			covered[hostedstore.AttachmentKey{Provider: item.Provider, NativeThreadID: item.NativeID}] = true
		}
	}
	attached := make(map[hostedstore.AttachmentKey]bool, len(live))
	var wanted []hostedstore.AttachmentKey
	for _, sess := range live {
		key := hostedstore.AttachmentKey{Provider: sess.Provider, NativeThreadID: sess.ThreadID}
		if sess.ThreadID == "" || attached[key] || (query.ThreadID != "" && sess.ThreadID != query.ThreadID) {
			continue
		}
		attached[key] = true
		if !covered[key] {
			wanted = append(wanted, key)
		}
	}
	if query.ThreadID == "" && s.Runtime.attachments != nil {
		for _, rec := range s.Runtime.attachments.DetachedExcept(attached) {
			if key := rec.Key(); !covered[key] {
				wanted = append(wanted, key)
			}
		}
	}
	indexed, err := s.threadsByIdentity(ctx, wanted)
	if err != nil {
		log.Printf("catalog runtime rows for %d threads: %v", len(wanted), err)
	}
	var rows []Thread
	for _, sess := range live {
		if query.ThreadID != "" && sess.ThreadID != query.ThreadID {
			continue
		}
		key := hostedstore.AttachmentKey{Provider: sess.Provider, NativeThreadID: sess.ThreadID}
		if _, found := indexed[key]; sess.ThreadID == "" || (err == nil && !covered[key] && !found) {
			rows = append(rows, MemoryThreadRow(sess))
		}
	}
	if query.ThreadID == "" {
		for _, key := range wanted {
			if row, found := indexed[key]; found {
				rows = append(rows, row)
			}
		}
	}
	return rows
}

// threadsByIdentity reads the durable rows for keys with every fetch-time
// filter widened, in one read per history.MaxThreadIdentities keys. One thread
// can sit under two roots; the first row in list order wins, as the former
// one-row point read did.
func (s *CatalogViews) threadsByIdentity(ctx context.Context, keys []hostedstore.AttachmentKey) (map[hostedstore.AttachmentKey]Thread, error) {
	found := make(map[hostedstore.AttachmentKey]Thread, len(keys))
	for start := 0; start < len(keys); start += history.MaxThreadIdentities {
		query := threadindex.ListQuery{
			Identities:       catalogIdentities(keys[start:min(start+history.MaxThreadIdentities, len(keys))]),
			IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true, Archived: archivedIncludeFilter,
		}
		query.Limit = 2 * len(query.Identities)
		for {
			page, err := s.Catalog.ListThreads(ctx, query)
			if err != nil {
				return found, err
			}
			for _, item := range page.Items {
				key := hostedstore.AttachmentKey{Provider: item.Provider, NativeThreadID: item.NativeID}
				if _, seen := found[key]; !seen {
					found[key] = item
				}
			}
			if page.NextCursor == nil {
				break
			}
			query.Cursor = *page.NextCursor
		}
	}
	return found, nil
}

// lineageParentRows closes the graph around every worker in a list response:
// a worker is never returned without the indexed supervisors that connect it
// to the thread tree, which can sit outside the requested window. It walks one
// level at a time with one hosted-record read, one assignment read and one
// identity read per level, never per row, and returns parents in the order a
// one-row walk finds them.
func (s *CatalogViews) lineageParentRows(ctx context.Context, query threadindex.ListQuery, page []Thread) []Thread {
	if query.ThreadID != "" {
		return nil
	}
	covered, visited := make(map[hostedstore.AttachmentKey]bool, len(page)), map[hostedstore.AttachmentKey]bool{}
	for _, item := range page {
		if item.NativeID != "" {
			covered[hostedstore.AttachmentKey{Provider: item.Provider, NativeThreadID: item.NativeID}] = true
		}
	}
	var parents []Thread
	for level := page; len(level) > 0; {
		var wanted []hostedstore.AttachmentKey
		for _, key := range s.lineageParentKeys(level) {
			if !covered[key] && !visited[key] {
				visited[key] = true
				wanted = append(wanted, key)
			}
		}
		found, err := s.threadsByIdentity(ctx, wanted)
		if err != nil {
			log.Printf("catalog lineage parents for %d threads: %v", len(wanted), err)
		}
		level = nil
		for _, key := range wanted {
			if parent, ok := found[key]; ok {
				covered[key] = true
				parents = append(parents, parent)
				level = append(level, parent)
			}
		}
	}
	return parents
}

// lineageParentKeys returns each row's supervisor, in row order. A current
// related-thread assignment wins, then a standalone thread stops, then the
// hosted invoker, then a subagent's provider-native parent.
func (s *CatalogViews) lineageParentKeys(rows []Thread) []hostedstore.AttachmentKey {
	records, found := s.Runtime.SidebarHostedRecords(rows)
	var assignments map[string]*hostedstore.AssignmentView
	if s.Runtime.controlMetadata != nil && s.Runtime.controlMetadataErr == "" {
		ids := make([]string, 0, len(rows))
		for i := range rows {
			if found[i] {
				ids = append(ids, records[i].ID)
			}
		}
		assignments = s.Runtime.controlMetadata.CurrentAssignmentsByHostedIDs(ids)
	}
	var keys []hostedstore.AttachmentKey
	for i, row := range rows {
		parentID := ""
		if found[i] {
			if assignment := assignments[records[i].ID]; assignment != nil {
				parentID = assignment.RelatedHostedThreadID
			}
			if parentID == "" {
				if records[i].SidebarPlacement == threads.SidebarPlacementStandalone {
					continue
				}
				parentID = records[i].InvokerHostedThreadID
			}
			if parent, ok := s.Runtime.agentExecutions.Session(parentID); ok && parent.Provider != "" && parent.NativeThreadID != "" {
				keys = append(keys, hostedstore.AttachmentKey{Provider: parent.Provider, NativeThreadID: parent.NativeThreadID})
				continue
			}
		}
		if parentID == "" && row.Kind == string(history.ThreadSubagent) && row.ParentNativeID != "" {
			keys = append(keys, hostedstore.AttachmentKey{Provider: row.Provider, NativeThreadID: row.ParentNativeID})
		}
	}
	return keys
}

// HandleThread serves the visible catalog projection.
func (s *CatalogViews) HandleThread(w http.ResponseWriter, r *http.Request, rawID string) {
	if s.Catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	if rawID == "" {
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
		return
	}
	reference := rawID
	if strings.TrimSpace(reference) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	id, err := resolveCatalogReference(r.Context(), s.Catalog, reference)
	if err != nil {
		threadindex.WriteThreadReferenceError(w, err)
		return
	}
	thread, err := s.Catalog.LoadThread(r.Context(), id)
	if err != nil {
		threadindex.WriteCatalogBackendError(w, err)
		return
	}
	if thread.Sources == nil {
		sources := []threadindex.SourceDetail{}
		thread.Sources = &sources
	}
	s.Runtime.AttachRuntimeOverlay(&thread)
	s.Runtime.StarThread(&thread)
	mailRow := []Thread{thread}
	s.Runtime.AttachMailOverlay(mailRow)
	thread.Mail = mailRow[0].Mail
	if thread.Provider == "codex" && s.HistoricalCodex != nil {
		resolution := s.HistoricalCodex.ResolvePrepared(r.Context(), thread.Thread)
		thread.CurrentResolution = &resolution
	} else if thread.Provider == "claude" && s.HistoricalClaude != nil {
		resolution := s.HistoricalClaude.ResolvePrepared(r.Context(), thread.Thread)
		thread.CurrentResolution = &resolution
	}
	threadindex.WriteCatalogJSON(w, http.StatusOK, thread)
}

// HandleEvents streams catalog_version bumps as Server-Sent Events.
// The first event on every connect is the current version (contract item 2:
// version-on-connect closes the missed-while-disconnected hole), then one event
// per bump, with a heartbeat comment so a quiet stream reads as alive not dead.
// Everything that can fail is checked before the 200 header commits the stream.
// HandleEvents serves the visible catalog projection.
func (s *CatalogViews) HandleEvents(w http.ResponseWriter, r *http.Request) {
	stream, ok := s.Catalog.(catalogStream)
	if s.Catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteError(w, http.StatusInternalServerError, "streaming_unsupported", "Streaming is not supported")
		return
	}

	// Subscribe before the version read so a bump between the two lands in the
	// channel rather than slipping through the gap.
	events, unsubscribe := stream.SubscribeCatalog()
	defer unsubscribe()
	// Runtime deltas ride the same stream as a second named event type. A
	// browser names the stream position of its session list read
	// (runtimeGeneration, runtimeSeq) and first receives what it missed since,
	// or a refresh when that cannot be replayed. A consumer that names none
	// gets only the deltas from now on. A server without the hub (tests)
	// leaves the channel nil, which never fires.
	var runtimeEvents <-chan RuntimeDelta
	if s.Runtime.runtimeEvents != nil {
		var ch chan RuntimeDelta
		if query := r.URL.Query(); query.Has("runtimeGeneration") {
			seq, _ := strconv.ParseUint(query.Get("runtimeSeq"), 10, 64)
			ch = s.Runtime.runtimeEvents.SubscribeFrom(RuntimePosition{Generation: query.Get("runtimeGeneration"), Seq: seq})
		} else {
			ch = s.Runtime.runtimeEvents.Subscribe()
		}
		defer s.Runtime.runtimeEvents.Unsubscribe(ch)
		runtimeEvents = ch
	}
	// Fleet transitions mirror onto the same stream for always-connected
	// consumers. Best-effort with no replay by contract: the bookmark feed at
	// /api/v1/fleet/changes is what guarantees a transition is never missed.
	fleetEvents, stopFleet := s.Runtime.SubscribeFleet()
	defer stopFleet()
	// Mail count changes ride the same stream as a fourth named event. No
	// connect-time event: the client's initial window fetch carries current
	// counts on the rows (attachMailOverlay), same as the runtime overlay.
	var mailEvents <-chan featuremail.Delta
	if s.MailEvents != nil {
		ch := s.MailEvents.Subscribe()
		defer s.MailEvents.Unsubscribe(ch)
		mailEvents = ch
	}
	// The materialized sidebar publishes row/removal/root patches on this
	// existing browser stream. A refresh marker on connect closes the race with
	// the caller's cached whole-state GET without adding another SSE route.
	var sidebarEvents <-chan Delta
	if s.Runtime != nil {
		ch := s.Runtime.Subscribe()
		defer s.Runtime.Unsubscribe(ch)
		sidebarEvents = ch
	}
	// Transcript audit requests reuse the app-wide browser stream. The event is
	// content-free and targets one exact tab and pane; the browser fetches the
	// bounded SQLite reference only after that pane claims the request.
	var transcriptAuditEvents <-chan observability.AuditEvent
	if s.Audits != nil {
		ch := s.Audits.Subscribe()
		defer s.Audits.Unsubscribe(ch)
		transcriptAuditEvents = ch
	}
	version, err := stream.CatalogVersion(r.Context())
	if err != nil {
		threadindex.WriteCatalogBackendError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	writeCatalogChangeEvent(w, catalog.Change{Version: uint64(version), Refresh: true})
	if marker, ok := s.Runtime.Marker(version); ok {
		writeSidebarEvent(w, marker)
	}
	flusher.Flush()

	heartbeat := time.NewTicker(threadindex.CatalogEventHeartbeat)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case change, ok := <-events:
			if !ok {
				return // hub closed on service shutdown
			}
			writeCatalogChangeEvent(w, change)
			flusher.Flush()
		case delta, ok := <-runtimeEvents:
			if !ok {
				runtimeEvents = nil
				continue
			}
			writeRuntimeEvent(w, delta)
			flusher.Flush()
		case transition, ok := <-fleetEvents:
			if !ok {
				fleetEvents = nil // mirror gone; the catalog stream carries on
				continue
			}
			writeFleetEvent(w, transition)
			flusher.Flush()
		case delta, ok := <-mailEvents:
			if !ok {
				mailEvents = nil
				continue
			}
			writeMailEvent(w, delta)
			flusher.Flush()
		case delta, ok := <-sidebarEvents:
			if !ok {
				sidebarEvents = nil
				continue
			}
			writeSidebarEvent(w, delta)
			flusher.Flush()
		case audit, ok := <-transcriptAuditEvents:
			if !ok {
				transcriptAuditEvents = nil
				continue
			}
			observability.WriteTranscriptAuditEvent(w, audit)
			flusher.Flush()
		case <-heartbeat.C:
			io.WriteString(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func writeCatalogChangeEvent(w io.Writer, change catalog.Change) {
	payload := struct {
		CatalogVersion int64    `json:"catalogVersion"`
		ThreadIDs      []string `json:"threadIds,omitempty"`
		Refresh        bool     `json:"refresh,omitempty"`
	}{CatalogVersion: threadindex.CatalogVersion(change.Version), ThreadIDs: change.ThreadIDs, Refresh: change.Refresh}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: catalog\ndata: %s\n\n", encoded)
}

// writeRuntimeEvent emits the compact runtime delta: the overlay of each
// thread whose runtime changed, null for a thread Nano released. The named
// event stays separate from catalog version changes because these rows are
// volatile and must not advance catalog_version.
func writeRuntimeEvent(w io.Writer, delta RuntimeDelta) {
	payload, err := json.Marshal(delta)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: runtime\ndata: %s\n\n", payload)
}

// writeMailEvent emits the current inbox counts for each changed hosted
// thread plus the global summary — last-value-wins state, like runtime.
func writeMailEvent(w io.Writer, delta featuremail.Delta) {
	payload, err := json.Marshal(delta)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: mail\ndata: %s\n\n", payload)
}

func writeSidebarEvent(w io.Writer, delta Delta) {
	payload, err := json.Marshal(delta)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: sidebar\ndata: %s\n\n", payload)
}

// writeFleetEvent mirrors one fleet transition. The payload carries the same
// shape the changes endpoint returns, so a consumer parses one format.
func writeFleetEvent(w io.Writer, transition FleetTransition) {
	payload, err := json.Marshal(transition)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: fleet\ndata: %s\n\n", payload)
}

// CatalogPage is a full-list page with its catalog position.
type CatalogPage struct {
	Items          []Thread `json:"items"`
	NextCursor     *string  `json:"nextCursor"`
	CatalogVersion int64    `json:"catalogVersion"`
}

func resolveCatalogReference(ctx context.Context, catalog CatalogReader, ref string) (string, error) {
	resolver, _ := catalog.(threadindex.ReferenceResolver)
	return threadindex.ResolveReference(ctx, resolver, ref)
}

const (
	archivedOnlyFilter    = "only"
	archivedIncludeFilter = string(history.ArchivedInclude)
)
