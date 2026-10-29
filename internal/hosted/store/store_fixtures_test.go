package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/features/subscriptions"
)

func newControlAgentExecutionStore(control *MetadataStore, runID string) (*ExecutionStore, error) {
	return NewExecutionStore(control, runID, subscriptions.IsFeatureAction)
}

const (
	sessionStatusExited     = StatusExited
	postureFieldEffort      = PostureFieldEffort
	postureFieldModel       = PostureFieldModel
	postureFieldServiceTier = PostureFieldServiceTier
	postureSourceCreation   = PostureSourceCreation
	postureSourceOperator   = PostureSourceOperator
	postureSourceProvider   = PostureSourceProvider
)

func assertCurrentValue(t *testing.T, store *MetadataStore, hostedID, field, wantValue, wantDisposition string) {
	t.Helper()
	var value, disposition string
	if err := store.CanonicalDatabase().QueryRow(`SELECT effective_value_json,effective_disposition FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, field).Scan(&value, &disposition); err != nil {
		t.Fatal(err)
	}
	if value != wantValue || disposition != wantDisposition {
		t.Fatalf("effective = %s/%s, want %s/%s", value, disposition, wantValue, wantDisposition)
	}
}

func assertNoEffective(t *testing.T, store *MetadataStore, hostedID, field string) {
	t.Helper()
	var value sql.NullString
	if err := store.CanonicalDatabase().QueryRow(`SELECT effective_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, field).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value.Valid {
		t.Fatalf("unexpected effective value %s", value.String)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

// beginSession idempotently creates one ordinary hosted-thread record. The
// caller id is only an optional creation alias; the server-owned record id is
// what every control surface uses after creation.
func (s *ExecutionStore) beginSession(clientSessionID, requestHash, provider, presentationMode string) (agentSessionRecord, bool, error) {
	return s.beginSessionWithSubscriptions(clientSessionID, requestHash, provider, presentationMode, agentSessionRecord{}, subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None})
}

// beginSessionWithInvoker adds the durable invocation edge used by agent API
// creation. The edge names an ordinary hosted thread, not a provider-native
// parent, so it remains valid when Codex invokes Claude or Grok (and vice
// versa). beginSession remains the zero-edge helper for human-created sessions
// and existing tests.
func (s *ExecutionStore) beginSessionWithInvoker(clientSessionID, requestHash, provider, presentationMode string, invoker agentSessionRecord, assignment ...*Assignment) (agentSessionRecord, bool, error) {
	return s.beginSessionWithSubscriptions(clientSessionID, requestHash, provider, presentationMode, invoker, subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None}, assignment...)
}

func (s *ExecutionStore) beginSessionWithSubscriptions(clientSessionID, requestHash, provider, presentationMode string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*Assignment) (agentSessionRecord, bool, error) {
	return s.beginSessionWithSubscriptionsAndPlacement(clientSessionID, requestHash, provider, presentationMode, sidebarPlacementInvoker, invoker, choices, assignment...)
}

func (s *ExecutionStore) beginSessionWithSubscriptionsAndPlacement(clientSessionID, requestHash, provider, presentationMode, sidebarPlacement string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*Assignment) (agentSessionRecord, bool, error) {
	return s.BeginNamedSessionWithSubscriptionsAndPlacement(clientSessionID, requestHash, provider, presentationMode, sidebarPlacement, "", invoker, choices, assignment...)
}

// ensureSession gives UI-created and resumed sessions the same durable
// execution identity as API-created sessions. It never creates a caller alias
// and therefore carries no creation-idempotency semantics.
func (s *ExecutionStore) ensureSession(id, provider, presentationMode string) (agentSessionRecord, error) {
	return s.ensureSessionWithIdentity(id, provider, presentationMode, "", "", "")
}

// ensureSessionWithIdentity is the human New Window counterpart to API
// creation. In control mode it creates (or validates) immutable hosted
// identity and its execution-runtime row in one transaction before any
// provider transport can be started.
func (s *ExecutionStore) ensureSessionWithIdentity(id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider string) (agentSessionRecord, error) {
	return s.EnsureNamedSessionWithIdentity(id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider, "")
}

func (s *ExecutionStore) beginTurn(sessionID, actionID, promptHash string) (agentTurnRecord, bool, error) {
	return s.BeginTurnAttributed(sessionID, actionID, promptHash, TurnAttribution{})
}
