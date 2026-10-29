package sessions

import (
	"errors"
	"testing"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// The app sibling verifies committed metadata and duplicate notifications with
// the canonical store. Rejected provider writes must also publish degradation.
func TestPostureRejectionFailurePublishesSessionDegradation(t *testing.T) {
	metadata := &rejectedPostureWrite{}
	notified := 0
	sess := New(Config{ID: "nano-1", HostedThreadID: "hosted", Metadata: metadata, RuntimeNotify: func() { notified++ }})
	sess.recordProviderRejection("test_rejection", providerCausationSnapshot{postureFieldServiceTier: {EventID: 1, Value: "priority"}})
	if notified != 1 || !sess.metadataDegraded {
		t.Fatalf("failed provider rejection did not publish degraded state: notifications=%d degraded=%v", notified, sess.metadataDegraded)
	}
	if len(metadata.events) != 1 || metadata.events[0].CausationEventID == nil || *metadata.events[0].CausationEventID != 1 || metadata.events[0].Disposition != metadataDispositionRejected {
		t.Fatalf("provider rejection lost its causal request: %+v", metadata.events)
	}
}

type rejectedPostureWrite struct {
	Metadata
	events []hostedstore.MetadataEvent
}

func (m *rejectedPostureWrite) AppendEvent(event hostedstore.MetadataEvent) (hostedstore.MetadataEvent, error) {
	m.events = append(m.events, event)
	return hostedstore.MetadataEvent{}, errors.New("injected rejected write")
}
