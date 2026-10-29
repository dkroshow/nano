package config

// SessionDefaults are prepared launch values used for a new attachment.
type SessionDefaults struct {
	CWD              string
	Model            string
	Sandbox          string
	Approval         string
	ReasoningSummary string
	AllowedTools     string
	ClaudeBin        string
	ClaudeShimWS     string
	// ClaudeShimSpools is the shim daemon's spool directory, read-only here.
	ClaudeShimSpools string
	GrokBin          string
	GrokLeaderSocket string
	// APIOrigin is the address hosted agents are told to reach this server on;
	// it rides the injected identity block (agent_identity.go).
	APIOrigin string
}
