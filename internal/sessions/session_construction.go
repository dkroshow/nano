package sessions

import (
	"context"
	"encoding/json"
	"log"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/wire"
)

// Config supplies one attachment's values and existing owner services.
// Runtime state, synchronization and provider handles are initialized by session.
type Config struct {
	ID                    string
	Provider              string
	HostedThreadID        string
	InvokerHostedThreadID string
	InvokerNativeThreadID string
	InvokerProvider       string
	PresentationMode      string
	SidebarPlacement      string
	CWD                   string
	PermissionMode        string
	CreatedAt             time.Time
	SpawnMode             string
	ReasoningSummary      string
	Projection            *LiveProjection
	TurnEffort            string
	PendingName           string
	DisplayName           string
	NameThread            func(provider, nativeThreadID, name string)
	PublishCreationName   func(provider, nativeThreadID, name string)
	CreationName          func() string
	MetadataName          func(provider, nativeThreadID string) (hostedstore.MetadataName, bool)
	Skills                *setup.SkillsListCache
	CatalogRefresh        func(string)
	RuntimeNotify         func()
	RememberAttachment    func(rec hostedstore.Attachment)
	ForgetAttachment      func(provider, nativeThreadID string)
	UpdateTurnAnchor      func(provider, nativeThreadID string, startedAtMs, anchorSeq int64)
	UpdateShimSession     func(provider, nativeThreadID, shimSessionID string)
	Turns                 *prompts.Store
	AgentExecutions       Executions
	Metadata              Metadata
	AppendUnread          func(string, string) string
	NanoStopping          *atomic.Bool
	DeliveryExpiries      *DeliveryExpiries
	ProcessStarted        time.Time
	SendAttempts          Journal
	QueuedTurns           Queue
	CompletedTurns        func(context.Context) (*coreturns.Store, error)
}

// New constructs an unstarted attachment from its identity and owner services.
func New(cfg Config) *Session {
	sess := &Session{

		stateOwner: State{
			presentationMode:     cfg.PresentationMode,
			sidebarPlacement:     cfg.SidebarPlacement,
			cwd:                  cfg.CWD,
			permissionMode:       cfg.PermissionMode,
			state:                map[string]json.RawMessage{},
			turnEffort:           cfg.TurnEffort,
			pendingName:          cfg.PendingName,
			displayName:          cfg.DisplayName,
			catalogRefresh:       cfg.CatalogRefresh,
			runtimeNotify:        cfg.RuntimeNotify,
			rememberAttachment:   cfg.RememberAttachment,
			forgetAttachment:     cfg.ForgetAttachment,
			updateTurnAnchor:     cfg.UpdateTurnAnchor,
			updateShimSession:    cfg.UpdateShimSession,
			pendingTurnCausation: []turnCausationEntry{},
			lastSettingsEvidence: map[string]turnCausationEntry{}},
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}},
		runtime: Runtime{
			reasoningSummary: cfg.ReasoningSummary,
			completedTurns:   cfg.CompletedTurns},
		view: View{
			projection:          cfg.Projection,
			nameThread:          cfg.NameThread,
			publishCreationName: cfg.PublishCreationName,
			creationName:        cfg.CreationName,
			metadataName:        cfg.MetadataName,
			skills:              cfg.Skills},
		delivery: Delivery{
			turns:            cfg.Turns,
			appendUnread:     cfg.AppendUnread,
			sendOps:          map[string]*turnSendOperation{},
			nanoStopping:     cfg.NanoStopping,
			deliveryExpiries: cfg.DeliveryExpiries,
			sendAttempts:     cfg.SendAttempts,
			queuedTurns:      cfg.QueuedTurns},
		identity: attachmentIdentity{id: cfg.ID, provider: cfg.Provider, hostedThreadID: cfg.HostedThreadID, invokerHostedThreadID: cfg.InvokerHostedThreadID, invokerNativeThreadID: cfg.InvokerNativeThreadID, invokerProvider: cfg.InvokerProvider, createdAt: cfg.CreatedAt, spawnMode: cfg.SpawnMode, agentExecutions: cfg.AgentExecutions, metadata: cfg.Metadata, processStarted: cfg.ProcessStarted},
	}
	sess.wire.identity = &sess.identity
	sess.wire.delivery = &sess.delivery
	sess.wire.stateOwner = &sess.stateOwner
	sess.wire.runtime = &sess.runtime
	sess.wire.lifecycle = &sess.lifecycle
	sess.wire.view = &sess.view
	sess.delivery.identity = &sess.identity
	sess.delivery.wire = &sess.wire
	sess.delivery.stateOwner = &sess.stateOwner
	sess.delivery.runtime = &sess.runtime
	sess.delivery.lifecycle = &sess.lifecycle
	sess.delivery.view = &sess.view
	sess.stateOwner.identity = &sess.identity
	sess.stateOwner.wire = &sess.wire
	sess.stateOwner.delivery = &sess.delivery
	sess.stateOwner.runtime = &sess.runtime
	sess.stateOwner.lifecycle = &sess.lifecycle
	sess.stateOwner.view = &sess.view
	sess.runtime.identity = &sess.identity
	sess.runtime.wire = &sess.wire
	sess.runtime.delivery = &sess.delivery
	sess.runtime.stateOwner = &sess.stateOwner
	sess.runtime.lifecycle = &sess.lifecycle
	sess.runtime.view = &sess.view
	sess.lifecycle.identity = &sess.identity
	sess.lifecycle.wire = &sess.wire
	sess.lifecycle.delivery = &sess.delivery
	sess.lifecycle.stateOwner = &sess.stateOwner
	sess.lifecycle.runtime = &sess.runtime
	sess.lifecycle.view = &sess.view
	sess.view.identity = &sess.identity
	sess.view.wire = &sess.wire
	sess.view.delivery = &sess.delivery
	sess.view.stateOwner = &sess.stateOwner
	sess.view.runtime = &sess.runtime
	sess.view.lifecycle = &sess.lifecycle
	return sess
}

// ReserveNativeID establishes the pre-publication native identity of a resume.
func (sess *Session) ReserveNativeID(nativeID string) {
	sess.stateOwner.threadID = nativeID
	sess.stateOwner.setProjectionThreadID(nativeID)
}

// InitializeBinding applies the canonical generation before provider activity.
func (sess *Session) InitializeBinding(generation int64) error {
	sess.stateOwner.attachmentGeneration = generation
	var err error
	sess.stateOwner.lifecycleState, err = lifecycle.New(uint64(generation))
	return err
}

// ConfigureSavedReaders connects the catalog reads before registration.
func (sess *Session) ConfigureSavedReaders(turn func(context.Context, string) (liveview.DurableTurnEvidence, bool), prompts SavedPromptReader, nativePrompts savedPromptNativeReader, final savedTurnFinalReader, sweep func(string)) {
	sess.stateOwner.savedTurn = turn
	sess.delivery.savedPrompts = prompts
	sess.delivery.savedPromptsIn = nativePrompts
	sess.delivery.savedTurnFinal = final
	sess.stateOwner.subscriptionSweep = sweep
}

// OpenWireSpool opens the attachment's ordered wire log.
func (sess *Wire) OpenWireSpool(dir string) error {
	spool, err := newWireSpool(dir, sess.identity.id)
	if err != nil {
		return err
	}
	sess.spool = spool
	return nil
}

// ConfigureRegistration connects the progress and catalog owners before publication.
func (sess *Session) ConfigureRegistration(progress func(observability.RestartStep), failure func(string), background bool, sources AttachedCatalog) {
	sess.stateOwner.restartProgress = progress
	sess.stateOwner.restartFailure = failure
	sess.stateOwner.catalogBackground = background
	if sources != nil {
		sess.stateOwner.catalogSources = sources
	}
}

// DidRegister binds the catalog and hosted runtime after registry publication.
func (sess *Session) DidRegister() {
	sess.stateOwner.bindCatalogSource(sess.stateOwner.ThreadID())
	sess.stateOwner.recordRestartStep(restartRegistered)
	if sess.identity.agentExecutions != nil {
		// Presentation is mutable once this attachment is resolvable.
		sess.stateOwner.stateMu.Lock()
		presentationMode, status, threadID := sess.stateOwner.presentationMode, sess.stateOwner.connectionStatusLocked(), sess.stateOwner.threadID
		sess.stateOwner.stateMu.Unlock()
		if err := sess.identity.agentExecutions.BindSession(sess.identity.hostedThreadID, sess.identity.id, threadID, status, presentationMode); err != nil {
			log.Printf("bind hosted thread %s: %v", sess.identity.hostedThreadID, err)
		}
	}
	sess.stateOwner.subscriptionSweep(sess.identity.hostedThreadID)
}

// WarmSkills begins the shared cwd read without waiting for a pane request.
func (sess *State) WarmSkills() { sess.view.skills.Current(sess.cwd) }

// RecordCreationPosture records the provider values of the admitted creation.
func (sess *State) RecordCreationPosture(req ProviderRequest) {
	sess.recordPosture(creationPostureEntries(req), metadataFreshnessLive)
}

// DiscardSpawnResources releases an unpublished or refused attachment's resources.
func (sess *Session) DiscardSpawnResources() {
	sess.stateOwner.releaseCatalogSource()
	if sess.wire.spool != nil {
		if err := sess.wire.spool.close(true); err != nil {
			log.Printf("close wire spool %s: %v", sess.identity.id, err)
		}
	}
}

// AttachLiveText connects the view and its ordered parsed-evidence observer.
func (sess *Session) AttachLiveText(view *LiveTextLoop) {
	sess.view.liveText = view
	sess.view.projection.setLiveText(view)
}

// LiveTextService returns the view service for application-owned pane reads.
func (sess *View) LiveTextService() *LiveTextLoop { return sess.liveText }
