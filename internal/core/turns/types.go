package turns

// Record is one adapter-reported provider turn. Its durable ID is a feed
// cursor; consumers keep their own delivery state.
type Record struct {
	ID, TargetHostedThreadID, Provider, TurnID, FinalMessageID, CompletedAt, Source, CreatedAt string
	ClientActionID, NativeThreadID, Outcome                                                    string
	UsageInputTokens, UsageCachedInputTokens, UsageOutputTokens                                *int64
}
