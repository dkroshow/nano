package hosted

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
)

type controllerRuntime struct{ ControlRuntime }

func (controllerRuntime) Admission(string) AdmissionSession { return nil }
func (controllerRuntime) ResolveLiveRecord(string, func(string) (agentSessionRecord, bool)) (agentSessionRecord, bool) {
	return agentSessionRecord{}, false
}
func (controllerRuntime) Waiting(string) string                    { return "" }
func (controllerRuntime) Reattach(string)                          {}
func (controllerRuntime) HasDeliveryEvidence(string, string) bool  { return false }
func (controllerRuntime) DeliveryWasAttempted(string, string) bool { return false }

type emptyViewSessions struct{ ViewSessions }

func (emptyViewSessions) Session(string) (SessionSnapshot, bool)   { return SessionSnapshot{}, false }
func (emptyViewSessions) Turn(string, bool) (TurnSnapshot, bool)   { return TurnSnapshot{}, false }
func (emptyViewSessions) Attachment(string) (AttachmentView, bool) { return AttachmentView{}, false }
func (emptyViewSessions) Availability(string) (AvailabilitySnapshot, bool) {
	return AvailabilitySnapshot{}, false
}
func (emptyViewSessions) HostedIDs() []string              { return nil }
func (emptyViewSessions) QueuePosition(string, string) int { return 0 }

type emptyViewNames struct{}

func (emptyViewNames) DurableName(string, string) (string, bool) { return "", false }
func (emptyViewNames) DurableNameRef(string, string) (MetadataName, bool) {
	return MetadataName{}, false
}

func testController(t *testing.T, store *ExecutionStore) *Controller {
	t.Helper()
	control := store.Control()
	migrateTestControl(t, control)
	turns, err := prompts.OpenStore(context.Background(), control.CanonicalDatabase())
	if err != nil {
		t.Fatal(err)
	}
	queue, err := prompts.NewLegacyQueue(control.CanonicalDatabase(), "", turns)
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Executions: store, Metadata: control, Queue: queue, Runtime: controllerRuntime{},
		PromptWithUnreadFooter: func(_ string, text string) (string, error) { return text, nil },
		NotifyRuntime:          func() {}, RefreshRuntime: func() {}, PublishRelationshipChange: func() {},
		FeatureFooter:  func(string) (string, bool) { return "", false },
		CloseLifecycle: func(_ string, close func() error) error { return close() },
	}
	c.Views = func() *ViewService {
		return &ViewService{Executions: store, Metadata: control, Sessions: emptyViewSessions{}, Names: emptyViewNames{}, Waiting: func(string) string { return "" }, ResolveRecord: c.ResolveHostedRecord}
	}
	return c
}

type guardRuntime struct {
	ControlRuntime
	session *guardAdmission
}

func (a guardRuntime) Admission(string) AdmissionSession        { return a.session }
func (a guardRuntime) HasDeliveryEvidence(string, string) bool  { return a.session.sent > 0 }
func (a guardRuntime) DeliveryWasAttempted(string, string) bool { return a.session.sent > 0 }

type guardAdmission struct {
	store   *ExecutionStore
	prompts chan string
	sent    int
}

func (a *guardAdmission) WithIdleDelivery(_ bool, deliver func() error) (string, error) {
	return "", deliver()
}
func (a *guardAdmission) StartPrepared(action, text, presentation, hash, origin string, next bool) (bool, int, string, error) {
	a.sent++
	a.prompts <- text
	record, _ := a.store.TurnForTransportAction(action)
	err := a.store.MarkTurnDelivery(record.ID, "provider-turn", agentTurnRunning, nil)
	return true, 0, "", err
}
func (a *guardAdmission) StartLockedPrepared(action, text, presentation, hash, origin string, next bool) (bool, int, string, error) {
	return a.StartPrepared(action, text, presentation, hash, origin, next)
}
func (a *guardAdmission) QueuedText(string) (string, bool) { return "", false }
func (a *guardAdmission) NotifyRuntime()                   {}

// submitHostedTurn is the single ordinary-turn path used by the public route
// and Nano's notification dispatcher. A persisted pre-delivery operation with
// no outbox or send-journal evidence is safe to enqueue again after a crash.
func (s *Controller) submitHostedTurn(hostedThreadID, actionID, prompt string) (agentTurnRecord, bool, error) {
	return s.submitHostedTurnWithOptions(hostedThreadID, actionID, prompt, false)
}

// submitHostedTurnWithOptions is for Nano-internal system actions only. Public
// agent submissions must enter through HandleTurnSubmit, which admits a
// complete sender before any operation can exist.
func (s *Controller) submitHostedTurnWithOptions(hostedThreadID, actionID, prompt string, requireIdle bool) (agentTurnRecord, bool, error) {
	return s.submitHostedTurnGuardedWithOptions(hostedThreadID, actionID, prompt, requireIdle, nil)
}

// submitHostedTurnGuardedWithOptions lets a Nano-internal caller validate the
// current hosted-operation set atomically with creation of its operation. The
// guard is never exposed by the public turn API.
func (s *Controller) submitHostedTurnGuardedWithOptions(hostedThreadID, actionID, prompt string, requireIdle bool, guard agentTurnCreationGuard) (agentTurnRecord, bool, error) {
	if !strings.HasPrefix(actionID, "nano:") {
		return agentTurnRecord{}, false, errors.New("agent-origin submissions require senderThreadRef through the public hosted-turn endpoint")
	}
	return s.submitHostedTurnFull(hostedThreadID, actionID, hostedTurnSubmission{
		Prompt: prompt, Delivered: prompt + "\n\n" + senderFooter(TurnAttribution{Origin: turnOriginSystem}), RequireIdle: requireIdle, Origin: turnOriginSystem, Attribution: TurnAttribution{Origin: turnOriginSystem}, CreateGuard: guard,
	})
}
