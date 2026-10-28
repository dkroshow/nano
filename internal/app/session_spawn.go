package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/lifecycle"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// spawnSession registers a session shell and hands it back as soon as the
// request itself is known to be good — the provider handshake runs behind it.
//
// Validation stays synchronous, so a bad cwd or an already-attached thread is
// still the POST's own answer. The handshake is not: a slow app-server made the
// New Window dialog sit on "Creating…" for up to a minute
// (codexHandshakeTimeout), which the operator reads as a hung app. Both spawn
// kinds now follow the contract resume has had since Slice R3 — status
// "starting", the card visible immediately, failure surfaced through failSpawn
// on the session rather than as an HTTP error nobody is still looking at.
func (s *server) spawnSession(req spawnRequest) (*session, error) {
	if s.agentExecutions == nil || s.controlMetadata == nil || s.executionControlErr != "" || s.controlMetadataErr != "" {
		return nil, errHostedControlUnavailable
	}
	if req.Mode == "resume" {
		// Whichever path registers a still-starting thread (the boot loop, a
		// request, a manual resume) is tracked from here, lock wait included.
		if key := (attachmentKey{Provider: req.Provider, NativeThreadID: req.ThreadID}); req.restart == nil && s.boot.waiting(key) {
			req.restart = s.restart.thread(key)
			req.restart.at(restartPicked)
		}
		lock := s.resumeSpawnLock(req.Provider)
		lock.Lock()
		defer lock.Unlock()
		req.restart.at(restartLock)
	}
	req, err := s.resolveSpawn(req)
	if err != nil {
		return nil, err
	}
	if req.externalResume {
		record, err := s.agentExecutions.ensureExternalResumeSession(req)
		if err != nil {
			return nil, err
		}
		failRegistration := func(cause error) error {
			if updateErr := s.agentExecutions.updateSession(record.ID, "", "", sessionStatusExited, cause.Error()); updateErr != nil {
				return fmt.Errorf("%w (also failed to mark hosted thread %s retryable: %v)", cause, record.ID, updateErr)
			}
			return cause
		}
		req.HostedThreadID = record.ID
		// The read-only resolver ran before registration so preview could use the
		// same path. Re-apply the now-canonical metadata and rebuild the provider
		// resume shape so Claude's identity block, in particular, carries the new
		// hosted id rather than an empty pre-registration placeholder.
		if err := s.applyCanonicalResumeMetadata(&req); err != nil {
			return nil, failRegistration(err)
		}
		switch req.Provider {
		case "codex":
			req, err = s.resolveCodexResume(req)
		case "claude":
			req, err = s.resolveClaudeResume(req)
		case "grok":
			req, err = s.resolveGrokResume(req)
		}
		if err != nil {
			return nil, failRegistration(err)
		}
	}
	if req.Mode != "resume" {
		if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("cwd %q is not a directory", req.Cwd)
		}
	}
	// One nano session per thread, mapped to the frozen 409 by
	// handleSessionCreate. Resumes are serialized above, so the first one
	// registers and every later one finds its session here. This runs before
	// any hosted binding is written: Codex also claims a daemon slot, but only
	// after its shell registers, too late to keep a losing resume from
	// rebinding the hosted record. The shim enforces the same rule one layer
	// down for a second nano instance, where it can only surface as a plain
	// spawn error: there is no sibling session id in this process to send the
	// client to.
	if req.Mode == "resume" {
		if req.retiredClaudeHandle != "" {
			s.forgetAttachment("claude", req.retiredClaudeHandle)
		}
		if existing := s.liveSessionForResume(req.Provider, req.ThreadID, req.HostedThreadID); existing != nil {
			return nil, &threadAttachedError{ExistingSessionID: existing.id}
		}
	}
	hostedThreadID := req.HostedThreadID
	creationRequestName := ""
	if req.Mode == "start" {
		// Only the creation request may establish the immutable creation fact.
		// Resume inputs, including boot's staged rename, are never that request.
		creationRequestName = strings.TrimSpace(req.Name)
	}
	record, err := s.agentExecutions.ensureNamedSessionWithIdentity(hostedThreadID, req.Provider, req.PresentationMode, req.InvokerHostedThreadID, req.InvokerNativeThreadID, req.InvokerProvider, creationRequestName)
	if err != nil {
		return nil, err
	}
	hostedThreadID = record.ID
	req.SidebarPlacement = record.SidebarPlacement
	if req.InvokerHostedThreadID == "" {
		req.InvokerHostedThreadID = record.InvokerHostedThreadID
		req.InvokerNativeThreadID = record.InvokerNativeThreadID
		req.InvokerProvider = record.InvokerProvider
	}
	// Provider builders (claudeArgs, codexThreadParams) read the hosted identity
	// from the request; without this write-back the UI path hands them an empty
	// id and only agent-API spawns would carry identity.
	req.HostedThreadID = hostedThreadID
	if req.Mode == "resume" && s.controlMetadata != nil {
		if err := s.controlMetadata.persistMissingResumeCwd(hostedThreadID, req.Cwd); err != nil {
			return nil, fmt.Errorf("persist resume cwd: %w", err)
		}
	}
	creationName := s.creationNameForHosted(hostedThreadID)
	if creationName == "" {
		creationName = creationRequestName
	}
	pendingName := strings.TrimSpace(req.PendingName)
	displayName := threadvalues.FirstNonEmpty(pendingName, creationName)
	if req.Mode == "resume" {
		if durableName, ok := s.durableThreadName(req.Provider, req.ThreadID); ok {
			displayName = durableName
		}
	}
	nanoSessionID := newSessionID()
	sess := &session{
		id:                    nanoSessionID,
		provider:              req.Provider,
		hostedThreadID:        hostedThreadID,
		invokerHostedThreadID: req.InvokerHostedThreadID,
		invokerNativeThreadID: req.InvokerNativeThreadID,
		invokerProvider:       req.InvokerProvider,
		presentationMode:      req.PresentationMode,
		sidebarPlacement:      req.SidebarPlacement,
		cwd:                   req.Cwd,
		permissionMode:        threadvalues.FirstNonEmpty(req.PermissionMode, req.Approval),
		createdAt:             time.Now(),
		spawnMode:             req.Mode,
		subs:                  map[chan wireEvent]struct{}{},
		state:                 map[string]json.RawMessage{},
		reasoningSummary:      s.defaults.reasoningSummary,
		projection:            s.newSessionProjection(req.Provider, hostedThreadID),
		turnEffort:            req.Effort,
		pendingName:           pendingName,
		displayName:           displayName,
		nameThread:            s.assignSpawnName,
		publishCreationName:   s.publishCreationName,
		creationName: func() string {
			return s.creationNameForHosted(hostedThreadID)
		},
		metadataName:         s.durableThreadNameRef,
		skills:               s.skills,
		catalogRefresh:       s.catalogRefresh,
		runtimeNotify:        s.notifyRuntime,
		rememberAttachment:   s.rememberAttachment,
		forgetAttachment:     s.forgetAttachment,
		updateTurnAnchor:     s.setAttachmentTurnAnchor,
		updateShimSession:    s.setAttachmentShimSession,
		turns:                s.turns,
		agentExecutions:      s.agentExecutions,
		metadata:             s.controlMetadata,
		notices:              s.notices,
		pendingTurnCausation: []turnCausationEntry{},
		lastSettingsEvidence: map[string]turnCausationEntry{},
		sendOps:              map[string]*turnSendOperation{},
		nanoStopping:         &s.stopping,
		deliveryExpiries:     s.deliveryExpiries,
	}
	if s.sendAttempts != nil {
		sess.sendAttempts = s.sendAttempts
	}
	if s.queuedTurns != nil {
		sess.queuedTurns = s.queuedTurns
	}
	if s.completedTurns != nil && s.completedTurns.store != nil {
		sess.completedTurns = s.completedTurns.store.coreStore
	}
	// Reserve a resumed native identity before the shell becomes visible or any
	// asynchronous provider work begins. Claude and Grok do not have Codex's
	// daemon claim table, so sessionForThread is their duplicate-attachment
	// guard. A background handshake may confirm or succeed this identity later,
	// but it must never leave a window where a second Resume sees no owner.
	if req.Mode == "resume" && (req.Provider == "claude" || req.Provider == "grok") {
		sess.threadID = req.ThreadID
		sess.setProjectionThreadID(req.ThreadID)
	}
	if s.controlMetadata != nil {
		if err := s.controlMetadata.ensureHostedThread(hostedThreadID, req.Provider, req.PresentationMode, req.InvokerHostedThreadID, req.InvokerNativeThreadID, req.InvokerProvider); err != nil {
			return nil, fmt.Errorf("canonical hosted metadata: %w", err)
		}
		generation, err := s.controlMetadata.beginTransportBinding(hostedThreadID)
		if err != nil {
			return nil, fmt.Errorf("begin hosted transport binding: %w", err)
		}
		sess.attachmentGeneration = generation
		sess.lifecycleState, err = lifecycle.New(uint64(generation))
		if err != nil {
			return nil, err
		}
	}
	s.startLiveText(sess)
	// Runtime posture hydration lives at the provider seams —
	// seedCodexTurnOverrides and the Claude resume overlay — because what
	// "re-applying posture" means is provider mechanics.
	sess.savedTurn = func(ctx context.Context, turnID string) (durableTurnEvidence, bool) {
		return s.savedTurnEvidence(ctx, sess, turnID)
	}
	sess.savedPrompts = s.savedPromptsFor(sess)
	sess.savedPromptsIn = s.savedPromptRows
	sess.savedTurnFinal = s.savedTurnFinal
	sess.subscriptionSweep = s.sweepSubscriptions
	spool, err := newWireSpool(s.spoolDir, sess.id)
	if err != nil {
		return nil, fmt.Errorf("create wire spool: %w", err)
	}
	sess.spool = spool

	// Registered BEFORE any provider work: a close arriving while the handshake
	// is still in flight has to be able to reach this session, and the card has
	// to appear at once rather than when the provider answers. Bind: the session
	// is now visible to the catalog's runtime overlay.
	sess.restartProgress = req.restart.at
	sess.restartFailure = req.restart.fail
	sess.catalogBackground = req.catalogBackground
	if s.catalogSources != nil {
		sess.catalogSources = s.catalogSources
	}
	s.registerSession(sess)
	sess.bindCatalogSource(sess.currentThreadID())
	sess.recordRestartStep(restartRegistered)
	if sess.agentExecutions != nil {
		// presentationMode is mutable under stateMu once the session is
		// resolvable by the presentation endpoint, which it is as of the
		// registration above.
		sess.stateMu.Lock()
		presentationMode, status, threadID := sess.presentationMode, sess.connectionStatusLocked(), sess.threadID
		sess.stateMu.Unlock()
		if err := sess.agentExecutions.bindSession(
			sess.hostedThreadID, sess.id, threadID, status, presentationMode,
		); err != nil {
			log.Printf("bind hosted thread %s: %v", sess.hostedThreadID, err)
		}
	}
	// Asynchronous: a prompt notice that prepares this thread holds the
	// dispatcher while it waits for this registration.
	sess.subscriptionSweep(sess.hostedThreadID)
	if req.immediateRuntimePublish {
		s.publishRuntime(false)
	} else {
		s.notifyRuntime()
	}
	if req.Provider == "codex" {
		// Boot can register dozens of panes at once. Start the shared cwd refresh
		// now, without waiting for a browser to ask each session for /state.
		sess.skills.Current(req.Cwd)
	}

	if req.Mode != "resume" {
		sess.recordPosture(creationPostureEntries(req.providerRequest), metadataFreshnessLive)
	}
	if err := sess.beginProviderSpawn(req.providerRequest, s.codex, func() { s.discardSpawn(sess) }); err != nil {
		s.discardSpawn(sess)
		return nil, err
	}
	return sess, nil
}

// discardSpawn unwinds a session whose provider refused it outright. The shell
// was registered before the attempt (so a concurrent close could reach it), so
// registration has to be undone with it, and the spool file removed — nothing
// will ever read it.
func (s *server) discardSpawn(sess *session) {
	s.removeSession(sess.id)
	sess.releaseCatalogSource()
	if sess.spool != nil {
		if err := sess.spool.close(true); err != nil {
			log.Printf("close wire spool %s: %v", sess.id, err)
		}
	}
	s.notifyRuntime()
}
