package prompts

// ReasonInterruptedBeforeTranscript records delivery interrupted before a saved transcript exists.
const ReasonInterruptedBeforeTranscript = "interrupted_before_transcript"

// LegacyQueuedTurn projects a transactional record into the schema-20 read shape.
func LegacyQueuedTurn(record Record) QueuedTurn {
	providerTurnID := record.ProviderTurnID
	if providerTurnID == "" && record.Outbox.Binding != nil && record.Outbox.Binding.Scope.NativeTurnID != nil {
		providerTurnID = *record.Outbox.Binding.Scope.NativeTurnID
	}
	return QueuedTurn{
		ClientActionID: record.Outbox.ActionID, Provider: string(record.Provider), HostedThreadID: record.Outbox.HostedThreadID,
		NativeThreadID: record.NativeThreadID, Text: record.ProviderText, PresentationText: record.Outbox.PresentationText,
		TextHash: record.TextHash, RequestHash: record.RequestHash, State: string(record.Outbox.State), Origin: string(record.Origin),
		CreatedAt: record.CreatedAt, ProviderTurnID: providerTurnID,
		ProviderTerminalStatus: record.ProviderTerminal, ProviderTerminalAt: record.ProviderTerminalAt,
	}
}

// LegacySendAttempt projects the journal into the schema-20 read shape.
func LegacySendAttempt(record Record, attempt Attempt) SendAttempt {
	status := string(attempt.State)
	switch attempt.State {
	case JournalUnknown:
		status = "unknown"
	case JournalConfirmed:
		status = string(JournalAccepted)
	}
	if attempt.State == JournalRefused && attempt.Reason == ReasonInterruptedBeforeTranscript {
		status = ReasonInterruptedBeforeTranscript
	}
	if record.Outbox.State == Parked {
		status = "parked"
	}
	if record.Outbox.State == Uncertain && attempt.Reason == ReasonStopped {
		status = "unknown" // held by a Stop, for the person to resend or discard
	}
	return SendAttempt{
		ClientActionID: attempt.ActionID, Provider: string(attempt.Provider), NativeThreadID: attempt.NativeThreadID,
		TextHash: attempt.TextHash, Status: status, RunID: attempt.RunID, Reason: attempt.Reason, CreatedAt: attempt.CreatedAt, UpdatedAt: attempt.UpdatedAt,
		Confirmed: attempt.Confirmed, ConfirmedAt: attempt.ConfirmedAt, ProviderTurnID: attempt.ProviderTurnID,
		ProviderTerminalStatus: attempt.ProviderTerminal, ProviderTerminalAt: attempt.ProviderTerminalAt,
		CanonicalMessageID: attempt.CanonicalItemID, IdentityConflict: attempt.IdentityConflict,
	}
}
