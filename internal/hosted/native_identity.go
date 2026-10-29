package hosted

// hostedThreadNativeID returns the current native thread id for a hosted id,
// preferring the live session, then the durable execution record. The second
// return is false when no hosted record exists at all.
// NativeID returns a hosted thread's current live or durable native identity.
func (s *IdentityService) NativeID(hostedID string) (string, bool) {
	if live, ok := s.Sessions.LiveHostedSummary(hostedID, s.Executions.SessionNeedsAttention); ok {
		return live.ThreadID, true
	}
	if s.Executions == nil {
		return "", false
	}
	record, ok := s.Executions.Session(hostedID)
	if !ok {
		return "", false
	}
	return record.NativeThreadID, true
}
