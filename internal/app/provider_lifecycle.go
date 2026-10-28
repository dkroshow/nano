package app

import (
	"context"
	"time"

	"github.com/dkroshow/nano/internal/lifecycle"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// Provider parsing stays beside each transport. These are evidence readers,
// not start/submit/interrupt methods; stage 5 owns those operations.
type providerLifecycleRules struct {
	capabilities   lifecycle.Capabilities
	turn           func(protocol.Direction, []byte) (bool, string, int64, bool)
	nudge          func(protocol.Direction, []byte) bool
	observe        func(*session, protocol.Direction, []byte, bool)
	provisional    func(string) bool
	runtime        func(*session, protocol.Direction, []byte, time.Time)
	accepted       func(*session)
	release        func(*session)
	ingressBlocked func(*session) bool
}

var lifecycleProviders map[string]providerLifecycleRules

func init() {
	lifecycleProviders = map[string]providerLifecycleRules{
		"codex":  {capabilities: (codexLifecycleAdapter{}).Capabilities(), turn: codexWorkEvidence, nudge: codexLifecycleNudge, observe: observeCodexWork, runtime: codexRuntimeEvidence, accepted: codexAcceptedEvidence, provisional: func(string) bool { return false }},
		"claude": {capabilities: (claudeLifecycleAdapter{}).Capabilities(), turn: claudeWorkEvidence, nudge: claudeLifecycleNudge, observe: observeClaudeWork, runtime: claudeRuntimeEvidence, release: claudeReleaseAcceptedEvidence, ingressBlocked: claudeEvidenceIngressBlocked, provisional: claudeNativeIDProvisional},
		"grok":   {capabilities: (grokLifecycleAdapter{}).Capabilities(), turn: grokWorkEvidence, nudge: grokLifecycleNudge, observe: observeGrokWork, runtime: grokRuntimeEvidence, provisional: func(string) bool { return false }},
	}
}

func providerLifecycleCapabilities(provider string) lifecycle.Capabilities {
	return lifecycleProviders[provider].capabilities
}

func turnActiveTransition(provider string, dir protocol.Direction, line []byte) (bool, string, int64, bool) {
	if decode := lifecycleProviders[provider].turn; decode != nil {
		return decode(dir, line)
	}
	return false, "", 0, false
}

func wireNudgesCatalog(provider string, dir protocol.Direction, line []byte) bool {
	if nudge := lifecycleProviders[provider].nudge; nudge != nil {
		return nudge(dir, line)
	}
	return false
}

func (sess *session) observeProviderWork(dir protocol.Direction, line []byte, live bool) {
	if observe := lifecycleProviders[sess.provider].observe; observe != nil {
		observe(sess, dir, line, live)
	}
}

// evidenceCoordinates is captured at the provider response's ordered ingress,
// before a later wire delta can arrive. A snapshot must keep that ordering when
// replay and identity reconciliation take longer than the provider response.
func (sess *session) evidenceCoordinates() (uint64, uint64) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.lifecycleState.Epoch, sess.nextEvidenceSequenceLocked()
}

func (sess *session) nextEvidenceSequenceLocked() uint64 {
	if sess.evidenceSequence < sess.lifecycleState.Sequence {
		sess.evidenceSequence = sess.lifecycleState.Sequence
	}
	sess.evidenceSequence++
	return sess.evidenceSequence
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
func (sess *session) awaitSavedCatchUp(transport <-chan struct{}, neverPrompted func() bool) bool {
	sess.recordRestartStep(restartAttached)
	sess.stateMu.Lock()
	release, fresh := sess.releaseHandshakeSlot, sess.savedCaughtUp
	sess.savedCaughtUp = true
	sess.stateMu.Unlock()
	if release != nil {
		release()
	}
	if sess.catalogSources == nil {
		return true
	}
	if !sess.catalogSources.AwaitAttachedCurrent(sess.id, fresh, neverPrompted, transport) {
		return false
	}
	for delay := time.Second; ; delay = min(2*delay, 30*time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), savedPromptSettleTimeout)
		err := sess.settleSavedPrompts(ctx)
		cancel()
		if err == nil {
			return true
		}
		select {
		case <-transport:
			return false
		case <-time.After(delay):
		}
		if sess.ended() {
			return false
		}
	}
}

// ended reports a closed or exited session.
func (sess *session) ended() bool {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.closed || sess.connectionStatusLocked() == sessionStatusExited
}

// settleProviderEvidence combines the response with any strictly newer ordered
// work delta. Stored anchors can date a validated turn, but never choose work.
func (sess *session) settleProviderEvidence(adapter lifecycle.Adapter, turnID string, startedAtMs int64) bool {
	observation, ok := adapter.Current()
	if !ok {
		return false
	}
	sess.stateMu.Lock()
	if observation.Epoch != sess.lifecycleState.Epoch || sess.closed ||
		sess.lifecycleState.Sequence > observation.Sequence || !sess.turnRestoreInProgress {
		sess.stateMu.Unlock()
		return false
	}
	if observation.Sequence >= sess.turnRestoreSequence {
		active := observation.Event == lifecycle.EventReadyWorking || observation.Event == lifecycle.EventReadyAccepted || observation.Event == lifecycle.EventReadyWaiting
		if turnID != "" && (turnID == sess.durablyTerminalTurnID || turnID == sess.lastProviderTerminalTurnID) {
			active = false
		}
		sess.turnRestoreActive = active
		sess.turnRestoreAccepted = active && observation.Event == lifecycle.EventReadyAccepted
		if active {
			sess.turnRestoreID, sess.turnRestoreStartedAtMs = turnID, startedAtMs
		} else {
			sess.turnRestoreID, sess.turnRestoreStartedAtMs = "", 0
		}
	}
	drain, status := sess.settleRestoredTurnStateLocked()
	sess.stateMu.Unlock()
	sess.publishRestoredTurnState(drain, status)
	return true
}
