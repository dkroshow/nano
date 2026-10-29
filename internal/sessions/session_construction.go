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

		state: State{
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
	sess.wire.state = &sess.state
	sess.wire.runtime = &sess.runtime

	sess.wire.view = &sess.view
	sess.delivery.identity = &sess.identity
	sess.delivery.wire = &sess.wire
	sess.delivery.state = &sess.state
	sess.delivery.runtime = &sess.runtime
	sess.delivery.teardown = &sess.teardown
	sess.delivery.view = &sess.view
	sess.state.identity = &sess.identity
	sess.state.wire = &sess.wire
	sess.state.delivery = &sess.delivery
	sess.state.runtime = &sess.runtime

	sess.state.view = &sess.view
	sess.runtime.identity = &sess.identity
	sess.runtime.wire = &sess.wire
	sess.runtime.delivery = &sess.delivery
	sess.runtime.state = &sess.state

	sess.runtime.view = &sess.view
	sess.teardown.identity = &sess.identity
	sess.teardown.wire = &sess.wire
	sess.teardown.delivery = &sess.delivery
	sess.teardown.state = &sess.state
	sess.teardown.runtime = &sess.runtime

	sess.view.identity = &sess.identity
	sess.view.wire = &sess.wire

	sess.view.state = &sess.state
	sess.view.runtime = &sess.runtime

	return sess
}

// ReserveNativeID establishes the pre-publication native identity of a resume.
func (sess *Session) ReserveNativeID(nativeID string) {
	sess.state.threadID = nativeID
	sess.state.setProjectionThreadID(nativeID)
}

// InitializeBinding applies the canonical generation before provider activity.
func (sess *Session) InitializeBinding(generation int64) error {
	sess.state.attachmentGeneration = generation
	var err error
	sess.state.lifecycleState, err = lifecycle.New(uint64(generation))
	return err
}

// ConfigureSavedReaders connects the catalog reads before registration.
func (sess *Session) ConfigureSavedReaders(turn func(context.Context, string) (liveview.DurableTurnEvidence, bool), prompts SavedPromptReader, nativePrompts savedPromptNativeReader, final savedTurnFinalReader, sweep func(string)) {
	sess.state.savedTurn = turn
	sess.delivery.savedPrompts = prompts
	sess.delivery.savedPromptsIn = nativePrompts
	sess.delivery.savedTurnFinal = final
	sess.state.subscriptionSweep = sweep
}

// OpenWireSpool opens the attachment's ordered wire log.
func (wireState *Wire) OpenWireSpool(dir string) error {
	spool, err := newWireSpool(dir, wireState.identity.id)
	if err != nil {
		return err
	}
	wireState.spool = spool
	return nil
}

// ConfigureRegistration connects the progress and catalog owners before publication.
func (sess *Session) ConfigureRegistration(progress func(observability.RestartStep), failure func(string), background bool, sources AttachedCatalog) {
	sess.state.restartProgress = progress
	sess.state.restartFailure = failure
	sess.state.catalogBackground = background
	if sources != nil {
		sess.state.catalogSources = sources
	}
}

// DidRegister binds the catalog and hosted runtime after registry publication.
func (attachmentState *State) DidRegister() {
	attachmentState.bindCatalogSource(attachmentState.ThreadID())
	attachmentState.recordRestartStep(restartRegistered)
	if attachmentState.identity.agentExecutions != nil {
		// Presentation is mutable once this attachment is resolvable.
		attachmentState.stateMu.Lock()
		presentationMode, status, threadID := attachmentState.presentationMode, attachmentState.connectionStatusLocked(), attachmentState.threadID
		attachmentState.stateMu.Unlock()
		if err := attachmentState.identity.agentExecutions.BindSession(attachmentState.identity.hostedThreadID, attachmentState.identity.id, threadID, status, presentationMode); err != nil {
			log.Printf("bind hosted thread %s: %v", attachmentState.identity.hostedThreadID, err)
		}
	}
	attachmentState.subscriptionSweep(attachmentState.identity.hostedThreadID)
}

// WarmSkills begins the shared cwd read without waiting for a pane request.
func (attachmentState *State) WarmSkills() { attachmentState.view.skillsFor(attachmentState.cwd) }

// RecordCreationPosture records the provider values of the admitted creation.
func (attachmentState *State) RecordCreationPosture(req ProviderRequest) {
	attachmentState.recordPosture(creationPostureEntries(req), metadataFreshnessLive)
}

// DiscardSpawnResources releases an unpublished or refused attachment's resources.
func (sess *Session) DiscardSpawnResources() {
	sess.state.releaseCatalogSource()
	sess.wire.closeSpool()
}

// AttachLiveText connects the view and its ordered parsed-evidence observer.
func (sess *Session) AttachLiveText(view *LiveTextLoop) {
	sess.view.liveText = view
	sess.view.liveProjection().setLiveText(view)
}

// LiveTextService returns the view service for application-owned pane reads.
func (views *View) LiveTextService() *LiveTextLoop { return views.liveText }
