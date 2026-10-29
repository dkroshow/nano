package sessions

import (
	"github.com/dkroshow/nano/internal/inspection"

	"github.com/dkroshow/nano/internal/setup"

	"context"
	"encoding/json"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/lifecycle"
	liveview "github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/wire"
)

// PublishProjectionRuntime publishes the current attachment runtime into the live projection.
func (sess *State) PublishProjectionRuntime() {
	if sess.view.projection == nil {
		return
	}
	sess.stateMu.Lock()
	runtime := liveview.Runtime{
		SessionStatus:     sess.connectionStatusLocked(),
		ThreadStatus:      sess.threadStatusLocked(),
		TurnActive:        sess.workActiveLocked(),
		TurnID:            sess.turnID,
		TurnStartedAtMs:   sess.turnStartedAtMs,
		CompactingSinceMs: sess.compactingSinceMsLocked(),
		TokenUsage:        append(json.RawMessage(nil), sess.tokenUsage...),
		LastError:         sess.lastError,
		DeliveryExpiries:  sess.delivery.deliveryExpiries.count(sess.identity.provider),
	}
	// The transcript's runtime upsert must not interpret hidden disconnected
	// work as a completed turn. Only provider terminal evidence completes it.
	if !runtime.TurnActive {
		runtime.TurnID, runtime.TurnStartedAtMs = "", 0
	}
	sess.stateMu.Unlock()
	sess.view.projection.updateRuntime(runtime)
}

func (sess *View) applyLiveRuntimeWire(dir wire.Direction, line []byte, at time.Time) {
	if sess.projection == nil {
		return
	}
	if observe := lifecycleProviders[sess.identity.provider].runtime; observe != nil {
		observe(sess.stateOwner, dir, line, at)
	}
}

func (sess *State) setLiveTokenUsage(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	sess.stateMu.Lock()
	sess.tokenUsage = append(json.RawMessage(nil), raw...)
	sess.stateMu.Unlock()
	sess.view.projection.updateTokenUsage(raw)
}

func (sess *State) setLiveTokenUsageForTurn(turnID string, raw json.RawMessage) {
	sess.setLiveTokenUsage(raw)
	usage := normalizeTokenUsage(raw)
	if usage == nil || turnID == "" {
		return
	}
	sess.stateMu.Lock()
	if sess.turnTokenUsage == nil {
		sess.turnTokenUsage = make(map[string]*interact.TokenUsage)
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

func (sess *State) completedTokenUsage(turnID string) *interact.TokenUsage {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.turnTokenUsage[turnID]
}

func (sess *State) setLiveError(message string) {
	if message == "" {
		return
	}
	sess.stateMu.Lock()
	sess.lastError = message
	sess.stateMu.Unlock()
	sess.view.projection.updateError(message)
}

// ReconcileDurableTurn settles the session's open turn from its saved status
// when the live terminal event was lost. It reads only while a turn is open.
func (sess *State) ReconcileDurableTurn(ctx context.Context) {
	if sess == nil || sess.view.projection == nil || sess.savedTurn == nil {
		return
	}
	sess.stateMu.Lock()
	turnID := sess.turnID
	threadID := sess.threadID
	generation := sess.attachmentGeneration
	sessionTurnActive := sess.workActiveLocked()
	sess.stateMu.Unlock()
	if turnID == "" {
		turnID = sess.view.projection.currentTurnID()
	}
	if turnID == "" {
		return
	}
	liveTurn, wireEvidenceAtMs, version, retained := sess.view.projection.retainedTurnEvidence(turnID)
	if !retained {
		return
	}
	if !sessionTurnActive && liveTurn.Status != "in_progress" {
		return
	}
	exact, found := sess.savedTurn(ctx, turnID)
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
	sess.stateMu.Lock()
	if sess.attachmentGeneration != generation || threadID != "" && sess.threadID != threadID ||
		!sess.view.projection.completeTurnUnlessWorked(turnID, version, status, exact.Turn.DurationMS, exact.Turn.CompletedAtMs) ||
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
	sess.PublishProjectionRuntime()
	sess.NotifyRuntime()
	sess.delivery.kickQueuedTurnDrain()
}

// HandleConversation serves the default conversation view for this attachment.
func (sess *View) HandleConversation(w http.ResponseWriter, r *http.Request) {
	sess.HandleConversationView(w, r, conversationViewFull)
}

// HandleConversationView serves a paginated conversation view with the supplied default mode.
func (sess *View) HandleConversationView(w http.ResponseWriter, r *http.Request, defaultView liveview.View) {
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
func (sess *View) HandleConversationEvents(w http.ResponseWriter, r *http.Request) {
	sess.HandleConversationEventsView(w, r, conversationViewFull)
}

// HandleConversationEventsView streams changes to a conversation view with the supplied default mode.
func (sess *View) HandleConversationEventsView(w http.ResponseWriter, r *http.Request, defaultView liveview.View) {
	view, err := parseConversationView(r, defaultView)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	sess.handleFileConversationEvents(w, r, view)
}

// ReconcileSavedTranscript confirms saved items and settles their waiting prompts.
func ReconcileSavedTranscript(ctx context.Context, sess *State) {
	if sess == nil {
		return
	}
	sess.ReconcileDurableTurn(ctx)
	sess.SettleWaitingPrompts(ctx)
}

// View owns transcript projection access, session views and their caches.
type View struct {
	identity   *attachmentIdentity
	wire       *Wire
	delivery   *Delivery
	stateOwner *State
	runtime    *Runtime
	lifecycle  *Lifecycle
	projection *LiveProjection // normalized live transcript, one per session
	nameThread func(provider, nativeThreadID, name string)
	// publishCreationName refreshes the derived name projection from the
	// canonical creation fact without writing it into the manual names file.
	publishCreationName func(provider, nativeThreadID, name string)
	// creationName reloads the immutable creation request from the canonical
	// threads store. Every bind site consults it, so a restart cannot lose the
	// name merely because the first in-memory pendingName was consumed.
	creationName func() string
	// metadataName reads the names-store overlay with its provenance. It is
	// separate from displayName, which is only the immediate pre-catalog UI
	// value while a new native id has not yet been indexed.
	metadataName func(provider, nativeThreadID string) (hostedstore.MetadataName, bool)

	// mcp caches the projected MCP tool registry served by
	// /api/sessions/{id}/mcp, invalidated from the wire (mcp_inventory.go).
	mcp inspection.MCPCache
	// skills is shared across every session because app-server scopes its
	// skills/list answer to cwd, not to one provider thread.
	skills *setup.SkillsListCache

	// liveText is set for a provider switched to file-only transcripts.
	liveText *LiveTextLoop
}
