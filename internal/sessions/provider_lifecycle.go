package sessions

import (
	"context"
	"time"

	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/wire"
)

// Provider parsing stays beside each transport. These are evidence readers,
// not start/submit/interrupt methods; stage 5 owns those operations.
type providerLifecycleRules struct {
	capabilities   lifecycle.Capabilities
	turn           func(wire.Direction, []byte) (bool, string, int64, bool)
	nudge          func(wire.Direction, []byte) bool
	observe        func(*State, wire.Direction, []byte, bool)
	runtime        func(*State, wire.Direction, []byte, time.Time)
	accepted       func(*State)
	release        func(*State)
	ingressBlocked func(*State) bool
}

var lifecycleProviders map[string]providerLifecycleRules

func init() {
	lifecycleProviders = map[string]providerLifecycleRules{
		"codex":  {capabilities: (codexLifecycleAdapter{}).Capabilities(), turn: codexWorkEvidence, nudge: codexLifecycleNudge, observe: observeCodexWork, runtime: codexRuntimeEvidence, accepted: codexAcceptedEvidence},
		"claude": {capabilities: (claudeLifecycleAdapter{}).Capabilities(), turn: claudeWorkEvidence, nudge: claudeLifecycleNudge, observe: observeClaudeWork, runtime: claudeRuntimeEvidence, release: claudeReleaseAcceptedEvidence, ingressBlocked: claudeEvidenceIngressBlocked},
		"grok":   {capabilities: (grokLifecycleAdapter{}).Capabilities(), turn: grokWorkEvidence, nudge: grokLifecycleNudge, observe: observeGrokWork, runtime: grokRuntimeEvidence},
	}
}

// ProviderLifecycleCapabilities returns the lifecycle guarantees of a provider.
func ProviderLifecycleCapabilities(provider string) lifecycle.Capabilities {
	return lifecycleProviders[provider].capabilities
}

func turnActiveTransition(provider string, dir wire.Direction, line []byte) (bool, string, int64, bool) {
	if decode := lifecycleProviders[provider].turn; decode != nil {
		return decode(dir, line)
	}
	return false, "", 0, false
}

func wireNudgesCatalog(provider string, dir wire.Direction, line []byte) bool {
	if nudge := lifecycleProviders[provider].nudge; nudge != nil {
		return nudge(dir, line)
	}
	return false
}

func (attachmentState *State) observeProviderWork(dir wire.Direction, line []byte, live bool) {
	if observe := lifecycleProviders[attachmentState.identity.provider].observe; observe != nil {
		observe(attachmentState, dir, line, live)
	}
}

// evidenceCoordinates is captured at the provider response's ordered ingress,
// before a later wire delta can arrive. A snapshot must keep that ordering when
// replay and identity reconciliation take longer than the provider response.
func (attachmentState *State) evidenceCoordinates() (uint64, uint64) {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return attachmentState.lifecycleState.Epoch, attachmentState.nextEvidenceSequenceLocked()
}

func (attachmentState *State) nextEvidenceSequenceLocked() uint64 {
	if attachmentState.evidenceSequence < attachmentState.lifecycleState.Sequence {
		attachmentState.evidenceSequence = attachmentState.lifecycleState.Sequence
	}
	attachmentState.evidenceSequence++
	return attachmentState.evidenceSequence
}

// awaitSavedCatchUp holds a resumed attachment's readiness until the catalog
// has proved its saved files current (catalog_attached.go) and a settlement
// pass has read the prompts they prove. Lines the provider wrote while Nano
// could not watch are indexed before any delivery, settlement or resend reads
// them, and a failed saved read is retried, never skipped. A session's first
// attach uses the catch-up its hold took at registration; a later one (a
// reconnect) needs a fresh pass. The provider is attached by now, so the
// restart record marks that and any boot handshake slot goes back before the
// wait. It reports false, and the caller publishes no readiness, when
// transport closes or the session ends first. neverPrompted, when set, reads
// the evidence that the thread was never prompted (catalog_attached.go).
func (attachmentState *State) awaitSavedCatchUp(transport <-chan struct{}, neverPrompted func() bool) bool {
	attachmentState.recordRestartStep(restartAttached)
	attachmentState.stateMu.Lock()
	release, fresh := attachmentState.releaseHandshakeSlot, attachmentState.savedCaughtUp
	attachmentState.savedCaughtUp = true
	attachmentState.stateMu.Unlock()
	if release != nil {
		release()
	}
	if attachmentState.catalogSources == nil {
		return true
	}
	if !attachmentState.catalogSources.AwaitAttachedCurrent(attachmentState.identity.id, fresh, neverPrompted, transport) {
		return false
	}
	for delay := time.Second; ; delay = min(2*delay, 30*time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), savedPromptSettleTimeout)
		err := attachmentState.delivery.settleSavedPrompts(ctx)
		cancel()
		if err == nil {
			return true
		}
		select {
		case <-transport:
			return false
		case <-time.After(delay):
		}
		if attachmentState.ended() {
			return false
		}
	}
}

// ended reports a closed or exited session.
func (attachmentState *State) ended() bool {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return attachmentState.closed || attachmentState.connectionStatusLocked() == StatusExited
}

// settleProviderEvidence combines the response with any strictly newer ordered
// work delta. Stored anchors can date a validated turn, but never choose work.
func (attachmentState *State) settleProviderEvidence(adapter lifecycle.Adapter, turnID string, startedAtMs int64) bool {
	observation, ok := adapter.Current()
	if !ok {
		return false
	}
	attachmentState.stateMu.Lock()
	if observation.Epoch != attachmentState.lifecycleState.Epoch || attachmentState.closed ||
		attachmentState.lifecycleState.Sequence > observation.Sequence || !attachmentState.turnRestoreInProgress {
		attachmentState.stateMu.Unlock()
		return false
	}
	if observation.Sequence >= attachmentState.turnRestoreSequence {
		active := observation.Event == lifecycle.EventReadyWorking || observation.Event == lifecycle.EventReadyAccepted || observation.Event == lifecycle.EventReadyWaiting
		if turnID != "" && (turnID == attachmentState.durablyTerminalTurnID || turnID == attachmentState.lastProviderTerminalTurnID) {
			active = false
		}
		attachmentState.turnRestoreActive = active
		attachmentState.turnRestoreAccepted = active && observation.Event == lifecycle.EventReadyAccepted
		if active {
			attachmentState.turnRestoreID, attachmentState.turnRestoreStartedAtMs = turnID, startedAtMs
		} else {
			attachmentState.turnRestoreID, attachmentState.turnRestoreStartedAtMs = "", 0
		}
	}
	drain, status := attachmentState.settleRestoredTurnStateLocked()
	attachmentState.stateMu.Unlock()
	attachmentState.publishRestoredTurnState(drain, status)
	return true
}
