package model

import "time"

// Input contains caller-owned bytes and source context, never a path to open.
// Source.Label and Incarnation identify a capture. Scope contains only context
// the caller actually knows. File readers assign byte/line positions; wire
// readers assign frame positions. CaptureAt and ObservedAt are optional clocks.
type Input struct {
	// Format is caller-selected framing: empty/jsonl for records, json for a
	// whole document. Unsupported provider/format pairs return a diagnostic.
	Format      string
	Bytes       []byte
	Source      SourceRef
	Scope       Scope
	MaxRowBytes int
	Closed      bool
	CaptureAt   *time.Time
	ObservedAt  *time.Time
}

// Versions separate independent reasons a consumer must rebuild its projection.
type Versions struct {
	Schema     string `json:"schema"`
	Reader     string `json:"reader"`
	Assembly   string `json:"assembly"`
	Taxonomy   string `json:"taxonomy"`
	Usage      string `json:"usage"`
	Checkpoint int    `json:"checkpoint"`
}

// Change has exactly one non-nil field. Upserts retain the item's first source
// position; relations do not move their endpoints or imply completion.
type Change struct {
	Item     *Item     `json:"item,omitempty"`
	Turn     *Turn     `json:"turn,omitempty"`
	Relation *Relation `json:"relation,omitempty"`
}

type Disposition string

const (
	RecordSemantic   Disposition = "semantic"
	RecordNonContent Disposition = "non_content"
	RecordUnknown    Disposition = "unknown"
	RecordMalformed  Disposition = "malformed"
)

type Record struct {
	Source      SourceRef    `json:"source"`
	Disposition Disposition  `json:"disposition"`
	Capture     CaptureState `json:"capture"`
}

const RepairHintCodexCompletedUserTwin = "codex_completed_user_serialization_twin"

// Batch is an ordered set of changes and source observations from one call.
// Non-content observations need no empty item or turn. Diagnostics contain no
// raw private record; Records account for every consumed source record.
type Batch struct {
	Versions     Versions      `json:"versions"`
	RepairHints  []string      `json:"repairHints,omitempty"`
	Changes      []Change      `json:"changes,omitempty"`
	Observations []Observation `json:"observations,omitempty"`
	Diagnostics  []Diagnostic  `json:"diagnostics,omitempty"`
	Records      []Record      `json:"records,omitempty"`
}
