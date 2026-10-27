package app

// repairableLocked says why the repair cannot be reported now, or "" when the
// stopped attachment is unchanged, idle and ready. Callers hold stateMu.
func (sess *session) repairableLocked(threadID string, attachmentGeneration int64) string {
	if sess.threadID != threadID || sess.attachmentGeneration != attachmentGeneration {
		return transcriptRepairStale
	}
	if sess.closed || sess.connectionStatusLocked() != "ready" || sess.workActiveLocked() || sess.turnRestoreInProgress {
		return "attachment is not idle and ready"
	}
	if len(sess.interactions) != 0 {
		return "a provider interaction is still pending"
	}
	return ""
}
