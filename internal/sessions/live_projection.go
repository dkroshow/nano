package sessions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	interactmodel "github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const (
	liveProjectionMaxBytes       = 2 << 20
	liveProjectionTextCap        = 32 << 10
	liveProjectionOutputCap      = 64 << 10
	liveProjectionStateItemCap   = 1024
	liveProjectionItemPayloadCap = 96 << 10
)

const (
	projectionTurnUpsert        = liveview.TurnUpsertOp
	projectionInteraction       = liveview.InteractionOp
	projectionInteractionRemove = liveview.InteractionRemoveOp
	projectionMetaRefresh       = liveview.MetaRefreshOp
)

// liveTurnWork is one turn's compact completion facts: ids and times, no
// text.
type liveTurnWork struct {
	finalAnswer string // newest finished final_answer agent message
	lastAnswer  string // newest finished agent message
	atMs        int64  // newest live, non-replay work of the turn
	version     uint64 // bumped with every such work
}

type liveAdapter interface {
	apply(protocol.Direction, []byte, time.Time) model.Batch
}

// LiveProjection owns the ordered live turn, runtime and interaction view.
type LiveProjection struct {
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
	interactions     map[string]interactmodel.PendingInteraction
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
	liveText *LiveTextLoop
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

// NewLiveProjection constructs the ordered provider view for one attachment.
func NewLiveProjection(provider, hostedThreadID string, dropLiveTextDeltas bool) *LiveProjection {
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
	projection := &LiveProjection{
		provider: provider, epoch: epoch, adapter: adapter,
		turns: map[string]*liveProjectionTurn{}, turnWork: map[string]*liveTurnWork{},
		interactions: map[string]interactmodel.PendingInteraction{}, subs: map[chan liveProjectionUpdate]struct{}{},
		runtime: liveProjectionRuntime{ThreadStatus: "unknown"},
	}
	projection.hostedThreadID = hostedThreadID
	projection.dropLiveTextDeltas = dropLiveTextDeltas
	return projection
}

// setNativeThreadID sets the native identity represented by the view.
func (p *LiveProjection) setNativeThreadID(id string) {
	p.mu.Lock()
	p.nativeThread = id
	p.mu.Unlock()
}

// setClaudeProviderTurnID sets the provider identity of the current Claude turn.
func (p *LiveProjection) setClaudeProviderTurnID(id string) {
	if p == nil || p.provider != "claude" || id == "" {
		return
	}
	p.mu.Lock()
	p.claudeTurnID = id
	p.mu.Unlock()
}

// setClaudeFrameTurnID adopts one spooled frame's own turn identity, empty
// included: the shim tags only a Nano prompt's frames, so an unprompted
// turn's frames clear the previous prompt's id instead of inheriting it.
func (p *LiveProjection) setClaudeFrameTurnID(id string) {
	if p == nil || p.provider != "claude" {
		return
	}
	p.mu.Lock()
	p.claudeTurnID = id
	p.mu.Unlock()
}

// setClaudeResultTurnID names the turn of the result about to be applied,
// "" when it names none.
func (p *LiveProjection) setClaudeResultTurnID(id string) {
	if p == nil || p.provider != "claude" {
		return
	}
	p.mu.Lock()
	p.claudeResultTurnID = id
	p.mu.Unlock()
}

// claudeResultTurn is the turn the last applied Claude result named.
func (p *LiveProjection) claudeResultTurn() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claudeResultTurnID
}

// currentTurnID reads the current projected turn identity.
func (p *LiveProjection) currentTurnID() string {
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

// ensureClaudeActiveTurn gives provider execution evidence a normalized turn
// when no Nano-submitted prompt opened one. It reuses an existing in-progress
// turn, so streaming deltas are keep-alives rather than new turns.
func (p *LiveProjection) ensureClaudeActiveTurn(startedAtMs int64) string {
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

// applyWireLive keeps replay provenance separate from live browser emission.
// Only the Claude shim's re-attach spool sets replay; other non-live
// observations remain ordinary.
func (p *LiveProjection) applyWireLive(dir protocol.Direction, line []byte, at time.Time, live, replay bool, _ ...int64) {
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
				(live && dir == protocol.Recv && ClaudeExecutionEvidence(line))
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
	batch := p.adapter.apply(dir, line, at)
	if p.liveText != nil {
		p.liveText.observeBatch(batch, providerTurnID)
		if claudeTurnClosed {
			p.liveText.turnEnded(providerTurnID)
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
			turn.Status, turn.Error = ClaudeProjectionTerminal(line)
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
func (p *LiveProjection) recordTurnWorkLocked(batch model.Batch, providerTurnID string, at time.Time, live bool) {
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
func (p *LiveProjection) turnWorkLocked(turnID string) *liveTurnWork {
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

func (p *LiveProjection) latestTurnInProgressLocked() bool {
	if len(p.turnOrder) == 0 {
		return false
	}
	turn := p.turns[p.turnOrder[len(p.turnOrder)-1]]
	return turn != nil && turn.Status == "in_progress"
}

// trimTurnsLocked keeps the in-progress turns and the newest completed ones.
func (p *LiveProjection) trimTurnsLocked() {
	completed := make([]string, 0, len(p.turnOrder))
	for _, turnID := range p.turnOrder {
		if turn := p.turns[turnID]; turn != nil && CodexResumeTurnTerminal(turn.Status) {
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

func (p *LiveProjection) upsertTurnLocked(turn *liveProjectionTurn) {
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

// updateRuntime publishes the attachment’s runtime view.
func (p *LiveProjection) updateRuntime(runtime liveProjectionRuntime) {
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

// updateTokenUsage records current provider token usage.
func (p *LiveProjection) updateTokenUsage(raw json.RawMessage) {
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

// updateError records the latest live error.
func (p *LiveProjection) updateError(message string) {
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

// nudgeMetaRefresh publishes a view metadata refresh.
func (p *LiveProjection) nudgeMetaRefresh() {
	p.mu.Lock()
	p.appendLocked(liveProjectionUpdate{Op: projectionMetaRefresh})
	p.mu.Unlock()
}

// updateCompletedTurn records terminal evidence for a projected turn.
func (p *LiveProjection) updateCompletedTurn(turnID, status string, durationMS int64, completedAtMs int64, turnError ...json.RawMessage) {
	p.mu.Lock()
	p.updateCompletedTurnLocked(turnID, status, durationMS, completedAtMs, turnError...)
	p.mu.Unlock()
}

// completeTurnUnlessWorked records a saved turn end unless the turn has done
// live work since version was read. Live work records under mu, so the check
// and the end are one step.
func (p *LiveProjection) completeTurnUnlessWorked(turnID string, version uint64, status string, durationMS, completedAtMs int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if work := p.turnWork[turnID]; work != nil && work.version != version {
		return false
	}
	p.updateCompletedTurnLocked(turnID, status, durationMS, completedAtMs)
	return true
}

func (p *LiveProjection) updateCompletedTurnLocked(turnID, status string, durationMS int64, completedAtMs int64, turnError ...json.RawMessage) {
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

// latestTerminalTurnID reads the newest terminal turn identity.
func (p *LiveProjection) latestTerminalTurnID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for index := len(p.turnOrder) - 1; index >= 0; index-- {
		if turn := p.turns[p.turnOrder[index]]; turn != nil && threads.TurnTerminal(turn.Status) {
			return turn.ID
		}
	}
	return ""
}

// finalAgentItemID names a turn's final answer by source id, the identity
// records outside this projection use; saved history carries the same one.
func (p *LiveProjection) finalAgentItemID(turnID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.finalAnswerIDLocked(turnID)
}

// finalAnswerIDLocked is a turn's newest finished final_answer, else its
// newest finished answer. Callers hold mu.
func (p *LiveProjection) finalAnswerIDLocked(turnID string) string {
	if work := p.turnWork[turnID]; work != nil {
		return threads.FirstNonEmpty(work.finalAnswer, work.lastAnswer)
	}
	return ""
}

// turnList copies the projection's turns, oldest first.
func (p *LiveProjection) turnList() []liveProjectionTurn {
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

// upsertInteraction records a pending provider interaction.
func (p *LiveProjection) upsertInteraction(interaction interactmodel.PendingInteraction) {
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

// removeInteraction removes a resolved interaction from the view.
func (p *LiveProjection) removeInteraction(id string) {
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

func (p *LiveProjection) interactionsLocked() []interactmodel.PendingInteraction {
	interactions := make([]interactmodel.PendingInteraction, 0, len(p.interactions))
	for _, interaction := range p.interactions {
		interactions = append(interactions, liveview.CloneInteraction(interaction))
	}
	sort.Slice(interactions, func(i, j int) bool {
		return threads.TimestampBefore(interactions[i].CreatedAt, interactions[j].CreatedAt)
	})
	return interactions
}

// status is the thread's runtime and pending questions at the current
// revision, without rows: what a file-only pane's stream carries.
func (p *LiveProjection) status() liveTextStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return liveTextStatus{Epoch: p.epoch, Revision: p.revision, Runtime: liveview.CloneRuntime(p.runtime), Interactions: p.interactionsLocked()}
}

// retainedTurnEvidence is a retained turn, the time of its newest start, end
// or live work, and its work counter.
func (p *LiveProjection) retainedTurnEvidence(turnID string) (liveProjectionTurn, int64, uint64, bool) {
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

func (p *LiveProjection) runtimeTurnLocked() *liveProjectionTurn {
	return &liveProjectionTurn{
		ID: p.runtime.TurnID, Status: map[bool]string{true: "in_progress", false: "completed"}[p.runtime.TurnActive],
		StartedAtMs: p.runtime.TurnStartedAtMs,
	}
}

func (p *LiveProjection) appendLocked(update liveProjectionUpdate) {
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

// subscribe registers a subscriber for every later update. A caller reads
// the state it sends first only after subscribing, so no update falls
// between them.
func (p *LiveProjection) subscribe() (ch chan liveProjectionUpdate, cancel func()) {
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
		snapshot.Interactions = append([]interactmodel.PendingInteraction(nil), update.Snapshot.Interactions...)
		out.Snapshot = &snapshot
	}
	return out
}

// providerName reports the view’s provider.
func (p *LiveProjection) providerName() string {
	if p == nil {
		return ""
	}
	return p.provider
}

// dropsLiveTextDeltas reports whether live text delta publication is disabled.
func (p *LiveProjection) dropsLiveTextDeltas() bool { return p.dropLiveTextDeltas }

// SetLiveText connects the observer of ordered parsed evidence.
func (p *LiveProjection) setLiveText(observer *LiveTextLoop) { p.liveText = observer }

// turnActive reports whether the projected turn is active.
func (p *LiveProjection) turnActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtime.TurnActive
}
