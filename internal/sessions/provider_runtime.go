package sessions

import (
	"sync"

	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/providers"
	providerclaude "github.com/dkroshow/nano/internal/providers/claude"
	providercodex "github.com/dkroshow/nano/internal/providers/codex"
	providergrok "github.com/dkroshow/nano/internal/providers/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

type providerAdapterFactory func(providers.Operations) (providers.Adapter, error)
type providerOperationFactory func(*Runtime) providers.Operations

var providerAdapterFactories = map[model.Provider]providerAdapterFactory{
	model.ProviderCodex: func(operations providers.Operations) (providers.Adapter, error) { return providercodex.New(operations) },
	model.ProviderClaude: func(operations providers.Operations) (providers.Adapter, error) {
		return providerclaude.New(operations)
	},
	model.ProviderGrok: func(operations providers.Operations) (providers.Adapter, error) { return providergrok.New(operations) },
}

func (providerRuntime *Runtime) ensureDeliveryCoordinator() error {
	return providerRuntime.ensureCoreSession()
}

// requireTransactionalTurnsLocked enforces the boot invariant at runtime
// boundaries too. Store views are wired once before provider attachment; this
// check must never mutate them while restore and queue drains run concurrently.
func (delivery *Delivery) requireTransactionalTurnsLocked() error {
	if delivery.turns == nil {
		return errors.New("transactional turns store is unavailable")
	}
	if delivery.queuedTurns != nil && delivery.queuedTurns.TransactionStore() != delivery.turns {
		return errors.New("transactional turn queue view is unavailable")
	}
	if delivery.sendAttempts != nil && delivery.sendAttempts.TransactionStore() != delivery.turns {
		return errors.New("transactional turn journal view is unavailable")
	}
	return nil
}

func (providerRuntime *Runtime) ensureProviderCoordinator() error {
	return providerRuntime.ensureCoreSession()
}

func (providerRuntime *Runtime) ensureCoreSession() error {
	if providerRuntime == nil {
		return errors.New("session is unavailable")
	}
	providerRuntime.coreOnce.Do(func() {
		if providerRuntime.providerAdapter == nil {
			provider := model.Provider(providerRuntime.identity.provider)
			adapterFactory, ok := providerAdapterFactories[provider]
			if !ok {
				providerRuntime.coreErr = fmt.Errorf("provider %q has no adapter", providerRuntime.identity.provider)
				return
			}
			operationFactory, ok := providerOperationFactories()[provider]
			if !ok {
				providerRuntime.coreErr = fmt.Errorf("provider %q has no runtime operations", providerRuntime.identity.provider)
				return
			}
			providerRuntime.providerAdapter, providerRuntime.coreErr = adapterFactory(operationFactory(providerRuntime))
			if providerRuntime.coreErr != nil {
				return
			}
		}
		var canonicalDB = func() *sql.DB {
			if providerRuntime.delivery.queuedPromptStore() != nil && providerRuntime.delivery.queuedPromptStore().Database() != nil {
				return providerRuntime.delivery.queuedPromptStore().Database()
			}
			if providerRuntime.identity.metadata != nil {
				return providerRuntime.identity.metadata.CanonicalDatabase()
			}
			return nil
		}()
		if err := providerRuntime.delivery.initializePromptStore(canonicalDB); err != nil {
			providerRuntime.coreErr = err
			return
		}
		initial := providerRuntime.state.lifecycleSnapshot()
		if providerRuntime.completedTurns == nil && canonicalDB != nil {
			// Root compatibility fixtures and older construction sites already
			// carry the canonical threads handle through a queue or metadata view.
			// Compose the turn store from that handle; it never owns another file.
			store, err := coreturns.OpenStore(context.Background(), canonicalDB)
			if err != nil {
				providerRuntime.coreErr = err
				return
			}
			providerRuntime.completedTurns = func(context.Context) (*coreturns.Store, error) { return store, nil }
		}
		if providerRuntime.delivery.PromptStore() == nil || providerRuntime.completedTurns == nil {
			providerRuntime.coreErr = errors.New("core prompt or turn store is unavailable")
			return
		}
		turnStore, err := providerRuntime.completedTurns(context.Background())
		if err != nil {
			providerRuntime.coreErr = err
			return
		}
		core, err := coresession.New(coresession.Config{
			HostedThreadID: providerRuntime.identity.hostedThreadID, Wire: providerRuntime.providerAdapter, Prompts: providerRuntime.delivery.PromptStore(), Turns: finishedTurnNotifier{Store: turnStore, session: providerRuntime.delivery},
			Outbox: coreOutboxPublisher{session: providerRuntime.delivery}, Lifecycle: initial, ExternalDrain: true,
			AsyncError: providerRuntime.recordCoreAsyncError,
		})
		if err != nil {
			providerRuntime.coreErr = err
			return
		}
		providerRuntime.coreMu.Lock()
		providerRuntime.core = core
		providerRuntime.coreMu.Unlock()
	})
	return providerRuntime.coreErr
}

// finishedTurnNotifier tells completion subscribers about a finished turn
// only after the core session has saved it, so their read of the
// completed-turn feed finds it.
type finishedTurnNotifier struct {
	*coreturns.Store
	session *Delivery
}

// Finished persists completion before notifying subscribers and prompt settlement.
func (n finishedTurnNotifier) Finished(ctx context.Context, finish coreturns.Finish) (coreturns.Record, bool, error) {
	record, created, err := n.Store.Finished(ctx, finish)
	if err != nil {
		return record, created, err
	}
	if n.session.state.subscriptionSweep != nil {
		n.session.state.subscriptionSweep(n.session.identity.hostedThreadID)
	}
	// A Claude result may finish members other prompts' proof decided: the
	// owed member step runs under settleMu, never waiting for it here inside
	// the core loop. Raw store writes there notify nothing.
	if n.session.identity.provider == string(model.ProviderClaude) {
		n.session.RequestMemberStep()
	} else {
		n.session.finishJoinedAgentTurns(ctx, finish.TurnID)
	}
	return record, created, nil
}

func (providerRuntime *Runtime) recordCoreAsyncError(_ error) {
	if providerRuntime == nil || providerRuntime.view.liveProjection() == nil {
		return
	}
	providerRuntime.view.liveProjection().RecordDiagnostic(observability.ThreadDiagnostic{
		Code:    "async_session_event_failed",
		Message: "An asynchronous session event could not be applied.",
		Source:  "session",
	}, false)
	providerRuntime.view.liveProjection().nudgeMetaRefresh()
	providerRuntime.state.NotifyRuntime()
}

func (providerRuntime *Runtime) coreSession() *coresession.Session {
	if providerRuntime == nil {
		return nil
	}
	providerRuntime.coreMu.RLock()
	defer providerRuntime.coreMu.RUnlock()
	return providerRuntime.core
}

type coreOutboxPublisher struct{ session *Delivery }

// PublishOutbox publishes a prompt state change to its live view.
func (publisher coreOutboxPublisher) PublishOutbox(entry prompts.OutboxEntry) {
	publisher.session.publishTransactionalOutbox(entry)
}

func providerOperationFactories() map[model.Provider]providerOperationFactory {
	return map[model.Provider]providerOperationFactory{
		model.ProviderCodex:  codexProviderOperations,
		model.ProviderClaude: claudeProviderOperations,
		model.ProviderGrok:   grokProviderOperations,
	}
}

func unavailableProviderStart(context.Context, providers.StartRequest) (providers.Attachment, error) {
	return providers.Attachment{}, errors.New("provider start must run through the attachment coordinator")
}

func unavailableProviderResume(context.Context, providers.ResumeRequest) (providers.Attachment, error) {
	return providers.Attachment{}, errors.New("provider resume must run through the attachment coordinator")
}

func codexProviderOperations(providerRuntime *Runtime) providers.Operations {
	return providers.Operations{
		Start: unavailableProviderStart, Resume: unavailableProviderResume,
		Submit: providerRuntime.delivery.providerSubmitOperation(func(ctx context.Context, request providers.SubmissionRequest) (json.RawMessage, error) {
			// Codex saves this id on the prompt's user item, so each prompt
			// keeps its own identity even when several share one turn.
			clientID := SubmissionEventID(request.Outbox.HostedThreadID, request.Outbox.ActionID)
			if request.RunningTurnID != "" {
				return codexSteer(ctx, providerRuntime, request.RunningTurnID, clientID, request.Text)
			}
			turnParams, turnFacts := codexTurnParams(providerRuntime, request.Text)
			turnParams["clientUserMessageId"] = clientID
			causationID := providerRuntime.state.beginTurnCausation(turnFacts)
			var result json.RawMessage
			if err := providerRuntime.codex.callForSession(ctx, providerRuntime, "turn/start", turnParams, &result); err != nil {
				providerRuntime.state.abandonTurnCausation(causationID)
				return nil, err
			}
			return withSubmissionEvent(result, clientID)
		}),
		Interrupt: func(ctx context.Context, request providers.InterruptRequest) (providers.InterruptionResult, error) {
			threadID, turnID := scopeNativeThread(request.Scope), scopeNativeTurn(request.Scope)
			if threadID == "" || turnID == "" {
				return providers.InterruptionResult{Reason: "no turn in flight"}, nil
			}
			if err := providerRuntime.codex.interruptTurn(ctx, providerRuntime, threadID, turnID); err != nil {
				if reason, raced := interruptRaceReason(err); raced {
					return providers.InterruptionResult{Reason: reason}, nil
				}
				return providers.InterruptionResult{}, err
			}
			return providers.InterruptionResult{Interrupted: true}, nil
		},
		RespondInteraction: providerInteractionOperation(func(p interact.PendingInteraction, decision string, answers json.RawMessage) (any, *protocol.RPCError) {
			return interactionDecisionResult(p.Method, decision, p.Params, answers)
		}, func(p interact.PendingInteraction, result any, rpcErr *protocol.RPCError) error {
			return providerRuntime.codex.respondToInteraction(p.ID, result, rpcErr)
		}),
	}
}

func claudeProviderOperations(providerRuntime *Runtime) providers.Operations {
	return providers.Operations{
		Start: unavailableProviderStart, Resume: unavailableProviderResume,
		Submit: providerRuntime.delivery.providerSubmitOperation(func(ctx context.Context, request providers.SubmissionRequest) (json.RawMessage, error) {
			claude := providerRuntime.state.claudeHandle()
			if claude == nil {
				return nil, errors.New("session transport is still starting")
			}
			eventID := claudeSubmissionEventIDForOutbox(request.Outbox)
			// c2: core serializes callback evidence behind this submit, so the
			// legacy ingress latch can release at the transport acknowledgement.
			turnID, err := claude.sendTurnWithEventID(request.Text, eventID, ctx)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{
				"turn":  map[string]string{"id": turnID},
				"event": map[string]string{"id": eventID},
			})
		}),
		Interrupt: func(ctx context.Context, _ providers.InterruptRequest) (providers.InterruptionResult, error) {
			claude := providerRuntime.state.claudeHandle()
			if claude == nil {
				return providers.InterruptionResult{Reason: "no turn in flight"}, nil
			}
			if err := claude.interrupt(ctx); err != nil {
				return providers.InterruptionResult{}, err
			}
			return providers.InterruptionResult{Interrupted: true}, nil
		},
		RespondInteraction: providerInteractionOperation(claudeInteractionResult, func(p interact.PendingInteraction, result any, rpcErr *protocol.RPCError) error {
			claude := providerRuntime.state.claudeHandle()
			if claude == nil {
				return errClaudeControlClosed
			}
			if rpcErr != nil {
				return claude.respondControlError(p.ID, rpcErr.Message)
			}
			return claude.respondControl(p.ID, result)
		}),
	}
}

func grokProviderOperations(providerRuntime *Runtime) providers.Operations {
	return providers.Operations{
		Start: unavailableProviderStart, Resume: unavailableProviderResume,
		Submit: providerRuntime.delivery.providerSubmitOperation(func(ctx context.Context, request providers.SubmissionRequest) (json.RawMessage, error) {
			grok := providerRuntime.state.grokHandle()
			if grok == nil {
				return nil, errors.New("session transport is still starting")
			}
			return grok.SendTurn(ctx, request.Text, request.Outbox.ActionID)
		}),
		Interrupt: func(_ context.Context, _ providers.InterruptRequest) (providers.InterruptionResult, error) {
			grok := providerRuntime.state.grokHandle()
			if grok == nil {
				return providers.InterruptionResult{Reason: "no turn in flight"}, nil
			}
			if err := grok.interrupt(); err != nil {
				return providers.InterruptionResult{}, err
			}
			return providers.InterruptionResult{Interrupted: true}, nil
		},
		RespondInteraction: providerInteractionOperation(grokInteractionResult, func(p interact.PendingInteraction, result any, rpcErr *protocol.RPCError) error {
			grok := providerRuntime.state.grokHandle()
			if grok == nil {
				return errors.New("grok ACP proxy is not connected")
			}
			return grok.client.RespondToServerRequest(p.ID, result, rpcErr)
		}),
	}
}

type providerInteractionTranslator func(interact.PendingInteraction, string, json.RawMessage) (any, *protocol.RPCError)
type providerInteractionResponder func(interact.PendingInteraction, any, *protocol.RPCError) error

func providerInteractionOperation(translate providerInteractionTranslator, respond providerInteractionResponder) func(context.Context, providers.InteractionResponse) (model.Batch, error) {
	return func(_ context.Context, response providers.InteractionResponse) (model.Batch, error) {
		pending := interact.PendingInteraction{
			ID: response.RequestID, Method: response.Method, Kind: string(response.Kind),
			ThreadID: scopeNativeThread(response.Request.Scope), TurnID: scopeNativeTurn(response.Request.Scope),
			Params: append(json.RawMessage(nil), response.Params...),
		}
		result, rpcErr := translate(pending, response.Decision, response.Answers)
		if rpcErr != nil && result == nil {
			return model.Batch{}, rpcErr
		}
		return model.Batch{}, respond(pending, result, rpcErr)
	}
}

func (delivery *Delivery) providerSubmitOperation(send func(context.Context, providers.SubmissionRequest) (json.RawMessage, error)) func(context.Context, providers.SubmissionRequest) (providers.SubmissionResult, error) {
	return func(ctx context.Context, request providers.SubmissionRequest) (providers.SubmissionResult, error) {
		var receipt json.RawMessage
		var err error
		if delivery.turnSender != nil {
			receipt, err = delivery.turnSender(ctx, request.Text)
		} else {
			receipt, err = send(ctx, request)
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				delivery.expiryCounter().record(delivery.identity.provider)
			}
			state := prompts.Uncertain
			nativeMissing := delivery.runtime.codexZeroTurnNativeMissing(request.Outbox.ActionID, err)
			var rpcErr *protocol.RPCError
			// A definite refusal parks the prompt: Nano keeps it with its
			// text for the person's Resend or Discard, since the sender may
			// never see the answer that says so.
			switch {
			case request.RunningTurnID != "":
				// Only the provider's answer makes a running-turn refusal
				// definite; a lost connection may follow delivery, so the
				// prompt stays uncertain even if it was queued first.
				if errors.As(err, &rpcErr) {
					state = prompts.Parked
				}
			case errors.Is(err, context.DeadlineExceeded):
				// The provider may take it after Nano stopped waiting; its
				// echo can still bind it (expectCodexEcho), so it is not parked.
			case request.PreviouslyQueued || nativeMissing || errors.As(err, &rpcErr):
				state = prompts.Parked
			}
			failureCode := ""
			if nativeMissing {
				failureCode = codexNativeMissing
			}
			if errors.Is(err, errRunningTurnEnded) {
				failureCode = providers.FailureTurnEnded
			}
			return providers.SubmissionResult{State: state, Receipt: receipt, Reason: err.Error(), FailureCode: failureCode}, nil
		}
		turnID := providerTurnIDFromStart(receipt)
		if turnID == "" {
			return providers.SubmissionResult{State: prompts.Accepted, Receipt: receipt}, nil
		}
		binding, supersedesNativeThreadID := delivery.runtime.submissionBinding(request.Scope, turnID, providerEventIDFromStart(receipt))
		return providers.SubmissionResult{
			State: prompts.Accepted, Binding: &binding,
			SupersedesNativeThreadID: supersedesNativeThreadID, Receipt: receipt,
		}, nil
	}
}

// submissionBinding binds a prompt sent to scope to the provider turn that
// took it. A provider can reveal its durable native thread while a submission
// is in flight, so the binding names that identity rather than the
// provisional one the caller captured, and reports the one it supersedes.
func (providerRuntime *Runtime) submissionBinding(scope model.Scope, turnID, eventID string) (prompts.Binding, string) {
	supersedes := ""
	if nativeThreadID := providerRuntime.state.ThreadID(); nativeThreadID != "" {
		if scope.NativeThreadID != nil && *scope.NativeThreadID != nativeThreadID {
			supersedes = *scope.NativeThreadID
		}
		scope.NativeThreadID = &nativeThreadID
	}
	scope.Kind = model.ScopeTurn
	scope.NativeTurnID = &turnID
	return prompts.Binding{Scope: scope, EventID: eventID}, supersedes
}

// errRunningTurnEnded is Codex's definite answer that no running turn takes
// input now (it ended, or is pending or in plan mode), so the prompt was not
// delivered.
var errRunningTurnEnded = errors.New("running turn ended")

// codexSteer delivers a prompt into the running turn. expectedTurnId makes
// Codex refuse rather than join a different turn. The receipt has the same
// turn and event shape as a started turn's.
func codexSteer(ctx context.Context, providerRuntime *Runtime, turnID, clientID, text string) (json.RawMessage, error) {
	params := map[string]any{
		"threadId": providerRuntime.state.ThreadID(), "expectedTurnId": turnID, "clientUserMessageId": clientID,
		"input": []map[string]any{{"type": "text", "text": text}},
	}
	var result struct {
		TurnID string `json:"turnId"`
	}
	if err := providerRuntime.codex.callForSession(ctx, providerRuntime, "turn/steer", params, &result); err != nil {
		var rpcErr *protocol.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Message == codexNoActiveTurnToSteer {
			return nil, fmt.Errorf("%w: %w", errRunningTurnEnded, err)
		}
		return nil, err
	}
	return json.Marshal(map[string]any{"turn": map[string]string{"id": result.TurnID}, "event": map[string]string{"id": clientID}})
}

// codexNoActiveTurnToSteer is the message Codex app-server gives when no turn
// takes input (rust-v0.159.2 turn_processor.rs); no error code distinguishes it.
const codexNoActiveTurnToSteer = "no active turn to steer"

// withSubmissionEvent adds the submission's event id to an accepted turn's
// receipt, which becomes the prompt's exact event binding. A receipt that is
// not an object stays as received: the turn started either way.
func withSubmissionEvent(receipt json.RawMessage, eventID string) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(receipt, &fields) != nil || fields == nil {
		return receipt, nil
	}
	fields["event"], _ = json.Marshal(map[string]string{"id": eventID})
	return json.Marshal(fields)
}

func claudeSubmissionEventIDForOutbox(entry prompts.OutboxEntry) string {
	if entry.Binding != nil && entry.Binding.EventID != "" {
		return entry.Binding.EventID
	}
	return SubmissionEventID(entry.HostedThreadID, entry.ActionID)
}

func (providerRuntime *Runtime) deliveryScope() model.Scope {
	nativeThreadID := providerRuntime.state.ThreadID()
	return model.Scope{Kind: model.ScopeTurn, Provider: model.Provider(providerRuntime.identity.provider), NativeThreadID: &nativeThreadID}
}

func (providerRuntime *Runtime) deliverySettings() providers.Settings {
	core := providerRuntime.coreSession()
	if core == nil {
		return providers.Settings{}
	}
	capabilities := core.TurnCapabilities()
	modelName, effort, serviceTier := providerRuntime.state.turnOptions()
	var settings providers.Settings
	if modelName != "" && (capabilities.ModelChange == providers.AtNextTurn || capabilities.ModelChange == providers.InSession) {
		settings.Model = &modelName
	}
	if effort != "" && (capabilities.EffortChange == providers.AtNextTurn || capabilities.EffortChange == providers.InSession) {
		settings.Effort = &effort
	}
	if serviceTier != "" && capabilities.ServiceTier {
		settings.ServiceTier = &serviceTier
	}
	return settings
}

// acceptsRunningTurnInput reports whether the provider takes an ordinary
// prompt into a working turn. It is false until the core session exists.
func (providerRuntime *Runtime) acceptsRunningTurnInput() bool {
	core := providerRuntime.coreSession()
	return core != nil && core.TurnCapabilities().RunningTurnInput
}

// CorePromptInFlight reports whether the prompt owner has an active delivery claim.
func (providerRuntime *Runtime) CorePromptInFlight() bool {
	core := providerRuntime.coreSession()
	return core != nil && core.Snapshot().InFlightAction != ""
}

func scopeNativeThread(scope model.Scope) string {
	if scope.NativeThreadID == nil {
		return ""
	}
	return *scope.NativeThreadID
}

func scopeNativeTurn(scope model.Scope) string {
	if scope.NativeTurnID == nil {
		return ""
	}
	return *scope.NativeTurnID
}

// Runtime owns provider handles, handshake serialization and the core event coordinator.
type Runtime struct {
	identity *attachmentIdentity
	wire     *Wire
	delivery *Delivery
	state    *State
	view     *View

	codex            *CodexDaemon // codex provider: shared daemon client
	reasoningSummary string       // codex: turn/start summary level
	completedTurns   func(context.Context) (*coreturns.Store, error)
	core             *coresession.Session
	coreMu           sync.RWMutex
	coreOnce         sync.Once
	coreErr          error
	providerAdapter  providers.Adapter
	// A daemon reconnect may begin while the previous thread handshake is still
	// unwinding its response. Serialize the whole bind transaction per session so
	// its identity, attachment record, and ready transition cannot interleave.
	codexHandshakeMu sync.Mutex
}

func (providerRuntime *Runtime) daemon() *CodexDaemon              { return providerRuntime.codex }
func (providerRuntime *Runtime) coordinator() *coresession.Session { return providerRuntime.core }
func (providerRuntime *Runtime) completedTurnReader() func(context.Context) (*coreturns.Store, error) {
	return providerRuntime.completedTurns
}
func (providerRuntime *Runtime) summaryLevel() string { return providerRuntime.reasoningSummary }
