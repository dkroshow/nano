package observability

// ActiveTurn is the monitor view of one session turn in progress.
type ActiveTurn struct {
	Provider        string `json:"provider"`
	ThreadID        string `json:"threadId"`
	HostedThreadID  string `json:"hostedThreadId,omitempty"`
	Name            string `json:"name,omitempty"`
	TurnStartedAtMs int64  `json:"turnStartedAtMs,omitempty"`
}
