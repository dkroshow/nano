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
func (attachmentState *State) PublishProjectionRuntime() {
	if attachmentState.view.liveProjection() == nil {
		return
	}
	attachmentState.stateMu.Lock()
	runtime := liveview.Runtime{
		SessionStatus:     attachmentState.connectionStatusLocked(),
		ThreadStatus:      attachmentState.threadStatusLocked(),
		TurnActive:        attachmentState.workActiveLocked(),
		TurnID:            attachmentState.turnID,
		TurnStartedAtMs:   attachmentState.turnStartedAtMs,
		CompactingSinceMs: attachmentState.compactingSinceMsLocked(),
		TokenUsage:        append(json.RawMessage(nil), attachmentState.tokenUsage...),
		LastError:         attachmentState.lastError,
		DeliveryExpiries:  attachmentState.delivery.expiryCounter().count(attachmentState.identity.provider),
	}
	// The transcript's runtime upsert must not interpret hidden disconnected
	// work as a completed turn. Only provider terminal evidence completes it.
	if !runtime.TurnActive {
		runtime.TurnID, runtime.TurnStartedAtMs = "", 0
	}
	attachmentState.stateMu.Unlock()
	attachmentState.view.liveProjection().updateRuntime(runtime)
}

func (views *View) applyLiveRuntimeWire(dir wire.Direction, line []byte, at time.Time) {
	if views.projection == nil {
		return
	}
	if observe := lifecycleProviders[views.identity.provider].runtime; observe != nil {
		observe(views.state, dir, line, at)
	}
}

func (attachmentState *State) setLiveTokenUsage(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	attachmentState.stateMu.Lock()
	attachmentState.tokenUsage = append(json.RawMessage(nil), raw...)
	attachmentState.stateMu.Unlock()
	attachmentState.view.liveProjection().updateTokenUsage(raw)
}

func (attachmentState *State) setLiveTokenUsageForTurn(turnID string, raw json.RawMessage) {
	attachmentState.setLiveTokenUsage(raw)
	usage := normalizeTokenUsage(raw)
	if usage == nil || turnID == "" {
		return
	}
	attachmentState.stateMu.Lock()
	if attachmentState.turnTokenUsage == nil {
		attachmentState.turnTokenUsage = make(map[string]*interact.TokenUsage)
	}
	attachmentState.turnTokenUsage[turnID] = usage
	if len(attachmentState.turnTokenUsage) > liveProjectionStateTurnCap {
		for id := range attachmentState.turnTokenUsage {
			if id != turnID {
				delete(attachmentState.turnTokenUsage, id)
				break
			}
		}
	}
	attachmentState.stateMu.Unlock()
}

func (attachmentState *State) completedTokenUsage(turnID string) *interact.TokenUsage {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return attachmentState.turnTokenUsage[turnID]
}

func (attachmentState *State) setLiveError(message string) {
	if message == "" {
		return
	}
	attachmentState.stateMu.Lock()
	attachmentState.lastError = message
	attachmentState.stateMu.Unlock()
	attachmentState.view.liveProjection().updateError(message)
}

// ReconcileDurableTurn settles the session's open turn from its saved status
// when the live terminal event was lost. It reads only while a turn is open.
func (attachmentState *State) ReconcileDurableTurn(ctx context.Context) {
	if attachmentState == nil || attachmentState.view.liveProjection() == nil || attachmentState.savedTurn == nil {
		return
	}
	attachmentState.stateMu.Lock()
	turnID := attachmentState.turnID
	threadID := attachmentState.threadID
	generation := attachmentState.attachmentGeneration
	sessionTurnActive := attachmentState.workActiveLocked()
	attachmentState.stateMu.Unlock()
	if turnID == "" {
		turnID = attachmentState.view.liveProjection().currentTurnID()
	}
	if turnID == "" {
		return
	}
	liveTurn, wireEvidenceAtMs, version, retained := attachmentState.view.liveProjection().retainedTurnEvidence(turnID)
	if !retained {
		return
	}
	if !sessionTurnActive && liveTurn.Status != "in_progress" {
		return
	}
	exact, found := attachmentState.savedTurn(ctx, turnID)
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
	attachmentState.stateMu.Lock()
	if attachmentState.attachmentGeneration != generation || threadID != "" && attachmentState.threadID != threadID ||
		!attachmentState.view.liveProjection().completeTurnUnlessWorked(turnID, version, status, exact.Turn.DurationMS, exact.Turn.CompletedAtMs) ||
		attachmentState.workActiveLocked() && attachmentState.turnID != turnID {
		attachmentState.stateMu.Unlock()
		return
	}
	if threadID == "" {
		threadID = attachmentState.threadID
	}
	attachmentState.durablyTerminalTurnID = turnID
	attachmentState.durablyTerminalTurnThreadID = threadID
	turnActiveChanged := attachmentState.workActiveLocked()
	if turnActiveChanged {
		attachmentState.transitionLifecycleLocked(lifecycle.EventFinished)
		attachmentState.turnID = ""
		attachmentState.turnStartedAtMs = 0
		if exact.Turn.CompletedAtMs > 0 {
			attachmentState.lastTurnEndedAt = time.UnixMilli(exact.Turn.CompletedAtMs).UTC().Format(time.RFC3339Nano)
		}
	}
	attachmentState.stateMu.Unlock()

	if !turnActiveChanged {
		return
	}
	attachmentState.PublishProjectionRuntime()
	attachmentState.NotifyRuntime()
	attachmentState.delivery.kickQueuedTurnDrain()
}

// HandleConversation serves the default conversation view for this attachment.
func (views *View) HandleConversation(w http.ResponseWriter, r *http.Request) {
	views.HandleConversationView(w, r, conversationViewFull)
}

// HandleConversationView serves a paginated conversation view with the supplied default mode.
func (views *View) HandleConversationView(w http.ResponseWriter, r *http.Request, defaultView liveview.View) {
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
	snapshot, err := views.fileConversationSnapshot(r.Context(), view, before, limit)
	if err != nil {
		views.liveText.writeBackendError(w, err)
		return
	}
	httpapi.WriteJSON(w, snapshot)
}

// HandleConversationEvents streams changes to the default conversation view.
func (views *View) HandleConversationEvents(w http.ResponseWriter, r *http.Request) {
	views.HandleConversationEventsView(w, r, conversationViewFull)
}

// HandleConversationEventsView streams changes to a conversation view with the supplied default mode.
func (views *View) HandleConversationEventsView(w http.ResponseWriter, r *http.Request, defaultView liveview.View) {
	view, err := parseConversationView(r, defaultView)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	views.handleFileConversationEvents(w, r, view)
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
	state      *State
	runtime    *Runtime
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

func (views *View) liveProjection() *LiveProjection { return views.projection }
func (views *View) skillsFor(cwd string) []byte     { return views.skills.Current(cwd) }
func (views *View) invalidateMCP()                  { views.mcp.Invalidate() }
