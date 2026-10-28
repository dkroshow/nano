package sessions

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/protocol"
)

// deliveryTestProjection records the view writes made by delivery tests.
// Unused projection operations remain unimplemented and fail if called.
type deliveryTestProjection struct {
	Projection
	mu                                  sync.Mutex
	provider                            string
	runtime                             liveProjectionRuntime
	diagnostics                         []threadDiagnostic
	frameTurn, providerTurn, resultTurn string
	terminalTurn                        string
	interactionWrites                   []pendingInteraction
	interactionRemovals                 []string
}

func newDeliveryTestProjection(provider string) *deliveryTestProjection {
	return &deliveryTestProjection{provider: provider}
}
func (p *deliveryTestProjection) ProviderName() string     { return p.provider }
func (p *deliveryTestProjection) SetNativeThreadID(string) {}
func (p *deliveryTestProjection) UpdateRuntime(value liveProjectionRuntime) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runtime = value
}
func (p *deliveryTestProjection) Status() liveTextStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return liveTextStatus{Runtime: p.runtime}
}
func (p *deliveryTestProjection) NudgeMetaRefresh() {}
func (p *deliveryTestProjection) RecordDiagnostic(value threadDiagnostic, _ bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.diagnostics = append(p.diagnostics, value)
}
func (p *deliveryTestProjection) ThreadDiagnosticsSnapshot() []threadDiagnostic {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]threadDiagnostic(nil), p.diagnostics...)
}
func (p *deliveryTestProjection) UpdateError(value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runtime.LastError = value
}
func (p *deliveryTestProjection) UpdateTokenUsage(value json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runtime.TokenUsage = append(json.RawMessage(nil), value...)
}
func (p *deliveryTestProjection) ApplyWireLive(protocol.Direction, []byte, time.Time, bool, bool, ...int64) {
}

// blockedProjection isolates a slow projection consumer from wire ingestion.
// Its lock gates the same explicit view writes as the former real-view fixture.
type blockedProjection struct {
	*deliveryTestProjection
	mu sync.Mutex
}

func newBlockedProjection(provider string) *blockedProjection {
	return &blockedProjection{deliveryTestProjection: newDeliveryTestProjection(provider)}
}
func (p *blockedProjection) ApplyWireLive(protocol.Direction, []byte, time.Time, bool, bool, ...int64) {
	p.mu.Lock()
	p.mu.Unlock()
}
func (p *blockedProjection) UpdateRuntime(value liveProjectionRuntime) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deliveryTestProjection.UpdateRuntime(value)
}

func (p *deliveryTestProjection) SetClaudeFrameTurnID(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frameTurn = id
}
func (p *deliveryTestProjection) SetClaudeProviderTurnID(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.providerTurn = id
}
func (p *deliveryTestProjection) SetClaudeResultTurnID(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resultTurn = id
}
func (p *deliveryTestProjection) ClaudeResultTurn() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resultTurn
}
func (p *deliveryTestProjection) CurrentTurnID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.providerTurn
}
func (p *deliveryTestProjection) EnsureClaudeActiveTurn(int64) string { return p.CurrentTurnID() }
func (p *deliveryTestProjection) FinalAgentItemID(string) string      { return "" }
func (p *deliveryTestProjection) LatestTerminalTurnID() string        { return "" }

func (p *deliveryTestProjection) SeedResumeTurns(string, []*liveProjectionTurn) {}
func (p *deliveryTestProjection) RetainedTurnEvidence(string) (liveProjectionTurn, int64, uint64, bool) {
	return liveProjectionTurn{}, 0, 0, false
}
func (p *deliveryTestProjection) TurnList() []liveProjectionTurn { return nil }

func (p *deliveryTestProjection) UpdateCompletedTurn(id, _ string, _, _ int64, _ ...json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminalTurn = id
}

func (p *deliveryTestProjection) UpsertInteraction(value pendingInteraction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.interactionWrites = append(p.interactionWrites, value)
}

func (p *deliveryTestProjection) RemoveInteraction(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.interactionRemovals = append(p.interactionRemovals, id)
}

func (p *deliveryTestProjection) DropsLiveTextDeltas() bool { return false }
