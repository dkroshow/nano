package liveview

// DurableTerminalStatus reports a conclusive saved terminal state.
func DurableTerminalStatus(status string) bool {
	switch status {
	case "completed", "interrupted", "truncated":
		return true
	default:
		return false
	}
}

// DurableTerminalAuthoritative is deliberately small. A later catalog turn is
// conclusive. Otherwise the catalog may settle the exact turn only when the
// current projection carries no later wire evidence; catalog completion is
// provisional while live traffic still advances that same turn.
func DurableTerminalAuthoritative(exact DurableTurnEvidence, found bool, wireEvidenceAtMs int64) bool {
	if !found {
		return false
	}
	if exact.Newer {
		return true
	}
	return DurableTerminalStatus(exact.Turn.Status) && exact.Turn.CompletedAtMs > 0 &&
		wireEvidenceAtMs <= exact.Turn.CompletedAtMs
}
