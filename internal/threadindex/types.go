package threadindex

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

const (
	// limit bounds a GET /api/v1/threads response, live and supervisor rows
	// included; a larger request is clamped to the maximum. One exception: a
	// page's first row always comes with its complete supervisor chain, so a
	// chain longer than the limit makes that page exceed it.
	defaultThreadCatalogLimit = 100
	// MaxThreadCatalogLimit caps saved-thread windows.
	MaxThreadCatalogLimit   = 250
	maxThreadCatalogIDBytes = 256

	defaultThreadTranscriptLimit = 40
	// MaxThreadTranscriptLimit caps items in one transcript page.
	MaxThreadTranscriptLimit     = 100
	maxThreadTranscriptTurnLimit = 20
	// MaxThreadTranscriptBytes bounds a serialized transcript response.
	MaxThreadTranscriptBytes = 256 << 10
	// maxThreadEvidenceBytes bounds native evidence returned by the API.
	maxThreadEvidenceBytes = 8 << 20
	// maxThreadRetainedBodyBytes bounds a retained-body response.
	maxThreadRetainedBodyBytes = 8 << 20
	// DefaultThreadTranscriptView is the canonical enhanced message view.
	DefaultThreadTranscriptView = "enhanced"
	// DevThreadTranscriptView exposes the complete saved transcript taxonomy.
	DevThreadTranscriptView    = "dev"
	legacyThreadTranscriptView = "readable"
	threadTranscriptTaxonomy   = "transcript-categories-v2"
	clientClosedRequestStatus  = 499
	// CatalogEventHeartbeat is the heartbeat interval used by the combined app event stream.
	CatalogEventHeartbeat = 30 * time.Second
	// MaxThreadNameRequestBytes bounds a name-write request body.
	MaxThreadNameRequestBytes = 4 << 10
)

// enhancedHiddenCategories is the Enhanced view's default profile: every
// category but messages, so Enhanced shows user and agent messages only.
var enhancedHiddenCategories = []string{"reasoning", "toolCall", "toolResult", "subagent", "system", "error", "unclassified"}

var (
	// ErrThreadCatalogUnavailable reports an unavailable saved catalog.
	ErrThreadCatalogUnavailable = errors.New("thread catalog unavailable")
	// ErrCatalogThreadNotFound reports an unknown saved thread.
	ErrCatalogThreadNotFound         = errors.New("catalog thread not found")
	errInvalidTranscriptCursor       = errors.New("invalid transcript cursor")
	errTranscriptCursorStale         = errors.New("transcript cursor stale")
	errTranscriptEvidenceLarge       = errors.New("transcript evidence exceeds limit")
	errTranscriptEvidenceUnavailable = errors.New("transcript evidence unavailable")
)

// ListQuery selects and pages saved catalog rows.
type ListQuery struct {
	IncludeSubagents    bool
	IncludeGuardians    bool
	IncludeProgrammatic bool
	Archived            string
	ThreadID            string
	// Identities narrows the read to these threads; internal only, never
	// parsed from a request.
	Identities  []history.ThreadIdentity
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	UpdatedFrom *time.Time
	UpdatedTo   *time.Time
	Limit       int
	Cursor      string
}

// Value pairs a metadata value with its state and evidence.
type Value struct {
	State    string  `json:"state"`
	Value    *string `json:"value"`
	Evidence *string `json:"evidence"`
}

// Metadata contains the provider metadata projected from saved evidence.
type Metadata struct {
	CWD              Value `json:"cwd"`
	Model            Value `json:"model"`
	Effort           Value `json:"effort"`
	Priority         Value `json:"priority"`
	Permission       Value `json:"permission"`
	HistoricalBranch Value `json:"historicalBranch"`
	Origin           Value `json:"origin"`
}

// SourceSummary describes the health of a thread’s saved source.
type SourceSummary struct {
	Status          string `json:"status"`
	Conflict        bool   `json:"conflict"`
	DiagnosticCount int    `json:"diagnosticCount"`
	LastIndexedAt   string `json:"lastIndexedAt,omitempty"`
}

// SourceDetail describes one provider source and its bounded diagnostics.
type SourceDetail struct {
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

// ThreadName is the user-assigned name overlay. It is null on threads
// without a name record (never a synthesized value); evidence is always
// user_assigned, the read-time evidence class for the truth file.
type ThreadName struct {
	Value      string `json:"value"`
	Provenance string `json:"provenance"`
	Evidence   string `json:"evidence"`
}

// ThreadNameEvidence labels an operator-assigned name in catalog responses.
const ThreadNameEvidence = "user_assigned"

// Thread is saved catalog data, before application runtime and hosted overlays.
type Thread struct {
	ID              string          `json:"id"`
	Provider        string          `json:"provider"`
	NativeID        string          `json:"nativeId"`
	Kind            string          `json:"kind"`
	ParentNativeID  string          `json:"parentNativeId,omitempty"`
	ProviderVersion *string         `json:"providerVersion,omitempty"`
	Name            *ThreadName     `json:"name"`
	Title           *string         `json:"title"`
	Preview         *string         `json:"preview"`
	CreatedAt       *string         `json:"createdAt"`
	UpdatedAt       *string         `json:"updatedAt"`
	Archived        bool            `json:"archived"`
	Metadata        Metadata        `json:"metadata"`
	Interaction     string          `json:"interaction"`
	Source          SourceSummary   `json:"source"`
	Sources         *[]SourceDetail `json:"sources,omitempty"`
	// Took is how long the thread's work actually took: the sum of its turns'
	// native durations, with the count of turns that sum covers. Detail-only,
	// deliberately — the two surfaces that show it (the read-only pane's info
	// bar and Details) both already fetch the detail response, so putting it on
	// catalog rows would buy nothing and cost an index-time computation, a
	// schema addition and per-window staleness. Absent when no turn carries a
	// duration; never a wall-clock first-to-last span.
	Took *ThreadTook `json:"took,omitempty"`
}

// ThreadTook sums native turn durations with their evidence count.
type ThreadTook struct {
	DurationMS int64 `json:"durationMs"`
	Turns      int64 `json:"turns"`
}

// Page contains a window of saved catalog rows and its continuation cursor.
type Page struct {
	Items          []Thread `json:"items"`
	NextCursor     *string  `json:"nextCursor"`
	CatalogVersion int64    `json:"catalogVersion"`
}

// RootStatus reports catalog progress for one configured provider root.
type RootStatus struct {
	Provider        string  `json:"provider"`
	RootKind        string  `json:"rootKind,omitempty"`
	State           string  `json:"state"`
	SourceCount     int     `json:"sourceCount"`
	LastCompletedAt *string `json:"lastCompletedAt"`
	Error           *string `json:"error"`
}

// InteractionCounts groups indexed threads by interaction classification.
type InteractionCounts struct {
	Interactive  int `json:"interactive"`
	Programmatic int `json:"programmatic"`
	Unclassified int `json:"unclassified"`
}

// IndexStatus reports catalog readiness and pending transcript publication.
type IndexStatus struct {
	State                    string            `json:"state"`
	CatalogVersion           int64             `json:"catalogVersion"`
	Roots                    []RootStatus      `json:"roots"`
	InteractionCounts        InteractionCounts `json:"interactionCounts"`
	TranscriptChangeWorkers  int               `json:"transcriptChangeWorkers"`
	TranscriptChangeBacklog  int               `json:"transcriptChangeBacklog"`
	TranscriptChangeInFlight int               `json:"transcriptChangeInFlight"`
	// NamesError is set when the thread-names truth file is unreadable or
	// malformed; the catalog keeps serving the last good names and reports
	// degraded. Null when the file is healthy or absent.
	NamesError *string `json:"namesError"`
}

// TranscriptQuery selects a bounded saved transcript window and category profile.
type TranscriptQuery struct {
	Limit            int
	Before           string
	BeforeNativeTurn string
	BeforeTurn       int
	TurnLimit        int
	View             string
	HiddenCategories []string
	HiddenSpecified  bool
}

// TranscriptProjection identifies the saved projection and its evidence state.
type TranscriptProjection struct {
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

// TranscriptPageInfo describes the stable boundaries of a saved transcript page.
type TranscriptPageInfo struct {
	StartSeq         int64   `json:"startSeq"`
	EndSeq           int64   `json:"endSeq"`
	LatestSeq        int64   `json:"latestSeq"`
	SnapshotHeadSeq  int64   `json:"snapshotHeadSeq"`
	HasMoreBefore    bool    `json:"hasMoreBefore"`
	StartsMidTurn    bool    `json:"startsMidTurn"`
	NextBeforeCursor *string `json:"nextBeforeCursor"`
}

// TranscriptItem is one bounded item from a provider’s saved transcript.
type TranscriptItem struct {
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

// TranscriptTurn describes one turn represented by a saved transcript window.
type TranscriptTurn struct {
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

// TranscriptResponse contains saved items, turns and their paging metadata.
type TranscriptResponse struct {
	ThreadID         string               `json:"threadId"`
	View             string               `json:"view"`
	Taxonomy         string               `json:"taxonomy"`
	HiddenCategories []string             `json:"hiddenCategories"`
	Items            []TranscriptItem     `json:"items"`
	Turns            []TranscriptTurn     `json:"turns"`
	Page             TranscriptPageInfo   `json:"page"`
	Projection       TranscriptProjection `json:"projection"`
}

// TranscriptCatalog is the bounded saved-transcript read contract.
type TranscriptCatalog interface {
	LoadTranscript(context.Context, string, TranscriptQuery) (TranscriptResponse, error)
}

// usageCatalog reads saved turn usage and per-thread totals.
type usageCatalog interface {
	LoadThreadUsage(context.Context, string, int, int) (history.ThreadUsagePage, error)
	ListThreadUsageTotals(context.Context, history.ThreadUsageListQuery) (history.ThreadUsageListPage, error)
}

// TranscriptEvidenceResponse describes bounded native evidence for one saved item.
type TranscriptEvidenceResponse struct {
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

// transcriptEvidenceCatalog reads the native evidence of one saved item.
type transcriptEvidenceCatalog interface {
	LoadTranscriptEvidence(context.Context, string, string) (TranscriptEvidenceResponse, error)
}

// transcriptBodyCatalog reads a saved item’s retained full body.
type transcriptBodyCatalog interface {
	LoadTranscriptBody(context.Context, string, string) (history.TranscriptRetainedBody, error)
}

// EnhancedHiddenCategories returns the default categories hidden by the enhanced transcript view.
func EnhancedHiddenCategories() []string { return append([]string(nil), enhancedHiddenCategories...) }
