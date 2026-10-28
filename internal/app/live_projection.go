package app

// live_projection.go parses one hosted session's provider wire for live text,
// turns, runtime and pending questions, and keeps each turn's answer ids and
// live work (liveTurnWork). It holds no transcript rows: panes, agents and
// operations read saved rows from the catalog.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const (
	liveProjectionVersion        = liveview.Version
	liveProjectionMaxBytes       = 2 << 20
	liveProjectionTextCap        = 32 << 10
	liveProjectionOutputCap      = 64 << 10
	liveProjectionStateItemCap   = 1024
	liveProjectionStateTurnCap   = 256
	liveProjectionItemPayloadCap = 96 << 10
	liveProjectionTurnTail       = 5
	transcriptFirstPaintRows     = liveview.FirstPaintRows
	conversationPageMaxRows      = liveview.PageMaxRows
)

const (
	projectionSnapshot          = liveview.SnapshotOp
	projectionTurnUpsert        = liveview.TurnUpsertOp
	projectionInteraction       = liveview.InteractionOp
	projectionInteractionRemove = liveview.InteractionRemoveOp
	projectionMetaRefresh       = liveview.MetaRefreshOp
)

type conversationView = liveview.View

const (
	conversationViewFull     conversationView = liveview.Full
	conversationViewMessages conversationView = liveview.Messages
)

// liveProjectionItem is intentionally provider-neutral at the wire boundary.
// Item contains only the normalized fields the conversation renderer needs;
// provider protocol envelopes never cross this API.
type liveProjectionItem = liveview.Item

type liveProjectionTurn = liveview.Turn

// liveTurnWork is one turn's compact completion facts: ids and times, no
// text.
type liveTurnWork struct {
	finalAnswer string // newest finished final_answer agent message
	lastAnswer  string // newest finished agent message
	atMs        int64  // newest live, non-replay work of the turn
	version     uint64 // bumped with every such work
}

type liveProjectionRuntime = liveview.Runtime

type liveProjectionSnapshot = liveview.Snapshot

type liveProjectionPage = liveview.Page

type liveProjectionUpdate = liveview.Update

type liveAdapter interface {
	Apply(protocol.Direction, []byte, time.Time) model.Batch
}

type liveProjection struct {
	mu             sync.Mutex
	adapterMu      sync.Mutex
	provider       string
	hostedThreadID string
	nativeThread   string
	epoch          string
	revision       int64
	claudeTurnID   string
	// claudeResultTurnID is the turn the result being applied names
	// (claudeAnsweredTurn): set on the session loop just before it, and never
	// by a send acknowledgement, which can land in between.
	claudeResultTurnID string
	adapter            liveAdapter
	turns              map[string]*liveProjectionTurn
	turnOrder          []string
	// turnWork holds what each recent turn's completion needs now that no
	// rows are kept: its final answer's id and its latest live work. One
	// writer, applyWireLive; bounded like the turn list. It stays out of
	// liveProjectionTurn, which several writers replace whole and panes read.
	turnWork      map[string]*liveTurnWork
	turnWorkOrder []string
	// claudeRun is the answers of the Claude execution now running. Claude
	// runs one at a time, so the turn its result names takes them, whatever
	// turn the frames were tagged with: once a prompt is written mid-turn,
	// the rest of the running execution's frames carry that prompt's tag.
	claudeRun        liveTurnWork
	runtime          liveProjectionRuntime
	interactions     map[string]pendingInteraction
	interactionOrder []string
	subs             map[chan liveProjectionUpdate]struct{}
	// diagnostics is the thread's bounded list of delivery and session
	// facts (thread_diagnostics.go).
	diagnosticsMu sync.Mutex
	diagnostics   []threadDiagnostic
	// dropLiveTextDeltas skips Codex agent-message text deltas
	// (NANO_LIVE_TEXT_DELTAS=off); item/completed carries the full text, and a
	// message whose item/completed never arrives shows no live partial text.
	dropLiveTextDeltas bool

	// liveText receives each frame's parsed changes for a file-only thread;
	// set once before wire activity.
	liveText liveTextObserver
}

// codexAgentTextDelta reports an agent-message delta, or the empty
// item/started that would otherwise sit blank until item/completed, and the
// turn it names. Reasoning and command output keep streaming.
func codexAgentTextDelta(line []byte) (string, bool) {
	if !bytes.Contains(line, []byte("agentMessage")) {
		return "", false
	}
	var envelope struct {
		Method string `json:"method"`
		Params struct {
			TurnID string `json:"turnId"`
			Item   struct {
				Type string  `json:"type"`
				Text *string `json:"text"`
			} `json:"item"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return "", false
	}
	switch envelope.Method {
	case "item/agentMessage/delta":
		return envelope.Params.TurnID, true
	case "item/started":
		item := envelope.Params.Item
		return envelope.Params.TurnID, item.Type == "agentMessage" && (item.Text == nil || *item.Text == "")
	}
	return "", false
}

func newLiveProjection(provider string, hostedThreadID ...string) *liveProjection {
	seed := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", provider, time.Now().UnixNano())))
	epoch := "lpe_" + hex.EncodeToString(seed[:8])
	var adapter liveAdapter
	if provider == "claude" {
		adapter = newClaudeLiveAdapter()
	} else if provider == "grok" {
		adapter = newGrokLiveAdapter()
	} else {
		adapter = newCodexLiveAdapter()
	}
	projection := &liveProjection{
		provider: provider, epoch: epoch, adapter: adapter,
		turns: map[string]*liveProjectionTurn{}, turnWork: map[string]*liveTurnWork{},
		interactions: map[string]pendingInteraction{}, subs: map[chan liveProjectionUpdate]struct{}{},
		runtime: liveProjectionRuntime{ThreadStatus: "unknown"},
	}
	if len(hostedThreadID) > 0 {
		projection.hostedThreadID = hostedThreadID[0]
	}
	return projection
}

// newSessionProjection applies the server's live-transcript settings to a
// session's projection.
func (s *server) newSessionProjection(provider, hostedThreadID string) *liveProjection {
	projection := newLiveProjection(provider, hostedThreadID)
	projection.dropLiveTextDeltas = s.dropLiveTextDeltas
	return projection
}

// SetNativeThreadID sets the native identity represented by the view.
func (p *liveProjection) SetNativeThreadID(id string) {
	p.mu.Lock()
	p.nativeThread = id
	p.mu.Unlock()
}

// SetClaudeProviderTurnID sets the provider identity of the current Claude turn.
func (p *liveProjection) SetClaudeProviderTurnID(id string) {
	if p == nil || p.provider != "claude" || id == "" {
		return
	}
	p.mu.Lock()
	p.claudeTurnID = id
	p.mu.Unlock()
}

// SetClaudeFrameTurnID adopts one spooled frame's own turn identity, empty
// included: the shim tags only a Nano prompt's frames, so an unprompted
// turn's frames clear the previous prompt's id instead of inheriting it.
func (p *liveProjection) SetClaudeFrameTurnID(id string) {
	if p == nil || p.provider != "claude" {
		return
	}
	p.mu.Lock()
	p.claudeTurnID = id
	p.mu.Unlock()
}

// SetClaudeResultTurnID names the turn of the result about to be applied,
// "" when it names none.
func (p *liveProjection) SetClaudeResultTurnID(id string) {
	if p == nil || p.provider != "claude" {
		return
	}
	p.mu.Lock()
	p.claudeResultTurnID = id
	p.mu.Unlock()
}

// ClaudeResultTurn is the turn the last applied Claude result named.
func (p *liveProjection) ClaudeResultTurn() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claudeResultTurnID
}

// CurrentTurnID reads the current projected turn identity.
func (p *liveProjection) CurrentTurnID() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turnOrder) == 0 {
		return ""
	}
	return p.turnOrder[len(p.turnOrder)-1]
}

// EnsureClaudeActiveTurn gives provider execution evidence a normalized turn
// when no Nano-submitted prompt opened one. It reuses an existing in-progress
// turn, so streaming deltas are keep-alives rather than new turns.
func (p *liveProjection) EnsureClaudeActiveTurn(startedAtMs int64) string {
	if p == nil || p.provider != "claude" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turnOrder) > 0 {
		id := p.turnOrder[len(p.turnOrder)-1]
		if turn := p.turns[id]; turn != nil && turn.Status == "in_progress" {
			return id
		}
	}
	id := p.claudeTurnID
	if id == "" {
		return ""
	}
	turn := &liveProjectionTurn{ID: id, Status: "in_progress", StartedAtMs: startedAtMs}
	p.upsertTurnLocked(turn)
	p.appendLocked(liveProjectionUpdate{Op: projectionTurnUpsert, Turn: cloneTurn(turn)})
	return id
}

func (p *liveProjection) applyWire(dir protocol.Direction, line []byte, at time.Time) {
	p.ApplyWireLive(dir, line, at, true, false)
}

// ApplyWireLive keeps replay provenance separate from live browser emission.
// Only the Claude shim's re-attach spool sets replay; other non-live
// observations remain ordinary.
func (p *liveProjection) ApplyWireLive(dir protocol.Direction, line []byte, at time.Time, live, replay bool, _ ...int64) {
	if p.dropLiveTextDeltas && p.provider == "codex" && dir == protocol.Recv {
		if turn, hidden := codexAgentTextDelta(line); hidden {
			// Hidden text is still the turn's live work.
			if live && !replay && turn != "" {
				p.mu.Lock()
				p.turnWorkLocked(turn).worked(at)
				p.mu.Unlock()
			}
			return
		}
	}
	claudeTurnOpened := false
	claudeTurnClosed := false
	if p.provider == "claude" {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &envelope) == nil {
			claudeTurnOpened = (dir == protocol.Send && envelope.Type == "user") ||
				(live && dir == protocol.Recv && claudeExecutionEvidence(line))
			claudeTurnClosed = dir == protocol.Recv && envelope.Type == "result"
		}
	}
	providerTurnID := ""
	if p.provider == "claude" {
		p.mu.Lock()
		providerTurnID = p.claudeTurnID
		if claudeTurnClosed {
			providerTurnID = p.claudeResultTurnID
		}
		p.mu.Unlock()
	}
	p.adapterMu.Lock()
	batch := p.adapter.Apply(dir, line, at)
	if p.liveText != nil {
		p.liveText.ObserveBatch(batch, providerTurnID)
		if claudeTurnClosed {
			p.liveText.TurnEnded(providerTurnID)
		}
	}
	// Taken before adapterMu is released, so frames record in parse order.
	p.mu.Lock()
	p.adapterMu.Unlock()
	p.recordTurnWorkLocked(batch, providerTurnID, at, live && !replay)
	if claudeTurnClosed {
		if providerTurnID != "" && p.claudeRun.lastAnswer != "" {
			work := p.turnWorkLocked(providerTurnID)
			work.lastAnswer, work.finalAnswer = p.claudeRun.lastAnswer, p.claudeRun.finalAnswer
		}
		p.claudeRun = liveTurnWork{}
	}
	if claudeTurnOpened && p.claudeTurnID != "" && (dir == protocol.Send || !p.latestTurnInProgressLocked()) {
		turn := &liveProjectionTurn{ID: p.claudeTurnID, Status: "in_progress", StartedAtMs: at.UnixMilli()}
		p.upsertTurnLocked(turn)
		p.appendLocked(liveProjectionUpdate{Op: projectionTurnUpsert, Turn: cloneTurn(turn)})
	}
	if claudeTurnClosed && providerTurnID != "" {
		id := providerTurnID
		turn := cloneTurn(p.turns[id])
		if turn != nil {
			turn.Status, turn.Error = claudeProjectionTerminal(line)
			turn.CompletedAtMs = at.UnixMilli()
			if turn.StartedAtMs > 0 {
				turn.DurationMS = turn.CompletedAtMs - turn.StartedAtMs
				if turn.DurationMS < 0 {
					turn.DurationMS = 0
				}
			}
			if turn.Status == "completed" {
				turn.FinalItemID = p.finalAnswerIDLocked(id)
			}
			p.upsertTurnLocked(turn)
			p.trimTurnsLocked()
			p.appendLocked(liveProjectionUpdate{Op: projectionTurnUpsert, Turn: cloneTurn(turn)})
		}
	}
	p.mu.Unlock()
}

// recordTurnWorkLocked keeps, from one parsed frame, each turn's final answer
// and latest live work. Only a top-level, non-sidechain agent message of this
// native thread answers a turn, so a subagent's answer never stands for its
// parent's, and a finished final_answer outranks later commentary. A Claude
// item names no native turn; it belongs to the prompt's turn. Replayed frames
// are old output, not new work. Callers hold mu.
func (p *liveProjection) recordTurnWorkLocked(batch model.Batch, providerTurnID string, at time.Time, live bool) {
	for _, change := range batch.Changes {
		turnID, workAt, answer := "", at, ""
		final := false
		switch {
		case change.Item != nil:
			item := change.Item
			if item.Scope.NativeTurnID != nil {
				turnID = *item.Scope.NativeTurnID
			}
			if item.CreatedAt != nil {
				workAt = *item.CreatedAt
			}
			if item.Type == model.ItemAgentMessage && item.Scope.NativeParentCallID == nil && !item.Sidechain &&
				(item.Scope.NativeThreadID == nil || p.nativeThread == "" || *item.Scope.NativeThreadID == p.nativeThread) &&
				(item.Complete || p.provider == "grok" && item.Synthetic) {
				answer, final = threads.FirstNonEmpty(item.NativeItemID, item.ID), item.Phase == "final_answer" || item.IsFinalAnswer
			}
			if answer != "" && p.provider == "claude" {
				p.claudeRun.lastAnswer = answer
				if final {
					p.claudeRun.finalAnswer = answer
				}
			}
		case change.Turn != nil:
			turnID = change.Turn.NativeTurnID
		default:
			continue
		}
		if turnID = threads.FirstNonEmpty(turnID, providerTurnID); turnID == "" || !live && answer == "" {
			continue
		}
		work := p.turnWorkLocked(turnID)
		if live {
			work.worked(workAt)
		}
		if answer != "" {
			work.lastAnswer = answer
			if final {
				work.finalAnswer = answer
			}
		}
	}
}

// turnWorkLocked is the turn's work record, created when new; past the turn
// cap the oldest record goes. Callers hold mu.
func (p *liveProjection) turnWorkLocked(turnID string) *liveTurnWork {
	work := p.turnWork[turnID]
	if work == nil {
		work = &liveTurnWork{}
		p.turnWork[turnID] = work
		p.turnWorkOrder = append(p.turnWorkOrder, turnID)
		for len(p.turnWorkOrder) > liveProjectionStateTurnCap {
			delete(p.turnWork, p.turnWorkOrder[0])
			p.turnWorkOrder = p.turnWorkOrder[1:]
		}
	}
	return work
}

// worked notes one live frame of the turn's work.
func (w *liveTurnWork) worked(at time.Time) {
	w.atMs, w.version = max(w.atMs, at.UnixMilli()), w.version+1
}

func (p *liveProjection) latestTurnInProgressLocked() bool {
	if len(p.turnOrder) == 0 {
		return false
	}
	turn := p.turns[p.turnOrder[len(p.turnOrder)-1]]
	return turn != nil && turn.Status == "in_progress"
}

// trimTurnsLocked keeps the in-progress turns and the newest completed ones.
func (p *liveProjection) trimTurnsLocked() {
	completed := make([]string, 0, len(p.turnOrder))
	for _, turnID := range p.turnOrder {
		if turn := p.turns[turnID]; turn != nil && codexResumeTurnTerminal(turn.Status) {
			completed = append(completed, turnID)
		}
	}
	if len(completed) <= liveProjectionTurnTail {
		return
	}
	remove := make(map[string]struct{}, len(completed)-liveProjectionTurnTail)
	for _, turnID := range completed[:len(completed)-liveProjectionTurnTail] {
		remove[turnID] = struct{}{}
		delete(p.turns, turnID)
	}
	turns := p.turnOrder[:0]
	for _, turnID := range p.turnOrder {
		if _, drop := remove[turnID]; !drop {
			turns = append(turns, turnID)
		}
	}
	p.turnOrder = turns
}

func (p *liveProjection) upsertTurnLocked(turn *liveProjectionTurn) {
	if turn == nil || turn.ID == "" {
		return
	}
	if p.turns[turn.ID] == nil {
		p.turnOrder = append(p.turnOrder, turn.ID)
	}
	p.turns[turn.ID] = cloneTurn(turn)
	for len(p.turnOrder) > liveProjectionStateTurnCap {
		oldest := p.turnOrder[0]
		p.turnOrder = p.turnOrder[1:]
		delete(p.turns, oldest)
	}
}

// UpdateRuntime publishes the attachment’s runtime view.
func (p *liveProjection) UpdateRuntime(runtime liveProjectionRuntime) {
	runtime = liveview.CloneRuntime(runtime)
	p.mu.Lock()
	if liveview.RuntimeEqual(p.runtime, runtime) {
		p.mu.Unlock()
		return
	}
	p.runtime = runtime
	turn := p.runtimeTurnLocked()
	if turn.ID != "" {
		p.upsertTurnLocked(turn)
	}
	p.appendLocked(liveProjectionUpdate{Op: projectionTurnUpsert, Turn: turn, Runtime: &runtime})
	p.mu.Unlock()
}

// UpdateTokenUsage records current provider token usage.
func (p *liveProjection) UpdateTokenUsage(raw json.RawMessage) {
	raw = liveview.TruncateJSON(raw, 16<<10)
	p.mu.Lock()
	if bytes.Equal(p.runtime.TokenUsage, raw) {
		p.mu.Unlock()
		return
	}
	p.runtime.TokenUsage = append(json.RawMessage(nil), raw...)
	p.appendLocked(liveProjectionUpdate{Op: projectionTurnUpsert, Turn: p.runtimeTurnLocked(), Runtime: cloneRuntimePtr(p.runtime)})
	p.mu.Unlock()
}

// UpdateError records the latest live error.
func (p *liveProjection) UpdateError(message string) {
	message = threads.TruncateText(message, 4<<10)
	p.mu.Lock()
	if p.runtime.LastError == message {
		p.mu.Unlock()
		return
	}
	p.runtime.LastError = message
	p.appendLocked(liveProjectionUpdate{Op: projectionTurnUpsert, Turn: p.runtimeTurnLocked(), Runtime: cloneRuntimePtr(p.runtime)})
	p.mu.Unlock()
}

// NudgeMetaRefresh publishes a view metadata refresh.
func (p *liveProjection) NudgeMetaRefresh() {
	p.mu.Lock()
	p.appendLocked(liveProjectionUpdate{Op: projectionMetaRefresh})
	p.mu.Unlock()
}

func wireNudgesProjectionMeta(provider string, dir protocol.Direction, line []byte) bool {
	if dir != protocol.Recv {
		return false
	}
	if provider == "grok" {
		if _, _, _, ok := grokTerminalFromWire(line); ok {
			return true
		}
		var envelope struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(line, &envelope) != nil {
			return false
		}
		return envelope.Method == "_x.ai/models/update" || envelope.Method == "_x.ai/settings/update"
	}
	if provider == "claude" {
		var message struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(line, &message) != nil {
			return false
		}
		return message.Type == "result" || (message.Type == "system" && message.Subtype == "init")
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return false
	}
	switch envelope.Method {
	case "turn/completed", "skills/changed", "mcpServer/startupStatus/updated":
		return true
	default:
		return false
	}
}

// UpdateCompletedTurn records terminal evidence for a projected turn.
func (p *liveProjection) UpdateCompletedTurn(turnID, status string, durationMS int64, completedAtMs int64, turnError ...json.RawMessage) {
	p.mu.Lock()
	p.updateCompletedTurnLocked(turnID, status, durationMS, completedAtMs, turnError...)
	p.mu.Unlock()
}

// CompleteTurnUnlessWorked records a saved turn end unless the turn has done
// live work since version was read. Live work records under mu, so the check
// and the end are one step.
func (p *liveProjection) CompleteTurnUnlessWorked(turnID string, version uint64, status string, durationMS, completedAtMs int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if work := p.turnWork[turnID]; work != nil && work.version != version {
		return false
	}
	p.updateCompletedTurnLocked(turnID, status, durationMS, completedAtMs)
	return true
}

func (p *liveProjection) updateCompletedTurnLocked(turnID, status string, durationMS int64, completedAtMs int64, turnError ...json.RawMessage) {
	turnID = threads.TruncateText(turnID, 256)
	status = threads.TruncateText(status, 64)
	if turnID == "" {
		return
	}
	turn := cloneTurn(p.turns[turnID])
	if turn == nil {
		turn = &liveProjectionTurn{ID: turnID}
	}
	turn.Status = status
	if status == "completed" {
		turn.DurationMS = durationMS
	} else {
		turn.DurationMS = 0
	}
	turn.CompletedAtMs = completedAtMs
	turn.FinalItemID = ""
	if status == "completed" {
		turn.FinalItemID = p.finalAnswerIDLocked(turnID)
	}
	if len(turnError) > 0 && len(turnError[0]) > 0 && string(turnError[0]) != "null" {
		turn.Error = append(json.RawMessage(nil), turnError[0]...)
	}
	p.upsertTurnLocked(turn)
	p.trimTurnsLocked()
	p.appendLocked(liveProjectionUpdate{Op: projectionTurnUpsert, Turn: turn, Runtime: cloneRuntimePtr(p.runtime)})
}

// LatestTerminalTurnID reads the newest terminal turn identity.
func (p *liveProjection) LatestTerminalTurnID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for index := len(p.turnOrder) - 1; index >= 0; index-- {
		if turn := p.turns[p.turnOrder[index]]; turn != nil && threads.TurnTerminal(turn.Status) {
			return turn.ID
		}
	}
	return ""
}

// FinalAgentItemID names a turn's final answer by source id, the identity
// records outside this projection use; saved history carries the same one.
func (p *liveProjection) FinalAgentItemID(turnID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.finalAnswerIDLocked(turnID)
}

// finalAnswerIDLocked is a turn's newest finished final_answer, else its
// newest finished answer. Callers hold mu.
func (p *liveProjection) finalAnswerIDLocked(turnID string) string {
	if work := p.turnWork[turnID]; work != nil {
		return threads.FirstNonEmpty(work.finalAnswer, work.lastAnswer)
	}
	return ""
}

// TurnList copies the projection's turns, oldest first.
func (p *liveProjection) TurnList() []liveProjectionTurn {
	p.mu.Lock()
	defer p.mu.Unlock()
	turns := make([]liveProjectionTurn, 0, len(p.turnOrder))
	for _, id := range p.turnOrder {
		if turn := p.turns[id]; turn != nil {
			turns = append(turns, *cloneTurn(turn))
		}
	}
	return turns
}

func cloneRuntimePtr(runtime liveProjectionRuntime) *liveProjectionRuntime {
	out := liveview.CloneRuntime(runtime)
	return &out
}

// UpsertInteraction records a pending provider interaction.
func (p *liveProjection) UpsertInteraction(interaction pendingInteraction) {
	interaction = liveview.CloneInteraction(interaction)
	p.mu.Lock()
	if _, exists := p.interactions[interaction.ID]; !exists {
		p.interactionOrder = append(p.interactionOrder, interaction.ID)
	}
	p.interactions[interaction.ID] = interaction
	for len(p.interactionOrder) > liveProjectionStateItemCap {
		oldest := p.interactionOrder[0]
		p.interactionOrder = p.interactionOrder[1:]
		delete(p.interactions, oldest)
	}
	p.appendLocked(liveProjectionUpdate{Op: projectionInteraction, Interaction: &interaction})
	p.mu.Unlock()
}

// RemoveInteraction removes a resolved interaction from the view.
func (p *liveProjection) RemoveInteraction(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	if _, ok := p.interactions[id]; !ok {
		p.mu.Unlock()
		return
	}
	delete(p.interactions, id)
	for i, candidate := range p.interactionOrder {
		if candidate == id {
			p.interactionOrder = append(p.interactionOrder[:i], p.interactionOrder[i+1:]...)
			break
		}
	}
	p.appendLocked(liveProjectionUpdate{Op: projectionInteractionRemove, RequestID: id})
	p.mu.Unlock()
}

func (p *liveProjection) interactionsLocked() []pendingInteraction {
	interactions := make([]pendingInteraction, 0, len(p.interactions))
	for _, interaction := range p.interactions {
		interactions = append(interactions, liveview.CloneInteraction(interaction))
	}
	sort.Slice(interactions, func(i, j int) bool {
		return threads.TimestampBefore(interactions[i].CreatedAt, interactions[j].CreatedAt)
	})
	return interactions
}

// Status is the thread's runtime and pending questions at the current
// revision, without rows: what a file-only pane's stream carries.
func (p *liveProjection) Status() liveTextStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return liveTextStatus{Epoch: p.epoch, Revision: p.revision, Runtime: liveview.CloneRuntime(p.runtime), Interactions: p.interactionsLocked()}
}

// durableTurnEvidence is the saved status of one live turn, read by its exact
// native turn id.
type durableTurnEvidence = liveview.DurableTurnEvidence

// savedTurnEvidence reads the saved status of a Codex or Grok turn by exact
// native turn id. Claude saved turns carry no id the live turn shares, so a
// Claude turn settles only from live evidence.
func (s *server) savedTurnEvidence(ctx context.Context, sess *session, turnID string) (durableTurnEvidence, bool) {
	if s.catalog == nil || sess == nil || (sess.provider != "codex" && sess.provider != "grok") ||
		turnID == "" || sess.currentThreadID() == "" {
		return durableTurnEvidence{}, false
	}
	resolver, ok := s.catalog.(threadReferenceResolver)
	if !ok {
		return durableTurnEvidence{}, false
	}
	catalog, ok := s.catalog.(savedTurnStatusCatalog)
	if !ok {
		return durableTurnEvidence{}, false
	}
	nanoID, err := resolver.ResolveThreadReference(ctx, sess.currentThreadID())
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
func (s *server) confirmDurableThread(ctx context.Context, change transcriptChange) {
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
	s.agentExecutions.savedTranscriptApplied(provider, nativeThreadID)
	if sess := s.sessionForThread(provider, nativeThreadID); sess != nil {
		sess.notifySavedTranscript()
		s.confirmDurableItems(ctx, sess)
	}
	if provider == string(model.ProviderClaude) {
		s.finishDetachedClaudeOwed(nativeThreadID)
		// The commit can decide an answer a subscriber's feed waits on, and the
		// owed pass finishes turns with no notice of their own: the owners'
		// subscriptions read again (the dispatcher coalesces repeats).
		owners, err := s.controlMetadata.nativeOwners(provider, nativeThreadID)
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
	if s.sessionForThread(string(candidate.Provider), candidate.NativeThreadID) != nil {
		return 1
	}
	return 2
}

func (s *server) confirmDurableItems(ctx context.Context, sess *session) {
	if sess == nil {
		return
	}
	sess.reconcileDurableTurn(ctx)
	sess.settleWaitingPrompts(ctx)
}

func durableTerminalStatus(status string) bool {
	switch status {
	case "completed", "interrupted", "truncated":
		return true
	default:
		return false
	}
}

// durableTerminalAuthoritative is deliberately small. A later catalog turn is
// conclusive. Otherwise the catalog may settle the exact turn only when the
// current projection carries no later wire evidence; catalog completion is
// provisional while live traffic still advances that same turn.
func durableTerminalAuthoritative(exact durableTurnEvidence, found bool, wireEvidenceAtMs int64) bool {
	if !found {
		return false
	}
	if exact.Newer {
		return true
	}
	return durableTerminalStatus(exact.Turn.Status) && exact.Turn.CompletedAtMs > 0 &&
		wireEvidenceAtMs <= exact.Turn.CompletedAtMs
}

// RetainedTurnEvidence is a retained turn, the time of its newest start, end
// or live work, and its work counter.
func (p *liveProjection) RetainedTurnEvidence(turnID string) (liveProjectionTurn, int64, uint64, bool) {
	if p == nil || turnID == "" {
		return liveProjectionTurn{}, 0, 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	turn := cloneTurn(p.turns[turnID])
	if turn == nil {
		return liveProjectionTurn{}, 0, 0, false
	}
	latest, version := max(turn.StartedAtMs, turn.CompletedAtMs), uint64(0)
	if work := p.turnWork[turnID]; work != nil {
		latest, version = max(latest, work.atMs), work.version
	}
	return *turn, latest, version, true
}

func (s *server) handleConversationHistory(w http.ResponseWriter, r *http.Request, sess *session) {
	catalog, ok := s.catalog.(threadTranscriptCatalog)
	resolver, resolves := s.catalog.(threadReferenceResolver)
	if !ok || !resolves || sess == nil || sess.currentThreadID() == "" {
		httpapi.WriteHTTPError(w, "conversation history unavailable", http.StatusServiceUnavailable)
		return
	}
	nanoID, err := resolver.ResolveThreadReference(r.Context(), sess.currentThreadID())
	if err != nil {
		writeTranscriptBackendError(w, err)
		return
	}
	query, err := parseTranscriptListQuery(r)
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

func (p *liveProjection) runtimeTurnLocked() *liveProjectionTurn {
	return &liveProjectionTurn{
		ID: p.runtime.TurnID, Status: map[bool]string{true: "in_progress", false: "completed"}[p.runtime.TurnActive],
		StartedAtMs: p.runtime.TurnStartedAtMs,
	}
}

func (p *liveProjection) appendLocked(update liveProjectionUpdate) {
	p.revision++
	update.Version = liveProjectionVersion
	update.Epoch = p.epoch
	update.Revision = p.revision
	for ch := range p.subs {
		select {
		case ch <- cloneUpdate(update):
		default:
			// Silent loss is unsafe when the dropped row may be the terminal
			// turn update and no later row will expose the revision gap. Close
			// the lagging subscriber instead; its stream ends and the caller
			// reconnects to a fresh status.
			delete(p.subs, ch)
			close(ch)
		}
	}
}

// Subscribe registers a subscriber for every later update. A caller reads
// the state it sends first only after subscribing, so no update falls
// between them.
func (p *liveProjection) Subscribe() (ch chan liveProjectionUpdate, cancel func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch = make(chan liveProjectionUpdate, 256)
	p.subs[ch] = struct{}{}
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

func newLiveItem(id, typeName string, at time.Time) *liveProjectionItem {
	if typeName == "" {
		typeName = "unknown"
	}
	item := &liveProjectionItem{
		ID: id, Type: typeName,
		Category: liveCategory(typeName), Role: liveRole(typeName),
		Status: "in_progress", CreatedAt: at.UTC().Format(time.RFC3339Nano),
		Item: map[string]any{"type": typeName},
	}
	return item
}

func liveCategory(typeName string) string {
	switch typeName {
	case "userMessage", "agentMessage":
		return "messages"
	case "reasoning":
		return "reasoning"
	case "toolUse", "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall":
		return "toolCall"
	case "toolResult":
		return "toolResult"
	default:
		return "unclassified"
	}
}

func liveRole(typeName string) string {
	switch typeName {
	case "userMessage":
		return "user"
	case "toolResult":
		return "tool"
	default:
		return "assistant"
	}
}

// liveEventIdentityCap bounds a presented row's event identities.
const liveEventIdentityCap = 8

var liveItemPayloadKeys = []string{
	"type", "name", "command", "aggregatedOutput", "output", "inputJson", "input",
	"isError", "exitCode", "changes", "server", "tool", "arguments", "query", "text",
	"detail", "toolUseId", "phase", "status",
}

func claudeProjectionTerminal(line []byte) (string, json.RawMessage) {
	var message map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil {
		return agentTurnFailed, json.RawMessage(`{"message":"invalid Claude result"}`)
	}
	reason := protocol.String(message["terminal_reason"])
	if strings.HasPrefix(reason, "aborted") {
		return agentTurnInterrupted, nil
	}
	if protocol.Bool(message["is_error"]) {
		detail, _ := json.Marshal(map[string]any{
			"message":        threads.FirstNonEmpty(protocol.String(message["result"]), protocol.String(message["subtype"]), "Claude turn failed"),
			"terminalReason": reason,
		})
		return agentTurnFailed, detail
	}
	return agentTurnCompleted, nil
}

func cloneLiveItem(item *liveProjectionItem) *liveProjectionItem {
	if item == nil {
		return nil
	}
	out := *item
	if item.Item != nil {
		out.Item = map[string]any{}
		for key, value := range item.Item {
			out.Item[key] = value
		}
	}
	return &out
}

func cloneTurn(turn *liveProjectionTurn) *liveProjectionTurn {
	if turn == nil {
		return nil
	}
	out := *turn
	out.ID = threads.TruncateText(out.ID, 256)
	out.Status = threads.TruncateText(out.Status, 64)
	out.FinalItemID = threads.TruncateText(out.FinalItemID, 256)
	out.Error = liveview.TruncateJSON(out.Error, 16<<10)
	return &out
}

func cloneUpdate(update liveProjectionUpdate) liveProjectionUpdate {
	out := update
	out.Turn = cloneTurn(update.Turn)
	if update.Runtime != nil {
		out.Runtime = cloneRuntimePtr(*update.Runtime)
	}
	if update.Interaction != nil {
		interaction := liveview.CloneInteraction(*update.Interaction)
		out.Interaction = &interaction
	}
	if update.Snapshot != nil {
		snapshot := *update.Snapshot
		snapshot.Items = append([]liveProjectionItem(nil), update.Snapshot.Items...)
		for i := range snapshot.Items {
			snapshot.Items[i] = *cloneLiveItem(&snapshot.Items[i])
		}
		snapshot.Turns = append([]liveProjectionTurn(nil), update.Snapshot.Turns...)
		snapshot.Interactions = append([]pendingInteraction(nil), update.Snapshot.Interactions...)
		out.Snapshot = &snapshot
	}
	return out
}

// ProviderName reports the view’s provider.
func (p *liveProjection) ProviderName() string {
	if p == nil {
		return ""
	}
	return p.provider
}

// DropsLiveTextDeltas reports whether live text delta publication is disabled.
func (p *liveProjection) DropsLiveTextDeltas() bool { return p.dropLiveTextDeltas }

// SetLiveText connects the observer of ordered parsed evidence.
func (p *liveProjection) SetLiveText(observer liveTextObserver) { p.liveText = observer }

// TurnActive reports whether the projected turn is active.
func (p *liveProjection) TurnActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtime.TurnActive
}
