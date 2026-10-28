package app

import (
	"context"
	"encoding/json"
	"log"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/setup"
)

// sessionConfig supplies one attachment's values and existing owner services.
// Runtime state, synchronization and provider handles are initialized by session.
type sessionConfig struct {
	id                    string
	provider              string
	hostedThreadID        string
	invokerHostedThreadID string
	invokerNativeThreadID string
	invokerProvider       string
	presentationMode      string
	sidebarPlacement      string
	cwd                   string
	permissionMode        string
	createdAt             time.Time
	spawnMode             string
	reasoningSummary      string
	projection            sessionProjection
	turnEffort            string
	pendingName           string
	displayName           string
	nameThread            func(provider, nativeThreadID, name string)
	publishCreationName   func(provider, nativeThreadID, name string)
	creationName          func() string
	metadataName          func(provider, nativeThreadID string) (hostedMetadataName, bool)
	skills                *setup.SkillsListCache
	catalogRefresh        func(string)
	runtimeNotify         func()
	rememberAttachment    func(rec attachmentRecord)
	forgetAttachment      func(provider, nativeThreadID string)
	updateTurnAnchor      func(provider, nativeThreadID string, startedAtMs, anchorSeq int64)
	updateShimSession     func(provider, nativeThreadID, shimSessionID string)
	turns                 *prompts.Store
	agentExecutions       sessionExecutions
	metadata              sessionMetadata
	appendUnread          func(string, string) string
	nanoStopping          *atomic.Bool
	deliveryExpiries      *deliveryExpiries
	processStarted        time.Time
	sendAttempts          sessionJournal
	queuedTurns           sessionQueue
	completedTurns        func(context.Context) (*coreturns.Store, error)
}

func newSession(cfg sessionConfig) *session {
	return &session{
		id:                    cfg.id,
		provider:              cfg.provider,
		hostedThreadID:        cfg.hostedThreadID,
		invokerHostedThreadID: cfg.invokerHostedThreadID,
		invokerNativeThreadID: cfg.invokerNativeThreadID,
		invokerProvider:       cfg.invokerProvider,
		presentationMode:      cfg.presentationMode,
		sidebarPlacement:      cfg.sidebarPlacement,
		cwd:                   cfg.cwd,
		permissionMode:        cfg.permissionMode,
		createdAt:             cfg.createdAt,
		spawnMode:             cfg.spawnMode,
		subs:                  map[chan wireEvent]struct{}{},
		state:                 map[string]json.RawMessage{},
		reasoningSummary:      cfg.reasoningSummary,
		projection:            cfg.projection,
		turnEffort:            cfg.turnEffort,
		pendingName:           cfg.pendingName,
		displayName:           cfg.displayName,
		nameThread:            cfg.nameThread,
		publishCreationName:   cfg.publishCreationName,
		creationName:          cfg.creationName,
		metadataName:          cfg.metadataName,
		skills:                cfg.skills,
		catalogRefresh:        cfg.catalogRefresh,
		runtimeNotify:         cfg.runtimeNotify,
		rememberAttachment:    cfg.rememberAttachment,
		forgetAttachment:      cfg.forgetAttachment,
		updateTurnAnchor:      cfg.updateTurnAnchor,
		updateShimSession:     cfg.updateShimSession,
		turns:                 cfg.turns,
		agentExecutions:       cfg.agentExecutions,
		metadata:              cfg.metadata,
		appendUnread:          cfg.appendUnread,
		pendingTurnCausation:  []turnCausationEntry{},
		lastSettingsEvidence:  map[string]turnCausationEntry{},
		sendOps:               map[string]*turnSendOperation{},
		nanoStopping:          cfg.nanoStopping,
		deliveryExpiries:      cfg.deliveryExpiries,
		processStarted:        cfg.processStarted,
		sendAttempts:          cfg.sendAttempts,
		queuedTurns:           cfg.queuedTurns,
		completedTurns:        cfg.completedTurns,
	}
}

// reserveNativeID establishes the pre-publication native identity of a resume.
func (sess *session) reserveNativeID(nativeID string) {
	sess.threadID = nativeID
	sess.setProjectionThreadID(nativeID)
}

// initializeBinding applies the canonical generation before provider activity.
func (sess *session) initializeBinding(generation int64) error {
	sess.attachmentGeneration = generation
	var err error
	sess.lifecycleState, err = lifecycle.New(uint64(generation))
	return err
}

// configureSavedReaders connects the catalog reads before registration.
func (sess *session) configureSavedReaders(turn func(context.Context, string) (durableTurnEvidence, bool), prompts savedPromptReader, nativePrompts savedPromptNativeReader, final savedTurnFinalReader, sweep func(string)) {
	sess.savedTurn = turn
	sess.savedPrompts = prompts
	sess.savedPromptsIn = nativePrompts
	sess.savedTurnFinal = final
	sess.subscriptionSweep = sweep
}

// openWireSpool opens the attachment's ordered wire log.
func (sess *session) openWireSpool(dir string) error {
	spool, err := newWireSpool(dir, sess.id)
	if err != nil {
		return err
	}
	sess.spool = spool
	return nil
}

// configureRegistration connects the progress and catalog owners before publication.
func (sess *session) configureRegistration(progress func(observability.RestartStep), failure func(string), background bool, sources attachedSourceCatalog) {
	sess.restartProgress = progress
	sess.restartFailure = failure
	sess.catalogBackground = background
	if sources != nil {
		sess.catalogSources = sources
	}
}

// didRegister binds the catalog and hosted runtime after registry publication.
func (sess *session) didRegister() {
	sess.bindCatalogSource(sess.ThreadID())
	sess.recordRestartStep(restartRegistered)
	if sess.agentExecutions != nil {
		// Presentation is mutable once this attachment is resolvable.
		sess.stateMu.Lock()
		presentationMode, status, threadID := sess.presentationMode, sess.connectionStatusLocked(), sess.threadID
		sess.stateMu.Unlock()
		if err := sess.agentExecutions.BindSession(sess.hostedThreadID, sess.id, threadID, status, presentationMode); err != nil {
			log.Printf("bind hosted thread %s: %v", sess.hostedThreadID, err)
		}
	}
	sess.subscriptionSweep(sess.hostedThreadID)
}

// warmSkills begins the shared cwd read without waiting for a pane request.
func (sess *session) warmSkills() { sess.skills.Current(sess.cwd) }

// recordCreationPosture records the provider values of the admitted creation.
func (sess *session) recordCreationPosture(req providerRequest) {
	sess.recordPosture(creationPostureEntries(req), metadataFreshnessLive)
}

// discardSpawnResources releases an unpublished or refused attachment's resources.
func (sess *session) discardSpawnResources() {
	sess.releaseCatalogSource()
	if sess.spool != nil {
		if err := sess.spool.close(true); err != nil {
			log.Printf("close wire spool %s: %v", sess.id, err)
		}
	}
}

// attachLiveText connects the view and its ordered parsed-evidence observer.
func (sess *session) attachLiveText(view sessionLiveText) {
	sess.liveText = view
	sess.projection.SetLiveText(view)
}

// liveTextService returns the view service for application-owned pane reads.
func (sess *session) liveTextService() sessionLiveText { return sess.liveText }
