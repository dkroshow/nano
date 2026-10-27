package history

import (
	"errors"
	"time"
)

var ErrThreadNotFound = errors.New("history thread not found")

type SourceStatus string

const (
	SourcePresent SourceStatus = "present"
	SourceMissing SourceStatus = "source_missing"
	SourceError   SourceStatus = "error"
)

type IndexStateName string

const (
	IndexStarting IndexStateName = "starting"
	IndexScanning IndexStateName = "scanning"
	IndexReady    IndexStateName = "ready"
	IndexDegraded IndexStateName = "degraded"
)

type ArchivedFilter string

const (
	ArchivedInclude ArchivedFilter = "include"
	ArchivedExclude ArchivedFilter = "exclude"
	ArchivedOnly    ArchivedFilter = "only"
)

// PersistedRoot is a configured provider root after it has been assigned its
// store-local identifier. Root IDs never cross the HTTP boundary.
type PersistedRoot struct {
	ID      int64
	Root    SourceRoot
	Enabled bool
}

// StoredSource exposes the committed append checkpoint and last good
// projection needed by scanners to skip unchanged files or project an append.
// It contains no transcript content.
type StoredSource struct {
	ID                        int64
	ThreadID                  string
	CanonicalTranscriptSource bool
	RootID                    int64
	Source                    SourceCandidate
	CompleteOffset            int64
	CompleteLines             int
	Generation                int64
	ParserVersion             string
	Status                    SourceStatus
	Diagnostics               []Diagnostic
	DiagnosticSummary         DiagnosticSummary
	ErrorSummary              *string
	LastIndexedAt             *time.Time
	Projection                *ThreadProjection
}

// SourceProjection is the atomic persistence input for one observed native
// file. A nil Result records source health while preserving its last good
// thread projection and checkpoint.
type SourceProjection struct {
	Source        SourceCandidate
	Result        *ProjectionResult
	ParserVersion string
	Status        SourceStatus
	ErrorSummary  *string
	IndexedAt     time.Time
	// Rewritten marks a file rewritten in place, whose transcript must be
	// rebuilt. The new generation marks the old transcript stale in the same
	// commit, so a stop before the rebuild cannot leave it looking current;
	// its rows are still served until the rebuild replaces them.
	Rewritten bool
}

type ApplyResult struct {
	SourceID       int64
	ThreadID       string
	CatalogVersion uint64
	Changed        bool
}

// RootReconciliation applies projections from one discovery result. A complete
// sweep supplies the full ObservedPaths set, so stored paths absent from that set
// can be marked missing. Partial is reserved for watcher-reported files; it
// applies those projections but leaves absence detection to the next sweep.
type RootReconciliation struct {
	RootID        int64
	ObservedPaths []string
	Sources       []SourceProjection
	Partial       bool
	State         IndexStateName
	CompletedAt   time.Time
	Error         *string
}

type ReconcileResult struct {
	CatalogVersion   uint64
	Changed          bool
	ChangedThreadIDs []string
	Structural       bool
	SourceCount      int
}

type SourceSummary struct {
	Status          SourceStatus `json:"status"`
	Conflict        bool         `json:"conflict"`
	DiagnosticCount int          `json:"diagnosticCount"`
	LastIndexedAt   *time.Time   `json:"lastIndexedAt"`
}

// Thread is the catalog read model. NativeID is deliberately separate from
// the opaque Nano ID so callers never need SQLite row identifiers.
type Thread struct {
	ID              string        `json:"id"`
	Provider        Provider      `json:"provider"`
	NativeID        string        `json:"nativeId"`
	Kind            ThreadKind    `json:"kind"`
	ParentNativeID  string        `json:"parentNativeId,omitempty"`
	ProviderVersion *string       `json:"providerVersion,omitempty"`
	Title           *string       `json:"title"`
	Preview         *string       `json:"preview"`
	CreatedAt       *time.Time    `json:"createdAt"`
	UpdatedAt       *time.Time    `json:"updatedAt"`
	Archived        bool          `json:"archived"`
	Metadata        Metadata      `json:"metadata"`
	Interaction     Interaction   `json:"interaction"`
	Source          SourceSummary `json:"source"`
	// Name is the user-assigned name overlay from the truth file, joined at
	// read time. Nil when the thread has no name record — never fabricated.
	Name *ThreadNameRef `json:"name"`
}

type ThreadSource struct {
	RootKind          RootKind          `json:"rootKind"`
	Path              string            `json:"path"`
	Generation        int64             `json:"generation"`
	ParserVersion     string            `json:"parserVersion"`
	Status            SourceStatus      `json:"status"`
	Archived          bool              `json:"archived"`
	IdentityEvidence  Evidence          `json:"identityEvidence"`
	CompleteOffset    int64             `json:"completeOffset"`
	CompleteLines     int               `json:"completeLines"`
	DiagnosticSummary DiagnosticSummary `json:"diagnosticSummary"`
	Diagnostics       []Diagnostic      `json:"diagnostics,omitempty"`
	ErrorSummary      *string           `json:"error,omitempty"`
	LastIndexedAt     *time.Time        `json:"lastIndexedAt"`
}

type ThreadDetail struct {
	Thread
	Sources []ThreadSource `json:"sources"`
}

// ThreadCursor marks a page boundary: the updated_at of the last item served
// (nil in the NULL tail) plus its row ID. The list order is fixed
// updated-desc — the UI holds a full window in memory and sorts locally.
type ThreadCursor struct {
	UpdatedAt *time.Time
	ID        string
}

// ThreadIdentity is one thread's (provider, native thread id) key.
type ThreadIdentity struct {
	Provider       Provider
	NativeThreadID string
}

// MaxThreadIdentities bounds one identity-filtered thread read, well under
// SQLite's parameter limit.
const MaxThreadIdentities = 500

type ListThreadsParams struct {
	IncludeSubagents bool
	// IncludeGuardians is an explicit diagnostic-only catalog path. Guardians
	// never ride the ordinary include-subagents control.
	IncludeGuardians    bool
	IncludeProgrammatic bool
	Archived            ArchivedFilter
	ThreadID            string
	// Identities narrows the read to these threads, at most
	// MaxThreadIdentities per call. An internal seam, not an HTTP filter.
	Identities []ThreadIdentity
	// ParentNativeID narrows the read to workers belonging to one native
	// supervisor. It is an internal reconciliation seam, not an HTTP filter.
	ParentNativeID string
	// Inclusive time bounds on the created_at / updated_at columns; nil = open.
	// The UI's window fetch uses UpdatedFrom; the rest are kept symmetric.
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	UpdatedFrom *time.Time
	UpdatedTo   *time.Time
	Limit       int
	After       *ThreadCursor
}

// InteractionCounts tallies the non-subagent catalog by interaction class so the
// UI can report how many programmatic runs the default filter hides.
type InteractionCounts struct {
	Interactive  int `json:"interactive"`
	Programmatic int `json:"programmatic"`
	Unclassified int `json:"unclassified"`
}

type ThreadPage struct {
	Items          []Thread
	NextCursor     *ThreadCursor
	CatalogVersion uint64
}

type RootIndexStatus struct {
	Root            PersistedRoot
	State           IndexStateName
	SourceCount     int
	LastCompletedAt *time.Time
	Error           *string
}

type IndexStatus struct {
	State             IndexStateName
	CatalogVersion    uint64
	Roots             []RootIndexStatus
	InteractionCounts InteractionCounts
}
