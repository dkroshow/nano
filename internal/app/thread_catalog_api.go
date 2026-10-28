package app

import (
	"context"
	"crypto/sha256"
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
	"github.com/dkroshow/nano/internal/history"
	hostedvalues "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/httpapi"
)

const (
	// limit bounds a GET /api/v1/threads response, live and supervisor rows
	// included; a larger request is clamped to the maximum. One exception: a
	// page's first row always comes with its complete supervisor chain, so a
	// chain longer than the limit makes that page exceed it.
	defaultThreadCatalogLimit = 100
	maxThreadCatalogLimit     = 250
	maxThreadCatalogIDBytes   = 256

	defaultThreadTranscriptLimit = 40
	maxThreadTranscriptLimit     = 100
	maxThreadTranscriptTurnLimit = 20
	maxThreadTranscriptBytes     = 256 << 10
	maxThreadEvidenceBytes       = 8 << 20
	maxThreadRetainedBodyBytes   = 8 << 20
	defaultThreadTranscriptView  = "enhanced"
	devThreadTranscriptView      = "dev"
	legacyThreadTranscriptView   = "readable"
	threadTranscriptTaxonomy     = "transcript-categories-v2"
	clientClosedRequestStatus    = 499
	catalogEventHeartbeat        = 30 * time.Second
	maxThreadNameRequestBytes    = 4 << 10
)

// enhancedHiddenCategories is the Enhanced view's default profile: every
// category but messages, so Enhanced shows user and agent messages only.
var enhancedHiddenCategories = []string{"reasoning", "toolCall", "toolResult", "subagent", "system", "error", "unclassified"}

var (
	errThreadCatalogUnavailable      = errors.New("thread catalog unavailable")
	errCatalogThreadNotFound         = errors.New("catalog thread not found")
	errInvalidTranscriptCursor       = errors.New("invalid transcript cursor")
	errTranscriptCursorStale         = errors.New("transcript cursor stale")
	errTranscriptEvidenceLarge       = errors.New("transcript evidence exceeds limit")
	errTranscriptEvidenceUnavailable = errors.New("transcript evidence unavailable")
)

type catalogListQuery struct {
	IncludeSubagents    bool
	IncludeGuardians    bool
	IncludeProgrammatic bool
	Archived            string
	ThreadID            string
	// Identities narrows the read to these threads; internal only, never
	// parsed from a request.
	Identities  []attachmentKey
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	UpdatedFrom *time.Time
	UpdatedTo   *time.Time
	Limit       int
	Cursor      string
}

type catalogValue struct {
	State    string  `json:"state"`
	Value    *string `json:"value"`
	Evidence *string `json:"evidence"`
}

type catalogMetadata struct {
	CWD              catalogValue `json:"cwd"`
	Model            catalogValue `json:"model"`
	Effort           catalogValue `json:"effort"`
	Priority         catalogValue `json:"priority"`
	Permission       catalogValue `json:"permission"`
	HistoricalBranch catalogValue `json:"historicalBranch"`
	Origin           catalogValue `json:"origin"`
}

type catalogSourceSummary struct {
	Status          string `json:"status"`
	Conflict        bool   `json:"conflict"`
	DiagnosticCount int    `json:"diagnosticCount"`
	LastIndexedAt   string `json:"lastIndexedAt,omitempty"`
}

type catalogSourceDetail struct {
	RootKind         string   `json:"rootKind"`
	Path             string   `json:"path"`
	Generation       int64    `json:"generation"`
	ParserVersion    string   `json:"parserVersion,omitempty"`
	Status           string   `json:"status"`
	Archived         bool     `json:"archived"`
	IdentityEvidence string   `json:"identityEvidence,omitempty"`
	DiagnosticCount  int      `json:"diagnosticCount"`
	Diagnostics      []string `json:"diagnostics"`
	Error            *string  `json:"error"`
	LastIndexedAt    *string  `json:"lastIndexedAt"`
}

// catalogThreadName is the user-assigned name overlay. It is null on threads
// without a name record (never a synthesized value); evidence is always
// user_assigned, the read-time evidence class for the truth file.
type catalogThreadName struct {
	Value      string `json:"value"`
	Provenance string `json:"provenance"`
	Evidence   string `json:"evidence"`
}

const threadNameEvidence = "user_assigned"

type catalogThread struct {
	ID                    string                 `json:"id"`
	Provider              string                 `json:"provider"`
	NativeID              string                 `json:"nativeId"`
	Kind                  string                 `json:"kind"`
	ParentNativeID        string                 `json:"parentNativeId,omitempty"`
	HostedThreadID        string                 `json:"hostedThreadId,omitempty"`
	HostedStatus          string                 `json:"hostedStatus,omitempty"`
	InvokerHostedThreadID string                 `json:"invokerHostedThreadId,omitempty"`
	InvokerNativeThreadID string                 `json:"invokerNativeThreadId,omitempty"`
	InvokerProvider       string                 `json:"invokerProvider,omitempty"`
	SidebarPlacement      string                 `json:"sidebarPlacement,omitempty"`
	ProviderVersion       *string                `json:"providerVersion,omitempty"`
	Name                  *catalogThreadName     `json:"name"`
	Title                 *string                `json:"title"`
	Preview               *string                `json:"preview"`
	CreatedAt             *string                `json:"createdAt"`
	UpdatedAt             *string                `json:"updatedAt"`
	Archived              bool                   `json:"archived"`
	Metadata              catalogMetadata        `json:"metadata"`
	Interaction           string                 `json:"interaction"`
	Source                catalogSourceSummary   `json:"source"`
	Sources               *[]catalogSourceDetail `json:"sources,omitempty"`
	Transient             bool                   `json:"transient,omitempty"`
	// Runtime is the volatile hosted-session overlay, stamped at response time
	// from the session registry (runtime.go) — never stored in SQLite, never
	// part of catalog_version. Absent = no nano session attached.
	Runtime *runtimeInfo `json:"runtime,omitempty"`
	// Assignment is the current canonical hosted-thread assignment. It is a
	// read-time overlay beside hosted identity, never catalog storage.
	Assignment            *orchestrationAssignmentView `json:"assignment,omitempty"`
	AssignmentUnavailable bool                         `json:"assignmentUnavailable,omitempty"`
	// CurrentResolution is the passive, current-environment provider setup. It
	// is attached only by the detail endpoint; list responses stay fast.
	CurrentResolution *currentResolution `json:"currentResolution,omitempty"`
	// Fleet is the read-time fleet-state classification (fleet_status.go): whose
	// court the ball is in and whether the loop is still alive. Computed per
	// response from the projected messages, never stored in SQLite and never
	// part of catalog_version. Absent when the catalog cannot classify the row.
	Fleet *fleetState `json:"fleet,omitempty"`
	// court is the saved-file evidence Fleet was computed from, kept so the
	// fleet feed can re-evaluate the time bound without reading it again.
	court fleetCourtMessage
	// Took is how long the thread's work actually took: the sum of its turns'
	// native durations, with the count of turns that sum covers. Detail-only,
	// deliberately — the two surfaces that show it (the read-only pane's info
	// bar and Details) both already fetch the detail response, so putting it on
	// catalog rows would buy nothing and cost an index-time computation, a
	// schema addition and per-window staleness. Absent when no turn carries a
	// duration; never a wall-clock first-to-last span.
	Took *catalogThreadTook `json:"took,omitempty"`
	// Starred is an operator pin overlay. It is stored in a sibling
	// SQLite file, never in the catalog, and stamped at response time.
	Starred bool `json:"starred"`
	// Mail is the durable-inbox count overlay: total, unread, and
	// read-but-undispositioned mail for the row's hosted thread. Read-time only,
	// from control.sqlite; absent without a hosted identity or without mail.
	Mail *agentMailCounts `json:"mail,omitempty"`
}

type catalogThreadTook struct {
	DurationMS int64 `json:"durationMs"`
	Turns      int64 `json:"turns"`
}

type catalogPage struct {
	Items          []catalogThread `json:"items"`
	NextCursor     *string         `json:"nextCursor"`
	CatalogVersion int64           `json:"catalogVersion"`
}

type catalogRootStatus struct {
	Provider        string  `json:"provider"`
	RootKind        string  `json:"rootKind,omitempty"`
	State           string  `json:"state"`
	SourceCount     int     `json:"sourceCount"`
	LastCompletedAt *string `json:"lastCompletedAt"`
	Error           *string `json:"error"`
}

type catalogInteractionCounts struct {
	Interactive  int `json:"interactive"`
	Programmatic int `json:"programmatic"`
	Unclassified int `json:"unclassified"`
}

type catalogIndexStatus struct {
	State                    string                   `json:"state"`
	CatalogVersion           int64                    `json:"catalogVersion"`
	Roots                    []catalogRootStatus      `json:"roots"`
	InteractionCounts        catalogInteractionCounts `json:"interactionCounts"`
	TranscriptChangeWorkers  int                      `json:"transcriptChangeWorkers"`
	TranscriptChangeBacklog  int                      `json:"transcriptChangeBacklog"`
	TranscriptChangeInFlight int                      `json:"transcriptChangeInFlight"`
	// NamesError is set when the thread-names truth file is unreadable or
	// malformed; the catalog keeps serving the last good names and reports
	// degraded. Null when the file is healthy or absent.
	NamesError *string `json:"namesError"`
}

type threadCatalog interface {
	ListThreads(context.Context, catalogListQuery) (catalogPage, error)
	LoadThread(context.Context, string) (catalogThread, error)
	IndexStatus(context.Context) (catalogIndexStatus, error)
	RequestRefresh() bool
}

// threadCatalogStream is the SSE push surface: a subscription for version bumps
// plus the version-on-connect read. Optional so a catalog without live push
// (or a stub in tests) simply reports the endpoint unavailable.
type threadCatalogStream interface {
	SubscribeCatalog() (<-chan catalogChange, func())
	CatalogVersion(context.Context) (int64, error)
}

// threadCatalogNamer is the rename write surface. It resolves the nano id to the
// truth-file key, writes only the file (never the store), and requests a refresh
// so the existing scan chain converges every open tab. Optional so a catalog
// without a names file (or a test stub) reports the endpoint unavailable.
type threadCatalogNamer interface {
	RenameThread(ctx context.Context, id, name string) (catalogThreadName, error)
	ClearThreadName(ctx context.Context, id string) error
	// AssignThreadName writes by the truth-file's own key — the spawn-time
	// path, used before any catalog row exists for the thread.
	AssignThreadName(provider, nativeID, name string) error
	// AssignThreadNameIfEmpty applies a staged pre-bind operator rename without
	// overwriting a name already stored for the provider identity.
	AssignThreadNameIfEmpty(provider, nativeID, name string) error
}

// handleSessionName renames a live session before or after its native thread
// is indexed. A fresh Claude session has no native id until its first turn, so
// its validated name remains pending until applyPendingName can persist it.
func (s *server) handleSessionName(w http.ResponseWriter, r *http.Request, sess *session) {
	w.Header().Set("Cache-Control", "private, no-store")
	namer, ok := s.catalog.(threadCatalogNamer)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	var payload struct {
		Name string `json:"name"`
	}
	body := http.MaxBytesReader(w, r.Body, maxThreadNameRequestBytes)
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_name", `Request body must be {"name": "..."}`)
		return
	}
	name, err := history.ValidateThreadName(payload.Name)
	if err != nil {
		writeThreadNameError(w, threadNameInvalidError{reason: err.Error()})
		return
	}

	// A native id, once present, is immutable for the hosted session. Holding
	// the lock while merely recording a pre-bind name lets applyPendingName see
	// exactly this value when the provider announces its identity.
	provider, nativeID, staged := sess.stageNameBeforeBinding(name)
	if staged {
		s.notifyRuntimeRefresh()
		httpapi.WriteJSON(w, sess.summary())
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
		nativeID, staged = sess.stageProvisionalName(nativeID, name)
		if staged {
			s.setAttachmentPendingName(provider, nativeID, name)
			s.notifyRuntimeRefresh()
			httpapi.WriteJSON(w, sess.summary())
			return
		}
		// The real id landed while we probed; assign under it normally.
	}

	if err := namer.AssignThreadName(provider, nativeID, name); err != nil {
		writeThreadNameError(w, err)
		return
	}
	sess.setDisplayName(name)
	s.notifyRuntimeRefresh()
	httpapi.WriteJSON(w, sess.summary())
}

// assignSpawnName is the session's nameThread closure: best-effort by design —
// a name that cannot be written must not fail the spawn it was attached to.
func (s *server) assignSpawnName(provider, nativeThreadID, name string) {
	namer, ok := s.catalog.(threadCatalogNamer)
	if s.catalog == nil || !ok {
		log.Printf("session name %q for %s/%s dropped: catalog unavailable", name, provider, nativeThreadID)
		return
	}
	if err := namer.AssignThreadNameIfEmpty(provider, nativeThreadID, name); err != nil {
		log.Printf("session name %q for %s/%s: %v", name, provider, nativeThreadID, err)
	}
}

type transcriptListQuery struct {
	Limit            int
	Before           string
	BeforeNativeTurn string
	BeforeTurn       int
	TurnLimit        int
	View             string
	HiddenCategories []string
	HiddenSpecified  bool
}

type transcriptProjection struct {
	Key               string  `json:"key,omitempty"`
	ParserVersion     string  `json:"parserVersion,omitempty"`
	SchemaVersion     int     `json:"schemaVersion"`
	State             string  `json:"state"`
	SourceStatus      string  `json:"sourceStatus"`
	LastIndexedAt     *string `json:"lastIndexedAt"`
	EvidenceAvailable bool    `json:"evidenceAvailable"`
	ItemCount         int64   `json:"itemCount"`
	ErrorSummary      *string `json:"errorSummary,omitempty"`
}

type transcriptPageInfo struct {
	StartSeq         int64   `json:"startSeq"`
	EndSeq           int64   `json:"endSeq"`
	LatestSeq        int64   `json:"latestSeq"`
	SnapshotHeadSeq  int64   `json:"snapshotHeadSeq"`
	HasMoreBefore    bool    `json:"hasMoreBefore"`
	StartsMidTurn    bool    `json:"startsMidTurn"`
	NextBeforeCursor *string `json:"nextBeforeCursor"`
}

type transcriptItem struct {
	ID               string          `json:"id"`
	OwnerItemID      string          `json:"-"`
	ProjectedSeq     int64           `json:"projectedSeq"`
	TurnID           string          `json:"turnId,omitempty"`
	NativeItemID     string          `json:"nativeItemId,omitempty"`
	Type             string          `json:"type"`
	Category         string          `json:"category"`
	Status           string          `json:"status,omitempty"`
	Role             string          `json:"role,omitempty"`
	Text             string          `json:"text,omitempty"`
	ToolName         string          `json:"toolName,omitempty"`
	CallID           string          `json:"callId,omitempty"`
	Detail           json.RawMessage `json:"detail,omitempty"`
	CreatedAt        *string         `json:"createdAt"`
	Phase            string          `json:"phase,omitempty"`
	FilterTags       []string        `json:"filterTags,omitempty"`
	Synthetic        bool            `json:"synthetic,omitempty"`
	Sidechain        bool            `json:"sidechain"`
	IsFinalAnswer    bool            `json:"isFinalAnswer"`
	ContentRef       string          `json:"contentRef,omitempty"`
	ContentTruncated bool            `json:"contentTruncated"`
	OriginalBytes    int64           `json:"originalBytes,omitempty"`
}

type transcriptTurn struct {
	ID                 string          `json:"id"`
	Ordinal            int             `json:"ordinal"`
	StartSeq           int64           `json:"startSeq"`
	EndSeq             int64           `json:"endSeq"`
	Status             string          `json:"status"`
	CompletionEvidence string          `json:"completionEvidence,omitempty"`
	FinalItemID        string          `json:"finalItemId,omitempty"`
	FinalitySource     string          `json:"finalitySource,omitempty"`
	FinalityConfidence string          `json:"finalityConfidence,omitempty"`
	DurationMS         int64           `json:"durationMs,omitempty"`
	NativeTurnID       string          `json:"nativeTurnId,omitempty"`
	Config             json.RawMessage `json:"config,omitempty"`
	StartedAt          *string         `json:"startedAt"`
	CompletedAt        *string         `json:"completedAt"`
}

type transcriptResponse struct {
	ThreadID         string               `json:"threadId"`
	View             string               `json:"view"`
	Taxonomy         string               `json:"taxonomy"`
	HiddenCategories []string             `json:"hiddenCategories"`
	Items            []transcriptItem     `json:"items"`
	Turns            []transcriptTurn     `json:"turns"`
	Page             transcriptPageInfo   `json:"page"`
	Projection       transcriptProjection `json:"projection"`
}

type threadTranscriptCatalog interface {
	LoadTranscript(context.Context, string, transcriptListQuery) (transcriptResponse, error)
}

type threadCompletedTranscriptCatalog interface {
	LoadCompletedTranscriptTurn(context.Context, string, string) (transcriptResponse, error)
}

type savedTurnStatusCatalog interface {
	ReadSavedTurnStatus(ctx context.Context, threadID, nativeTurnID string) (history.SavedTurnStatus, bool, error)
}

type threadUsageCatalog interface {
	LoadThreadUsage(context.Context, string, int, int) (history.ThreadUsagePage, error)
	ListThreadUsageTotals(context.Context, history.ThreadUsageListQuery) (history.ThreadUsageListPage, error)
}

type transcriptEvidenceResponse struct {
	ThreadID          string  `json:"threadId"`
	ItemID            string  `json:"itemId"`
	ProjectionKey     string  `json:"projectionKey"`
	Provider          string  `json:"provider"`
	SourcePath        string  `json:"sourcePath"`
	SourceLine        int     `json:"sourceLine"`
	SourceRowType     string  `json:"sourceRowType,omitempty"`
	SourcePayloadType string  `json:"sourcePayloadType,omitempty"`
	RawBytes          int     `json:"rawBytes"`
	SHA256            string  `json:"sha256"`
	ValidJSON         bool    `json:"validJson"`
	ValidUTF8         bool    `json:"validUtf8"`
	Raw               *string `json:"raw,omitempty"`
	RawBase64         string  `json:"rawBase64,omitempty"`
}

type threadTranscriptEvidenceCatalog interface {
	LoadTranscriptEvidence(context.Context, string, string) (transcriptEvidenceResponse, error)
}

type threadTranscriptBodyCatalog interface {
	LoadTranscriptBody(context.Context, string, string) (history.TranscriptRetainedBody, error)
}

func (s *server) handleCatalogThreads(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	query, err := parseCatalogListQuery(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	page, err := s.listCatalogPage(r.Context(), query)
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	writeCatalogPageJSON(w, r, http.StatusOK, page)
}

// listCatalogPage is the unified list read behind GET /api/v1/threads: durable
// rows plus memory, rescue and lineage rows, with the runtime and fleet
// overlays stamped. The compact sidebar reads a cheaper active graph through
// GET /api/v1/sidebar/threads instead of paging this full window.
func (s *server) listCatalogPage(ctx context.Context, query catalogListQuery) (catalogPage, error) {
	page, err := s.catalog.ListThreads(ctx, query)
	if err != nil {
		return catalogPage{}, err
	}
	if page.Items == nil {
		page.Items = []catalogThread{}
	}
	// Runtime rows take their place in the durable list order, so pages walk
	// one ordered stream and the cursor may point at any row. A runtime row at
	// or before the cursor went out on an earlier page; one past the last
	// durable row waits for the page that reaches it. All rows join before the
	// overlay pass, so the one compose pass stamps `runtime` on every class.
	var after, last *history.ThreadCursor
	if query.Cursor != "" {
		after, _ = decodeThreadCatalogCursor(query.Cursor)
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
		encoded, err := encodeThreadCatalogCursor(&key)
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
func listKey(row catalogThread) history.ThreadCursor {
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
	archivedIncludeFilter = "include"
)

func (s *server) runtimeThreadRows(ctx context.Context, query catalogListQuery, page []catalogThread, live []sessionSummary) []catalogThread {
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
		for _, rec := range s.attachments.detachedExcept(attached) {
			if key := rec.Key(); !covered[key] {
				wanted = append(wanted, key)
			}
		}
	}
	indexed, err := s.threadsByIdentity(ctx, wanted)
	if err != nil {
		log.Printf("catalog runtime rows for %d threads: %v", len(wanted), err)
	}
	var rows []catalogThread
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
func (s *server) threadsByIdentity(ctx context.Context, keys []attachmentKey) (map[attachmentKey]catalogThread, error) {
	found := make(map[attachmentKey]catalogThread, len(keys))
	for start := 0; start < len(keys); start += history.MaxThreadIdentities {
		query := catalogListQuery{
			Identities:       keys[start:min(start+history.MaxThreadIdentities, len(keys))],
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
func memoryThreadRow(sess sessionSummary) catalogThread {
	createdAt := sess.CreatedAt
	runtime := sessionRuntimeInfo(sess)
	var name *catalogThreadName
	if sess.Name != "" {
		name = &catalogThreadName{
			Value: sess.Name, Provenance: "manual", Evidence: threadNameEvidence,
		}
	}
	return catalogThread{
		ID:                    memoryThreadIDPrefix + sess.ID,
		Provider:              sess.Provider,
		NativeID:              sess.ThreadID,
		Kind:                  memoryThreadKind,
		HostedThreadID:        sess.HostedThreadID,
		InvokerHostedThreadID: sess.InvokerHostedThreadID,
		InvokerNativeThreadID: sess.InvokerNativeThreadID,
		InvokerProvider:       sess.InvokerProvider,
		SidebarPlacement:      sess.SidebarPlacement,
		CreatedAt:             &createdAt,
		UpdatedAt:             &createdAt,
		Metadata: catalogMetadata{
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
		},
		Interaction: memoryThreadInteraction,
		Name:        name,
		// Source summarizes the provider files behind a thread; a memory row
		// exists precisely because none have been indexed yet.
		Source:    catalogSourceSummary{},
		Transient: true,
		Runtime:   &runtime,
	}
}

// runtimeCatalogValue makes one metadata cell from a live session field: known
// when the session holds the value, unknown while it is still being learned
// (claude's model arrives with the init line, after the row can first appear).
func runtimeCatalogValue(value string) catalogValue {
	if value == "" {
		return unknownCatalogValue()
	}
	evidence := runtimeEvidence
	return catalogValue{State: string(history.MetadataKnown), Value: &value, Evidence: &evidence}
}

func unknownCatalogValue() catalogValue {
	return catalogValue{State: string(history.MetadataUnknown)}
}

// lineageParentRows closes the graph around every worker in a list response:
// a worker is never returned without the indexed supervisors that connect it
// to the thread tree, which can sit outside the requested window. It walks one
// level at a time with one hosted-record read, one assignment read and one
// identity read per level, never per row, and returns parents in the order a
// one-row walk finds them.
func (s *server) lineageParentRows(ctx context.Context, query catalogListQuery, page []catalogThread) []catalogThread {
	if query.ThreadID != "" {
		return nil
	}
	covered, visited := make(map[attachmentKey]bool, len(page)), map[attachmentKey]bool{}
	for _, item := range page {
		if item.NativeID != "" {
			covered[attachmentKey{Provider: item.Provider, NativeThreadID: item.NativeID}] = true
		}
	}
	var parents []catalogThread
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
func (s *server) lineageParentKeys(rows []catalogThread) []attachmentKey {
	records, found := s.sidebarHostedRecords(rows)
	var assignments map[string]*orchestrationAssignmentView
	if s.controlMetadata != nil && s.controlMetadataErr == "" {
		ids := make([]string, 0, len(rows))
		for i := range rows {
			if found[i] {
				ids = append(ids, records[i].ID)
			}
		}
		assignments = s.controlMetadata.currentAssignmentsByHostedIDs(ids)
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
			if parent, ok := s.agentExecutions.session(parentID); ok && parent.Provider != "" && parent.NativeThreadID != "" {
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
func (s *server) attachRuntimeOverlay(thread *catalogThread) {
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

func (s *server) applyHostedCatalogMetadata(thread *catalogThread, hostedID string) {
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
	view, err := s.controlMetadata.view(hostedID)
	if err != nil {
		setUnknown()
		return
	}
	canonical := func(field string) catalogValue {
		value, ok := hostedvalues.MetadataString(view.Fields[field])
		if !ok {
			return unknownCatalogValue()
		}
		evidence := "canonical_hosted_metadata"
		return catalogValue{State: string(history.MetadataKnown), Value: &value, Evidence: &evidence}
	}
	thread.Metadata.CWD = canonical("cwd")
	thread.Metadata.Model = canonical(postureFieldModel)
	thread.Metadata.Effort = canonical(postureFieldEffort)
	thread.Metadata.Priority = canonical(postureFieldServiceTier)
	thread.Metadata.Permission = canonical(postureFieldPermissionMode)
}

func (s *server) applyHostedCatalogAssignment(thread *catalogThread, hostedID string) {
	thread.Assignment = nil
	thread.AssignmentUnavailable = false
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
		thread.AssignmentUnavailable = true
		return
	}
	assignment, err := s.controlMetadata.currentOrchestrationAssignment(hostedID)
	if err != nil {
		thread.AssignmentUnavailable = true
		return
	}
	thread.Assignment = assignment
}

func (s *server) catalogHostedThreadRecord(thread catalogThread) (agentSessionRecord, bool) {
	if s.agentExecutions == nil {
		return agentSessionRecord{}, false
	}
	if thread.HostedThreadID != "" {
		if record, ok := s.agentExecutions.session(thread.HostedThreadID); ok {
			return record, true
		}
	}
	return s.agentExecutions.sessionForNative(thread.Provider, thread.NativeID)
}

// attachFleetStates stamps each row's turn state (fleet_status.go). Rows Nano
// does not hold are classified from their saved files in one batch query.
//
// Best-effort by design: a failed classification
// leaves those rows' field absent rather than failing the page.
func (s *server) attachFleetStates(ctx context.Context, threads []catalogThread) {
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
func (s *server) fleetCourts(ctx context.Context, threads []catalogThread) (map[string]fleetCourtMessage, error) {
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
		sources := []catalogSourceDetail{}
		thread.Sources = &sources
	}
	s.attachRuntimeOverlay(&thread)
	s.starThread(&thread)
	mailRow := []catalogThread{thread}
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

func (s *server) handleThreadTranscriptBody(w http.ResponseWriter, r *http.Request, rawThreadID, rawItemID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(threadTranscriptBodyCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	threadID, itemID := rawThreadID, rawItemID
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(itemID) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_body_id", "Invalid transcript body path")
		return
	}
	response, err := catalog.LoadTranscriptBody(r.Context(), threadID, itemID)
	if err != nil {
		writeTranscriptBackendError(w, err)
		return
	}
	body, err := json.Marshal(response)
	if err != nil || len(body) > maxThreadRetainedBodyBytes {
		httpapi.WriteError(w, http.StatusInternalServerError, "body_error", "Retained transcript body request failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *server) handleThreadTranscriptEvidence(w http.ResponseWriter, r *http.Request, rawThreadID, rawItemID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(threadTranscriptEvidenceCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	threadID, itemID := rawThreadID, rawItemID
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(itemID) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_evidence_id", "Invalid transcript evidence path")
		return
	}
	response, err := catalog.LoadTranscriptEvidence(r.Context(), threadID, itemID)
	if err != nil {
		writeTranscriptBackendError(w, err)
		return
	}
	body, err := json.Marshal(response)
	if err != nil || response.RawBytes > maxThreadEvidenceBytes {
		httpapi.WriteError(w, http.StatusInternalServerError, "evidence_error", "Transcript evidence request failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *server) handleThreadUsage(w http.ResponseWriter, r *http.Request, rawID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(threadUsageCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	reference := rawID
	if strings.TrimSpace(reference) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	afterOrdinal := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("afterOrdinal")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "afterOrdinal must be a non-negative integer")
			return
		}
		afterOrdinal = value
	}
	limit := history.DefaultThreadUsageTurnLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "limit must be a positive integer")
			return
		}
		limit = value
	}
	id, err := s.resolveThreadReference(r.Context(), reference)
	if err != nil {
		writeThreadReferenceError(w, err)
		return
	}
	page, err := catalog.LoadThreadUsage(r.Context(), id, afterOrdinal, limit)
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	if page.Turns == nil {
		page.Turns = []history.ThreadUsageTurn{}
	}
	writeCatalogJSON(w, http.StatusOK, page)
}

func (s *server) handleThreadUsageList(w http.ResponseWriter, r *http.Request) {
	catalog, ok := s.catalog.(threadUsageCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	query := history.ThreadUsageListQuery{Archived: history.ArchivedExclude}
	if provider := strings.TrimSpace(r.URL.Query().Get("provider")); provider != "" {
		query.Provider = history.Provider(provider)
		if query.Provider != history.ProviderClaude && query.Provider != history.ProviderCodex && query.Provider != history.ProviderGrok {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "provider must be claude, codex, or grok")
			return
		}
	}
	if kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind != "" {
		query.Kind = history.ThreadKind(kind)
	}
	switch archived := strings.TrimSpace(r.URL.Query().Get("archived")); archived {
	case "", "exclude":
		query.Archived = history.ArchivedExclude
	case "include":
		query.Archived = history.ArchivedInclude
	case "only":
		query.Archived = history.ArchivedOnly
	default:
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "archived must be include, exclude, or only")
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "limit must be a positive integer")
			return
		}
		query.Limit = value
	}
	query.AfterID = strings.TrimSpace(r.URL.Query().Get("cursor"))
	page, err := catalog.ListThreadUsageTotals(r.Context(), query)
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	if page.Items == nil {
		page.Items = []history.ThreadUsageTotals{}
	}
	writeCatalogJSON(w, http.StatusOK, page)
}

func (s *server) handleThreadTranscript(w http.ResponseWriter, r *http.Request, rawID string) {
	// Error responses must never become reusable transcript representations.
	// The successful path replaces this with its private revalidation policy.
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(threadTranscriptCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	reference := rawID
	if strings.TrimSpace(reference) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	query, err := parseTranscriptListQuery(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	id, err := s.resolveThreadReference(r.Context(), reference)
	if err != nil {
		writeThreadReferenceError(w, err)
		return
	}
	response, err := catalog.LoadTranscript(r.Context(), id, query)
	if err != nil {
		writeTranscriptBackendError(w, err)
		return
	}
	if response.Items == nil {
		response.Items = []transcriptItem{}
	}
	if response.Turns == nil {
		response.Turns = []transcriptTurn{}
	}
	body, err := json.Marshal(response)
	if err != nil || len(body) > maxThreadTranscriptBytes {
		httpapi.WriteError(w, http.StatusInternalServerError, "transcript_error", "Transcript request failed")
		return
	}
	etag := transcriptETag(body)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Accept-Encoding")
	if requestETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *server) threadNamerAndID(w http.ResponseWriter, r *http.Request) (threadCatalogNamer, string, bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	namer, ok := s.catalog.(threadCatalogNamer)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return nil, "", false
	}
	id := strings.TrimSpace(r.PathValue("threadId"))
	if id == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return nil, "", false
	}
	return namer, id, true
}

// Thread names write only the truth file; the catalog worker reconciles the
// derived copy asynchronously with a names-only refresh.
func (s *server) handleThreadNameSet(w http.ResponseWriter, r *http.Request) {
	namer, id, ok := s.threadNamerAndID(w, r)
	if !ok {
		return
	}
	var payload struct {
		Name string `json:"name"`
	}
	body := http.MaxBytesReader(w, r.Body, maxThreadNameRequestBytes)
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_name", `Request body must be {"name": "..."}`)
		return
	}
	name, err := namer.RenameThread(r.Context(), id, payload.Name)
	if err != nil {
		writeThreadNameError(w, err)
		return
	}
	writeCatalogJSON(w, http.StatusOK, map[string]catalogThreadName{"name": name})
}

func (s *server) handleThreadNameClear(w http.ResponseWriter, r *http.Request) {
	namer, id, ok := s.threadNamerAndID(w, r)
	if !ok {
		return
	}
	if err := namer.ClearThreadName(r.Context(), id); err != nil {
		writeThreadNameError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeThreadNameError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidThreadName):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_name", err.Error())
	case errors.Is(err, history.ErrThreadNamesMalformed):
		httpapi.WriteError(w, http.StatusConflict, "names_file_malformed",
			"Thread names file is malformed; repair it by hand before renaming — nano will not overwrite broken truth")

	case errors.Is(err, errCatalogThreadNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
	case errors.Is(err, errNamesFileDisabled):
		httpapi.WriteError(w, http.StatusServiceUnavailable, "names_unavailable", "Thread names are not configured")
	case errors.Is(err, context.Canceled):
		httpapi.WriteError(w, clientClosedRequestStatus, "request_cancelled", "Request was cancelled")
	default:
		httpapi.WriteError(w, http.StatusInternalServerError, "name_error", "Thread name update failed")
	}
}

func parseTranscriptListQuery(r *http.Request) (transcriptListQuery, error) {
	q := r.URL.Query()
	query := transcriptListQuery{
		Limit:  defaultThreadTranscriptLimit,
		Before: strings.TrimSpace(q.Get("before")),
		View:   strings.TrimSpace(q.Get("view")),
	}
	if query.View == "" {
		query.View = defaultThreadTranscriptView
	}
	if query.View == legacyThreadTranscriptView {
		query.View = defaultThreadTranscriptView
	}
	if query.View != defaultThreadTranscriptView && query.View != devThreadTranscriptView {
		return query, errors.New("view must be enhanced or dev")
	}
	var hiddenSpecified bool
	if _, ok := q["hide"]; ok {
		hiddenSpecified = true
		query.HiddenSpecified = true
		values := make([]string, 0)
		for _, raw := range q["hide"] {
			for _, value := range strings.Split(raw, ",") {
				value = strings.TrimSpace(value)
				if value != "" {
					values = append(values, value)
				}
			}
		}
		if len(values) == 1 && values[0] == "none" {
			values = nil
		} else {
			for _, value := range values {
				if value == "none" {
					return query, errors.New("hide=none cannot be combined with categories")
				}
			}
		}
		query.HiddenCategories = values
	}
	if !hiddenSpecified && query.View == defaultThreadTranscriptView {
		query.HiddenCategories = append([]string(nil), enhancedHiddenCategories...)
	}
	canonical, err := canonicalTranscriptCategories(query.HiddenCategories)
	if err != nil {
		return query, err
	}
	query.HiddenCategories = canonical
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return query, errors.New("limit must be a positive integer")
		}
		query.Limit = value
	}
	if query.Limit > maxThreadTranscriptLimit {
		query.Limit = maxThreadTranscriptLimit
	}
	_, hasBeforeNativeTurn := q["beforeNativeTurn"]
	_, hasBeforeTurn := q["beforeTurn"]
	_, hasTurnLimit := q["turnLimit"]
	if hasBeforeNativeTurn && hasBeforeTurn {
		return query, errors.New("beforeNativeTurn and beforeTurn cannot be combined")
	}
	if (hasBeforeNativeTurn || hasBeforeTurn) != hasTurnLimit {
		return query, errors.New("turnLimit requires exactly one turn boundary")
	}
	if query.Before != "" && hasTurnLimit {
		return query, errors.New("before cursor and turn window cannot be combined")
	}
	if hasBeforeNativeTurn {
		query.BeforeNativeTurn = strings.TrimSpace(q.Get("beforeNativeTurn"))
		if query.BeforeNativeTurn == "" {
			return query, errors.New("beforeNativeTurn must not be empty")
		}
	}
	if hasBeforeTurn {
		value, err := strconv.Atoi(strings.TrimSpace(q.Get("beforeTurn")))
		if err != nil || value <= 0 {
			return query, errors.New("beforeTurn must be a positive integer")
		}
		query.BeforeTurn = value
	}
	if hasTurnLimit {
		value, err := strconv.Atoi(strings.TrimSpace(q.Get("turnLimit")))
		if err != nil || value <= 0 || value > maxThreadTranscriptTurnLimit {
			return query, fmt.Errorf("turnLimit must be an integer from 1 to %d", maxThreadTranscriptTurnLimit)
		}
		query.TurnLimit = value
	}
	return query, nil
}

func canonicalTranscriptCategories(values []string) ([]string, error) {
	order := []string{"messages", "reasoning", "toolCall", "toolResult", "subagent", "system", "error", "unclassified"}
	known := make(map[string]bool, len(order))
	for _, category := range order {
		known[category] = true
	}
	seen := make(map[string]bool, len(values))
	for _, category := range values {
		if !known[category] {
			return nil, fmt.Errorf("unknown transcript category %q", category)
		}
		seen[category] = true
	}
	canonical := make([]string, 0, len(seen))
	for _, category := range order {
		if seen[category] {
			canonical = append(canonical, category)
		}
	}
	return canonical, nil
}

func writeTranscriptBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidTranscriptCursor):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_cursor", "Invalid transcript cursor")
	case errors.Is(err, errTranscriptCursorStale):
		httpapi.WriteError(w, http.StatusConflict, "transcript_changed", "Transcript changed; reload the newest page")
	case errors.Is(err, errCatalogThreadNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
	case errors.Is(err, errThreadCatalogUnavailable):
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
	case errors.Is(err, errTranscriptEvidenceLarge):
		httpapi.WriteError(w, http.StatusRequestEntityTooLarge, "evidence_too_large", "Native transcript row exceeds the evidence response limit")
	case errors.Is(err, errTranscriptEvidenceUnavailable):
		httpapi.WriteError(w, http.StatusGone, "evidence_unavailable", "Native transcript evidence is no longer available")
	case errors.Is(err, history.ErrTranscriptBodyUnavailable):
		httpapi.WriteError(w, http.StatusGone, "body_unavailable", "Retained transcript body is no longer available")
	case errors.Is(err, context.Canceled):
		httpapi.WriteError(w, clientClosedRequestStatus, "request_cancelled", "Request was cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		httpapi.WriteError(w, http.StatusGatewayTimeout, "request_timeout", "Request timed out")
	default:
		httpapi.WriteError(w, http.StatusInternalServerError, "transcript_error", "Transcript request failed")
	}
}

func transcriptETag(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("\"%x\"", digest[:16])
}

func requestETagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func (s *server) handleCatalogIndexStatus(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	status, err := s.catalog.IndexStatus(r.Context())
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	if status.Roots == nil {
		status.Roots = []catalogRootStatus{}
	}
	writeCatalogJSON(w, http.StatusOK, status)
}

// handleCatalogRefresh is Explorer's Refresh: the only request that walks the
// provider roots (a full recovery walk).
func (s *server) handleCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	writeCatalogJSON(w, http.StatusAccepted, map[string]bool{"accepted": s.catalog.RequestRefresh()})
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
	var runtimeEvents <-chan runtimeDelta
	if s.runtimeEvents != nil {
		var ch chan runtimeDelta
		if query := r.URL.Query(); query.Has("runtimeGeneration") {
			seq, _ := strconv.ParseUint(query.Get("runtimeSeq"), 10, 64)
			ch = s.runtimeEvents.subscribeFrom(runtimePosition{Generation: query.Get("runtimeGeneration"), Seq: seq})
		} else {
			ch = s.runtimeEvents.subscribe()
		}
		defer s.runtimeEvents.unsubscribe(ch)
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
	var mailEvents <-chan mailDelta
	if s.mailEvents != nil {
		ch := s.mailEvents.subscribe()
		defer s.mailEvents.unsubscribe(ch)
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
	writeCatalogChangeEvent(w, catalogChange{Version: uint64(version), Refresh: true})
	if marker, ok := s.sidebar.marker(version); ok {
		writeSidebarEvent(w, marker)
	}
	flusher.Flush()

	heartbeat := time.NewTicker(catalogEventHeartbeat)
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

func writeCatalogChangeEvent(w io.Writer, change catalogChange) {
	payload := struct {
		CatalogVersion int64    `json:"catalogVersion"`
		ThreadIDs      []string `json:"threadIds,omitempty"`
		Refresh        bool     `json:"refresh,omitempty"`
	}{CatalogVersion: catalogVersion(change.Version), ThreadIDs: change.ThreadIDs, Refresh: change.Refresh}
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
func writeRuntimeEvent(w io.Writer, delta runtimeDelta) {
	payload, err := json.Marshal(delta)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: runtime\ndata: %s\n\n", payload)
}

// writeMailEvent emits the current inbox counts for each changed hosted
// thread plus the global summary — last-value-wins state, like runtime.
func writeMailEvent(w io.Writer, delta mailDelta) {
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

func parseCatalogListQuery(r *http.Request) (catalogListQuery, error) {
	q := r.URL.Query()
	query := catalogListQuery{
		Archived: strings.TrimSpace(q.Get("archived")),
		ThreadID: strings.TrimSpace(q.Get("threadId")),
		Cursor:   strings.TrimSpace(q.Get("cursor")),
		Limit:    defaultThreadCatalogLimit,
	}
	if len(query.ThreadID) > maxThreadCatalogIDBytes {
		return query, fmt.Errorf("threadId must be at most %d bytes", maxThreadCatalogIDBytes)
	}
	if query.Archived == "" {
		query.Archived = "include"
	}
	if query.Archived != "include" && query.Archived != "exclude" && query.Archived != "only" {
		return query, errors.New("archived must be include, exclude, or only")
	}
	if raw := strings.TrimSpace(q.Get("includeSubagents")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return query, errors.New("includeSubagents must be true or false")
		}
		query.IncludeSubagents = v
	}
	if raw := strings.TrimSpace(q.Get("includeGuardians")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return query, errors.New("includeGuardians must be true or false")
		}
		query.IncludeGuardians = v
	}
	if raw := strings.TrimSpace(q.Get("includeProgrammatic")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return query, errors.New("includeProgrammatic must be true or false")
		}
		query.IncludeProgrammatic = v
	}
	timeBounds := []struct {
		param string
		at    **time.Time
	}{
		{"createdFrom", &query.CreatedFrom},
		{"createdTo", &query.CreatedTo},
		{"updatedFrom", &query.UpdatedFrom},
		{"updatedTo", &query.UpdatedTo},
	}
	for _, b := range timeBounds {
		raw := strings.TrimSpace(q.Get(b.param))
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return query, fmt.Errorf("%s must be an RFC3339 timestamp", b.param)
		}
		*b.at = &parsed
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			return query, errors.New("limit must be a positive integer")
		}
		query.Limit = v
	}
	if query.Limit > maxThreadCatalogLimit {
		query.Limit = maxThreadCatalogLimit
	}
	return query, nil
}

func writeCatalogBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidThreadCatalogCursor):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_cursor", "Invalid thread catalog cursor")
	case errors.Is(err, errCatalogThreadNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
	case errors.Is(err, errThreadCatalogUnavailable):
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
	default:
		httpapi.WriteError(w, http.StatusInternalServerError, "catalog_error", "Thread catalog request failed")
	}
}

func writeCatalogJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeCatalogPageJSON gives the window endpoint a stable validator. Runtime
// and fleet overlays are part of the representation, so the hash changes when
// a visible overlay changes even though catalog_version does not.
func writeCatalogPageJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "encode_error", "Catalog response failed")
		return
	}
	etag := transcriptETag(body)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Accept-Encoding")
	if requestETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
