package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/sessions"
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
func (s *server) spawnSession(req spawnRequest) (*sessions.Session, error) {
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
			if updateErr := s.agentExecutions.UpdateSession(record.ID, "", "", sessions.StatusExited, cause.Error()); updateErr != nil {
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
		if existing := s.LiveSessionForResume(req.Provider, req.ThreadID, req.HostedThreadID); existing != nil {
			return nil, &sessions.ThreadAttachedError{ExistingSessionID: existing.ID()}
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
	nanoSessionID := sessions.NewID()
	var journal sessions.Journal
	if s.sendAttempts != nil {
		journal = s.sendAttempts
	}
	var queue sessions.Queue
	if s.queuedTurns != nil {
		queue = s.queuedTurns
	}
	var completed func(context.Context) (*coreturns.Store, error)
	if s.completedTurns != nil && s.completedTurns.store != nil {
		completed = s.completedTurns.store.coreStore
	}
	sess := sessions.New(sessions.Config{
		ID:                    nanoSessionID,
		Provider:              req.Provider,
		HostedThreadID:        hostedThreadID,
		InvokerHostedThreadID: req.InvokerHostedThreadID,
		InvokerNativeThreadID: req.InvokerNativeThreadID,
		InvokerProvider:       req.InvokerProvider,
		PresentationMode:      req.PresentationMode,
		SidebarPlacement:      req.SidebarPlacement,
		CWD:                   req.Cwd,
		PermissionMode:        threadvalues.FirstNonEmpty(req.PermissionMode, req.Approval),
		CreatedAt:             time.Now(),
		SpawnMode:             req.Mode,
		ReasoningSummary:      s.defaults.reasoningSummary,
		Projection:            s.newSessionProjection(req.Provider, hostedThreadID),
		TurnEffort:            req.Effort,
		PendingName:           pendingName,
		DisplayName:           displayName,
		NameThread:            s.assignSpawnName,
		PublishCreationName:   s.publishCreationName,
		CreationName: func() string {
			return s.creationNameForHosted(hostedThreadID)
		},
		MetadataName:       s.durableThreadNameRef,
		Skills:             s.skills,
		CatalogRefresh:     s.catalogRefresh,
		RuntimeNotify:      s.notifyRuntime,
		RememberAttachment: s.rememberAttachment,
		ForgetAttachment:   s.forgetAttachment,
		UpdateTurnAnchor:   s.setAttachmentTurnAnchor,
		UpdateShimSession:  s.setAttachmentShimSession,
		Turns:              s.turns,
		AgentExecutions:    s.agentExecutions,
		Metadata:           s.controlMetadata,
		AppendUnread:       s.notices.Append,
		NanoStopping:       &s.stopping,
		DeliveryExpiries:   s.deliveryExpiries,
		ProcessStarted:     processStarted,
		SendAttempts:       journal,
		QueuedTurns:        queue,
		CompletedTurns:     completed,
	})

	// Reserve a resumed native identity before the shell becomes visible or any
	// asynchronous provider work begins. Claude and Grok do not have Codex's
	// daemon claim table, so sessionForThread is their duplicate-attachment
	// guard. A background handshake may confirm or succeed this identity later,
	// but it must never leave a window where a second Resume sees no owner.
	if req.Mode == "resume" && (req.Provider == "claude" || req.Provider == "grok") {
		sess.ReserveNativeID(req.ThreadID)
	}
	if s.controlMetadata != nil {
		if err := s.controlMetadata.ensureHostedThread(hostedThreadID, req.Provider, req.PresentationMode, req.InvokerHostedThreadID, req.InvokerNativeThreadID, req.InvokerProvider); err != nil {
			return nil, fmt.Errorf("canonical hosted metadata: %w", err)
		}
		generation, err := s.controlMetadata.beginTransportBinding(hostedThreadID)
		if err != nil {
			return nil, fmt.Errorf("begin hosted transport binding: %w", err)
		}
		if err := sess.InitializeBinding(generation); err != nil {
			return nil, err
		}
	}
	s.startLiveText(sess)
	// Runtime posture hydration lives at the provider seams —
	// seedCodexTurnOverrides and the Claude resume overlay — because what
	// "re-applying posture" means is provider mechanics.
	sess.ConfigureSavedReaders(func(ctx context.Context, turnID string) (durableTurnEvidence, bool) {
		return s.savedTurnEvidence(ctx, sess, turnID)
	}, s.savedPromptsFor(sess), s.savedPromptRows, s.savedTurnFinal, s.sweepSubscriptions)
	if err := sess.OpenWireSpool(s.spoolDir); err != nil {
		return nil, fmt.Errorf("create wire spool: %w", err)
	}

	// Registered BEFORE any provider work: a close arriving while the handshake
	// is still in flight has to be able to reach this session, and the card has
	// to appear at once rather than when the provider answers. Bind: the session
	// is now visible to the catalog's runtime overlay.
	sess.ConfigureRegistration(req.restart.at, req.restart.fail, req.catalogBackground, s.catalogSources)
	s.RegisterSession(sess)
	sess.DidRegister()

	if req.immediateRuntimePublish {
		s.publishRuntime(false)
	} else {
		s.notifyRuntime()
	}
	if req.Provider == "codex" {
		// Boot can register dozens of panes at once. Start the shared cwd refresh
		// now, without waiting for a browser to ask each session for /state.
		sess.WarmSkills()
	}

	if req.Mode != "resume" {
		sess.RecordCreationPosture(req.ProviderRequest)
	}
	if err := sess.BeginProviderSpawn(req.ProviderRequest, s.codex, func() { s.discardSpawn(sess) }); err != nil {
		s.discardSpawn(sess)
		return nil, err
	}
	return sess, nil
}

// discardSpawn unwinds a session whose provider refused it outright. The shell
// was registered before the attempt (so a concurrent close could reach it), so
// registration has to be undone with it, and the spool file removed — nothing
// will ever read it.
func (s *server) discardSpawn(sess *sessions.Session) {
	s.RemoveSession(sess.ID())
	sess.DiscardSpawnResources()
	s.notifyRuntime()
}
