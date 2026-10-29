package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/lifecycle"
	liveview "github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/wire"
)

// PublishProjectionRuntime publishes the current attachment runtime into the live projection.
func (sess *Session) PublishProjectionRuntime() {
	if sess.projection == nil {
		return
	}
	sess.stateOwner.stateMu.Lock()
	runtime := liveview.Runtime{
		SessionStatus:     sess.stateOwner.connectionStatusLocked(),
		ThreadStatus:      sess.stateOwner.threadStatusLocked(),
		TurnActive:        sess.stateOwner.workActiveLocked(),
		TurnID:            sess.stateOwner.turnID,
		TurnStartedAtMs:   sess.stateOwner.turnStartedAtMs,
		CompactingSinceMs: sess.stateOwner.compactingSinceMsLocked(),
		TokenUsage:        append(json.RawMessage(nil), sess.stateOwner.tokenUsage...),
		LastError:         sess.stateOwner.lastError,
		DeliveryExpiries:  sess.delivery.deliveryExpiries.count(sess.provider),
	}
	// The transcript's runtime upsert must not interpret hidden disconnected
	// work as a completed turn. Only provider terminal evidence completes it.
	if !runtime.TurnActive {
		runtime.TurnID, runtime.TurnStartedAtMs = "", 0
	}
	sess.stateOwner.stateMu.Unlock()
	sess.projection.updateRuntime(runtime)
}

func (sess *Session) applyLiveRuntimeWire(dir wire.Direction, line []byte, at time.Time) {
	if sess.projection == nil {
		return
	}
	if observe := lifecycleProviders[sess.provider].runtime; observe != nil {
		observe(sess, dir, line, at)
	}
}

func (sess *Session) setLiveTokenUsage(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	sess.stateOwner.stateMu.Lock()
	sess.stateOwner.tokenUsage = append(json.RawMessage(nil), raw...)
	sess.stateOwner.stateMu.Unlock()
	sess.projection.updateTokenUsage(raw)
}

func (sess *Session) setLiveTokenUsageForTurn(turnID string, raw json.RawMessage) {
	sess.setLiveTokenUsage(raw)
	usage := normalizeTokenUsage(raw)
	if usage == nil || turnID == "" {
		return
	}
	sess.stateOwner.stateMu.Lock()
	if sess.stateOwner.turnTokenUsage == nil {
		sess.stateOwner.turnTokenUsage = make(map[string]*interact.TokenUsage)
	}
	sess.stateOwner.turnTokenUsage[turnID] = usage
	if len(sess.stateOwner.turnTokenUsage) > liveProjectionStateTurnCap {
		for id := range sess.stateOwner.turnTokenUsage {
			if id != turnID {
				delete(sess.stateOwner.turnTokenUsage, id)
				break
			}
		}
	}
	sess.stateOwner.stateMu.Unlock()
}

func (sess *Session) completedTokenUsage(turnID string) *interact.TokenUsage {
	sess.stateOwner.stateMu.Lock()
	defer sess.stateOwner.stateMu.Unlock()
	return sess.stateOwner.turnTokenUsage[turnID]
}

func (sess *Session) setLiveError(message string) {
	if message == "" {
		return
	}
	sess.stateOwner.stateMu.Lock()
	sess.stateOwner.lastError = message
	sess.stateOwner.stateMu.Unlock()
	sess.projection.updateError(message)
}

// ReconcileDurableTurn settles the session's open turn from its saved status
// when the live terminal event was lost. It reads only while a turn is open.
func (sess *Session) ReconcileDurableTurn(ctx context.Context) {
	if sess == nil || sess.projection == nil || sess.stateOwner.savedTurn == nil {
		return
	}
	sess.stateOwner.stateMu.Lock()
	turnID := sess.stateOwner.turnID
	threadID := sess.stateOwner.threadID
	generation := sess.stateOwner.attachmentGeneration
	sessionTurnActive := sess.stateOwner.workActiveLocked()
	sess.stateOwner.stateMu.Unlock()
	if turnID == "" {
		turnID = sess.projection.currentTurnID()
	}
	if turnID == "" {
		return
	}
	liveTurn, wireEvidenceAtMs, version, retained := sess.projection.retainedTurnEvidence(turnID)
	if !retained {
		return
	}
	if !sessionTurnActive && liveTurn.Status != "in_progress" {
		return
	}
	exact, found := sess.stateOwner.savedTurn(ctx, turnID)
	if !liveview.DurableTerminalAuthoritative(exact, found, wireEvidenceAtMs) {
		return
	}
	status := exact.Turn.Status
	if !liveview.DurableTerminalStatus(status) {
		status = "completed"
	}

	// The guard, the turn end and the runtime clear share stateMu so a resume
	// handshake cannot observe a terminal projection and then resurrect that
	// exact turn. The saved end applies only to the attachment that read it,
	// and only if the turn did no live work while it was read.
	sess.stateOwner.stateMu.Lock()
	if sess.stateOwner.attachmentGeneration != generation || threadID != "" && sess.stateOwner.threadID != threadID ||
		!sess.projection.completeTurnUnlessWorked(turnID, version, status, exact.Turn.DurationMS, exact.Turn.CompletedAtMs) ||
		sess.stateOwner.workActiveLocked() && sess.stateOwner.turnID != turnID {
		sess.stateOwner.stateMu.Unlock()
		return
	}
	if threadID == "" {
		threadID = sess.stateOwner.threadID
	}
	sess.stateOwner.durablyTerminalTurnID = turnID
	sess.stateOwner.durablyTerminalTurnThreadID = threadID
	turnActiveChanged := sess.stateOwner.workActiveLocked()
	if turnActiveChanged {
		sess.stateOwner.transitionLifecycleLocked(lifecycle.EventFinished)
		sess.stateOwner.turnID = ""
		sess.stateOwner.turnStartedAtMs = 0
		if exact.Turn.CompletedAtMs > 0 {
			sess.stateOwner.lastTurnEndedAt = time.UnixMilli(exact.Turn.CompletedAtMs).UTC().Format(time.RFC3339Nano)
		}
	}
	sess.stateOwner.stateMu.Unlock()

	if !turnActiveChanged {
		return
	}
	sess.PublishProjectionRuntime()
	sess.stateOwner.NotifyRuntime()
	sess.delivery.kickQueuedTurnDrain()
}

// HandleConversation serves the default conversation view for this attachment.
func (sess *Session) HandleConversation(w http.ResponseWriter, r *http.Request) {
	sess.HandleConversationView(w, r, conversationViewFull)
}

// HandleConversationView serves a paginated conversation view with the supplied default mode.
func (sess *Session) HandleConversationView(w http.ResponseWriter, r *http.Request, defaultView liveview.View) {
	view, err := parseConversationView(r, defaultView)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	before, limit, err := parseConversationPage(r)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	snapshot, err := sess.fileConversationSnapshot(r.Context(), view, before, limit)
	if err != nil {
		sess.liveText.writeBackendError(w, err)
		return
	}
	httpapi.WriteJSON(w, snapshot)
}

// HandleConversationEvents streams changes to the default conversation view.
func (sess *Session) HandleConversationEvents(w http.ResponseWriter, r *http.Request) {
	sess.HandleConversationEventsView(w, r, conversationViewFull)
}

// HandleConversationEventsView streams changes to a conversation view with the supplied default mode.
func (sess *Session) HandleConversationEventsView(w http.ResponseWriter, r *http.Request, defaultView liveview.View) {
	view, err := parseConversationView(r, defaultView)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	sess.handleFileConversationEvents(w, r, view)
}

// ReconcileSavedTranscript confirms saved items and settles their waiting prompts.
func ReconcileSavedTranscript(ctx context.Context, sess *Session) {
	if sess == nil {
		return
	}
	sess.ReconcileDurableTurn(ctx)
	sess.delivery.SettleWaitingPrompts(ctx)
}
