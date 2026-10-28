package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/hosted"
)

// The fixtures exercise hosted admission while stubbing the provider actions.
// A root fixture supplies an empty lineage tuple, as the human creation path does.
type fixtureAdmissionRuntime struct {
	hosted.ControlRuntime
	sender    agentSessionRecord
	accepting bool
}

func (r fixtureAdmissionRuntime) ResolveLiveRecord(ref string, _ func(string) (agentSessionRecord, bool)) (agentSessionRecord, bool) {
	if ref == "fixture:root" {
		return agentSessionRecord{}, true
	}
	if ref == "fixture:sender" {
		return r.sender, true
	}
	return agentSessionRecord{}, false
}
func (r fixtureAdmissionRuntime) Reattach(string)       {}
func (r fixtureAdmissionRuntime) Waiting(string) string { return "" }
func (r fixtureAdmissionRuntime) Admission(string) hosted.AdmissionSession {
	if r.accepting {
		return fixtureAdmission{}
	}
	return nil
}
func (r fixtureAdmissionRuntime) HasDeliveryEvidence(string, string) bool  { return false }
func (r fixtureAdmissionRuntime) DeliveryWasAttempted(string, string) bool { return false }

type fixtureAdmission struct{ hosted.AdmissionSession }

func (fixtureAdmission) StartPrepared(string, string, string, string, string, bool) (bool, int, string, error) {
	return true, 0, "", nil
}
func (fixtureAdmission) NotifyRuntime()                   {}
func (fixtureAdmission) QueuedText(string) (string, bool) { return "", false }

func fixtureAdmissionController(store *agentExecutionStore, runtime fixtureAdmissionRuntime) *hosted.Controller {
	s := &server{agentExecutions: store, controlMetadata: store.Control(), Registry: newSessionRegistry()}
	c := &hosted.Controller{Executions: store, Metadata: store.Control(), Runtime: runtime, Views: s.hostedViews,
		PromptWithUnreadFooter: func(_ string, text string) (string, error) { return text, nil },
		FeatureFooter:          func(string) (string, bool) { return "", false },
	}
	c.PrepareCreation = func(req hosted.CreationRequest) (hosted.CreationPlan, error) {
		return hosted.CreationPlan{
			ClientSessionID: req.ClientSessionID, Provider: req.Provider, Mode: "start", PresentationMode: req.PresentationMode, SidebarPlacement: req.SidebarPlacement,
			Attach: func(record agentSessionRecord, _ bool) (string, error) { return record.ID, nil },
		}, nil
	}
	c.ResolveIdentity = func(context.Context, string) (hosted.IdentityResolution, error) {
		return hosted.IdentityResolution{MatchCount: 1, HostedThreadID: runtime.sender.ID, Provider: runtime.sender.Provider, NativeThreadID: runtime.sender.NativeThreadID, DisplayName: "fixture sender"}, nil
	}
	return c
}

func fixtureCreateHosted(store *agentExecutionStore, clientID, hash, provider, mode, placement string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*orchestrationAssignment) (agentSessionRecord, bool, error) {
	ref := invoker.ID
	if ref == "" {
		ref = "fixture:root"
	}
	req := hosted.CreationRequest{Provider: provider, Mode: "start", ClientSessionID: clientID, BaseInstructions: hash, PresentationMode: mode, SidebarPlacement: placement, InvokerThreadRef: ref, Subscriptions: choices}
	if len(assignment) > 0 {
		req.Assignment = assignment[0]
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return agentSessionRecord{}, false, err
	}
	response := httptest.NewRecorder()
	fixtureAdmissionController(store, fixtureAdmissionRuntime{}).HandleCreate(response, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads", bytes.NewReader(raw)))
	if response.Code >= 400 {
		return agentSessionRecord{}, false, fmt.Errorf("fixture creation: %s", response.Body.String())
	}
	var view agentSessionView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		return agentSessionRecord{}, false, err
	}
	record, _ := store.Session(view.ID)
	return record, response.Code == http.StatusCreated, nil
}

func fixtureAdmitTurn(store *agentExecutionStore, id, action, prompt string) (agentTurnRecord, bool, error) {
	runtime := fixtureAdmissionRuntime{accepting: true, sender: agentSessionRecord{ID: "nht_fixture_sender", Provider: "codex", NativeThreadID: "fixture-native"}}
	request := map[string]any{"prompt": prompt, "clientActionId": action, "senderThreadRef": "fixture:sender"}
	if strings.HasPrefix(action, "nano:") {
		request["origin"] = "system"
		delete(request, "senderThreadRef")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return agentTurnRecord{}, false, err
	}
	response := httptest.NewRecorder()
	fixtureAdmissionController(store, runtime).HandleTurnSubmit(response, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+id+"/turns", bytes.NewReader(raw)), id)
	if response.Code >= 400 {
		return agentTurnRecord{}, false, fmt.Errorf("fixture turn: %s", response.Body.String())
	}
	var view agentTurnView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		return agentTurnRecord{}, false, err
	}
	record, _ := store.Turn(view.ID)
	return record, response.Code == http.StatusAccepted, nil
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
