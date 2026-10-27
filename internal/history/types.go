// Package history discovers and projects metadata from native coding-agent
// history files. It is deliberately read-only toward provider-owned files.
package history

import (
	"time"
)

// Provider identifies the native history producer.
type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
	ProviderGrok   Provider = "grok"
)

// RootKind identifies a standard provider history root.
type RootKind string

const (
	RootCodexSessions  RootKind = "codex_sessions"
	RootCodexArchived  RootKind = "codex_archived"
	RootClaudeProjects RootKind = "claude_projects"
	RootGrokSessions   RootKind = "grok_sessions"
)

// ThreadKind is the provider-native thread classification known in Slice 1.
type ThreadKind string

const (
	ThreadMain     ThreadKind = "main"
	ThreadSubagent ThreadKind = "subagent"
	ThreadFork     ThreadKind = "fork"
	// ThreadGuardian is Codex's internal permission-review session. It is
	// distinct from a delegated worker even though its native source is nested
	// under the subagent shape.
	ThreadGuardian ThreadKind = "guardian"
)

// SourceRoot is one configured discovery boundary. Path is absolute and
// canonical when the directory exists.
type SourceRoot struct {
	Provider Provider `json:"provider"`
	Kind     RootKind `json:"kind"`
	Path     string   `json:"path"`
	Archived bool     `json:"archived"`
}

// SourceCandidate is a provider JSONL found under a SourceRoot. Device and
// Inode are zero when the platform does not expose them.
type SourceCandidate struct {
	Root         SourceRoot `json:"root"`
	Path         string     `json:"path"`
	RelativePath string     `json:"relativePath"`
	Kind         ThreadKind `json:"kind"`
	// ParentNativeID is provider-owned relationship evidence assembled during
	// discovery. It is not Nano hosted invocation lineage.
	ParentNativeID string    `json:"parentNativeId,omitempty"`
	Archived       bool      `json:"archived"`
	Size           int64     `json:"size"`
	ModTime        time.Time `json:"modTime"`
	Device         uint64    `json:"device,omitempty"`
	Inode          uint64    `json:"inode,omitempty"`
}

// MetadataState distinguishes absent evidence from an unsupported provider
// concept. Unknown and unsupported fields never carry a value.
type MetadataState string

const (
	MetadataKnown       MetadataState = "known"
	MetadataUnknown     MetadataState = "unknown"
	MetadataUnsupported MetadataState = "unsupported"
)

// Evidence labels the authority used for a canonical field.
type Evidence string

const (
	EvidenceNativeDurable    Evidence = "native_durable"
	EvidenceProviderAPI      Evidence = "provider_api"
	EvidenceDerived          Evidence = "derived"
	EvidenceFilenameFallback Evidence = "filename_fallback"
)

// MetadataField is the canonical field-level value/provenance shape shared
// with the Slice 1 HTTP contract.
type MetadataField struct {
	State      MetadataState `json:"state"`
	Value      *string       `json:"value"`
	Evidence   *Evidence     `json:"evidence"`
	ObservedAt *time.Time    `json:"observedAt,omitempty"`
}

// Metadata contains only fields needed by the read-only catalog slice.
type Metadata struct {
	CWD              MetadataField `json:"cwd"`
	Model            MetadataField `json:"model"`
	Effort           MetadataField `json:"effort"`
	Priority         MetadataField `json:"priority"`
	Permission       MetadataField `json:"permission"`
	HistoricalBranch MetadataField `json:"historicalBranch"`
	// Origin is the provider-native session-origin string verbatim (Claude
	// `entrypoint`, Codex session_meta `originator`). The derived interaction
	// class is computed from it, never the reverse.
	Origin MetadataField `json:"origin"`
}

// DiagnosticKind classifies bounded, non-fatal parser and discovery findings.
type DiagnosticKind string

const (
	DiagnosticMalformed        DiagnosticKind = "malformed_line"
	DiagnosticUnknownRow       DiagnosticKind = "unknown_row"
	DiagnosticMissingIdentity  DiagnosticKind = "missing_identity"
	DiagnosticIdentityConflict DiagnosticKind = "identity_conflict"
	DiagnosticOversizedLine    DiagnosticKind = "oversized_line"
	DiagnosticFilesystem       DiagnosticKind = "filesystem"
)

// Diagnostic is safe to persist and expose. It never includes source-line
// content, which may contain prompts or tool output.
type Diagnostic struct {
	Kind    DiagnosticKind `json:"kind"`
	Line    int            `json:"line,omitempty"`
	Message string         `json:"message"`
}

// DiagnosticSummary counts all findings even when the detail list is bounded.
type DiagnosticSummary struct {
	Total             int `json:"total"`
	Malformed         int `json:"malformed"`
	UnknownRows       int `json:"unknownRows"`
	MissingIdentity   int `json:"missingIdentity"`
	IdentityConflicts int `json:"identityConflicts"`
	OversizedLines    int `json:"oversizedLines"`
	Filesystem        int `json:"filesystem"`
	Dropped           int `json:"dropped"`
}

// DiscoveryResult is one complete root walk. A missing optional root is a
// successful result with Present false and no candidates.
type DiscoveryResult struct {
	Root        SourceRoot        `json:"root"`
	Present     bool              `json:"present"`
	Candidates  []SourceCandidate `json:"candidates"`
	Diagnostics []Diagnostic      `json:"diagnostics,omitempty"`
	Summary     DiagnosticSummary `json:"diagnosticSummary"`
}

// ThreadProjection is the metadata-only provider projection attached to one
// source file. The store, not the adapter, mints Nano thread IDs.
type ThreadProjection struct {
	Provider         Provider   `json:"provider"`
	NativeThreadID   string     `json:"nativeThreadId,omitempty"`
	IdentityEvidence Evidence   `json:"identityEvidence,omitempty"`
	Kind             ThreadKind `json:"kind"`
	ParentNativeID   string     `json:"parentNativeId,omitempty"`
	Title            *string    `json:"title"`
	Preview          *string    `json:"preview"`
	CreatedAt        *time.Time `json:"createdAt"`
	UpdatedAt        *time.Time `json:"updatedAt"`
	Metadata         Metadata   `json:"metadata"`
	ProviderVersion  *string    `json:"providerVersion,omitempty"`
}

// ProjectionResult checkpoints only bytes ending in a newline. An incomplete
// trailing JSON value is deliberately excluded from CompleteOffset.
type ProjectionResult struct {
	Projection     ThreadProjection  `json:"projection"`
	CompleteOffset int64             `json:"completeOffset"`
	CompleteLines  int               `json:"completeLines"`
	BytesRead      int64             `json:"bytesRead"`
	Diagnostics    []Diagnostic      `json:"diagnostics,omitempty"`
	Summary        DiagnosticSummary `json:"diagnosticSummary"`
}

// UnknownField returns the canonical absence representation.
func UnknownField() MetadataField {
	return MetadataField{State: MetadataUnknown}
}

// UnsupportedField returns the canonical provider-inapplicable representation.
func UnsupportedField() MetadataField {
	return MetadataField{State: MetadataUnsupported}
}
