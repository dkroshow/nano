package app

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type conversationView = liveview.View

const (
	conversationViewFull     = liveview.Full
	conversationViewMessages = liveview.Messages
)

// liveProjectionItem is intentionally provider-neutral at the wire boundary.
// Item contains only the normalized fields the conversation renderer needs;
// provider protocol envelopes never cross this API.
type liveProjectionItem = liveview.Item

type liveProjectionTurn = liveview.Turn

type liveProjectionRuntime = liveview.Runtime

type liveProjectionSnapshot = liveview.Snapshot

type liveProjectionPage = liveview.Page

type liveProjectionUpdate = liveview.Update

// newSessionProjection applies the server's live-transcript settings to a
// session's projection.
func (s *server) newSessionProjection(provider, hostedThreadID string) *sessions.LiveProjection {
	return sessions.NewLiveProjection(provider, hostedThreadID, s.dropLiveTextDeltas)
}

// durableTurnEvidence is the saved status of one live turn, read by its exact
// native turn id.
type durableTurnEvidence = liveview.DurableTurnEvidence

// savedTurnEvidence reads the saved status of a Codex or Grok turn by exact
// native turn id. Claude saved turns carry no id the live turn shares, so a
// Claude turn settles only from live evidence.
func (s *server) savedTurnEvidence(ctx context.Context, sess *sessions.Session, turnID string) (durableTurnEvidence, bool) {
	if s.catalog == nil || sess == nil || (sess.Provider() != "codex" && sess.Provider() != "grok") ||
		turnID == "" || sess.ThreadID() == "" {
		return durableTurnEvidence{}, false
	}
	resolver, ok := s.catalog.(threadindex.ReferenceResolver)
	if !ok {
		return durableTurnEvidence{}, false
	}
	catalog, ok := s.catalog.(threadindex.SavedTurnStatusCatalog)
	if !ok {
		return durableTurnEvidence{}, false
	}
	nanoID, err := resolver.ResolveThreadReference(ctx, sess.ThreadID())
	if err != nil || nanoID == "" {
		return durableTurnEvidence{}, false
	}
	thread, err := s.catalog.LoadThread(ctx, nanoID)
	if err != nil || thread.Source.Conflict {
		return durableTurnEvidence{}, false
	}
	saved, found, err := catalog.ReadSavedTurnStatus(ctx, nanoID, turnID)
	if err != nil || !found {
		return durableTurnEvidence{}, false
	}
	turn := liveProjectionTurn{ID: turnID, Status: string(saved.Turn.Status), DurationMS: saved.Turn.DurationMS}
	if saved.Turn.StartedAt != nil {
		turn.StartedAtMs = saved.Turn.StartedAt.UnixMilli()
	}
	if saved.Turn.CompletedAt != nil {
		turn.CompletedAtMs = saved.Turn.CompletedAt.UnixMilli()
	}
	return durableTurnEvidence{Turn: turn, Newer: saved.Newer}, true
}

// confirmDurableThread joins one changed durable transcript to its attached
// live projection. The scanner carries the provider/native identity it already
// resolved; the fallback loads only this catalog thread. It never resolves
// every attached session through SQLite. Exact native item or call ids are
// required; transcript text is not a join key.
func (s *server) confirmDurableThread(ctx context.Context, change threadindex.TranscriptChange) {
	if change.ThreadID == "" {
		return
	}
	provider, nativeThreadID := change.Provider, change.NativeThreadID
	if provider == "" || nativeThreadID == "" {
		thread, err := s.catalog.LoadThread(ctx, change.ThreadID)
		if err != nil {
			return
		}
		provider, nativeThreadID = thread.Provider, thread.NativeID
	}
	// Attached or not, the commit may hold an answer indexed after its
	// operation's wait.
	s.agentExecutions.SavedTranscriptApplied(provider, nativeThreadID)
	if sess := s.SessionForThread(provider, nativeThreadID); sess != nil {
		sess.NotifySavedTranscript()
		s.confirmDurableItems(ctx, sess)
	}
	if provider == string(model.ProviderClaude) {
		s.finishDetachedClaudeOwed(nativeThreadID)
		// The commit can decide an answer a subscriber's feed waits on, and the
		// owed pass finishes turns with no notice of their own: the owners'
		// subscriptions read again (the dispatcher coalesces repeats).
		owners, err := s.controlMetadata.NativeOwners(provider, nativeThreadID)
		if err != nil {
			log.Printf("owners of %s unreadable: %v", nativeThreadID, err)
		}
		for _, owner := range owners {
			s.sweepSubscriptions(owner)
		}
	}
}

func (s *server) transcriptRepairPriority(candidate history.TranscriptParserBackfillCandidate) int {
	if s.transcriptValidations != nil && s.transcriptValidations.freshVisibleThread(candidate.NativeThreadID, time.Now().UTC()) {
		return 0
	}
	if s.SessionForThread(string(candidate.Provider), candidate.NativeThreadID) != nil {
		return 1
	}
	return 2
}

func (s *server) confirmDurableItems(ctx context.Context, sess *sessions.Session) {
	if sess == nil {
		return
	}
	sess.ReconcileDurableTurn(ctx)
	sess.SettleWaitingPrompts(ctx)
}

func (s *server) handleConversationHistory(w http.ResponseWriter, r *http.Request, sess *sessions.Session) {
	catalog, ok := s.catalog.(threadindex.TranscriptCatalog)
	resolver, resolves := s.catalog.(threadindex.ReferenceResolver)
	if !ok || !resolves || sess == nil || sess.ThreadID() == "" {
		httpapi.WriteHTTPError(w, "conversation history unavailable", http.StatusServiceUnavailable)
		return
	}
	nanoID, err := resolver.ResolveThreadReference(r.Context(), sess.ThreadID())
	if err != nil {
		writeTranscriptBackendError(w, err)
		return
	}
	query, err := threadindex.ParseTranscriptListQuery(r)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	response, err := catalog.LoadTranscript(r.Context(), nanoID, query)
	if err != nil {
		writeTranscriptBackendError(w, err)
		return
	}
	httpapi.WriteJSON(w, response)
}
