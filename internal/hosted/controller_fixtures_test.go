package hosted

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
	hostedprompts "github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

func testController(t *testing.T, store *hostedstore.ExecutionStore) *Controller {
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
	c := &Controller{Executions: store, Metadata: control, Queue: queue, Sessions: func() *sessions.Registry { r := sessions.NewRegistry(); return &r }(), Waiting: func(string) string { return "" }, Reattach: func(string) {},
		PromptWithUnreadFooter: func(_ string, text string) (string, error) { return text, nil },
		NotifyRuntime:          func() {}, RefreshRuntime: func() {}, PublishRelationshipChange: func() {},
		FeatureFooter:  func(string) (string, bool) { return "", false },
		CloseLifecycle: func(_ string, close func() error) error { return close() },
	}
	c.Views = func() *ViewService {
		return &ViewService{Executions: store, Metadata: control, Sessions: &sessions.Registry{}, Queue: queue, Waiting: func(string) string { return "" }, ResolveRecord: c.ResolveHostedRecord}
	}
	return c
}

// submitHostedTurn is the single ordinary-turn path used by the public route
// and Nano's notification dispatcher. A persisted pre-delivery operation with
// no outbox or send-journal evidence is safe to enqueue again after a crash.
func (s *Controller) submitHostedTurn(hostedThreadID, actionID, prompt string) (ownedthreads.TurnRecord, bool, error) {
	return s.submitHostedTurnWithOptions(hostedThreadID, actionID, prompt, false)
}

// submitHostedTurnWithOptions is for Nano-internal system actions only. Public
// agent submissions must enter through HandleTurnSubmit, which admits a
// complete sender before any operation can exist.
func (s *Controller) submitHostedTurnWithOptions(hostedThreadID, actionID, prompt string, requireIdle bool) (ownedthreads.TurnRecord, bool, error) {
	return s.submitHostedTurnGuardedWithOptions(hostedThreadID, actionID, prompt, requireIdle, nil)
}

// submitHostedTurnGuardedWithOptions lets a Nano-internal caller validate the
// current hosted-operation set atomically with creation of its operation. The
// guard is never exposed by the public turn API.
func (s *Controller) submitHostedTurnGuardedWithOptions(hostedThreadID, actionID, prompt string, requireIdle bool, guard hostedstore.AgentTurnCreationGuard) (ownedthreads.TurnRecord, bool, error) {
	if !strings.HasPrefix(actionID, "nano:") {
		return ownedthreads.TurnRecord{}, false, errors.New("agent-origin submissions require senderThreadRef through the public hosted-turn endpoint")
	}
	return s.submitHostedTurnFull(hostedThreadID, actionID, hostedTurnSubmission{
		Prompt: prompt, Delivered: prompt + "\n\n" + senderFooter(hostedstore.TurnAttribution{Origin: string(hostedprompts.SystemOrigin)}), RequireIdle: requireIdle, Origin: string(hostedprompts.SystemOrigin), Attribution: hostedstore.TurnAttribution{Origin: string(hostedprompts.SystemOrigin)}, CreateGuard: guard,
	})
}
