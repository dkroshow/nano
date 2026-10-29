package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

type threadCatalog interface {
	ListThreads(context.Context, threadindex.ListQuery) (catalogPage, error)
	LoadThread(context.Context, string) (sidebar.Thread, error)
	IndexStatus(context.Context) (threadindex.IndexStatus, error)
	RequestRefresh() bool
}

// threadCatalogStream is the SSE push surface: a subscription for version bumps
// plus the version-on-connect read. Optional so a catalog without live push
// (or a stub in tests) simply reports the endpoint unavailable.
type threadCatalogStream interface {
	SubscribeCatalog() (<-chan catalog.Change, func())
	CatalogVersion(context.Context) (int64, error)
}

// handleSessionName renames a live session before or after its native thread
// is indexed. A fresh Claude session has no native id until its first turn, so
// its validated name remains pending until applyPendingName can persist it.
func (s *server) handleSessionName(w http.ResponseWriter, r *http.Request, sess *sessions.Session) {
	w.Header().Set("Cache-Control", "private, no-store")
	namer, ok := s.catalog.(threadindex.Namer)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	var payload struct {
		Name string `json:"name"`
	}
	body := http.MaxBytesReader(w, r.Body, threadindex.MaxThreadNameRequestBytes)
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_name", `Request body must be {"name": "..."}`)
		return
	}
	name, err := history.ValidateThreadName(payload.Name)
	if err != nil {
		threadindex.WriteThreadNameError(w, threadindex.NameInvalidError{Reason: err.Error()})
		return
	}

	// A native id, once present, is immutable for the hosted session. Holding
	// the lock while merely recording a pre-bind name lets applyPendingName see
	// exactly this value when the provider announces its identity.
	provider, nativeID, staged := sess.StageNameBeforeBinding(name)
	if staged {
		s.notifyRuntimeRefresh()
		httpapi.WriteJSON(w, sess.Summary())
		return
	}

	// A provisional Claude identity is the shim handle standing in for a native
	// id that has not been announced yet (attachmentRecord.provisional). A
	// catalog write here would key the name to a handle captureInit is about to
	// retire, and clearing pendingName would leave applyPendingName nothing to
	// apply — the name would vanish with the first prompt. Stage it like the
	// pre-bind branch instead, and persist it onto the provisional record so a
	// Nano restart in this window keeps the rename. captureInit updates threadID
	// under stateMu before it consumes pendingName, so an unchanged threadID
	// proves the staged value will still be picked up.
	if rec, ok := s.attachmentFor(provider, nativeID); ok && rec.Provisional() {
		var staged bool
		nativeID, staged = sess.StageProvisionalName(nativeID, name)
		if staged {
			s.setAttachmentPendingName(provider, nativeID, name)
			s.notifyRuntimeRefresh()
			httpapi.WriteJSON(w, sess.Summary())
			return
		}
		// The real id landed while we probed; assign under it normally.
	}

	if err := namer.AssignThreadName(provider, nativeID, name); err != nil {
		threadindex.WriteThreadNameError(w, err)
		return
	}
	sess.SetDisplayName(name)
	s.notifyRuntimeRefresh()
	httpapi.WriteJSON(w, sess.Summary())
}

// assignSpawnName is the session's nameThread closure: best-effort by design —
// a name that cannot be written must not fail the spawn it was attached to.
func (s *server) assignSpawnName(provider, nativeThreadID, name string) {
	namer, ok := s.catalog.(threadindex.Namer)
	if s.catalog == nil || !ok {
		log.Printf("session name %q for %s/%s dropped: catalog unavailable", name, provider, nativeThreadID)
		return
	}
	if err := namer.AssignThreadNameIfEmpty(provider, nativeThreadID, name); err != nil {
		log.Printf("session name %q for %s/%s: %v", name, provider, nativeThreadID, err)
	}
}

func (s *server) handleCatalogThreads(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
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
		writeCatalogBackendError(w, err)
		return
	}
	threadindex.WriteCatalogPageJSON(w, r, http.StatusOK, page)
}

// listCatalogPage is the unified list read behind GET /api/v1/threads: durable
// rows plus memory, rescue and lineage rows, with the runtime and fleet
// overlays stamped. The compact sidebar reads a cheaper active graph through
// GET /api/v1/sidebar/threads instead of paging this full window.
func (s *server) listCatalogPage(ctx context.Context, query threadindex.ListQuery) (catalogPage, error) {
	page, err := s.catalog.ListThreads(ctx, query)
	if err != nil {
		return catalogPage{}, err
	}
	if page.Items == nil {
		page.Items = []sidebar.Thread{}
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
	runtime := s.sidebarRuntimeSnapshot()
	items := page.Items
	for _, row := range s.runtimeThreadRows(ctx, query, page.Items, runtime.summaries) {
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
			return catalogPage{}, err
		}
		page.NextCursor = &encoded
	}
	page.Items = append(items, parents...)
	s.composeCatalogThreads(ctx, page.Items, runtime, false)
	return page, nil
}

// listKey is a row's place in the list order the store pages by.
func listKey(row sidebar.Thread) history.ThreadCursor {
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

const (
	// memoryThreadIDPrefix namespaces a memory row's id away from the durable
	// `nth_` space: it addresses a session, not a stored thread, so
	// GET /api/v1/threads/mem_* has nothing to load by design.
	memoryThreadIDPrefix = "mem_"
	// memoryThreadKind marks a row no scanner has judged yet — main and
	// subagent are evidence-derived classes, and a memory row has no evidence.
	memoryThreadKind = "thread"
	// memoryThreadInteraction is the honest answer until the provider file is
	// read, and the one the interaction filters always admit.
	memoryThreadInteraction = "unclassified"
	// runtimeEvidence labels cells whose authority is the live session itself;
	// history.Evidence covers file-derived authorities only.
	runtimeEvidence = "runtime"
	// archivedOnlyFilter is the query class no live session can belong to.
	archivedOnlyFilter = "only"
	// archivedIncludeFilter is the widest archived class — what a rescue lookup
	// asks for, since archived says nothing about whether a thread is live.
	archivedIncludeFilter = string(history.ArchivedInclude)
)

func (s *server) runtimeThreadRows(ctx context.Context, query threadindex.ListQuery, page []sidebar.Thread, live []sessions.Summary) []sidebar.Thread {
	if query.Archived == archivedOnlyFilter {
		return nil
	}
	covered := make(map[attachmentKey]bool, len(page))
	for _, item := range page {
		if item.NativeID != "" {
			covered[attachmentKey{Provider: item.Provider, NativeThreadID: item.NativeID}] = true
		}
	}
	attached := make(map[attachmentKey]bool, len(live))
	var wanted []attachmentKey
	for _, sess := range live {
		key := attachmentKey{Provider: sess.Provider, NativeThreadID: sess.ThreadID}
		if sess.ThreadID == "" || attached[key] || (query.ThreadID != "" && sess.ThreadID != query.ThreadID) {
			continue
		}
		attached[key] = true
		if !covered[key] {
			wanted = append(wanted, key)
		}
	}
	if query.ThreadID == "" && s.attachments != nil {
		for _, rec := range s.attachments.DetachedExcept(attached) {
			if key := rec.Key(); !covered[key] {
				wanted = append(wanted, key)
			}
		}
	}
	indexed, err := s.threadsByIdentity(ctx, wanted)
	if err != nil {
		log.Printf("catalog runtime rows for %d threads: %v", len(wanted), err)
	}
	var rows []sidebar.Thread
	for _, sess := range live {
		if query.ThreadID != "" && sess.ThreadID != query.ThreadID {
			continue
		}
		key := attachmentKey{Provider: sess.Provider, NativeThreadID: sess.ThreadID}
		if _, found := indexed[key]; sess.ThreadID == "" || (err == nil && !covered[key] && !found) {
			rows = append(rows, memoryThreadRow(sess))
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
func (s *server) threadsByIdentity(ctx context.Context, keys []attachmentKey) (map[attachmentKey]sidebar.Thread, error) {
	found := make(map[attachmentKey]sidebar.Thread, len(keys))
	for start := 0; start < len(keys); start += history.MaxThreadIdentities {
		query := threadindex.ListQuery{
			Identities:       catalogIdentities(keys[start:min(start+history.MaxThreadIdentities, len(keys))]),
			IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true, Archived: archivedIncludeFilter,
		}
		query.Limit = 2 * len(query.Identities)
		for {
			page, err := s.catalog.ListThreads(ctx, query)
			if err != nil {
				return found, err
			}
			for _, item := range page.Items {
				key := attachmentKey{Provider: item.Provider, NativeThreadID: item.NativeID}
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

// memoryThreadRow projects one live session into a catalog row. Only what the
// session actually knows becomes a known cell; the rest stays unknown per the
// three-way metadata contract — a runtime row is thin and honest about it
// rather than padded with defaults. Title and preview stay null: they are
// derived from the provider transcript, which this row does not have. A
// dialog-supplied name is session-owned until the durable overlay takes over,
// so it is safe to carry through immediately.
//
// The runtime overlay is stamped here rather than left to attachRuntimeOverlay:
// a memory row is attached by construction, and a session still starting has no
// native id for the (provider, nativeId) lookup to match. The later overlay pass
// leaves a stamped row alone.
func memoryThreadRow(sess sessions.Summary) sidebar.Thread {
	createdAt := sess.CreatedAt
	runtime := sessionRuntimeInfo(sess)
	var name *threadindex.ThreadName
	if sess.Name != "" {
		name = &threadindex.ThreadName{
			Value: sess.Name, Provenance: "manual", Evidence: threadindex.ThreadNameEvidence,
		}
	}
	return sidebar.Thread{Thread: threadindex.Thread{ID: memoryThreadIDPrefix + sess.ID, Provider: sess.Provider, NativeID: sess.ThreadID, Kind: memoryThreadKind, CreatedAt: &createdAt, UpdatedAt: &createdAt, Metadata: threadindex.Metadata{
		CWD: runtimeCatalogValue(sess.Cwd),
		// The posture cells serve the same unified projection as the
		// summary and /state (posture_view.go, decision 4) — which is what
		// lights up effort/priority/permission for a live row, and shows
		// the creation-request model before the provider's own handshake
		// reports one.
		Model:            runtimeCatalogValue(sess.Posture.Model),
		Effort:           runtimeCatalogValue(sess.Posture.Effort),
		Priority:         runtimeCatalogValue(sess.Posture.ServiceTier),
		Permission:       runtimeCatalogValue(sess.Posture.PermissionMode),
		HistoricalBranch: unknownCatalogValue(),
		Origin:           unknownCatalogValue(),
	}, Interaction: memoryThreadInteraction, Name: name, Source: threadindex.SourceSummary{}}, HostedThreadID: sess.HostedThreadID, InvokerHostedThreadID: sess.InvokerHostedThreadID, InvokerNativeThreadID: sess.InvokerNativeThreadID, InvokerProvider: sess.InvokerProvider, SidebarPlacement: sess.SidebarPlacement, Transient: true, Runtime: &runtime}
}

// runtimeCatalogValue makes one metadata cell from a live session field: known
// when the session holds the value, unknown while it is still being learned
// (claude's model arrives with the init line, after the row can first appear).
func runtimeCatalogValue(value string) threadindex.Value {
	if value == "" {
		return unknownCatalogValue()
	}
	evidence := runtimeEvidence
	return threadindex.Value{State: string(history.MetadataKnown), Value: &value, Evidence: &evidence}
}

func unknownCatalogValue() threadindex.Value {
	return threadindex.Value{State: string(history.MetadataUnknown)}
}

// lineageParentRows closes the graph around every worker in a list response:
// a worker is never returned without the indexed supervisors that connect it
// to the thread tree, which can sit outside the requested window. It walks one
// level at a time with one hosted-record read, one assignment read and one
// identity read per level, never per row, and returns parents in the order a
// one-row walk finds them.
func (s *server) lineageParentRows(ctx context.Context, query threadindex.ListQuery, page []sidebar.Thread) []sidebar.Thread {
	if query.ThreadID != "" {
		return nil
	}
	covered, visited := make(map[attachmentKey]bool, len(page)), map[attachmentKey]bool{}
	for _, item := range page {
		if item.NativeID != "" {
			covered[attachmentKey{Provider: item.Provider, NativeThreadID: item.NativeID}] = true
		}
	}
	var parents []sidebar.Thread
	for level := page; len(level) > 0; {
		var wanted []attachmentKey
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
func (s *server) lineageParentKeys(rows []sidebar.Thread) []attachmentKey {
	records, found := s.sidebarHostedRecords(rows)
	var assignments map[string]*orchestrationAssignmentView
	if s.controlMetadata != nil && s.controlMetadataErr == "" {
		ids := make([]string, 0, len(rows))
		for i := range rows {
			if found[i] {
				ids = append(ids, records[i].ID)
			}
		}
		assignments = s.controlMetadata.CurrentAssignmentsByHostedIDs(ids)
	}
	var keys []attachmentKey
	for i, row := range rows {
		parentID := ""
		if found[i] {
			if assignment := assignments[records[i].ID]; assignment != nil {
				parentID = assignment.RelatedHostedThreadID
			}
			if parentID == "" {
				if records[i].SidebarPlacement == sidebarPlacementStandalone {
					continue
				}
				parentID = records[i].InvokerHostedThreadID
			}
			if parent, ok := s.agentExecutions.Session(parentID); ok && parent.Provider != "" && parent.NativeThreadID != "" {
				keys = append(keys, attachmentKey{Provider: parent.Provider, NativeThreadID: parent.NativeThreadID})
				continue
			}
		}
		if parentID == "" && row.Kind == string(history.ThreadSubagent) && row.ParentNativeID != "" {
			keys = append(keys, attachmentKey{Provider: row.Provider, NativeThreadID: row.ParentNativeID})
		}
	}
	return keys
}

// attachRuntimeOverlay stamps the volatile runtime fields and Nano's durable
// hosted-thread identity overlay onto one catalog thread. Invocation lineage
// is deliberately separate from provider-native parentNativeId: an ordinary
// Codex hosted thread may have invoked a Claude or Grok hosted thread.
func (s *server) attachRuntimeOverlay(thread *sidebar.Thread) {
	if s.runtime != nil {
		if info, ok := s.runtime.RuntimeFor(thread.Provider, thread.NativeID); ok {
			thread.Runtime = &info
		}
	}
	record, ok := s.catalogHostedThreadRecord(*thread)
	if !ok {
		return
	}
	thread.HostedThreadID = record.ID
	thread.HostedStatus = record.Status
	thread.InvokerHostedThreadID = record.InvokerHostedThreadID
	thread.InvokerNativeThreadID = record.InvokerNativeThreadID
	thread.InvokerProvider = record.InvokerProvider
	thread.SidebarPlacement = record.SidebarPlacement
	// A durable hosted overlay supersedes transcript-derived standing posture.
	// Provider files remain valuable evidence for non-hosted threads, but once
	// a hosted identity exists the control store is the only metadata authority.
	s.applyHostedCatalogMetadata(thread, record.ID)
	s.applyHostedCatalogAssignment(thread, record.ID)
}

func (s *server) applyHostedCatalogMetadata(thread *sidebar.Thread, hostedID string) {
	setUnknown := func() {
		thread.Metadata.CWD = unknownCatalogValue()
		thread.Metadata.Model = unknownCatalogValue()
		thread.Metadata.Effort = unknownCatalogValue()
		thread.Metadata.Priority = unknownCatalogValue()
		thread.Metadata.Permission = unknownCatalogValue()
	}
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
		setUnknown()
		return
	}
	view, err := s.controlMetadata.View(hostedID)
	if err != nil {
		setUnknown()
		return
	}
	canonical := func(field string) threadindex.Value {
		value, ok := hostedstore.MetadataString(view.Fields[field])
		if !ok {
			return unknownCatalogValue()
		}
		evidence := "canonical_hosted_metadata"
		return threadindex.Value{State: string(history.MetadataKnown), Value: &value, Evidence: &evidence}
	}
	thread.Metadata.CWD = canonical("cwd")
	thread.Metadata.Model = canonical(hostedstore.PostureFieldModel)
	thread.Metadata.Effort = canonical(hostedstore.PostureFieldEffort)
	thread.Metadata.Priority = canonical(hostedstore.PostureFieldServiceTier)
	thread.Metadata.Permission = canonical(hostedstore.PostureFieldPermissionMode)
}

func (s *server) applyHostedCatalogAssignment(thread *sidebar.Thread, hostedID string) {
	thread.Assignment = nil
	thread.AssignmentUnavailable = false
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
		thread.AssignmentUnavailable = true
		return
	}
	assignment, err := s.controlMetadata.CurrentOrchestrationAssignment(hostedID)
	if err != nil {
		thread.AssignmentUnavailable = true
		return
	}
	thread.Assignment = assignment
}

func (s *server) catalogHostedThreadRecord(thread sidebar.Thread) (agentSessionRecord, bool) {
	if s.agentExecutions == nil {
		return agentSessionRecord{}, false
	}
	if thread.HostedThreadID != "" {
		if record, ok := s.agentExecutions.Session(thread.HostedThreadID); ok {
			return record, true
		}
	}
	return s.agentExecutions.SessionForNative(thread.Provider, thread.NativeID)
}

// attachFleetStates stamps each row's turn state (fleet_status.go). Rows Nano
// does not hold are classified from their saved files in one batch query.
//
// Best-effort by design: a failed classification
// leaves those rows' field absent rather than failing the page.
func (s *server) attachFleetStates(ctx context.Context, threads []sidebar.Thread) {
	courts, err := s.fleetCourts(ctx, threads)
	now := time.Now()
	for i := range threads {
		if err != nil && !nanoHolds(threads[i]) {
			continue
		}
		threads[i].Fleet = threadFleetState(threads[i], courts[threads[i].ID], now)
	}
}

// fleetCourts reads the saved-file evidence of the rows Nano does not hold.
func (s *server) fleetCourts(ctx context.Context, threads []sidebar.Thread) (map[string]fleetCourtMessage, error) {
	source, ok := s.catalog.(catalog.FleetCourtSource)
	if !ok {
		return nil, errors.New("catalog has no fleet court read")
	}
	ids := make([]string, 0, len(threads))
	for _, thread := range threads {
		if !nanoHolds(thread) {
			ids = append(ids, thread.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	courts, err := source.FleetCourt(ctx, ids)
	if err != nil {
		log.Printf("catalog fleet states for %d threads: %v", len(ids), err)
		return nil, err
	}
	return courts, nil
}

func (s *server) handleCatalogThreadRead(w http.ResponseWriter, r *http.Request, rawID string) {
	if s.catalog == nil {
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
	id, err := s.resolveThreadReference(r.Context(), reference)
	if err != nil {
		writeThreadReferenceError(w, err)
		return
	}
	thread, err := s.catalog.LoadThread(r.Context(), id)
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	if thread.Sources == nil {
		sources := []threadindex.SourceDetail{}
		thread.Sources = &sources
	}
	s.attachRuntimeOverlay(&thread)
	s.starThread(&thread)
	mailRow := []sidebar.Thread{thread}
	s.attachMailOverlay(mailRow)
	thread.Mail = mailRow[0].Mail
	if thread.Provider == "codex" && s.historicalCodex != nil {
		resolution := s.historicalCodex.ResolvePrepared(r.Context(), thread)
		thread.CurrentResolution = &resolution
	} else if thread.Provider == "claude" && s.historicalClaude != nil {
		resolution := s.historicalClaude.ResolvePrepared(r.Context(), thread)
		thread.CurrentResolution = &resolution
	}
	writeCatalogJSON(w, http.StatusOK, thread)
}

// handleCatalogIndexEvents streams catalog_version bumps as Server-Sent Events.
// The first event on every connect is the current version (contract item 2:
// version-on-connect closes the missed-while-disconnected hole), then one event
// per bump, with a heartbeat comment so a quiet stream reads as alive not dead.
// Everything that can fail is checked before the 200 header commits the stream.
func (s *server) handleCatalogIndexEvents(w http.ResponseWriter, r *http.Request) {
	stream, ok := s.catalog.(threadCatalogStream)
	if s.catalog == nil || !ok {
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
	var runtimeEvents <-chan sidebar.RuntimeDelta
	if s.runtimeEvents != nil {
		var ch chan sidebar.RuntimeDelta
		if query := r.URL.Query(); query.Has("runtimeGeneration") {
			seq, _ := strconv.ParseUint(query.Get("runtimeSeq"), 10, 64)
			ch = s.runtimeEvents.SubscribeFrom(sidebar.RuntimePosition{Generation: query.Get("runtimeGeneration"), Seq: seq})
		} else {
			ch = s.runtimeEvents.Subscribe()
		}
		defer s.runtimeEvents.Unsubscribe(ch)
		runtimeEvents = ch
	}
	// Fleet transitions mirror onto the same stream for always-connected
	// consumers. Best-effort with no replay by contract: the bookmark feed at
	// /api/v1/fleet/changes is what guarantees a transition is never missed.
	var fleetEvents <-chan fleetTransition
	if s.fleet != nil {
		ch := s.fleet.hub.subscribe()
		defer s.fleet.hub.unsubscribe(ch)
		fleetEvents = ch
	}
	// Mail count changes ride the same stream as a fourth named event. No
	// connect-time event: the client's initial window fetch carries current
	// counts on the rows (attachMailOverlay), same as the runtime overlay.
	var mailEvents <-chan featuremail.Delta
	if s.mailEvents != nil {
		ch := s.mailEvents.Subscribe()
		defer s.mailEvents.Unsubscribe(ch)
		mailEvents = ch
	}
	// The materialized sidebar publishes row/removal/root patches on this
	// existing browser stream. A refresh marker on connect closes the race with
	// the caller's cached whole-state GET without adding another SSE route.
	var sidebarEvents <-chan sidebarDelta
	if s.sidebar != nil {
		ch := s.sidebar.events.subscribe()
		defer s.sidebar.events.unsubscribe(ch)
		sidebarEvents = ch
	}
	// Transcript audit requests reuse the app-wide browser stream. The event is
	// content-free and targets one exact tab and pane; the browser fetches the
	// bounded SQLite reference only after that pane claims the request.
	var transcriptAuditEvents <-chan transcriptAuditEvent
	if s.transcriptAuditEvents != nil {
		ch := s.transcriptAuditEvents.subscribe()
		defer s.transcriptAuditEvents.unsubscribe(ch)
		transcriptAuditEvents = ch
	}
	version, err := stream.CatalogVersion(r.Context())
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	writeCatalogChangeEvent(w, catalog.Change{Version: uint64(version), Refresh: true})
	if marker, ok := s.sidebar.marker(version); ok {
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
			writeTranscriptAuditEvent(w, audit)
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
func writeRuntimeEvent(w io.Writer, delta sidebar.RuntimeDelta) {
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

func writeSidebarEvent(w io.Writer, delta sidebarDelta) {
	payload, err := json.Marshal(delta)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: sidebar\ndata: %s\n\n", payload)
}

// writeFleetEvent mirrors one fleet transition. The payload carries the same
// shape the changes endpoint returns, so a consumer parses one format.
func writeFleetEvent(w io.Writer, transition fleetTransition) {
	payload, err := json.Marshal(transition)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: fleet\ndata: %s\n\n", payload)
}

type catalogPage struct {
	Items          []sidebar.Thread `json:"items"`
	NextCursor     *string          `json:"nextCursor"`
	CatalogVersion int64            `json:"catalogVersion"`
}
