package model

// MetadataField is limited to fields already projected by the history catalog.
// Provider readers interpret native envelopes; consumers apply these values
// without decoding provider detail or deriving identities from filenames.
type MetadataField string

const (
	MetadataNativeThreadID   MetadataField = "nativeThreadId"
	MetadataParentNativeID   MetadataField = "parentNativeId"
	MetadataSourceKind       MetadataField = "sourceKind"
	MetadataTitle            MetadataField = "title"
	MetadataPreview          MetadataField = "preview"
	MetadataProviderVersion  MetadataField = "providerVersion"
	MetadataCWD              MetadataField = "cwd"
	MetadataModel            MetadataField = "model"
	MetadataEffort           MetadataField = "effort"
	MetadataPriority         MetadataField = "priority"
	MetadataPermission       MetadataField = "permission"
	MetadataHistoricalBranch MetadataField = "historicalBranch"
	MetadataOrigin           MetadataField = "origin"
)

type MetadataState string

const (
	MetadataKnown       MetadataState = "known"
	MetadataUnknown     MetadataState = "unknown"
	MetadataUnsupported MetadataState = "unsupported"
)

// MetadataValue inherits its observation's scope, source and native clocks.
// Value is meaningful only in the known state. Absent fields make no claim;
// unknown and unsupported are explicit facts, not empty known strings.
type MetadataValue struct {
	State MetadataState `json:"state"`
	Value string        `json:"value,omitempty"`
}

// Metadata contains changed catalog facts, not a latest thread snapshot.
// It adds no transcript unit: Item and Turn remain the assembled units.
type Metadata map[MetadataField]MetadataValue
