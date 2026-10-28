package hosted

const (
	// PermissionEventRequested records a request before a decision.
	PermissionEventRequested = "requested"
	// PermissionEventResolved records the decision applied to a request.
	PermissionEventResolved = "resolved"
)

// PermissionEvent is one durable provider permission request or resolution.
type PermissionEvent struct {
	ID                   int64  `json:"id"`
	HostedThreadID       string `json:"hostedThreadId"`
	Provider             string `json:"provider"`
	NativeThreadID       string `json:"nativeThreadId,omitempty"`
	NanoSessionID        string `json:"nanoSessionId"`
	AttachmentGeneration int64  `json:"attachmentGeneration"`
	ProviderRequestID    string `json:"providerRequestId"`
	TurnID               string `json:"turnId,omitempty"`
	ItemID               string `json:"itemId,omitempty"`
	Method               string `json:"method"`
	InteractionKind      string `json:"interactionKind"`
	ToolName             string `json:"toolName,omitempty"`
	EventKind            string `json:"eventKind"`
	PermissionMode       string `json:"permissionMode,omitempty"`
	Decision             string `json:"decision,omitempty"`
	DecisionSource       string `json:"decisionSource,omitempty"`
	Outcome              string `json:"outcome,omitempty"`
	RequestSummary       string `json:"requestSummary"`
	RequestSHA256        string `json:"requestSha256"`
	ObservedAt           string `json:"observedAt"`
	SourceEventKey       string `json:"-"`
}

// PermissionEventPage is a cursor-bounded page of permission evidence.
type PermissionEventPage struct {
	Events     []PermissionEvent `json:"events"`
	NextCursor int64             `json:"nextCursor,omitempty"`
}
