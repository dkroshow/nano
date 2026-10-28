package sessions

import (
	"errors"
	"testing"
)

// The app sibling verifies committed metadata and duplicate notifications with
// the canonical store. Rejected provider writes must also publish degradation.
func TestRecordPosturePublishesRuntimeOnlyAfterCommittedMutation(t *testing.T) {
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
	events []metadataEvent
}

func (m *rejectedPostureWrite) AppendEvent(event metadataEvent) (metadataEvent, error) {
	m.events = append(m.events, event)
	return metadataEvent{}, errors.New("injected rejected write")
}
