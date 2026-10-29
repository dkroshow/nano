package hosted

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

func newControlAgentExecutionStore(control *hostedstore.MetadataStore, runID string) (*hostedstore.ExecutionStore, error) {
	return hostedstore.NewExecutionStore(control, runID, subscriptions.IsFeatureAction)
}

const (
	sessionStatusExited     = hostedstore.StatusExited
	postureFieldEffort      = hostedstore.PostureFieldEffort
	postureFieldModel       = hostedstore.PostureFieldModel
	postureFieldServiceTier = hostedstore.PostureFieldServiceTier
	postureSourceCreation   = hostedstore.PostureSourceCreation
	postureSourceOperator   = hostedstore.PostureSourceOperator
	postureSourceProvider   = hostedstore.PostureSourceProvider
)

func assertCurrentValue(t *testing.T, store *hostedstore.MetadataStore, hostedID, field, wantValue, wantDisposition string) {
	t.Helper()
	var value, disposition string
	if err := store.CanonicalDatabase().QueryRow(`SELECT effective_value_json,effective_disposition FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, field).Scan(&value, &disposition); err != nil {
		t.Fatal(err)
	}
	if value != wantValue || disposition != wantDisposition {
		t.Fatalf("effective = %s/%s, want %s/%s", value, disposition, wantValue, wantDisposition)
	}
}

func assertNoEffective(t *testing.T, store *hostedstore.MetadataStore, hostedID, field string) {
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
func fixtureBeginSession(s *hostedstore.ExecutionStore, clientSessionID, requestHash, provider, presentationMode string) (ownedthreads.SessionRecord, bool, error) {
	return fixtureBeginSessionWithSubscriptions(s, clientSessionID, requestHash, provider, presentationMode, ownedthreads.SessionRecord{}, subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None})
}

// beginSessionWithInvoker adds the durable invocation edge used by agent API
// creation. The edge names an ordinary hosted thread, not a provider-native
// parent, so it remains valid when Codex invokes Claude or Grok (and vice
// versa). beginSession remains the zero-edge helper for human-created sessions
// and existing tests.
func fixtureBeginSessionWithInvoker(s *hostedstore.ExecutionStore, clientSessionID, requestHash, provider, presentationMode string, invoker ownedthreads.SessionRecord, assignment ...*hostedstore.Assignment) (ownedthreads.SessionRecord, bool, error) {
	return fixtureBeginSessionWithSubscriptions(s, clientSessionID, requestHash, provider, presentationMode, invoker, subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None}, assignment...)
}

func fixtureBeginSessionWithSubscriptions(s *hostedstore.ExecutionStore, clientSessionID, requestHash, provider, presentationMode string, invoker ownedthreads.SessionRecord, choices subscriptions.Choices, assignment ...*hostedstore.Assignment) (ownedthreads.SessionRecord, bool, error) {
	return fixtureBeginSessionWithSubscriptionsAndPlacement(s, clientSessionID, requestHash, provider, presentationMode, ownedthreads.SidebarPlacementInvoker, invoker, choices, assignment...)
}

func fixtureBeginSessionWithSubscriptionsAndPlacement(s *hostedstore.ExecutionStore, clientSessionID, requestHash, provider, presentationMode, sidebarPlacement string, invoker ownedthreads.SessionRecord, choices subscriptions.Choices, assignment ...*hostedstore.Assignment) (ownedthreads.SessionRecord, bool, error) {
	return s.BeginNamedSessionWithSubscriptionsAndPlacement(clientSessionID, requestHash, provider, presentationMode, sidebarPlacement, "", invoker, choices, assignment...)
}

// ensureSession gives UI-created and resumed sessions the same durable
// execution identity as API-created sessions. It never creates a caller alias
// and therefore carries no creation-idempotency semantics.
func fixtureEnsureSession(s *hostedstore.ExecutionStore, id, provider, presentationMode string) (ownedthreads.SessionRecord, error) {
	return fixtureEnsureSessionWithIdentity(s, id, provider, presentationMode, "", "", "")
}

// ensureSessionWithIdentity is the human New Window counterpart to API
// creation. In control mode it creates (or validates) immutable hosted
// identity and its execution-runtime row in one transaction before any
// provider transport can be started.
func fixtureEnsureSessionWithIdentity(s *hostedstore.ExecutionStore, id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider string) (ownedthreads.SessionRecord, error) {
	return s.EnsureNamedSessionWithIdentity(id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider, "")
}

func fixtureBeginTurn(s *hostedstore.ExecutionStore, sessionID, actionID, promptHash string) (ownedthreads.TurnRecord, bool, error) {
	return s.BeginTurnAttributed(sessionID, actionID, promptHash, hostedstore.TurnAttribution{})
}
