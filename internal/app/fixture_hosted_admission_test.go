package app

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// Fixture rows enter through the canonical durable store used by admission.
// Provider delivery is not part of these historical read fixtures.
func fixtureCreateHosted(store *agentExecutionStore, clientID, hash, provider, mode, placement string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*orchestrationAssignment) (agentSessionRecord, bool, error) {
	req := hosted.CreationRequest{Provider: provider, Mode: "start", ClientSessionID: clientID, BaseInstructions: hash, PresentationMode: mode, SidebarPlacement: placement, InvokerThreadRef: invoker.ID, Subscriptions: choices}
	if len(assignment) > 0 {
		req.Assignment = assignment[0]
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return agentSessionRecord{}, false, err
	}
	return store.BeginNamedSessionWithSubscriptionsAndPlacement(clientID, hostedstore.AgentHash(raw), provider, mode, placement, "", invoker, choices, assignment...)
}

func fixtureAdmitTurn(store *agentExecutionStore, id, action, prompt string) (agentTurnRecord, bool, error) {
	attribution := turnAttribution{Origin: "agent", SenderHostedThreadID: "nht_fixture_sender", SenderProvider: "codex", SenderNativeThreadID: "fixture-native", SenderName: "fixture sender"}
	if strings.HasPrefix(action, "nano:") {
		attribution = turnAttribution{Origin: "system"}
	}
	return store.BeginTurnAttributed(id, action, hostedstore.AgentHash([]byte(prompt)), attribution)
}

// Historical rows include attribution states that current public admission rejects.
// Seed those captured fields after ordinary admission; consumers read the durable row.
func fixtureHistoricalTurn(store *agentExecutionStore, id, action, prompt string, attribution turnAttribution) (agentTurnRecord, bool, error) {
	record, created, err := fixtureAdmitTurn(store, id, action, prompt)
	if err != nil {
		return record, created, err
	}
	_, err = store.Control().CanonicalDatabase().Exec(`UPDATE hosted_turn_operations SET origin=?,sender_hosted_id=?,sender_native_id=?,sender_provider=?,sender_name=?,sender_submitted_at=? WHERE operation_id=?`, attribution.Origin, attribution.SenderHostedThreadID, attribution.SenderNativeThreadID, attribution.SenderProvider, attribution.SenderName, attribution.SenderSubmittedAt, record.ID)
	record.Origin = attribution.Origin
	record.SenderHostedThreadID = attribution.SenderHostedThreadID
	record.SenderNativeThreadID = attribution.SenderNativeThreadID
	record.SenderProvider = attribution.SenderProvider
	record.SenderName = attribution.SenderName
	record.SenderSubmittedAt = attribution.SenderSubmittedAt
	return record, created, err
}
