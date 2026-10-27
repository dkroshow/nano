package prompts

import "github.com/dkroshow/nano/internal/transcriptparse/model"

type Origin string

const (
	HumanOrigin  Origin = "human"
	AgentOrigin  Origin = "agent"
	SystemOrigin Origin = "system"
)

type JournalState string

const (
	JournalQueued    JournalState = "queued"
	JournalSending   JournalState = "sending"
	JournalAccepted  JournalState = "accepted"
	JournalUnknown   JournalState = "unknown"
	JournalFailed    JournalState = "failed"
	JournalRefused   JournalState = "refused"
	JournalConfirmed JournalState = "confirmed"
)

type Submission struct {
	HostedThreadID   string
	Provider         model.Provider
	NativeThreadID   string
	ActionID         string
	ProviderText     string
	PresentationText string
	TextHash         string
	RequestHash      string
	Origin           Origin
	// NextTurn keeps an ordinary prompt out of a running turn: it waits for
	// that turn to end, as system-origin prompts always do.
	NextTurn  bool
	CreatedAt string
}

type Record struct {
	Outbox             OutboxEntry
	Provider           model.Provider
	NativeThreadID     string
	ProviderText       string
	TextHash           string
	RequestHash        string
	Origin             Origin
	CreatedAt          string
	Revision           uint64
	DeliveryRank       int
	ClaimToken         string
	ClaimEpoch         uint64
	ProviderTurnID     string
	ProviderTerminal   string
	ProviderTerminalAt string
}

type ClaimRequest struct {
	HostedThreadID  string
	Token           string
	AttachmentEpoch uint64
	Ready           bool
	// IntoRunningTurn claims only a prompt that may join the running turn:
	// not system-origin and not held for the next turn.
	IntoRunningTurn bool
	At              string
}

// Requeue returns a claimed prompt to the queue, for the next turn, after the
// provider definitely refused it because its target turn had already ended.
type Requeue struct {
	ActionID        string
	ClaimToken      string
	AttachmentEpoch uint64
	At              string
}

type Acceptance struct {
	ActionID        string
	ClaimToken      string
	AttachmentEpoch uint64
	Binding         Binding
	At              string
}

type Uncertainty struct {
	ActionID        string
	ClaimToken      string
	AttachmentEpoch uint64
	Reason          string
	At              string
}

type Refusal struct {
	ActionID        string
	ClaimToken      string
	AttachmentEpoch uint64
	State           DeliveryState
	Reason          string
	At              string
}

type Attempt struct {
	HostedThreadID     string
	ActionID           string
	Provider           model.Provider
	NativeThreadID     string
	TextHash           string
	State              JournalState
	Reason             string
	RunID              string
	CreatedAt          string
	UpdatedAt          string
	Revision           uint64
	Binding            *Binding
	ClaimToken         string
	Confirmed          bool
	ConfirmedAt        string
	ProviderTurnID     string
	ProviderTerminal   string
	ProviderTerminalAt string
	CanonicalItemID    string
	IdentityConflict   string
}

// AttemptStatus is the delivery journal row plus the optional outbox state
// needed by compatibility readers. Legacy attempts may have no outbox row.
type AttemptStatus struct {
	Attempt     Attempt
	OutboxState DeliveryState
}

type Restored struct {
	Outbox      []OutboxEntry
	Attempts    []Attempt
	Diagnostics []model.Diagnostic
}

// BootPark identifies one incomplete outbox row that startup made
// non-deliverable. Missing names structural fields only; it never contains
// prompt content.
type BootPark struct {
	ActionID       string
	Provider       model.Provider
	HostedThreadID string
	NativeThreadID string
	Missing        []string
}

// AdoptionReport describes the one-time handoff of paired schema-20 rows to
// the prompt writer. Unpaired or contradictory rows remain untouched
// and visible to maintenance as legacy evidence.
type AdoptionReport struct {
	Adopted     int
	Uncertain   int
	Unmanaged   int
	Diagnostics []model.Diagnostic
}
