package hosted

import (
	"context"
	"log"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// ConfirmDurableThread joins one changed durable transcript to its attached
// live projection. The scanner carries the provider/native identity it already
// resolved; the fallback loads only this catalog thread. It never resolves
// every attached session through SQLite. Exact native item or call ids are
// required; transcript text is not a join key.
func (s *AttachmentCoordinator) ConfirmDurableThread(ctx context.Context, change threadindex.TranscriptChange) {
	if change.ThreadID == "" {
		return
	}
	provider, nativeThreadID := change.Provider, change.NativeThreadID
	if provider == "" || nativeThreadID == "" {
		thread, err := s.Catalog.LoadThread(ctx, change.ThreadID)
		if err != nil {
			return
		}
		provider, nativeThreadID = thread.Provider, thread.NativeID
	}
	// Attached or not, the commit may hold an answer indexed after its
	// operation's wait.
	s.Executions.SavedTranscriptApplied(provider, nativeThreadID)
	if sess := s.Registry.SessionForThread(provider, nativeThreadID); sess != nil {
		sess.View().NotifySavedTranscript()
		sessions.ReconcileSavedTranscript(ctx, sess.State())
	}
	if provider == string(model.ProviderClaude) {
		s.finishDetachedClaudeOwed(nativeThreadID)
		// The commit can decide an answer a subscriber's feed waits on, and the
		// owed pass finishes turns with no notice of their own: the owners'
		// subscriptions read again (the dispatcher coalesces repeats).
		owners, err := s.Metadata.NativeOwners(provider, nativeThreadID)
		if err != nil {
			log.Printf("owners of %s unreadable: %v", nativeThreadID, err)
		}
		for _, owner := range owners {
			s.SweepSubscriptions(owner)
		}
	}
}

// TranscriptRepairPriority favors visible and attached transcripts during backfill.
func (s *AttachmentCoordinator) TranscriptRepairPriority(candidate history.TranscriptParserBackfillCandidate) int {
	if s.Validations != nil && s.Validations.FreshVisibleThread(candidate.NativeThreadID, time.Now().UTC()) {
		return 0
	}
	if s.Registry.SessionForThread(string(candidate.Provider), candidate.NativeThreadID) != nil {
		return 1
	}
	return 2
}
