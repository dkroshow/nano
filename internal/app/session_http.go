package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	hostedvalues "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/providers"
	"github.com/dkroshow/nano/internal/setup"
)

func (sess *session) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteHTTPError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ch := make(chan wireEvent, 512)
	sess.mu.Lock()
	replay := append([]wireEvent(nil), sess.events...)
	sess.subs[ch] = struct{}{}
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		delete(sess.subs, ch)
		sess.mu.Unlock()
	}()

	writeEvent := func(evt wireEvent) bool {
		data, err := json.Marshal(evt)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", data)
		return err == nil
	}
	for _, evt := range replay {
		if !writeEvent(evt) {
			return
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case evt := <-ch:
			if !writeEvent(evt) {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (sess *session) handleEventsPage(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			httpapi.WriteHTTPError(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if limit > 2000 {
		limit = 2000
	}

	sess.mu.Lock()
	firstSeq := int64(0)
	if len(sess.events) > 0 {
		firstSeq = sess.events[0].Seq
	}
	currentSeq := sess.seq
	memory := append([]wireEvent(nil), sess.events...)
	spool := sess.spool
	sess.mu.Unlock()

	beforeSeq := firstSeq
	if beforeSeq == 0 {
		beforeSeq = currentSeq + 1
	}
	if raw := r.URL.Query().Get("beforeSeq"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			httpapi.WriteHTTPError(w, "beforeSeq must be a positive integer", http.StatusBadRequest)
			return
		}
		beforeSeq = n
	}

	events, hasMore, err := readEventsBefore(memory, spool, beforeSeq, limit)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	oldestSeq, newestSeq := int64(0), int64(0)
	if len(events) > 0 {
		oldestSeq = events[0].Seq
		newestSeq = events[len(events)-1].Seq
	}
	httpapi.WriteJSON(w, map[string]any{
		"events":    events,
		"hasMore":   hasMore,
		"oldestSeq": oldestSeq,
		"newestSeq": newestSeq,
	})
}

func readEventsBefore(memory []wireEvent, spool *wireSpool, beforeSeq int64, limit int) ([]wireEvent, bool, error) {
	if spool != nil {
		return spool.readBefore(beforeSeq, limit)
	}
	var older []wireEvent
	for _, evt := range memory {
		if evt.Seq < beforeSeq {
			older = append(older, evt)
		}
	}
	hasMore := len(older) > limit
	if hasMore {
		older = older[len(older)-limit:]
	}
	return older, hasMore, nil
}

// handleState returns the handshake snapshots (init, thread/start). Every pane
// reads it when it opens, so what only a panel shows has its own endpoint,
// fetched when that panel opens: the MCP tool registry (measured 0.84–6.1s of
// a ~250 KB payload, mcp_inventory.go) and the skills list (about 80% of a
// 60 KB answer, skills_cache.go).
// Claude's system.init snapshot is all there is (no request surface without
// the control protocol).
func (sess *session) handleState(w http.ResponseWriter, r *http.Request) {
	sess.stateMu.Lock()
	out := map[string]any{
		"sessionId": sess.id, "provider": sess.provider, "threadId": sess.threadID,
		"status": sess.connectionStatusLocked(),
	}
	// The immutable anchor, and only the anchor (D4): a pane that opens
	// mid-turn learns when the turn started from the same value the rail reads,
	// and derives elapsed against its own clock. Omitted when there is none, so
	// a missing key is honestly "unknown", never epoch 0.
	if sess.turnStartedAtMs != 0 {
		out["turnStartedAtMs"] = sess.turnStartedAtMs
	}
	if sess.lastTurnEndedAt != "" {
		out["lastTurnEndedAt"] = sess.lastTurnEndedAt
	}
	// turnActive rides too: a pane opened (or reloaded) mid-turn has no wire
	// events yet, and without this seed it renders an idle composer and no
	// working surface while the rail — fed by the session summary — blinks
	// away (observed on the infra sessions, 2026-07-31).
	out["turnActive"] = sess.workActiveLocked()
	if sess.spawnError != "" {
		out["error"] = sess.spawnError
	}
	for k, v := range sess.state {
		out[k] = v
	}
	threadID := sess.threadID
	cwd := sess.cwd // the background resume handshake may still be updating it
	displayName := sess.displayName
	metadataDegraded := sess.metadataDegraded
	initRaw := sess.state["init"]
	sess.stateMu.Unlock()

	// The same unified projections the summary serves (decision 4), so /state
	// and /api/sessions cannot disagree about model/effort/permission/tier.
	out["posture"] = sess.posture()
	if sess.metadata != nil {
		if view, err := sess.metadata.view(sess.hostedThreadID); err == nil {
			view.Attachment = hostedAttachmentView{State: fmt.Sprint(out["status"]), Provenance: "live_session"}
			if message, _ := out["error"].(string); message != "" {
				view.Attachment.Error = message
			}
			if name, ok := sess.hostedMetadataName(sess.provider, threadID, displayName); ok {
				view.Name = name
			}
			out["metadata"] = view
			if canonicalCwd, ok := hostedvalues.MetadataString(view.Fields["cwd"]); ok {
				out["cwd"] = canonicalCwd
			}
		} else {
			out["metadataUnavailable"] = true
		}
	} else {
		out["metadataUnavailable"] = true
	}
	if metadataDegraded {
		out["metadataDegraded"] = true
	}

	// The pane's info bar names the branch this session is working on. Cached
	// per directory and refreshed only when /state is rebuilt (live_git.go).
	if branch := liveGit.Branch(r.Context(), cwd); branch != "" {
		out["gitBranch"] = branch
	}

	if sess.provider == "claude" {
		if p := claudeTranscriptPath(cwd, threadID); p != "" {
			out["transcriptPath"] = p
		}
		out["claudeContext"] = setup.ClaudeContext(cwd, initRaw)
		paths := []string{}
		for _, src := range setup.ClaudeInstructionSources(cwd, initRaw) {
			paths = append(paths, src.Path)
		}
		out["instructionFiles"] = paths
		// Both are provider-generic now that claude has a control channel: the
		// pane's model switcher reads turnOptions, and the dialog cards read the
		// pending list — which must come from the snapshot, because the wire
		// buffer is bounded and a late-opened tab cannot replay its way to a
		// parked request. `controlInit` rides along in the state map above and
		// carries the model catalog the switcher renders.
		out["turnOptions"] = sess.turnOptionsPayload()
		out["pendingInteractions"] = sess.pendingInteractionList()
		httpapi.WriteJSON(w, out)
		return
	}
	if sess.provider == "grok" {
		out["turnOptions"] = sess.turnOptionsPayload()
		out["pendingInteractions"] = sess.pendingInteractionList()
		httpapi.WriteJSON(w, out)
		return
	}

	out["turnOptions"] = sess.turnOptionsPayload()
	// Server-owned pending interactions. The bounded wire buffer means a tab
	// opened after the request landed cannot find it by replay, so the snapshot
	// is the truth and the feed event is only the nudge (interactions.go).
	out["pendingInteractions"] = sess.pendingInteractionList()
	httpapi.WriteJSON(w, out)
}

// handleInterrupt stops the provider turn in flight through the adapter.
//
// The completion race is the whole design here. Between the operator reading
// "Working" and the request landing, the turn can finish; the id nano holds is
// then stale. Probed against the running app-server (2026-07-30): a stale
// turn/interrupt is met with **silence** — no result, no error — so the race
// surfaces as a deadline, not as an RPC error. Both are the same non-event to
// the operator, who asked for a turn to stop and got a stopped turn, so both
// degrade to a quiet 200 with interrupted:false. That raced result also settles
// the exact active claim and releases the next queued prompt. Only a broken
// daemon (a transport failure, not a missing answer) is worth a 502.
//
// A session whose event loop is wedged is the other 502: the request is bounded
// by coresession.InterruptHandoffTimeout and reports the failure instead of
// hanging (2026-09-21 incident, where a backlogged loop froze this endpoint for
// minutes).
func (sess *session) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	body, err := sess.interruptTurn(r.Context())
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	httpapi.WriteJSON(w, body)
}

func (sess *session) interruptTurn(ctx context.Context) (map[string]any, error) {
	sess.stateMu.Lock()
	active := sess.workActiveLocked()
	sess.stateMu.Unlock()
	threadID, turnID := sess.activeTurn()
	if !active {
		return map[string]any{"interrupted": false, "reason": "no turn in flight"}, nil
	}
	if err := sess.ensureProviderCoordinator(); err != nil {
		return nil, err
	}
	scope := sess.deliveryScope()
	if turnID != "" {
		scope.NativeTurnID = &turnID
		// Recorded before the provider hears of it, so the turn's end can
		// never be read as a prompt lost without a Stop.
		sess.stateMu.Lock()
		if sess.stoppedTurns == nil {
			sess.stoppedTurns = make(map[string]bool)
		}
		sess.stoppedTurns[turnID] = true
		sess.stateMu.Unlock()
	}
	result, err := sess.core.Interrupt(ctx, providers.InterruptRequest{Scope: scope})
	if errors.Is(err, coresession.ErrInterruptNotAccepted) {
		// The stop button must fail loudly rather than hang. Core kept the
		// interrupt at the front of the session queue, so it still reaches the
		// provider if that event ever returns — but nothing here may claim the
		// turn was interrupted.
		return nil, fmt.Errorf("the session is not processing events: the interrupt was not handed to the provider within %s", coresession.InterruptHandoffTimeout)
	}
	if err != nil {
		return nil, err
	}
	body := map[string]any{"interrupted": result.Interrupted}
	if result.Reason != "" {
		body["reason"] = result.Reason
	}
	if threadID != "" && turnID != "" {
		body["turnId"] = turnID
	}
	return body, nil
}

// interruptRaceReason classifies a failed turn/interrupt: an unanswered call
// (the app-server's way of saying "no such turn") or an RPC error both mean the
// turn was already gone. Anything else is a real failure worth reporting.
func interruptRaceReason(err error) (string, bool) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "the turn had already finished", true
	}
	var rpcErr *protocol.RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Message, true
	}
	return "", false
}

func (sess *session) handleTurnOptionsRead(w http.ResponseWriter, _ *http.Request) {
	httpapi.WriteJSON(w, sess.turnOptionsPayload())
}

// Model and effort are next-turn overrides. Priority is a session setting:
// turn/start applies it to that turn and subsequent turns, while Nano also
// persists the choice immediately so a restart before the next prompt cannot
// forget it. A field absent from the body is left alone; an explicitly empty
// string clears its override back to the thread's own value.
func (sess *session) handleTurnOptionsUpdate(w http.ResponseWriter, r *http.Request) {
	if sess.provider == "claude" {
		sess.handleClaudeModelUpdate(w, r)
		return
	}
	if sess.provider == "grok" {
		httpapi.WriteHTTPError(w, "Grok model and effort are fixed when the native session starts", http.StatusNotImplemented)
		return
	}
	var req struct {
		Model       *string `json:"model"`
		Effort      *string `json:"effort"`
		ServiceTier *string `json:"serviceTier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteHTTPError(w, "body must contain model, effort, or serviceTier", http.StatusBadRequest)
		return
	}
	serviceTier := ""
	if req.ServiceTier != nil {
		serviceTier = strings.ToLower(strings.TrimSpace(*req.ServiceTier))
		if serviceTier != "" && serviceTier != "default" && serviceTier != "priority" && serviceTier != "ultrafast" {
			httpapi.WriteHTTPError(w, "serviceTier must be default, priority, or ultrafast", http.StatusBadRequest)
			return
		}
	}
	priorModel, _, priorTier := sess.turnOptions()
	validatedUltrafast := false
	if req.Model != nil || req.ServiceTier != nil {
		model, _, tier := sess.turnOptions()
		if req.ServiceTier != nil {
			tier = serviceTier
		}
		if tier == "" {
			tier = sess.posture().ServiceTier
		}
		if tier == "ultrafast" {
			validatedUltrafast = true
			if model == "" {
				model = sess.posture().Model
			}
			if req.Model != nil {
				model = strings.TrimSpace(*req.Model)
				if model == "" {
					model = sess.model
				}
			}
			if err := sess.codex.validateUltrafast(model, tier); err != nil {
				httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
	}
	// Each override the request carried is one operator decision — an empty
	// value included, which records "back to the thread's own" as a clear.
	var decisions []postureEntry
	record := func(field, value string) {
		decisions = append(decisions, postureEntry{
			Field: field, Value: value,
			Source: postureSourceOperator, Evidence: postureEvidenceTurnOptions,
		})
	}
	sess.stateMu.Lock()
	if validatedUltrafast && (sess.turnModel != priorModel || sess.turnServiceTier != priorTier) {
		sess.stateMu.Unlock()
		httpapi.WriteHTTPError(w, "speed settings changed during capability refresh; try again", http.StatusConflict)
		return
	}
	if req.Model != nil {
		sess.turnModel = strings.TrimSpace(*req.Model)
		record(postureFieldModel, sess.turnModel)
	}
	if req.Effort != nil {
		sess.turnEffort = strings.TrimSpace(*req.Effort)
		record(postureFieldEffort, sess.turnEffort)
	}
	if req.ServiceTier != nil {
		sess.turnServiceTier = serviceTier
		record(postureFieldServiceTier, serviceTier)
	}
	sess.stateMu.Unlock()
	sess.recordPosture(decisions, metadataFreshnessLive)
	httpapi.WriteJSON(w, sess.turnOptionsPayload())
}

// handleClaudeModel is the claude arm of the same endpoint, and its semantics
// are deliberately different: codex's model is a per-turn override merged into
// the next turn/start, while claude's `set_model` changes the live session
// immediately — the child, its native session id and the whole conversation
// survive (probed 2026-07-30). The posture-preserving `--resume` respawn stays
// where it was, as the fallback rung for a session with no live transport, and
// is not reached from here.
//
// One surface, two truths, so the payload keeps the same field names: `model`
// is the override in force ("" = the model the session was spawned with, which
// `threadModel` names).
func (sess *session) handleClaudeModelUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model *string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteHTTPError(w, "body must be {\"model\": \"…\"}", http.StatusBadRequest)
		return
	}
	if req.Model != nil {
		model := strings.TrimSpace(*req.Model)
		claude := sess.claudeHandle()
		if claude == nil {
			httpapi.WriteHTTPError(w, "session transport is still starting", http.StatusConflict)
			return
		}
		decision := postureEntry{Field: postureFieldModel, Value: model, Source: postureSourceOperator, Evidence: postureEvidenceSetModel}
		// Persist intent before delivery; the successful provider control reply
		// below is the causal confirmation rather than a local assignment.
		sess.recordPosture([]postureEntry{decision}, metadataFreshnessLive)
		snapshot := sess.snapshotProviderCausation(map[string]string{postureFieldModel: model})
		if err := claude.setModel(r.Context(), model); err != nil {
			httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
			return
		}
		sess.stateMu.Lock()
		sess.turnModel = model
		sess.stateMu.Unlock()
		sess.recordProviderEvidence("claude_set_model", map[string]string{postureFieldModel: model}, metadataFreshnessLive, snapshot)
	}
	httpapi.WriteJSON(w, sess.turnOptionsPayload())
}

// turnOptionsPayload is what the pane's controls render from: the overrides in
// force plus the thread's own model, so "no override" can be shown as the
// thread's actual model rather than as a blank.
func (sess *session) turnOptionsPayload() map[string]any {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return map[string]any{
		"model":       sess.turnModel,
		"effort":      sess.turnEffort,
		"serviceTier": sess.turnServiceTier,
		"summary":     sess.reasoningSummary,
		"threadModel": sess.model,
	}
}

// noteHumanPrompt records that a human submitted a prompt to this thread now.
// It is the only source of the "human typed recently" availability fact;
// agent-origin prompts never set it, even though they also land as user-role
// transcript items.
func (sess *session) noteHumanPrompt(at time.Time) {
	sess.stateMu.Lock()
	if at.After(sess.lastHumanPromptAt) {
		sess.lastHumanPromptAt = at
	}
	sess.stateMu.Unlock()
	if sess.agentExecutions != nil && sess.hostedThreadID != "" {
		if err := sess.agentExecutions.noteHumanPrompt(sess.hostedThreadID, at); err != nil {
			log.Printf("record human prompt time hosted=%s: %v", sess.hostedThreadID, err)
		}
	}
}

func (sess *session) humanPromptAt() time.Time {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.lastHumanPromptAt
}
