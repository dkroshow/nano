package app

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/protocol"
)

func (sess *session) publishProjectionRuntime() {
	if sess.projection == nil {
		return
	}
	sess.stateMu.Lock()
	runtime := liveProjectionRuntime{
		SessionStatus:     sess.connectionStatusLocked(),
		ThreadStatus:      sess.threadStatusLocked(),
		TurnActive:        sess.workActiveLocked(),
		TurnID:            sess.turnID,
		TurnStartedAtMs:   sess.turnStartedAtMs,
		CompactingSinceMs: sess.compactingSinceMsLocked(),
		TokenUsage:        append(json.RawMessage(nil), sess.tokenUsage...),
		LastError:         sess.lastError,
		DeliveryExpiries:  sess.deliveryExpiries.count(sess.provider),
	}
	// The transcript's runtime upsert must not interpret hidden disconnected
	// work as a completed turn. Only provider terminal evidence completes it.
	if !runtime.TurnActive {
		runtime.TurnID, runtime.TurnStartedAtMs = "", 0
	}
	sess.stateMu.Unlock()
	sess.projection.UpdateRuntime(runtime)
}

func (sess *session) applyLiveRuntimeWire(dir protocol.Direction, line []byte, at time.Time) {
	if sess.projection == nil {
		return
	}
	if observe := lifecycleProviders[sess.provider].runtime; observe != nil {
		observe(sess, dir, line, at)
	}
}

func (sess *session) setLiveTokenUsage(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	sess.stateMu.Lock()
	sess.tokenUsage = append(json.RawMessage(nil), raw...)
	sess.stateMu.Unlock()
	sess.projection.UpdateTokenUsage(raw)
}

func (sess *session) setLiveTokenUsageForTurn(turnID string, raw json.RawMessage) {
	sess.setLiveTokenUsage(raw)
	usage := normalizeTokenUsage(raw)
	if usage == nil || turnID == "" {
		return
	}
	sess.stateMu.Lock()
	if sess.turnTokenUsage == nil {
		sess.turnTokenUsage = make(map[string]*tokenUsage)
	}
	sess.turnTokenUsage[turnID] = usage
	if len(sess.turnTokenUsage) > liveProjectionStateTurnCap {
		for id := range sess.turnTokenUsage {
			if id != turnID {
				delete(sess.turnTokenUsage, id)
				break
			}
		}
	}
	sess.stateMu.Unlock()
}

func (sess *session) completedTokenUsage(turnID string) *tokenUsage {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.turnTokenUsage[turnID]
}

func (sess *session) setLiveError(message string) {
	if message == "" {
		return
	}
	sess.stateMu.Lock()
	sess.lastError = message
	sess.stateMu.Unlock()
	sess.projection.UpdateError(message)
}

// reconcileDurableTurn settles the session's open turn from its saved status
// when the live terminal event was lost. It reads only while a turn is open.
func (sess *session) reconcileDurableTurn(ctx context.Context) {
	if sess == nil || sess.projection == nil || sess.savedTurn == nil {
		return
	}
	sess.stateMu.Lock()
	turnID := sess.turnID
	threadID := sess.threadID
	generation := sess.attachmentGeneration
	sessionTurnActive := sess.workActiveLocked()
	sess.stateMu.Unlock()
	if turnID == "" {
		turnID = sess.projection.CurrentTurnID()
	}
	if turnID == "" {
		return
	}
	liveTurn, wireEvidenceAtMs, version, retained := sess.projection.RetainedTurnEvidence(turnID)
	if !retained {
		return
	}
	if !sessionTurnActive && liveTurn.Status != "in_progress" {
		return
	}
	exact, found := sess.savedTurn(ctx, turnID)
	if !durableTerminalAuthoritative(exact, found, wireEvidenceAtMs) {
		return
	}
	status := exact.Turn.Status
	if !durableTerminalStatus(status) {
		status = "completed"
	}

	// The guard, the turn end and the runtime clear share stateMu so a resume
	// handshake cannot observe a terminal projection and then resurrect that
	// exact turn. The saved end applies only to the attachment that read it,
	// and only if the turn did no live work while it was read.
	sess.stateMu.Lock()
	if sess.attachmentGeneration != generation || threadID != "" && sess.threadID != threadID ||
		!sess.projection.CompleteTurnUnlessWorked(turnID, version, status, exact.Turn.DurationMS, exact.Turn.CompletedAtMs) ||
		sess.workActiveLocked() && sess.turnID != turnID {
		sess.stateMu.Unlock()
		return
	}
	if threadID == "" {
		threadID = sess.threadID
	}
	sess.durablyTerminalTurnID = turnID
	sess.durablyTerminalTurnThreadID = threadID
	turnActiveChanged := sess.workActiveLocked()
	if turnActiveChanged {
		sess.transitionLifecycleLocked(lifecycle.EventFinished)
		sess.turnID = ""
		sess.turnStartedAtMs = 0
		if exact.Turn.CompletedAtMs > 0 {
			sess.lastTurnEndedAt = time.UnixMilli(exact.Turn.CompletedAtMs).UTC().Format(time.RFC3339Nano)
		}
	}
	sess.stateMu.Unlock()

	if !turnActiveChanged {
		return
	}
	sess.publishProjectionRuntime()
	sess.notifyRuntime()
	sess.kickQueuedTurnDrain()
}

func (sess *session) handleConversation(w http.ResponseWriter, r *http.Request) {
	sess.handleConversationView(w, r, conversationViewFull)
}

func (sess *session) handleConversationView(w http.ResponseWriter, r *http.Request, defaultView conversationView) {
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
		sess.liveText.WriteBackendError(w, err)
		return
	}
	httpapi.WriteJSON(w, snapshot)
}

func (sess *session) handleConversationEvents(w http.ResponseWriter, r *http.Request) {
	sess.handleConversationEventsView(w, r, conversationViewFull)
}

func (sess *session) handleConversationEventsView(w http.ResponseWriter, r *http.Request, defaultView conversationView) {
	view, err := parseConversationView(r, defaultView)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	sess.handleFileConversationEvents(w, r, view)
}
