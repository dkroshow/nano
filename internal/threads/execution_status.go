package threads

const (
	// PresentationShared shows a hosted thread in the shared interface.
	PresentationShared = "shared"
	// PresentationBackground keeps a hosted thread in the background.
	PresentationBackground = "background"
	// SidebarPlacementInvoker places a thread under its invoker.
	SidebarPlacementInvoker = "invoker"
	// SidebarPlacementStandalone places a thread independently.
	SidebarPlacementStandalone = "standalone"
	// AgentSessionCreating marks a provider session being created.
	AgentSessionCreating = "creating"
	// AgentSessionCreationUnknown marks creation interrupted before confirmation.
	AgentSessionCreationUnknown = "creation_unknown"
	// AgentSessionDetached marks a hosted thread without an attached provider session.
	AgentSessionDetached = "detached"
	// AgentTurnQueued marks an operation waiting for delivery.
	AgentTurnQueued = "queued"
	// AgentTurnDeliveryPending marks an operation being delivered.
	AgentTurnDeliveryPending = "delivery_pending"
	// AgentTurnDeliveryUnknown marks delivery interrupted before confirmation.
	AgentTurnDeliveryUnknown = "delivery_unknown"
	// AgentTurnRunning marks an operation accepted by the provider.
	AgentTurnRunning = "running"
	// AgentTurnWaiting marks an operation awaiting input.
	AgentTurnWaiting = "waiting_for_input"
	// AgentTurnAwaitingSaved marks an operation awaiting its saved result.
	AgentTurnAwaitingSaved = "awaiting_saved"
	// AgentTurnCompleted marks a successful terminal operation.
	AgentTurnCompleted = "completed"
	// AgentTurnFailed marks a failed terminal operation.
	AgentTurnFailed = "failed"
	// AgentTurnInterrupted marks an interrupted terminal operation.
	AgentTurnInterrupted = "interrupted"
)

// NormalizeTerminalStatus normalizes a provider terminal outcome to the stored operation status.
func NormalizeTerminalStatus(status string) string {
	switch status {
	case AgentTurnCompleted:
		return AgentTurnCompleted
	case AgentTurnInterrupted, "aborted", "cancelled", "canceled":
		return AgentTurnInterrupted
	default:
		return AgentTurnFailed
	}
}

// TurnTerminal reports a completed, failed or interrupted operation.
func TurnTerminal(status string) bool {
	return status == AgentTurnCompleted || status == AgentTurnFailed || status == AgentTurnInterrupted
}

// AnswerNotSaved reports an operation whose saved answer was not found within
// its wait: the one terminal outcome a later saved answer may still replace.
func AnswerNotSaved(rec TurnRecord) bool {
	return rec.Status == AgentTurnFailed && rec.Error != nil && rec.Error.Source == "saved" && rec.Error.Code == "result_not_saved"
}
